//! `--follow` — режим непрерывного дозакачивания («хвостовой» ingest).
//!
//! Единый контракт всех участников bake-off:
//!
//! ```text
//! tj-agent-rs --follow --input <dir> --sink clickhouse[:<dsn>] --state <dir>
//!             --stop-file <path> [--poll-ms 500] [--idle-close-ms 2000]
//!             [--threads N] [--stats-json <path>]
//! ```
//!
//! - **Стартовый проход**: существующие файлы дочитываются от чекпоинтов, затем
//!   агент продолжает следить за каталогом.
//! - **Обнаружение**: каждые poll-ms — рекурсивный обход `--input` ТОЛЬКО
//!   ради новых `*.log` (подхват нового файла < 2 с). Рост известных файлов
//!   обнаруживает сам воркер: каждый цикл файл открывается заново и
//!   дочитывается read-ом до EOF — без доверия каким-либо метаданным размера
//!   (директорные метаданные NTFS для файла, удерживаемого писателем,
//!   замерзают до его закрытия). Порог MIN_FILE_SIZE=100 переоценивается
//!   по мере роста файла (размер — по открытому хендлу, он точен).
//! - **Шаринг**: файлы открываются `std::fs::File::open` — на Windows это
//!   read | FILE_SHARE_READ|WRITE|DELETE, писателя не блокируем и читаем
//!   файл, удерживаемый писателем.
//! - **Закрытие события в хвосте**: (1) пришла следующая строка-маска
//!   `^\d{2}:\d{2}\.\d{6}-\d+,`; (2) хвост оканчивается `\n` и нет новых данных
//!   idle-close-ms → событие эмитится; (3) graceful-стоп → дренаж
//!   `\n`-терминированной части. Незавершённая СТРОКА (без `\n`) не эмитится
//!   никогда.
//! - **Чекпоинты** (`--state`): на файл — {identity: volume serial + file
//!   index, committed_offset}; сдвигаются ТОЛЬКО после подтверждения вставки
//!   ClickHouse (минимально-непрерывно: вставки последовательны, оффсет файла
//!   растёт в порядке событий); записываются атомарно (tmp+rename). Рестарт:
//!   identity совпала и размер ≥ оффсета → продолжаем; иначе → с нуля.
//!   Гарантия: at-least-once (падение между ack и записью чекпоинта даёт
//!   небольшие дубли), потерь — ноль.
//! - **Ошибки синка**: ограниченные повторы с backoff 1..30 с (см.
//!   [`MAX_INSERT_ATTEMPTS`]); очередь воркеры→синк ограничена по байтам —
//!   чтение встаёт (обратное давление), пока вставка не пройдёт.
//! - **Остановка**: появление файла `--stop-file` → дочитать, дренировать,
//!   финальный чекпоинт, `--stats-json`, exit 0. stdout чист — прогресс и
//!   итог в stderr.

use std::collections::HashMap;
use std::fs::{self, File};
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering::Relaxed};
use std::sync::{Arc, Condvar, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use crate::chsink::{self, ChClient, ChTarget, RowEmitter};
use crate::parser::{self, MIN_FILE_SIZE};
use crate::scanner::find_nl;
use crate::Stats;

/// Максимум попыток вставки одного батча (backoff 1,2,4,8,16,30,30,… с,
/// суммарно ~2.5 мин). После исчерпания — фатальная остановка (exit 1),
/// чекпоинты не сдвинуты, рестарт продолжит без потерь.
const MAX_INSERT_ATTEMPTS: u32 = 10;
const BACKOFF_START: Duration = Duration::from_secs(1);
const BACKOFF_MAX: Duration = Duration::from_secs(30);
/// Период прогресса в stderr.
const PROGRESS_EVERY: Duration = Duration::from_secs(5);
/// Буфер чтения воркера.
const READ_BUF: usize = 1 << 20;

pub struct FollowOpts {
    pub input: PathBuf,
    pub state_dir: PathBuf,
    pub stop_file: PathBuf,
    pub poll: Duration,
    pub idle_close: Duration,
    pub workers: usize,
    pub batch_rows: usize,
    pub batch_bytes: usize,
    pub flush: Duration,
}

/// Итог follow-сессии для `--stats-json` (файлы/вставки — поверх [`Stats`];
/// число батчей уходит в stderr-сводку синка).
pub struct FollowOutcome {
    pub code: i32,
    pub files: u64,
    pub inserted_rows: u64,
}

// ---------------------------------------------------------------------------
// Identity файла: volume serial + file index

/// (volume serial, file index) открытого файла — идентичность для чекпоинта.
/// Windows: GetFileInformationByHandle (объявление extern "system" — std-only).
#[cfg(windows)]
fn file_identity(f: &File) -> io::Result<(u64, u64)> {
    use std::os::windows::io::AsRawHandle;

    #[repr(C)]
    struct ByHandleFileInformation {
        attrs: u32,
        creation: [u32; 2],
        access: [u32; 2],
        write: [u32; 2],
        volume_serial: u32,
        size_high: u32,
        size_low: u32,
        links: u32,
        index_high: u32,
        index_low: u32,
    }
    #[link(name = "kernel32")]
    extern "system" {
        fn GetFileInformationByHandle(
            handle: *mut std::ffi::c_void,
            info: *mut ByHandleFileInformation,
        ) -> i32;
    }

    let mut info = ByHandleFileInformation {
        attrs: 0,
        creation: [0; 2],
        access: [0; 2],
        write: [0; 2],
        volume_serial: 0,
        size_high: 0,
        size_low: 0,
        links: 0,
        index_high: 0,
        index_low: 0,
    };
    // SAFETY: валидный HANDLE открытого файла и корректно выложенная структура.
    let ok = unsafe { GetFileInformationByHandle(f.as_raw_handle(), &mut info) };
    if ok == 0 {
        return Err(io::Error::last_os_error());
    }
    Ok((
        u64::from(info.volume_serial),
        (u64::from(info.index_high) << 32) | u64::from(info.index_low),
    ))
}

#[cfg(unix)]
fn file_identity(f: &File) -> io::Result<(u64, u64)> {
    use std::os::unix::fs::MetadataExt;
    let md = f.metadata()?;
    Ok((md.dev(), md.ino()))
}

// ---------------------------------------------------------------------------
// Чекпоинты

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Ckpt {
    pub vol: u64,
    pub idx: u64,
    pub off: u64,
}

/// FNV-1a 64 — имя файла чекпоинта из пути источника (сам путь хранится
/// внутри чекпоинта и сверяется при загрузке — коллизия не страшна).
fn fnv1a64(data: &[u8]) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for &b in data {
        h ^= u64::from(b);
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
    h
}

fn ckpt_file_name(path_str: &str) -> String {
    format!("{:016x}.ckpt", fnv1a64(path_str.as_bytes()))
}

/// Формат: одна строка `tjckpt\tv1\t<vol>\t<idx>\t<offset>\t<path>`.
/// Табуляция в именах файлов Windows невозможна (символы < 0x20 запрещены).
fn encode_ckpt(path_str: &str, c: &Ckpt) -> String {
    format!("tjckpt\tv1\t{}\t{}\t{}\t{}\n", c.vol, c.idx, c.off, path_str)
}

fn decode_ckpt(line: &str) -> Option<(String, Ckpt)> {
    let mut it = line.trim_end_matches(['\r', '\n']).splitn(6, '\t');
    if it.next()? != "tjckpt" || it.next()? != "v1" {
        return None;
    }
    let vol = it.next()?.parse().ok()?;
    let idx = it.next()?.parse().ok()?;
    let off = it.next()?.parse().ok()?;
    let path = it.next()?;
    if path.is_empty() {
        return None;
    }
    Some((path.to_string(), Ckpt { vol, idx, off }))
}

/// Атомарная запись чекпоинта: tmp в той же директории + fsync + rename
/// (std::fs::rename на Windows = MoveFileExW(MOVEFILE_REPLACE_EXISTING)).
fn save_ckpt(state_dir: &Path, path_str: &str, c: &Ckpt) -> io::Result<()> {
    let name = ckpt_file_name(path_str);
    let tmp = state_dir.join(format!("{name}.tmp"));
    let dst = state_dir.join(name);
    {
        let mut f = File::create(&tmp)?;
        f.write_all(encode_ckpt(path_str, c).as_bytes())?;
        f.sync_all()?;
    }
    fs::rename(&tmp, &dst)
}

/// Загружает все `*.ckpt` из `--state` в карту путь → чекпоинт.
/// Битые файлы пропускаются (эквивалент «чекпоинта нет» — просто перечитаем).
fn load_ckpts(state_dir: &Path) -> HashMap<String, Ckpt> {
    let mut out = HashMap::new();
    let Ok(rd) = fs::read_dir(state_dir) else {
        return out;
    };
    for ent in rd.flatten() {
        let p = ent.path();
        if p.extension().map(|e| e == "ckpt") != Some(true) {
            continue;
        }
        let Ok(text) = fs::read_to_string(&p) else {
            continue;
        };
        if let Some((path, c)) = decode_ckpt(&text) {
            // Имя должно соответствовать содержимому (защита от коллизий FNV
            // и от руками скопированных файлов)
            if p.file_name().and_then(|n| n.to_str()) == Some(&ckpt_file_name(&path)) {
                out.insert(path, c);
            }
        }
    }
    out
}

// ---------------------------------------------------------------------------
// Хвостовой ассемблер событий

/// Инкрементальная нарезка растущего файла на события. Семантика границ —
/// та же, что у batch-сканера (`scanner::scan_events`), но EOF не закрывает
/// событие: файл может дорасти. Правила закрытия — шапка модуля.
///
/// Скан построчный: строка считается решённой, когда её маска-статус
/// окончателен — либо маска полностью пришла (ранний true: паттерн
/// `^\d{2}:\d{2}\.\d{6}-\d+,` не может пересечь `\n`), либо строка полная
/// (есть `\n` — дальнейшие байты решения не меняют).
///
/// `consumed_off` продвигается только по решённым байтам: закрытым событиям и
/// полным не-масочным строкам вне события (мусор). Нерешённые байты остаются
/// в `pending` — рестарт с чекпоинта их перечитает.
struct TailAssembler {
    pending: Vec<u8>,
    /// Файловый оффсет pending[0].
    base: u64,
    /// Граница потреблённого: начало открытого события (in_event) либо курсор
    /// мусора. Всегда ≤ line_start.
    ev_start: usize,
    /// Начало текущей (нерешённой или решённой-маски) строки.
    line_start: usize,
    /// Досюда pending просканирован в поисках '\n'.
    searched: usize,
    /// Текущая строка решена (маска, границу применили) — ждём её '\n'.
    line_decided: bool,
    /// pending[ev_start] — начало открытого события.
    in_event: bool,
    /// BOM-проверка пройдена (актуальна только при старте с оффсета 0).
    bom_done: bool,
}

impl TailAssembler {
    fn new(start_off: u64) -> Self {
        TailAssembler {
            pending: Vec::new(),
            base: start_off,
            ev_start: 0,
            line_start: 0,
            searched: 0,
            line_decided: false,
            in_event: false,
            // BOM пропускается только на оффсете 0 (как в batch-режиме)
            bom_done: start_off != 0,
        }
    }

    /// Следующий оффсет чтения из файла.
    fn next_read_off(&self) -> u64 {
        self.base + self.pending.len() as u64
    }

    /// Граница полностью обработанных байтов (для чекпоинта после ack).
    fn consumed_off(&self) -> u64 {
        self.base + self.ev_start as u64
    }

    /// Скармливает свежепрочитанные байты; `sink(событие, конечный_оффсет)`
    /// вызывается для каждого закрытого события (правило 1 — следующая маска).
    fn feed(&mut self, data: &[u8], sink: &mut impl FnMut(&[u8], u64)) {
        self.pending.extend_from_slice(data);
        self.process(sink);
        self.compact();
    }

    fn process(&mut self, sink: &mut impl FnMut(&[u8], u64)) {
        if !self.bom_done && !self.check_bom() {
            return; // < 3 байт и всё ещё префикс BOM — ждём
        }
        loop {
            if self.line_decided {
                // Строка-маска уже применена — ищем её конец
                match find_nl(&self.pending[self.searched..]) {
                    None => {
                        self.searched = self.pending.len();
                        return;
                    }
                    Some(i) => {
                        let after = self.searched + i + 1;
                        self.searched = after;
                        self.line_start = after;
                        self.line_decided = false;
                    }
                }
                continue;
            }
            if self.line_start >= self.pending.len() {
                return;
            }
            if parser::is_event_start(&self.pending[self.line_start..]) {
                // Маска пришла целиком — решение окончательное даже без '\n'
                // (паттерн не может пересечь перевод строки). Правило 1.
                if self.in_event && self.line_start > self.ev_start {
                    sink(
                        &self.pending[self.ev_start..self.line_start],
                        self.base + self.line_start as u64,
                    );
                }
                self.ev_start = self.line_start;
                self.in_event = true;
                self.line_decided = true;
                continue;
            }
            // Пока не маска. Окончательно — только если строка полная.
            if self.searched < self.line_start {
                self.searched = self.line_start;
            }
            match find_nl(&self.pending[self.searched..]) {
                Some(i) => {
                    let after = self.searched + i + 1;
                    if !self.in_event {
                        // Полная строка мусора вне события: выбрасывается,
                        // потреблённый оффсет продвигается (строк не даёт)
                        self.ev_start = after;
                    }
                    self.line_start = after;
                    self.searched = after;
                }
                None => {
                    self.searched = self.pending.len();
                    return;
                }
            }
        }
    }

    /// `true` — BOM-статус решён. Проверка только на файловом оффсете 0.
    fn check_bom(&mut self) -> bool {
        const BOM: [u8; 3] = [0xEF, 0xBB, 0xBF];
        if self.pending.len() < 3 {
            if self.pending.iter().zip(BOM.iter()).all(|(a, b)| a == b) {
                return false; // всё ещё префикс BOM — нужен ещё байт
            }
        } else if self.pending[..3] == BOM {
            self.ev_start = 3;
            self.line_start = 3;
            self.searched = 3;
        }
        self.bom_done = true;
        true
    }

    fn compact(&mut self) {
        if self.ev_start > 0 {
            self.pending.drain(0..self.ev_start);
            self.base += self.ev_start as u64;
            self.line_start -= self.ev_start;
            self.searched -= self.ev_start;
            self.ev_start = 0;
        }
    }

    /// Правило 2 применимо: событие открыто и хвост оканчивается `\n`
    /// (текущая строка пуста — незавершённой строки нет).
    fn can_idle_close(&self) -> bool {
        self.in_event
            && self.line_start == self.pending.len()
            && self.pending.len() > self.ev_start
    }

    /// Правило 2: idle-таймаут — эмитит открытое `\n`-терминированное событие.
    fn idle_close(&mut self, sink: &mut (impl FnMut(&[u8], u64) + ?Sized)) -> bool {
        if !self.can_idle_close() {
            return false;
        }
        let end = self.pending.len();
        sink(&self.pending[self.ev_start..end], self.base + end as u64);
        self.ev_start = end;
        self.in_event = false;
        self.compact();
        true
    }

    /// Правило 3: graceful-стоп — дренаж `\n`-терминированной части открытого
    /// события. Незавершённая последняя строка не эмитится и оффсет через неё
    /// не переступает (рестарт дочитает).
    fn stop_drain(&mut self, sink: &mut (impl FnMut(&[u8], u64) + ?Sized)) -> bool {
        if !self.in_event || self.line_start <= self.ev_start {
            return false;
        }
        sink(
            &self.pending[self.ev_start..self.line_start],
            self.base + self.line_start as u64,
        );
        self.ev_start = self.line_start;
        self.in_event = false;
        self.compact();
        true
    }
}

// ---------------------------------------------------------------------------
// Разделяемое состояние файла и очередь воркеры → синк

/// Неизменяемый контекст файла + коммит-состояние (пишет синк, сбрасывает
/// воркер при truncate/replace; сериализация — mutex `st`).
struct FileShared {
    path: PathBuf,
    /// Ключ чекпоинта (и его содержимое-путь).
    path_str: String,
    filename: String,
    file_path: String,
    date_prefix: String,
    st: Mutex<FileCommit>,
}

struct FileCommit {
    /// Поколение: инкремент при reset (truncate/замена файла). Мета из
    /// очереди со старым gen игнорируется — оффсеты старого содержимого
    /// не должны коммитить новое.
    gen: u32,
    committed: u64,
    vol: u64,
    idx: u64,
}

/// Кусок RowBinary-строк одного файла + мета для чекпоинта. `data` может быть
/// пустым (rows == 0): чистое продвижение оффсета (мусор/parse_skip).
struct TailChunk {
    file: Arc<FileShared>,
    gen: u32,
    /// Потреблённый оффсет файла после последнего события куска.
    end_off: u64,
    data: Vec<u8>,
    rows: usize,
}

enum Pop {
    Chunk(TailChunk),
    Timeout,
    Done,
}

/// Очередь с байтовым лимитом (обратное давление на чтение) и флагом фатальной
/// ошибки вставки — зеркало `chsink::SinkQueue`, но с follow-метаданными.
struct FollowQueue {
    st: Mutex<QueueState>,
    cv_space: Condvar,
    cv_data: Condvar,
    cap_bytes: usize,
}

struct QueueState {
    q: std::collections::VecDeque<TailChunk>,
    bytes: usize,
    producers: usize,
    fatal: bool,
}

impl FollowQueue {
    fn new(cap_bytes: usize, producers: usize) -> Self {
        FollowQueue {
            st: Mutex::new(QueueState {
                q: std::collections::VecDeque::new(),
                bytes: 0,
                producers,
                fatal: false,
            }),
            cv_space: Condvar::new(),
            cv_data: Condvar::new(),
            cap_bytes,
        }
    }

    /// `false` — вставка фатально упала, чтение надо прекращать.
    fn push(&self, c: TailChunk) -> bool {
        let mut st = self.st.lock().unwrap();
        loop {
            if st.fatal {
                return false;
            }
            if st.bytes < self.cap_bytes || st.q.is_empty() {
                break;
            }
            st = self.cv_space.wait(st).unwrap();
        }
        st.bytes += c.data.len();
        st.q.push_back(c);
        drop(st);
        self.cv_data.notify_one();
        true
    }

    fn producer_done(&self) {
        let mut st = self.st.lock().unwrap();
        st.producers -= 1;
        let last = st.producers == 0;
        drop(st);
        if last {
            self.cv_data.notify_all();
        }
    }

    fn set_fatal(&self) {
        let mut st = self.st.lock().unwrap();
        st.fatal = true;
        st.q.clear();
        st.bytes = 0;
        drop(st);
        self.cv_space.notify_all();
    }

    fn pop(&self, deadline: Instant) -> Pop {
        let mut st = self.st.lock().unwrap();
        loop {
            if let Some(c) = st.q.pop_front() {
                st.bytes -= c.data.len();
                drop(st);
                self.cv_space.notify_all();
                return Pop::Chunk(c);
            }
            if st.producers == 0 {
                return Pop::Done;
            }
            let now = Instant::now();
            if now >= deadline {
                return Pop::Timeout;
            }
            st = self.cv_data.wait_timeout(st, deadline - now).unwrap().0;
        }
    }

    fn queued_bytes(&self) -> usize {
        self.st.lock().unwrap().bytes
    }
}

// ---------------------------------------------------------------------------
// Почтовый ящик планировщик → воркер

struct NewFileMsg {
    shared: Arc<FileShared>,
    ckpt: Option<Ckpt>,
}

#[derive(Default)]
struct MailboxState {
    new_files: Vec<NewFileMsg>,
    stop: bool,
}

#[derive(Default)]
struct Mailbox {
    st: Mutex<MailboxState>,
    cv: Condvar,
}

impl Mailbox {
    fn push(&self, msg: NewFileMsg) {
        self.st.lock().unwrap().new_files.push(msg);
        self.cv.notify_one();
    }

    fn stop(&self) {
        self.st.lock().unwrap().stop = true;
        self.cv.notify_one();
    }

    /// Ждёт до `deadline`, просыпаясь на новые файлы/стоп.
    /// Возвращает (новые файлы, стоп-флаг).
    fn wait_until(&self, deadline: Instant) -> (Vec<NewFileMsg>, bool) {
        let mut st = self.st.lock().unwrap();
        loop {
            if st.stop || !st.new_files.is_empty() {
                return (std::mem::take(&mut st.new_files), st.stop);
            }
            let now = Instant::now();
            if now >= deadline {
                return (Vec::new(), false);
            }
            st = self.cv.wait_timeout(st, deadline - now).unwrap().0;
        }
    }
}

// ---------------------------------------------------------------------------
// Воркер чтения

/// Приватное состояние файла у воркера-владельца (файл закреплён за одним
/// воркером — порядок событий файла в потоке вставки сохраняется).
struct OwnedFile {
    shared: Arc<FileShared>,
    ckpt: Option<Ckpt>,
    asm: TailAssembler,
    gen: u32,
    /// Первый успешный open состоялся (identity установлена, resume решён).
    started: bool,
    /// Момент последнего чтения новых байтов (для idle-close).
    last_data: Instant,
    /// consumed_off последнего отправленного куска (не шлём пустые повторно).
    last_pushed_off: u64,
    /// Файл хоть раз прошёл порог MIN_FILE_SIZE.
    gate_passed: bool,
    /// Ошибка открытия/чтения уже залогирована (не спамим каждые poll-ms).
    fail_logged: bool,
}

struct WorkerCtx<'a> {
    queue: &'a FollowQueue,
    stats: &'a Stats,
    state_dir: &'a Path,
    idle_close: Duration,
    poll: Duration,
}

fn worker_loop(ctx: &WorkerCtx, mailbox: &Mailbox) {
    let mut files: Vec<OwnedFile> = Vec::new();
    let mut em = RowEmitter::new();
    let mut read_buf = vec![0u8; READ_BUF];
    let mut alive = true;
    let mut next_poll = Instant::now();

    'outer: loop {
        let (new_files, stop) = mailbox.wait_until(next_poll);
        for msg in new_files {
            let mut of = OwnedFile {
                shared: msg.shared,
                ckpt: msg.ckpt,
                asm: TailAssembler::new(0),
                gen: 0,
                started: false,
                last_data: Instant::now(),
                last_pushed_off: 0,
                gate_passed: false,
                fail_logged: false,
            };
            // Новый файл обрабатывается сразу (подхват < 2 с), не ждём poll
            if !stop {
                alive = poll_file(ctx, &mut of, &mut em, &mut read_buf, false);
            }
            files.push(of);
            if !alive {
                break 'outer;
            }
        }
        if stop {
            // Правило 3: финальное дочитывание + дренаж '\n'-терминированного
            for of in &mut files {
                alive = poll_file(ctx, of, &mut em, &mut read_buf, true);
                if !alive {
                    break;
                }
            }
            break;
        }
        if Instant::now() >= next_poll {
            for of in &mut files {
                alive = poll_file(ctx, of, &mut em, &mut read_buf, false);
                if !alive {
                    break 'outer;
                }
            }
            next_poll = Instant::now() + ctx.poll;
        }
    }

    // Файлы, так и не прошедшие порог MIN_FILE_SIZE — в small_file_skips
    let gated = files.iter().filter(|f| f.started && !f.gate_passed).count();
    ctx.stats.small_skips.fetch_add(gated as u64, Relaxed);
    ctx.queue.producer_done();
}

/// Один цикл обслуживания файла: open (полный шаринг) → identity/truncate →
/// порог 100 байт → дочитать новое → idle-close / stop-drain → отправить куски.
/// `false` — очередь в фатале, воркеру пора выходить.
fn poll_file(
    ctx: &WorkerCtx,
    of: &mut OwnedFile,
    em: &mut RowEmitter,
    read_buf: &mut [u8],
    stopping: bool,
) -> bool {
    let mut f = match File::open(&of.shared.path) {
        Ok(f) => f,
        Err(e) => {
            // Файл исчез/недоступен: pending держим (вдруг вернётся тем же
            // identity), логируем один раз на переход
            if !of.fail_logged && e.kind() != io::ErrorKind::NotFound {
                eprintln!(
                    "follow: не открыть {}: {e}",
                    of.shared.path.display()
                );
                ctx.stats.failed.fetch_add(1, Relaxed);
                of.fail_logged = true;
            }
            return finish_cycle(ctx, of, em, stopping);
        }
    };
    of.fail_logged = false;

    let (vol, idx) = match file_identity(&f) {
        Ok(v) => v,
        Err(e) => {
            if !of.fail_logged {
                eprintln!(
                    "follow: identity {}: {e}",
                    of.shared.path.display()
                );
                ctx.stats.failed.fetch_add(1, Relaxed);
                of.fail_logged = true;
            }
            return finish_cycle(ctx, of, em, stopping);
        }
    };
    // Размер нужен ТОЛЬКО для детекции усечения и порога MIN_FILE_SIZE —
    // и берётся по хендлу (точен). Для детекции РОСТА размер не используется
    // вовсе: ниже безусловный read-to-EOF (см. комментарий там).
    let size = match f.metadata() {
        Ok(md) => md.len(),
        Err(e) => {
            if !of.fail_logged {
                eprintln!("follow: metadata {}: {e}", of.shared.path.display());
                ctx.stats.failed.fetch_add(1, Relaxed);
                of.fail_logged = true;
            }
            return finish_cycle(ctx, of, em, stopping);
        }
    };

    if !of.started {
        // Первое касание: resume по чекпоинту, если identity совпала и файл
        // не короче зафиксированного оффсета; иначе — с нуля
        let start_off = match of.ckpt.take() {
            Some(c) if c.vol == vol && c.idx == idx && size >= c.off => c.off,
            _ => 0,
        };
        of.asm = TailAssembler::new(start_off);
        of.last_pushed_off = start_off;
        of.started = true;
        let mut st = of.shared.st.lock().unwrap();
        st.gen = of.gen;
        st.committed = start_off;
        st.vol = vol;
        st.idx = idx;
        // Чекпоинт фиксируется сразу: если resume отвергнут (identity/размер),
        // на диске не должен остаться старый оффсет — файл может дорасти до
        // него до первого коммита, и рестарт потерял бы начало
        persist_ckpt_locked(ctx.state_dir, &of.shared, &st);
    } else {
        let replaced = {
            let st = of.shared.st.lock().unwrap();
            st.vol != vol || st.idx != idx
        };
        if replaced || size < of.asm.next_read_off() {
            // Файл пересоздан или усечён → сбрасываемся на оффсет 0
            eprintln!(
                "follow: {} {} — читаем с нуля",
                if replaced { "пересоздан" } else { "усечён" },
                of.shared.path.display()
            );
            of.gen += 1;
            of.asm = TailAssembler::new(0);
            of.last_pushed_off = 0;
            let mut st = of.shared.st.lock().unwrap();
            st.gen = of.gen;
            st.committed = 0;
            st.vol = vol;
            st.idx = idx;
            persist_ckpt_locked(ctx.state_dir, &of.shared, &st);
        }
    }

    // Порог MIN_FILE_SIZE переоценивается по мере роста (как в batch-режиме)
    if size < MIN_FILE_SIZE {
        return finish_cycle(ctx, of, em, stopping);
    }
    of.gate_passed = true;

    // Дочитываем новое. Попытка чтения НЕ зависит от снапшота размера:
    // рост файла детектирует сам read по свежеоткрытому хендлу (до EOF).
    // Метаданные КАТАЛОГА NTFS для файла, удерживаемого писателем,
    // замерзают до закрытия (ловушка S6 tail-серии) — здесь они не
    // используются вовсе; размер по хендлу точен на NTFS, но read-to-EOF
    // не доверяет и ему (иммунитет к ленивым атрибутам сетевых ФС).
    // Цена на файл без роста — один лишний seek+read (0 байт) за цикл.
    let read_off = of.asm.next_read_off();
    if let Err(e) = f.seek(SeekFrom::Start(read_off)) {
        if !of.fail_logged {
            eprintln!("follow: seek {}: {e}", of.shared.path.display());
            ctx.stats.failed.fetch_add(1, Relaxed);
            of.fail_logged = true;
        }
        return finish_cycle(ctx, of, em, stopping);
    }
    let mut got: u64 = 0;
    loop {
        let n = match f.read(read_buf) {
            Ok(0) => break,
            Ok(n) => n,
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => {
                if !of.fail_logged {
                    eprintln!("follow: чтение {}: {e}", of.shared.path.display());
                    ctx.stats.failed.fetch_add(1, Relaxed);
                    of.fail_logged = true;
                }
                break; // прочитанное обрабатываем, остальное — следующим циклом
            }
        };
        got += n as u64;
        if !feed_bytes(ctx, of, em, &read_buf[..n]) {
            return false; // фатал очереди
        }
        // Куски ≥ CHUNK_TARGET_BYTES уходят внутри feed_bytes; читаем до EOF
    }
    if got > 0 {
        of.last_data = Instant::now();
        ctx.stats.bytes.fetch_add(got, Relaxed);
    }

    finish_cycle(ctx, of, em, stopping)
}

/// Хвостовые правила 2/3 + отправка накопленного куска. `false` — фатал.
fn finish_cycle(ctx: &WorkerCtx, of: &mut OwnedFile, em: &mut RowEmitter, stopping: bool) -> bool {
    if of.started {
        if stopping {
            // Правило 3: graceful-стоп
            drain_with(ctx, of, em, |asm, sink| asm.stop_drain(sink));
        } else if of.asm.can_idle_close() && of.last_data.elapsed() >= ctx.idle_close {
            // Правило 2: '\n'-терминированный хвост + тишина idle-close-ms
            drain_with(ctx, of, em, |asm, sink| asm.idle_close(sink));
        }
    }
    push_pending(ctx, of, em)
}

/// Прогоняет правило закрытия через эмиттер с подсчётом статистики.
fn drain_with(
    ctx: &WorkerCtx,
    of: &mut OwnedFile,
    em: &mut RowEmitter,
    close: impl FnOnce(&mut TailAssembler, &mut dyn FnMut(&[u8], u64)) -> bool,
) {
    begin_file(of, em);
    let (mut events, mut skips) = (0u64, 0u64);
    close(&mut of.asm, &mut |ev, _end| {
        if parser::parse_event(ev, em) {
            events += 1;
        } else {
            skips += 1;
        }
    });
    ctx.stats.events.fetch_add(events, Relaxed);
    ctx.stats.parse_skips.fetch_add(skips, Relaxed);
}

fn begin_file(of: &OwnedFile, em: &mut RowEmitter) {
    em.begin_file(
        chsink::hour_base_micros(&of.shared.date_prefix),
        of.shared.filename.as_bytes(),
        of.shared.file_path.as_bytes(),
    );
}

/// Скармливает байты ассемблеру; закрытые события кодируются в RowBinary,
/// куски ≥ CHUNK_TARGET_BYTES уходят в очередь сразу. `false` — фатал очереди.
fn feed_bytes(ctx: &WorkerCtx, of: &mut OwnedFile, em: &mut RowEmitter, data: &[u8]) -> bool {
    begin_file(of, em);
    let (mut events, mut skips) = (0u64, 0u64);
    let mut overflow: Option<TailChunk> = None;
    {
        let shared = &of.shared;
        let gen = of.gen;
        let em_cell = &mut *em;
        let mut sink = |ev: &[u8], end: u64| {
            if parser::parse_event(ev, em_cell) {
                events += 1;
            } else {
                skips += 1;
            }
            if em_cell.chunk_bytes() >= chsink::CHUNK_TARGET_BYTES && overflow.is_none() {
                let c = em_cell.take_chunk();
                overflow = Some(TailChunk {
                    file: Arc::clone(shared),
                    gen,
                    end_off: end,
                    rows: c.ends.len(),
                    data: c.data,
                });
            }
        };
        of.asm.feed(data, &mut sink);
    }
    ctx.stats.events.fetch_add(events, Relaxed);
    ctx.stats.parse_skips.fetch_add(skips, Relaxed);
    if let Some(c) = overflow {
        of.last_pushed_off = c.end_off;
        if !ctx.queue.push(c) {
            return false;
        }
        // Остаток эмиттера уйдёт из push_pending / следующего переполнения
    }
    push_pending(ctx, of, em)
}

/// Отправляет накопленные строки эмиттера и/или чистое продвижение оффсета.
fn push_pending(ctx: &WorkerCtx, of: &mut OwnedFile, em: &mut RowEmitter) -> bool {
    if !of.started {
        return true;
    }
    let consumed = of.asm.consumed_off();
    if em.chunk_bytes() == 0 && consumed <= of.last_pushed_off {
        return true;
    }
    let c = em.take_chunk();
    of.last_pushed_off = consumed;
    ctx.queue.push(TailChunk {
        file: Arc::clone(&of.shared),
        gen: of.gen,
        end_off: consumed,
        rows: c.ends.len(),
        data: c.data,
    })
}

/// Запись чекпоинта под уже взятым mutex-ом (сериализация воркер/синк).
fn persist_ckpt_locked(state_dir: &Path, shared: &FileShared, st: &FileCommit) {
    let c = Ckpt {
        vol: st.vol,
        idx: st.idx,
        off: st.committed,
    };
    if let Err(e) = save_ckpt(state_dir, &shared.path_str, &c) {
        eprintln!(
            "follow: не записать чекпоинт для {}: {e}",
            shared.path.display()
        );
    }
}

// ---------------------------------------------------------------------------
// Обнаружение файлов (планировщик)

/// Рекурсивный сбор `*.log` БЕЗ фильтра размера (порог динамический).
fn scan_logs(dir: &Path, out: &mut Vec<PathBuf>, err_logged: &mut bool) {
    let rd = match fs::read_dir(dir) {
        Ok(rd) => rd,
        Err(e) => {
            if !*err_logged {
                eprintln!("follow: обход {}: {e}", dir.display());
                *err_logged = true;
            }
            return;
        }
    };
    for ent in rd.flatten() {
        let Ok(ft) = ent.file_type() else { continue };
        let path = ent.path();
        if ft.is_dir() {
            scan_logs(&path, out, err_logged);
            continue;
        }
        if !ft.is_file() {
            continue;
        }
        let name = ent.file_name();
        let name = name.to_string_lossy();
        // Тот же фильтр, что у batch-обхода: суффикс .log, имя ".log" — не лог
        if !name.ends_with(".log") || name.rfind('.') == Some(0) {
            continue;
        }
        out.push(path);
    }
}

struct SchedulerCtx<'a> {
    input: &'a Path,
    stop_file: &'a Path,
    poll: Duration,
    mailboxes: &'a [Mailbox],
    ckpts: Mutex<HashMap<String, Ckpt>>,
    files_seen: AtomicU64,
    fatal: &'a AtomicBool,
}

fn scheduler_loop(ctx: &SchedulerCtx) {
    let mut known: std::collections::HashSet<PathBuf> = std::collections::HashSet::new();
    let mut rr = 0usize;
    let mut err_logged = false;
    loop {
        if ctx.stop_file.exists() || ctx.fatal.load(Relaxed) {
            for mb in ctx.mailboxes {
                mb.stop();
            }
            return;
        }
        let mut found = Vec::new();
        scan_logs(ctx.input, &mut found, &mut err_logged);
        for path in found {
            if !known.insert(path.clone()) {
                continue;
            }
            let path_str = path.to_string_lossy().into_owned();
            let filename = path
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            let shared = Arc::new(FileShared {
                path_str: path_str.clone(),
                filename: filename.clone(),
                file_path: crate::rel_file_path(&path),
                date_prefix: parser::date_from_filename(&filename),
                st: Mutex::new(FileCommit {
                    gen: 0,
                    committed: 0,
                    vol: 0,
                    idx: 0,
                }),
                path,
            });
            let ckpt = ctx.ckpts.lock().unwrap().remove(&path_str);
            ctx.files_seen.fetch_add(1, Relaxed);
            ctx.mailboxes[rr % ctx.mailboxes.len()].push(NewFileMsg { shared, ckpt });
            rr += 1;
        }
        thread::sleep(ctx.poll);
    }
}

// ---------------------------------------------------------------------------
// Синк: батчирование по границам кусков, ретраи, коммит чекпоинтов

/// Батчер follow-режима: копит ЦЕЛЫЕ куски (границы вставки = границы кусков,
/// кусок ≤ CHUNK_TARGET_BYTES + одно событие) — мета чекпоинтов всегда
/// соответствует полностью вставленным строкам. Лимиты rows/bytes проверяются
/// между кусками, поэтому могут быть превышены не более чем на один кусок.
struct FollowBatcher {
    buf: Vec<u8>,
    rows: usize,
    meta: Vec<(Arc<FileShared>, u32, u64)>,
    started: Option<Instant>,
}

impl FollowBatcher {
    fn new() -> Self {
        FollowBatcher {
            buf: Vec::new(),
            rows: 0,
            meta: Vec::new(),
            started: None,
        }
    }

    fn add(&mut self, c: TailChunk) {
        if c.rows > 0 && self.rows == 0 {
            self.started = Some(Instant::now());
        }
        self.buf.extend_from_slice(&c.data);
        self.rows += c.rows;
        self.meta.push((c.file, c.gen, c.end_off));
    }

    fn deadline(&self, flush_after: Duration) -> Option<Instant> {
        self.started.map(|t| t + flush_after)
    }
}

struct SinkCtx<'a> {
    state_dir: &'a Path,
    files_seen: &'a AtomicU64,
    stats: &'a Stats,
}

/// Коммитит мету (оффсеты подтверждённых кусков) и пишет чекпоинты.
/// Вызывается ТОЛЬКО после ack вставки (или для кусков без строк, когда
/// невставленных строк нет вообще).
fn commit_meta(ctx: &SinkCtx, meta: &mut Vec<(Arc<FileShared>, u32, u64)>) {
    for (shared, gen, off) in meta.iter() {
        let mut st = shared.st.lock().unwrap();
        if st.gen != *gen || *off <= st.committed {
            continue;
        }
        st.committed = *off;
        persist_ckpt_locked(ctx.state_dir, shared, &st);
    }
    meta.clear();
}

/// Вставка с ограниченными повторами: сетевые сбои и 5xx/408/429 — retryable
/// (backoff 1..30 с); прочие HTTP-статусы (кривая схема/данные) — фатал сразу.
fn insert_with_retry(client: &mut ChClient, body: &[u8]) -> Result<(), chsink::ChError> {
    let mut backoff = BACKOFF_START;
    let mut attempt = 1u32;
    loop {
        match client.insert_classified(body) {
            Ok(()) => return Ok(()),
            Err(e) if e.retryable && attempt < MAX_INSERT_ATTEMPTS => {
                eprintln!(
                    "follow: вставка не прошла (попытка {attempt}/{MAX_INSERT_ATTEMPTS}), повтор через {} с: {}",
                    backoff.as_secs(),
                    e.err
                );
                thread::sleep(backoff);
                backoff = (backoff * 2).min(BACKOFF_MAX);
                attempt += 1;
            }
            Err(e) => return Err(e.err),
        }
    }
}

struct SinkTotals {
    inserted_rows: u64,
    batches: u64,
}

#[allow(clippy::too_many_arguments)]
fn run_follow_sink(
    ctx: &SinkCtx,
    client: &mut ChClient,
    queue: &FollowQueue,
    batch_rows: usize,
    batch_bytes: usize,
    flush_after: Duration,
    totals: &mut SinkTotals,
) -> Result<(), chsink::ChError> {
    let mut b = FollowBatcher::new();
    let mut next_tick = Instant::now() + PROGRESS_EVERY;
    loop {
        let mut deadline = next_tick;
        if let Some(d) = b.deadline(flush_after) {
            deadline = deadline.min(d);
        }
        match queue.pop(deadline) {
            Pop::Chunk(c) => {
                b.add(c);
                if b.rows >= batch_rows || b.buf.len() >= batch_bytes {
                    flush_batch(ctx, client, &mut b, totals)?;
                } else if b.rows == 0 && !b.meta.is_empty() {
                    // Только продвижение оффсетов: строк в полёте нет
                    // (вставка синхронная) — коммитим сразу
                    commit_meta(ctx, &mut b.meta);
                }
            }
            Pop::Timeout => {
                if b.deadline(flush_after).is_some_and(|d| Instant::now() >= d) {
                    flush_batch(ctx, client, &mut b, totals)?;
                }
                if Instant::now() >= next_tick {
                    progress(ctx, queue, totals);
                    next_tick = Instant::now() + PROGRESS_EVERY;
                }
            }
            Pop::Done => break,
        }
    }
    flush_batch(ctx, client, &mut b, totals)?;
    commit_meta(ctx, &mut b.meta);
    Ok(())
}

fn flush_batch(
    ctx: &SinkCtx,
    client: &mut ChClient,
    b: &mut FollowBatcher,
    totals: &mut SinkTotals,
) -> Result<(), chsink::ChError> {
    if b.rows > 0 {
        insert_with_retry(client, &b.buf)?;
        totals.inserted_rows += b.rows as u64;
        totals.batches += 1;
        b.buf.clear();
        b.rows = 0;
        b.started = None;
    }
    commit_meta(ctx, &mut b.meta);
    Ok(())
}

fn progress(ctx: &SinkCtx, queue: &FollowQueue, totals: &SinkTotals) {
    eprintln!(
        "follow: файлов {}, событий {}, вставлено {} строк / {} батчей, очередь {} КБ",
        ctx.files_seen.load(Relaxed),
        ctx.stats.events.load(Relaxed),
        totals.inserted_rows,
        totals.batches,
        queue.queued_bytes() / 1024,
    );
}

// ---------------------------------------------------------------------------
// Точка входа

pub fn run(opts: &FollowOpts, target: &ChTarget, stats: &Stats) -> FollowOutcome {
    let fail = |code: i32| FollowOutcome {
        code,
        files: 0,
        inserted_rows: 0,
    };
    if let Err(e) = fs::create_dir_all(&opts.state_dir) {
        eprintln!(
            "Ошибка: не создать --state {}: {e}",
            opts.state_dir.display()
        );
        return fail(1);
    }
    // Канонический корень: чекпоинты стабильны при запуске из разных cwd
    let input = match fs::canonicalize(&opts.input) {
        Ok(p) => p,
        Err(e) => {
            eprintln!("Ошибка: не открыть --input {}: {e}", opts.input.display());
            return fail(1);
        }
    };

    let mut client = ChClient::new(target);
    if let Err(e) = client.check_ready(target) {
        // Fail-fast, как в batch-режиме: неверный порт/БД/таблица виден сразу.
        // Ретраи с backoff применяются к вставкам уже запущенного агента.
        eprintln!("Ошибка ClickHouse ({}): {e}", target.host);
        return fail(1);
    }

    let queue = FollowQueue::new(opts.batch_bytes.saturating_mul(2).max(16 << 20), opts.workers);
    let mailboxes: Vec<Mailbox> = (0..opts.workers).map(|_| Mailbox::default()).collect();
    let fatal = AtomicBool::new(false);
    let sched = SchedulerCtx {
        input: &input,
        stop_file: &opts.stop_file,
        poll: opts.poll,
        mailboxes: &mailboxes,
        ckpts: Mutex::new(load_ckpts(&opts.state_dir)),
        files_seen: AtomicU64::new(0),
        fatal: &fatal,
    };
    let wctx = WorkerCtx {
        queue: &queue,
        stats,
        state_dir: &opts.state_dir,
        idle_close: opts.idle_close,
        poll: opts.poll,
    };
    let sctx = SinkCtx {
        state_dir: &opts.state_dir,
        files_seen: &sched.files_seen,
        stats,
    };

    let mut totals = SinkTotals {
        inserted_rows: 0,
        batches: 0,
    };
    let mut sink_err: Option<chsink::ChError> = None;

    thread::scope(|s| {
        s.spawn(|| scheduler_loop(&sched));
        for mb in &mailboxes {
            let wctx = &wctx;
            s.spawn(move || worker_loop(wctx, mb));
        }
        let res = run_follow_sink(
            &sctx,
            &mut client,
            &queue,
            opts.batch_rows,
            opts.batch_bytes,
            opts.flush,
            &mut totals,
        );
        if let Err(e) = res {
            queue.set_fatal(); // разбудить воркеров, ждущих место в очереди
            fatal.store(true, Relaxed); // планировщик разошлёт stop
            sink_err = Some(e);
        }
    });

    progress(&sctx, &queue, &totals); // финальная строка прогресса
    let files = sched.files_seen.load(Relaxed);
    if let Some(e) = sink_err {
        eprintln!("ОШИБКА ClickHouse ({}): {e}", target.host);
        eprintln!("ОШИБКА: вставка не восстановилась, чекпоинты не сдвинуты — рестарт продолжит без потерь");
        return FollowOutcome {
            code: 1,
            files,
            inserted_rows: totals.inserted_rows,
        };
    }
    eprintln!(
        "follow: остановка по {} — дренаж завершён, вставлено {} строк ({} батчей)",
        opts.stop_file.display(),
        totals.inserted_rows,
        totals.batches
    );
    FollowOutcome {
        code: 0,
        files,
        inserted_rows: totals.inserted_rows,
    }
}

// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    // --- ассемблер ---

    /// Прогон с нарезкой входа на куски size; события собираются с оффсетами.
    fn feed_pieces(asm: &mut TailAssembler, data: &[u8], size: usize) -> Vec<(Vec<u8>, u64)> {
        let mut got = Vec::new();
        for chunk in data.chunks(size.max(1)) {
            asm.feed(chunk, &mut |ev, off| got.push((ev.to_vec(), off)));
        }
        got
    }

    const EV1: &[u8] = b"00:01.000001-2,CALL,0,A=1\n";
    const EV2: &[u8] = b"00:02.000002-3,EXCP,1,B=2\n";

    #[test]
    fn closes_on_next_mask_any_split() {
        for size in [1usize, 2, 3, 7, 64] {
            let mut asm = TailAssembler::new(0);
            let data = [EV1, EV2].concat();
            let got = feed_pieces(&mut asm, &data, size);
            // Второе событие остаётся открытым (EOF не закрывает)
            assert_eq!(got.len(), 1, "size={size}");
            assert_eq!(got[0].0, EV1, "size={size}");
            assert_eq!(got[0].1, EV1.len() as u64, "size={size}");
            assert_eq!(asm.consumed_off(), EV1.len() as u64);
            assert_eq!(asm.next_read_off(), data.len() as u64);
            // Хвост '\n'-терминирован — idle-close применим
            assert!(asm.can_idle_close());
            let mut tail = Vec::new();
            assert!(asm.idle_close(&mut |ev, _| tail.extend_from_slice(ev)));
            assert_eq!(tail, EV2);
            assert_eq!(asm.consumed_off(), data.len() as u64);
            assert!(!asm.can_idle_close());
        }
    }

    #[test]
    fn early_mask_closes_before_newline() {
        let mut asm = TailAssembler::new(0);
        let mut got = Vec::new();
        // Маска второго события пришла целиком, но её строка не завершена —
        // правило 1: первое событие закрывается немедленно
        let data = b"00:01.000001-2,CALL,0,A=1\n00:02.000002-3,";
        asm.feed(data, &mut |ev, off| got.push((ev.to_vec(), off)));
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].0, EV1);
        // Хвост не '\n'-терминирован — ни idle, ни stop не эмитят
        assert!(!asm.can_idle_close());
        let mut drained = Vec::new();
        assert!(!asm.stop_drain(&mut |ev, _| drained.extend_from_slice(ev)));
        assert!(drained.is_empty());
    }

    #[test]
    fn partial_line_held_until_completed() {
        let mut asm = TailAssembler::new(0);
        let mut got: Vec<Vec<u8>> = Vec::new();
        asm.feed(b"00:01.000001-2,CALL,0,A='abc", &mut |ev, _| {
            got.push(ev.to_vec())
        });
        assert!(got.is_empty());
        assert!(!asm.can_idle_close()); // нет '\n' — держим
        asm.feed(b" def'\n", &mut |ev, _| got.push(ev.to_vec()));
        assert!(got.is_empty()); // следующей маски нет — событие ещё открыто
        assert!(asm.can_idle_close());
        assert!(asm.idle_close(&mut |ev, _| got.push(ev.to_vec())));
        assert_eq!(got.len(), 1);
        assert_eq!(got[0], b"00:01.000001-2,CALL,0,A='abc def'\n");
    }

    #[test]
    fn multiline_value_stays_one_event() {
        let mut asm = TailAssembler::new(0);
        let data = b"00:01.000001-2,CALL,0,Ctx='line1\nline2\nline3'\n";
        let mut got: Vec<Vec<u8>> = Vec::new();
        for b in data.iter() {
            asm.feed(&[*b], &mut |ev, _| got.push(ev.to_vec()));
        }
        assert!(got.is_empty()); // маски следующего события нет
        assert!(asm.can_idle_close());
        assert!(asm.idle_close(&mut |ev, _| got.push(ev.to_vec())));
        assert_eq!(got, vec![data.to_vec()]);
    }

    #[test]
    fn preamble_garbage_advances_offset_without_events() {
        let mut asm = TailAssembler::new(0);
        let mut got: Vec<Vec<u8>> = Vec::new();
        asm.feed(b"garbage line\nmore garbage\n", &mut |ev, _| {
            got.push(ev.to_vec())
        });
        assert!(got.is_empty());
        // Полные строки мусора потреблены — чекпоинт может продвинуться
        assert_eq!(asm.consumed_off(), 26);
        asm.feed(EV1, &mut |ev, _| got.push(ev.to_vec()));
        asm.feed(EV2, &mut |ev, _| got.push(ev.to_vec()));
        assert_eq!(got, vec![EV1.to_vec()]);
        assert_eq!(asm.consumed_off(), 26 + EV1.len() as u64);
    }

    #[test]
    fn incomplete_garbage_line_not_consumed() {
        let mut asm = TailAssembler::new(0);
        let mut got: Vec<Vec<u8>> = Vec::new();
        asm.feed(b"junk\n00:0", &mut |ev, _| got.push(ev.to_vec()));
        // "00:0" может дорасти до маски — оффсет стоит на её начале
        assert_eq!(asm.consumed_off(), 5);
        asm.feed(b"1.000001-2,CALL,0,A=1\n", &mut |ev, _| got.push(ev.to_vec()));
        assert!(got.is_empty()); // событие открыто
        assert!(asm.can_idle_close());
        assert!(asm.idle_close(&mut |ev, _| got.push(ev.to_vec())));
        assert_eq!(got, vec![EV1.to_vec()]);
    }

    #[test]
    fn bom_skipped_at_offset_zero_only() {
        let mut asm = TailAssembler::new(0);
        let mut got: Vec<(Vec<u8>, u64)> = Vec::new();
        // BOM по байту (проверка «префикс BOM — ждём»)
        asm.feed(&[0xEF], &mut |ev, off| got.push((ev.to_vec(), off)));
        asm.feed(&[0xBB], &mut |ev, off| got.push((ev.to_vec(), off)));
        asm.feed(&[0xBF], &mut |ev, off| got.push((ev.to_vec(), off)));
        asm.feed(EV1, &mut |ev, off| got.push((ev.to_vec(), off)));
        asm.feed(EV2, &mut |ev, off| got.push((ev.to_vec(), off)));
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].0, EV1);
        assert_eq!(got[0].1, 3 + EV1.len() as u64); // оффсеты учитывают BOM

        // При resume с ненулевого оффсета BOM-байты — обычные данные (мусор)
        let mut asm = TailAssembler::new(100);
        let mut got2: Vec<Vec<u8>> = Vec::new();
        asm.feed(&[0xEF, 0xBB, 0xBF], &mut |ev, _| got2.push(ev.to_vec()));
        asm.feed(b"\n", &mut |ev, _| got2.push(ev.to_vec()));
        asm.feed(EV1, &mut |ev, _| got2.push(ev.to_vec()));
        assert!(got2.is_empty());
        assert_eq!(asm.consumed_off(), 104); // мусорная строка потреблена
    }

    #[test]
    fn bom_only_prefix_never_emits() {
        let mut asm = TailAssembler::new(0);
        let mut got: Vec<Vec<u8>> = Vec::new();
        asm.feed(&[0xEF, 0xBB], &mut |ev, _| got.push(ev.to_vec()));
        assert!(got.is_empty());
        assert_eq!(asm.consumed_off(), 0);
        assert!(!asm.can_idle_close());
    }

    #[test]
    fn stop_drain_emits_complete_lines_only() {
        let mut asm = TailAssembler::new(0);
        let mut got: Vec<(Vec<u8>, u64)> = Vec::new();
        asm.feed(
            b"00:01.000001-2,CALL,0,Ctx='a\nb'\n00:02.000002-3,EXC",
            &mut |ev, off| got.push((ev.to_vec(), off)),
        );
        // Стоп: первое событие '\n'-терминировано... но оно уже закрыто маской
        // второго; открыт хвост "00:02.000002-3,EXC" без '\n' — не эмитится
        assert_eq!(got.len(), 1);
        let first_end = got[0].1;
        let mut drained: Vec<(Vec<u8>, u64)> = Vec::new();
        assert!(!asm.stop_drain(&mut |ev, off| drained.push((ev.to_vec(), off))));
        assert!(drained.is_empty());
        assert_eq!(asm.consumed_off(), first_end); // оффсет не переступил хвост
    }

    #[test]
    fn stop_drain_emits_terminated_event_and_drops_partial_line() {
        let mut asm = TailAssembler::new(0);
        let mut got: Vec<(Vec<u8>, u64)> = Vec::new();
        let data = b"00:01.000001-2,CALL,0,Ctx='a\nb'\npartial tail";
        asm.feed(data, &mut |ev, off| got.push((ev.to_vec(), off)));
        assert!(got.is_empty());
        // Дренаж: '\n'-терминированная часть события уходит, "partial tail" —
        // незавершённая СТРОКА — не эмитится никогда
        assert!(asm.stop_drain(&mut |ev, off| got.push((ev.to_vec(), off))));
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].0, b"00:01.000001-2,CALL,0,Ctx='a\nb'\n".to_vec());
        assert_eq!(got[0].1, 32);
        assert_eq!(asm.consumed_off(), 32);
    }

    #[test]
    fn resume_from_offset_counts_from_there() {
        let mut asm = TailAssembler::new(1000);
        let mut got: Vec<(Vec<u8>, u64)> = Vec::new();
        let data = [EV1, EV2].concat();
        asm.feed(&data, &mut |ev, off| got.push((ev.to_vec(), off)));
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].1, 1000 + EV1.len() as u64);
        assert_eq!(asm.next_read_off(), 1000 + data.len() as u64);
    }

    #[test]
    fn crlf_events_close_and_trim_by_parser() {
        // Ассемблер границ: \r\n — часть байтов события (обрезает parse_event)
        let mut asm = TailAssembler::new(0);
        let mut got: Vec<Vec<u8>> = Vec::new();
        asm.feed(
            b"00:01.000001-2,CALL,0,A=1\r\n00:02.000002-3,EXCP,1\r\n",
            &mut |ev, _| got.push(ev.to_vec()),
        );
        assert_eq!(got, vec![b"00:01.000001-2,CALL,0,A=1\r\n".to_vec()]);
        assert!(asm.can_idle_close());
    }

    /// Оракул: скормленный целиком статичный файл даёт те же события, что
    /// batch-сканер (последнее событие — через idle_close вместо EOF).
    #[test]
    fn matches_batch_scanner_on_static_content() {
        let mut data = Vec::new();
        data.extend_from_slice(b"\xEF\xBB\xBFpreamble junk\n");
        for i in 0..50 {
            data.extend_from_slice(
                format!("00:01.{i:06}-{i},CALL,0,N={i},Pad='x{}'\n", "y".repeat(i % 13))
                    .as_bytes(),
            );
        }
        let mut expected: Vec<Vec<u8>> = Vec::new();
        crate::parser::split_events(&data, |ev| expected.push(ev.to_vec()));

        for size in [1usize, 7, 4096] {
            let mut asm = TailAssembler::new(0);
            let mut got: Vec<Vec<u8>> = Vec::new();
            for chunk in data.chunks(size) {
                asm.feed(chunk, &mut |ev, _| got.push(ev.to_vec()));
            }
            asm.idle_close(&mut |ev, _| got.push(ev.to_vec()));
            assert_eq!(got, expected, "size={size}");
            assert_eq!(asm.consumed_off(), data.len() as u64);
        }
    }

    // --- чекпоинты ---

    #[test]
    fn ckpt_roundtrip() {
        let c = Ckpt {
            vol: 0xDEAD_BEEF,
            idx: u64::MAX,
            off: 123_456_789_012,
        };
        let path = r"E:\logs\Mem\rphost_1\25113021.log";
        let enc = encode_ckpt(path, &c);
        let (p2, c2) = decode_ckpt(&enc).unwrap();
        assert_eq!(p2, path);
        assert_eq!(c2, c);
        assert!(decode_ckpt("garbage").is_none());
        assert!(decode_ckpt("tjckpt\tv2\t1\t2\t3\tp").is_none());
        assert!(decode_ckpt("tjckpt\tv1\t1\t2\tNaN\tp").is_none());
    }

    #[test]
    fn ckpt_save_load() {
        let dir = std::env::temp_dir().join(format!("tj-follow-ckpt-{}", std::process::id()));
        fs::create_dir_all(&dir).unwrap();
        let c = Ckpt {
            vol: 7,
            idx: 42,
            off: 1000,
        };
        save_ckpt(&dir, r"C:\x\a.log", &c).unwrap();
        // Перезапись того же файла (rename поверх существующего)
        let c2 = Ckpt {
            vol: 7,
            idx: 42,
            off: 2000,
        };
        save_ckpt(&dir, r"C:\x\a.log", &c2).unwrap();
        save_ckpt(&dir, r"C:\x\b.log", &c).unwrap();
        let loaded = load_ckpts(&dir);
        assert_eq!(loaded.len(), 2);
        assert_eq!(loaded[r"C:\x\a.log"], c2);
        assert_eq!(loaded[r"C:\x\b.log"], c);
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn ckpt_name_stable() {
        // Хеш зафиксирован: смена алгоритма = потеря чекпоинтов при апгрейде
        assert_eq!(fnv1a64(b""), 0xcbf2_9ce4_8422_2325);
        assert_eq!(ckpt_file_name("a"), format!("{:016x}.ckpt", fnv1a64(b"a")));
        assert_ne!(ckpt_file_name(r"C:\a.log"), ckpt_file_name(r"C:\b.log"));
    }

    // --- identity ---

    #[test]
    fn identity_stable_and_distinct() {
        let dir = std::env::temp_dir().join(format!("tj-follow-id-{}", std::process::id()));
        fs::create_dir_all(&dir).unwrap();
        let p1 = dir.join("one.log");
        let p2 = dir.join("two.log");
        fs::write(&p1, b"x").unwrap();
        fs::write(&p2, b"y").unwrap();
        let id1a = file_identity(&File::open(&p1).unwrap()).unwrap();
        let id1b = file_identity(&File::open(&p1).unwrap()).unwrap();
        let id2 = file_identity(&File::open(&p2).unwrap()).unwrap();
        assert_eq!(id1a, id1b);
        assert_ne!(id1a, id2);
        // Пересоздание файла меняет identity (file index)
        fs::remove_file(&p1).unwrap();
        fs::write(&p1, b"z").unwrap();
        let id1c = file_identity(&File::open(&p1).unwrap()).unwrap();
        assert_ne!(id1a, id1c);
        fs::remove_dir_all(&dir).unwrap();
    }

    // --- очередь и батчер ---

    fn mk_chunk(shared: &Arc<FileShared>, rows: usize, bytes: usize, off: u64) -> TailChunk {
        TailChunk {
            file: Arc::clone(shared),
            gen: 0,
            end_off: off,
            data: vec![0xAB; bytes],
            rows,
        }
    }

    fn mk_shared() -> Arc<FileShared> {
        Arc::new(FileShared {
            path: PathBuf::from("x.log"),
            path_str: "x.log".into(),
            filename: "x.log".into(),
            file_path: "x.log".into(),
            date_prefix: String::new(),
            st: Mutex::new(FileCommit {
                gen: 0,
                committed: 0,
                vol: 0,
                idx: 0,
            }),
        })
    }

    #[test]
    fn queue_pop_timeout_and_done() {
        let q = FollowQueue::new(100, 1);
        let sh = mk_shared();
        assert!(q.push(mk_chunk(&sh, 1, 10, 5)));
        match q.pop(Instant::now()) {
            Pop::Chunk(c) => assert_eq!(c.end_off, 5),
            _ => panic!("ожидался кусок"),
        }
        match q.pop(Instant::now()) {
            Pop::Timeout => {}
            _ => panic!("ожидался Timeout"),
        }
        q.producer_done();
        match q.pop(Instant::now()) {
            Pop::Done => {}
            _ => panic!("ожидался Done"),
        }
        q.set_fatal();
        assert!(!q.push(mk_chunk(&sh, 1, 10, 6)));
    }

    #[test]
    fn batcher_deadline_starts_with_rows() {
        let sh = mk_shared();
        let mut b = FollowBatcher::new();
        b.add(mk_chunk(&sh, 0, 0, 10)); // мета без строк не взводит таймер
        assert!(b.deadline(Duration::from_secs(1)).is_none());
        b.add(mk_chunk(&sh, 5, 50, 20));
        assert!(b.deadline(Duration::from_secs(1)).is_some());
        assert_eq!(b.rows, 5);
        assert_eq!(b.buf.len(), 50);
        assert_eq!(b.meta.len(), 2);
    }

    #[test]
    fn commit_respects_generation_and_monotonic() {
        let dir = std::env::temp_dir().join(format!("tj-follow-gen-{}", std::process::id()));
        fs::create_dir_all(&dir).unwrap();
        let sh = mk_shared();
        let stats = Stats::default();
        let files = AtomicU64::new(1);
        let ctx = SinkCtx {
            state_dir: &dir,
            files_seen: &files,
            stats: &stats,
        };
        let mut meta = vec![
            (Arc::clone(&sh), 0u32, 100u64),
            (Arc::clone(&sh), 0u32, 50u64), // регресс — игнор
            (Arc::clone(&sh), 1u32, 10u64), // чужое поколение — игнор
        ];
        commit_meta(&ctx, &mut meta);
        assert_eq!(sh.st.lock().unwrap().committed, 100);
        assert!(meta.is_empty());
        // Поколение сменилось (truncate): новые оффсеты меньше — но коммитятся
        {
            let mut st = sh.st.lock().unwrap();
            st.gen = 1;
            st.committed = 0;
        }
        let mut meta = vec![(Arc::clone(&sh), 1u32, 30u64)];
        commit_meta(&ctx, &mut meta);
        assert_eq!(sh.st.lock().unwrap().committed, 30);
        fs::remove_dir_all(&dir).unwrap();
    }
}
