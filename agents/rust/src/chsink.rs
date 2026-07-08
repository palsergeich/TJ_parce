//! `--sink clickhouse` — batch-вставка нормализованных событий в ClickHouse.
//!
//! **Выбор клиента (фиксация по bakeoff-protocol §1.2).** Официальный
//! Rust-клиент (крейт `clickhouse` от ClickHouse Inc.) работает поверх
//! HTTP + RowBinary. Здесь — тот же протокол (HTTP POST
//! `INSERT … FORMAT RowBinary` на порт 8123), но своим минимальным
//! HTTP/1.1-клиентом на `std::net::TcpStream`. Причины:
//! - формат требует прозрачного проноса сырых байтов источника (KI-3:
//!   UTF-8 не валидируется), serde-модель крейта построена на `String`;
//! - агент остаётся без внешних зависимостей и без async-рантайма поверх
//!   синхронного конвейера (критерий сопровождаемости §1.3);
//! - политика батчей 50 000 строк / 64 МБ / 1000 мс реализуется явно и точно.
//!
//! **Маппинг NDJSON → таблица** (единый контракт всех участников bake-off):
//! - `timestamp` → DateTime64(6): дата+час из имени файла, MM:SS.ssssss из
//!   события; времени источника таймзона не приписывается (трактуется как
//!   UTC-стенка). Деградированный timestamp (не-датовое имя файла) →
//!   1970-01-01 00:00:00.000000 (тик 0);
//! - `duration` → UInt64 (переполнение сатурируется до u64::MAX);
//! - `event`, `filename`, `file_path` → String сырыми байтами;
//! - `level` → String сырым токеном (числовой уровень — десятичным текстом);
//! - `props` → ВСЕ свойства события, имя → значение-текст: в кавычках —
//!   распакованный контент (`''`→`'`, `""`→`"`), без кавычек — сырой токен;
//!   многострочные значения сохраняют реальные байты `\r\n`; дубликаты
//!   ключей сохраняются в Map как есть (KI-4).
//!
//! В RowBinary `LowCardinality(T)` кодируется прозрачно как `T`,
//! `Map(K,V)` — varint-число пар + пары подряд, `DateTime64(6)` — Int64 LE
//! (микросекунды эпохи), `UInt64` — 8 байт LE, `String` — varint-длина + байты.
//!
//! **Семантика сброса и ошибок.** Батч уходит, когда наступает первое из:
//! 50 000 строк, 64 МБ RowBinary, 1000 мс с первой строки батча (всё
//! настраивается `--batch-rows/--batch-bytes/--flush-ms`); остаток — по концу
//! входа. Вставка синхронная (`async_insert=0`): успех = HTTP 200, батч
//! закоммичен сервером (батчи ≤ max_insert_block_size ложатся одним блоком —
//! атомарно). Единственный повтор — пере-отправка целого батча на свежем
//! соединении, если ПЕРЕиспользованное keep-alive-соединение оказалось молча
//! закрытым сервером (обрыв при записи запроса или EOF до первого байта
//! ответа) — сервер такой запрос не начинал обрабатывать. Любая другая ошибка
//! фатальна: очередь переводится в fatal, воркеры прекращают разбор,
//! процесс завершается с exit 1. Зависаний нет: connect ограничен 5 с,
//! чтение/запись — 120 с.

use std::collections::VecDeque;
use std::fmt::Write as _;
use std::io::{self, ErrorKind, Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::sync::{Condvar, Mutex};
use std::time::{Duration, Instant};

use crate::parser::EventEmitter;

/// Политика батчей по умолчанию (bakeoff-protocol §1.2: 50 000 строк ИЛИ
/// 64 МБ ИЛИ 1000 мс — что наступит раньше).
pub const DEFAULT_BATCH_ROWS: usize = 50_000;
pub const DEFAULT_BATCH_BYTES: usize = 64 << 20;
pub const DEFAULT_FLUSH_MS: u64 = 1000;

/// Порог отправки куска RowBinary воркером в очередь синка.
pub const CHUNK_TARGET_BYTES: usize = 2 << 20;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const IO_TIMEOUT: Duration = Duration::from_secs(120);
/// Сколько байт тела ответа сохраняется для сообщений об ошибках.
const BODY_KEEP: usize = 16 << 10;

// ---------------------------------------------------------------------------
// DSN

/// Адрес назначения `--sink clickhouse[:http://host[:port][/db[/table]]]`.
/// По умолчанию `http://localhost:8123/tj_bench/events`.
pub struct ChTarget {
    /// `host:port` HTTP-интерфейса (для TCP-коннекта и заголовка Host).
    pub host: String,
    pub db: String,
    pub table: String,
}

/// Разбирает значение `--sink`. Поддерживается только HTTP-DSN: клиент
/// говорит по HTTP + RowBinary (см. шапку модуля), нативный TCP-порт (9001)
/// не обслуживается — на это отвечаем внятной ошибкой.
pub fn parse_sink_dsn(sink: &str) -> Result<ChTarget, String> {
    let mut t = ChTarget {
        host: "localhost:8123".to_string(),
        db: "tj_bench".to_string(),
        table: "events".to_string(),
    };
    let rest = match sink.strip_prefix("clickhouse") {
        Some("") => return Ok(t),
        Some(r) if r.starts_with(':') => &r[1..],
        _ => return Err(format!("не clickhouse-DSN: {sink}")),
    };
    if rest.is_empty() {
        return Err("пустой DSN в --sink clickhouse:<dsn>".to_string());
    }
    if rest.starts_with("//") {
        return Err(format!(
            "нативный TCP-DSN (clickhouse://…) не поддерживается; используйте HTTP-порт: \
             clickhouse:http://localhost:8123[/db[/table]] (получено: clickhouse:{rest})"
        ));
    }
    let url = match rest.strip_prefix("http://") {
        Some(u) => u,
        None => {
            return Err(format!(
                "поддерживается только http://-DSN (HTTP-порт ClickHouse, обычно 8123); получено: {rest}"
            ))
        }
    };
    if url.contains('?') || url.contains('#') || url.contains('@') {
        return Err(format!(
            "параметры запроса и авторизация в DSN не поддерживаются (пользователь default без пароля): {rest}"
        ));
    }
    let (hostport, path) = match url.find('/') {
        Some(i) => (&url[..i], &url[i + 1..]),
        None => (url, ""),
    };
    if hostport.is_empty() {
        return Err(format!("пустой host в DSN: {rest}"));
    }
    if let Some(colon) = hostport.rfind(':') {
        let port = &hostport[colon + 1..];
        if port.is_empty() || port.parse::<u16>().is_err() {
            return Err(format!("некорректный порт в DSN: {hostport}"));
        }
        t.host = hostport.to_string();
    } else {
        t.host = format!("{hostport}:8123");
    }
    let segs: Vec<&str> = path.split('/').filter(|s| !s.is_empty()).collect();
    if segs.len() > 2 {
        return Err(format!(
            "лишние сегменты пути в DSN (ожидается /db или /db/table): /{path}"
        ));
    }
    if let Some(db) = segs.first() {
        check_ident(db)?;
        t.db = (*db).to_string();
    }
    if let Some(tbl) = segs.get(1) {
        check_ident(tbl)?;
        t.table = (*tbl).to_string();
    }
    Ok(t)
}

/// Идентификатор БД/таблицы подставляется в текст INSERT — разрешаем только
/// безопасный алфавит.
fn check_ident(s: &str) -> Result<(), String> {
    if !s.is_empty() && s.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_') {
        Ok(())
    } else {
        Err(format!(
            "недопустимый идентификатор БД/таблицы {s:?} (разрешены [A-Za-z0-9_])"
        ))
    }
}

// ---------------------------------------------------------------------------
// Время

/// Дни от эпохи Unix для гражданской даты (алгоритм Говарда Хиннанта).
/// Целая арифметика без паник: «месяц 13» из кривого имени файла даёт
/// детерминированную условную дату, а не сбой (диапазоны имён файлов
/// формат сознательно не валидирует — format-spec §3).
fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400;
    let doy = (153 * (if m > 2 { m - 3 } else { m + 9 }) + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

/// Эпоха-микросекунды начала часа из date_prefix `20YY-MM-DDTHH:`.
/// `None` для пустого префикса — деградированный timestamp, событие ляжет
/// в 1970-01-01 00:00:00.000000 (единый контракт bake-off).
pub fn hour_base_micros(date_prefix: &str) -> Option<i64> {
    let b = date_prefix.as_bytes();
    if b.len() != 14 {
        return None;
    }
    let d = |i: usize| (b[i] - b'0') as i64;
    let y = d(0) * 1000 + d(1) * 100 + d(2) * 10 + d(3);
    let m = d(5) * 10 + d(6);
    let day = d(8) * 10 + d(9);
    let h = d(11) * 10 + d(12);
    Some((days_from_civil(y, m, day) * 86_400 + h * 3_600) * 1_000_000)
}

/// Тики DateTime64(6): база часа + `MM:SS.ssssss` события.
fn event_micros(base: Option<i64>, time_part: &[u8]) -> i64 {
    let Some(base) = base else { return 0 };
    if time_part.len() != 12 {
        return 0; // при маске §2.1 не бывает; защита от паники индексации
    }
    let d = |i: usize| (time_part[i] - b'0') as i64;
    let sec = (d(0) * 10 + d(1)) * 60 + d(3) * 10 + d(4);
    let mut frac = 0i64;
    for i in 6..12 {
        frac = frac * 10 + d(i);
    }
    base + sec * 1_000_000 + frac
}

/// Длительность из сырых цифр маски; переполнение UInt64 сатурируется.
fn parse_u64_saturating(digits: &[u8]) -> u64 {
    let mut v: u64 = 0;
    for &c in digits {
        v = v.saturating_mul(10).saturating_add((c - b'0') as u64);
    }
    v
}

// ---------------------------------------------------------------------------
// RowBinary

fn put_uvarint(dst: &mut Vec<u8>, mut v: u64) {
    while v >= 0x80 {
        dst.push((v as u8) | 0x80);
        v >>= 7;
    }
    dst.push(v as u8);
}

fn put_str(dst: &mut Vec<u8>, s: &[u8]) {
    put_uvarint(dst, s.len() as u64);
    dst.extend_from_slice(s);
}

/// Пачка закодированных RowBinary-строк. `ends[i]` — конец i-й строки
/// в `data`: границы нужны синку для точной нарезки батчей.
pub struct Chunk {
    pub data: Vec<u8>,
    pub ends: Vec<usize>,
}

impl Chunk {
    fn new() -> Self {
        Chunk {
            data: Vec::with_capacity(CHUNK_TARGET_BYTES + (64 << 10)),
            ends: Vec::with_capacity(4096),
        }
    }
}

/// Эмиттер RowBinary-строк — вторая реализация `parser::EventEmitter`
/// (первая, [`crate::parser::JsonEmitter`], собирает NDJSON). Синк цепляется
/// на уровне разобранного события: значения свойств приходят сырыми байтами
/// источника ДО JSON-экранирования, повторного разбора JSON нет.
///
/// Скрадч-буферы переиспользуются между событиями и файлами.
pub struct RowEmitter {
    chunk: Chunk,
    base_micros: Option<i64>,
    filename: Vec<u8>,
    file_path: Vec<u8>,
    /// имя₀·значение₀·имя₁·… текущего события (значение собирается из
    /// фрагментов между экранирующими кавычками)
    arena: Vec<u8>,
    /// (конец имени, конец значения) пары в `arena`
    pairs: Vec<(usize, usize)>,
    name_end: usize,
    pending: bool,
}

impl RowEmitter {
    pub fn new() -> Self {
        RowEmitter {
            chunk: Chunk::new(),
            base_micros: None,
            filename: Vec::new(),
            file_path: Vec::new(),
            arena: Vec::new(),
            pairs: Vec::new(),
            name_end: 0,
            pending: false,
        }
    }

    /// Контекст файла: база часа из имени (`hour_base_micros`) и сырые байты
    /// filename/file_path (клонируются в собственные буферы).
    pub fn begin_file(&mut self, base_micros: Option<i64>, filename: &[u8], file_path: &[u8]) {
        self.base_micros = base_micros;
        self.filename.clear();
        self.filename.extend_from_slice(filename);
        self.file_path.clear();
        self.file_path.extend_from_slice(file_path);
    }

    pub fn chunk_bytes(&self) -> usize {
        self.chunk.data.len()
    }

    /// Забирает накопленный кусок (целые строки), оставляя эмиттеру свежий.
    pub fn take_chunk(&mut self) -> Chunk {
        std::mem::replace(&mut self.chunk, Chunk::new())
    }
}

impl Default for RowEmitter {
    fn default() -> Self {
        Self::new()
    }
}

impl EventEmitter for RowEmitter {
    #[inline]
    fn header(&mut self, time_part: &[u8], duration: &[u8], event: &[u8], level: &[u8]) {
        let out = &mut self.chunk.data;
        out.extend_from_slice(&event_micros(self.base_micros, time_part).to_le_bytes());
        out.extend_from_slice(&parse_u64_saturating(duration).to_le_bytes());
        put_str(out, event);
        put_str(out, level);
        put_str(out, &self.filename);
        put_str(out, &self.file_path);
        self.arena.clear();
        self.pairs.clear();
        self.pending = false;
    }

    #[inline]
    fn prop_name(&mut self, name: &[u8]) {
        if self.pending {
            self.pairs.push((self.name_end, self.arena.len()));
        }
        self.arena.extend_from_slice(name);
        self.name_end = self.arena.len();
        self.pending = true;
    }

    #[inline]
    fn quoted_begin(&mut self) {}

    #[inline]
    fn quoted_frag(&mut self, frag: &[u8]) {
        self.arena.extend_from_slice(frag);
    }

    #[inline]
    fn quoted_quote(&mut self, quote: u8) {
        self.arena.push(quote);
    }

    #[inline]
    fn quoted_end(&mut self) {}

    #[inline]
    fn unquoted(&mut self, _name: &[u8], val: &[u8]) {
        self.arena.extend_from_slice(val);
    }

    #[inline]
    fn finish(&mut self) {
        if self.pending {
            self.pairs.push((self.name_end, self.arena.len()));
            self.pending = false;
        }
        let out = &mut self.chunk.data;
        put_uvarint(out, self.pairs.len() as u64);
        let mut start = 0usize;
        for &(ne, ve) in &self.pairs {
            put_str(out, &self.arena[start..ne]);
            put_str(out, &self.arena[ne..ve]);
            start = ve;
        }
        self.chunk.ends.push(out.len());
    }
}

// ---------------------------------------------------------------------------
// Очередь воркеры → синк

/// Очередь кусков с байтовым лимитом (обратное давление: разбор не обгоняет
/// вставку безгранично) и флагом фатальной ошибки вставки.
pub struct SinkQueue {
    st: Mutex<QueueState>,
    cv_space: Condvar, // ждут воркеры: место в очереди или фатал
    cv_data: Condvar,  // ждёт синк: кусок или завершение продюсеров
    cap_bytes: usize,
}

struct QueueState {
    q: VecDeque<Chunk>,
    bytes: usize,
    producers: usize,
    fatal: bool,
}

pub enum Pop {
    Chunk(Chunk),
    Timeout,
    Done,
}

impl SinkQueue {
    pub fn new(cap_bytes: usize, producers: usize) -> Self {
        SinkQueue {
            st: Mutex::new(QueueState {
                q: VecDeque::new(),
                bytes: 0,
                producers,
                fatal: false,
            }),
            cv_space: Condvar::new(),
            cv_data: Condvar::new(),
            cap_bytes,
        }
    }

    /// `false` — вставка фатально упала, разбор надо прекращать.
    pub fn push(&self, c: Chunk) -> bool {
        let mut st = self.st.lock().unwrap();
        loop {
            if st.fatal {
                return false;
            }
            // Пустая очередь принимает кусок любого размера — прогресс гарантирован
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

    /// Воркер завершился (успешно или по фаталу).
    pub fn producer_done(&self) {
        let mut st = self.st.lock().unwrap();
        st.producers -= 1;
        let last = st.producers == 0;
        drop(st);
        if last {
            self.cv_data.notify_all();
        }
    }

    /// Ошибка вставки: буферизованное сбрасывается, воркеры получают отказ.
    pub fn set_fatal(&self) {
        let mut st = self.st.lock().unwrap();
        st.fatal = true;
        st.q.clear();
        st.bytes = 0;
        drop(st);
        self.cv_space.notify_all();
    }

    /// Кусок / истёкший дедлайн (если задан) / все продюсеры завершились
    /// и очередь пуста.
    pub fn pop(&self, deadline: Option<Instant>) -> Pop {
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
            match deadline {
                Some(d) => {
                    let now = Instant::now();
                    if now >= d {
                        return Pop::Timeout;
                    }
                    st = self.cv_data.wait_timeout(st, d - now).unwrap().0;
                }
                None => st = self.cv_data.wait(st).unwrap(),
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Батчер

/// Сборщик батчей с точной политикой «50 000 строк ИЛИ 64 МБ ИЛИ flush-ms».
/// Куски режутся по границам строк: лимиты не превышаются (единственное
/// исключение — одиночная строка крупнее byte-лимита уходит своим батчем).
pub struct Batcher {
    rows_max: usize,
    bytes_max: usize,
    buf: Vec<u8>,
    rows: usize,
    started: Option<Instant>,
}

impl Batcher {
    pub fn new(rows_max: usize, bytes_max: usize) -> Self {
        Batcher {
            rows_max,
            bytes_max,
            buf: Vec::new(),
            rows: 0,
            started: None,
        }
    }

    /// Дедлайн сброса по таймеру: `flush_after` от ПЕРВОЙ строки батча.
    pub fn deadline(&self, flush_after: Duration) -> Option<Instant> {
        self.started.map(|t| t + flush_after)
    }

    /// Добавляет кусок, вызывая `flush(тело, строк)` на каждом заполнении.
    pub fn feed<F>(&mut self, c: &Chunk, flush: &mut F) -> Result<(), ChError>
    where
        F: FnMut(&[u8], usize) -> Result<(), ChError>,
    {
        let mut row = 0usize;
        let mut off = 0usize;
        let n = c.ends.len();
        while row < n {
            let rows_avail = self.rows_max - self.rows;
            let bytes_avail = self.bytes_max.saturating_sub(self.buf.len());
            let mut take = 0usize;
            let mut take_bytes = 0usize;
            while row + take < n && take < rows_avail {
                let sz = c.ends[row + take] - off;
                if sz > bytes_avail {
                    break;
                }
                take += 1;
                take_bytes = sz;
            }
            if take == 0 {
                if self.rows > 0 {
                    self.flush_now(flush)?;
                    continue; // пересчитать лимиты с пустым батчем
                }
                // Одиночная строка крупнее bytes_max — единственное превышение
                take = 1;
                take_bytes = c.ends[row] - off;
            }
            if self.rows == 0 {
                self.started = Some(Instant::now());
            }
            self.buf.extend_from_slice(&c.data[off..off + take_bytes]);
            self.rows += take;
            off += take_bytes;
            row += take;
            if self.rows >= self.rows_max || self.buf.len() >= self.bytes_max {
                self.flush_now(flush)?;
            }
        }
        Ok(())
    }

    /// Отправляет накопленное (частичный батч — по таймеру или концу входа).
    pub fn flush_now<F>(&mut self, flush: &mut F) -> Result<(), ChError>
    where
        F: FnMut(&[u8], usize) -> Result<(), ChError>,
    {
        if self.rows > 0 {
            flush(&self.buf, self.rows)?;
            self.buf.clear();
            self.rows = 0;
        }
        self.started = None;
        Ok(())
    }
}

/// Цикл синка (выполняется в главном потоке): очередь → батчи → INSERT.
/// Счётчики обновляются по мере подтверждённых вставок — при ошибке в них
/// остаётся фактически вставленное.
pub fn run_sink(
    client: &mut ChClient,
    queue: &SinkQueue,
    batch_rows: usize,
    batch_bytes: usize,
    flush_after: Duration,
    inserted_rows: &mut u64,
    batches: &mut u64,
) -> Result<(), ChError> {
    let mut b = Batcher::new(batch_rows, batch_bytes);
    let mut do_flush = |body: &[u8], rows: usize| -> Result<(), ChError> {
        client.insert(body)?;
        *inserted_rows += rows as u64;
        *batches += 1;
        Ok(())
    };
    loop {
        match queue.pop(b.deadline(flush_after)) {
            Pop::Chunk(c) => b.feed(&c, &mut do_flush)?,
            Pop::Timeout => b.flush_now(&mut do_flush)?,
            Pop::Done => break,
        }
    }
    b.flush_now(&mut do_flush)
}

// ---------------------------------------------------------------------------
// Минимальный HTTP/1.1-клиент

#[derive(Debug)]
pub struct ChError(pub String);

/// Ошибка вставки с флагом «имеет смысл повторить» (follow-режим).
#[derive(Debug)]
pub struct ChInsertError {
    pub err: ChError,
    pub retryable: bool,
}

impl std::fmt::Display for ChError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

struct Response {
    status: u16,
    body: Vec<u8>, // первые BODY_KEEP байт (для сообщений об ошибках)
    close: bool,
}

struct TryError {
    err: ChError,
    /// Обрыв переиспользованного keep-alive до начала обработки запроса
    /// сервером — единственный случай, когда батч безопасно переслать.
    retryable: bool,
}

pub struct ChClient {
    host: String,
    /// Готовый percent-закодированный query-параметр INSERT.
    insert_query: String,
    conn: Option<TcpStream>,
}

impl ChClient {
    pub fn new(t: &ChTarget) -> ChClient {
        let sql = format!(
            "INSERT INTO {}.{} (timestamp,duration,event,level,filename,file_path,props) FORMAT RowBinary",
            t.db, t.table
        );
        ChClient {
            host: t.host.clone(),
            insert_query: url_encode(&sql),
            conn: None,
        }
    }

    /// Доступность сервера и существование таблицы — проверяется ДО разбора,
    /// чтобы неверный порт/БД/таблица давали немедленный exit 1 без зависаний.
    pub fn check_ready(&mut self, t: &ChTarget) -> Result<(), ChError> {
        let head = format!(
            "GET /ping HTTP/1.1\r\nHost: {}\r\nConnection: keep-alive\r\n\r\n",
            self.host
        );
        let r = self.request(&head, b"")?;
        if r.status != 200 {
            return Err(ChError(format!(
                "/ping вернул HTTP {}: {}",
                r.status,
                body_snippet(&r.body)
            )));
        }
        let sql = format!("EXISTS TABLE {}.{}", t.db, t.table);
        let out = self.query(&sql)?;
        if out.trim() != "1" {
            return Err(ChError(format!(
                "таблица {}.{} не существует (EXISTS вернул {:?})",
                t.db,
                t.table,
                out.trim()
            )));
        }
        Ok(())
    }

    /// Запрос без данных: SQL в теле POST, ответ — текст (TabSeparated).
    pub fn query(&mut self, sql: &str) -> Result<String, ChError> {
        let head = format!(
            "POST / HTTP/1.1\r\nHost: {}\r\nContent-Length: {}\r\nConnection: keep-alive\r\n\r\n",
            self.host,
            sql.len()
        );
        let r = self.request(&head, sql.as_bytes())?;
        if r.status != 200 {
            return Err(ChError(format!(
                "запрос отвергнут, HTTP {}: {}",
                r.status,
                body_snippet(&r.body)
            )));
        }
        Ok(String::from_utf8_lossy(&r.body).into_owned())
    }

    /// Вставка одного батча RowBinary. Синхронно: успех = HTTP 200 =
    /// батч закоммичен (`async_insert=0` зафиксирован протоколом bake-off).
    pub fn insert(&mut self, body: &[u8]) -> Result<(), ChError> {
        self.insert_classified(body).map_err(|e| e.err)
    }

    /// То же, что [`ChClient::insert`], но с классификацией ошибки для
    /// follow-ретраев: транспортные сбои (сервер мог и не получить батч,
    /// а мог и закоммитить — at-least-once допускает повтор) и временные
    /// статусы 5xx/408/429 — retryable; остальные HTTP-статусы (кривая
    /// схема/данные/нет таблицы) повторять бессмысленно — фатал.
    pub fn insert_classified(&mut self, body: &[u8]) -> Result<(), ChInsertError> {
        let head = format!(
            "POST /?query={}&async_insert=0 HTTP/1.1\r\nHost: {}\r\nContent-Length: {}\r\nConnection: keep-alive\r\n\r\n",
            self.insert_query,
            self.host,
            body.len()
        );
        let r = self.request(&head, body).map_err(|err| ChInsertError {
            err,
            retryable: true,
        })?;
        if r.status != 200 {
            return Err(ChInsertError {
                err: ChError(format!(
                    "INSERT отвергнут, HTTP {}: {}",
                    r.status,
                    body_snippet(&r.body)
                )),
                retryable: matches!(r.status, 408 | 429 | 500..=599),
            });
        }
        Ok(())
    }

    fn connect(&self) -> Result<TcpStream, ChError> {
        let addrs: Vec<_> = self
            .host
            .to_socket_addrs()
            .map_err(|e| ChError(format!("адрес {} не разрешается: {e}", self.host)))?
            .collect();
        let mut last: Option<io::Error> = None;
        for a in &addrs {
            match TcpStream::connect_timeout(a, CONNECT_TIMEOUT) {
                Ok(s) => {
                    let _ = s.set_nodelay(true);
                    let _ = s.set_read_timeout(Some(IO_TIMEOUT));
                    let _ = s.set_write_timeout(Some(IO_TIMEOUT));
                    return Ok(s);
                }
                Err(e) => last = Some(e),
            }
        }
        Err(ChError(match last {
            Some(e) => format!("не удалось подключиться к {}: {e}", self.host),
            None => format!("адрес {} не разрешился ни в один сокет", self.host),
        }))
    }

    /// Запрос с одним повтором при молча закрытом сервером keep-alive
    /// (повтор только когда сервер гарантированно не начинал обработку).
    fn request(&mut self, head: &str, body: &[u8]) -> Result<Response, ChError> {
        match self.try_request(head, body) {
            Ok(r) => Ok(r),
            Err(te) if te.retryable => self.try_request(head, body).map_err(|te| te.err),
            Err(te) => Err(te.err),
        }
    }

    fn try_request(&mut self, head: &str, body: &[u8]) -> Result<Response, TryError> {
        let reused = self.conn.is_some();
        let mut s = match self.conn.take() {
            Some(s) => s,
            None => self.connect().map_err(|err| TryError {
                err,
                retryable: false,
            })?,
        };
        if let Err(e) = s
            .write_all(head.as_bytes())
            .and_then(|()| s.write_all(body))
            .and_then(|()| s.flush())
        {
            return Err(TryError {
                retryable: reused && is_conn_drop(&e),
                err: ChError(format!("отправка запроса на {}: {e}", self.host)),
            });
        }
        match read_response(&mut s) {
            Ok(r) => {
                if !r.close {
                    self.conn = Some(s); // keep-alive
                }
                Ok(r)
            }
            Err((e, got_any)) => Err(TryError {
                retryable: reused && !got_any && is_conn_drop(&e),
                err: ChError(format!("чтение ответа от {}: {e}", self.host)),
            }),
        }
    }
}

/// Ошибки, означающие «соединение умерло» (стухший keep-alive), в отличие
/// от таймаутов, когда сервер жив и мог принять батч — их повторять нельзя.
fn is_conn_drop(e: &io::Error) -> bool {
    matches!(
        e.kind(),
        ErrorKind::UnexpectedEof
            | ErrorKind::ConnectionReset
            | ErrorKind::ConnectionAborted
            | ErrorKind::BrokenPipe
    )
}

fn body_snippet(b: &[u8]) -> String {
    let s = String::from_utf8_lossy(&b[..b.len().min(2048)]);
    s.trim().replace('\n', " | ")
}

/// Percent-кодирование query-параметра (консервативный unreserved-набор).
fn url_encode(s: &str) -> String {
    let mut out = String::with_capacity(s.len() * 3 / 2);
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => {
                let _ = write!(out, "%{b:02X}");
            }
        }
    }
    out
}

/// Буферизованный читатель ответа поверх TcpStream.
struct LineReader<'a> {
    s: &'a mut TcpStream,
    buf: Vec<u8>,
    pos: usize,
    len: usize,
    /// Получен ли хоть один байт ответа (решает, можно ли повторять запрос).
    got_any: bool,
}

impl<'a> LineReader<'a> {
    fn new(s: &'a mut TcpStream) -> Self {
        LineReader {
            s,
            buf: vec![0u8; 16 << 10],
            pos: 0,
            len: 0,
            got_any: false,
        }
    }

    fn fill(&mut self) -> io::Result<()> {
        if self.pos == self.len {
            self.pos = 0;
            self.len = 0;
            let n = self.s.read(&mut self.buf)?;
            if n == 0 {
                return Err(io::Error::new(
                    ErrorKind::UnexpectedEof,
                    "соединение закрыто сервером",
                ));
            }
            self.len = n;
            self.got_any = true;
        }
        Ok(())
    }

    fn read_byte(&mut self) -> io::Result<u8> {
        self.fill()?;
        let b = self.buf[self.pos];
        self.pos += 1;
        Ok(b)
    }

    /// Строка до `\n` (без `\r\n`), лимит 64 КБ.
    fn read_line(&mut self) -> io::Result<Vec<u8>> {
        let mut line = Vec::new();
        loop {
            let b = self.read_byte()?;
            if b == b'\n' {
                if line.last() == Some(&b'\r') {
                    line.pop();
                }
                return Ok(line);
            }
            if line.len() >= (64 << 10) {
                return Err(io::Error::new(
                    ErrorKind::InvalidData,
                    "слишком длинная строка HTTP-заголовка",
                ));
            }
            line.push(b);
        }
    }

    /// Читает ровно `n` байт; в `out` сохраняется не больше `cap` суммарно.
    fn read_n(&mut self, mut n: u64, out: &mut Vec<u8>, cap: usize) -> io::Result<()> {
        while n > 0 {
            self.fill()?;
            let avail = (self.len - self.pos).min(n.min(usize::MAX as u64) as usize);
            let keep = cap.saturating_sub(out.len()).min(avail);
            out.extend_from_slice(&self.buf[self.pos..self.pos + keep]);
            self.pos += avail;
            n -= avail as u64;
        }
        Ok(())
    }
}

/// Читает один HTTP-ответ. Ошибка несёт флаг `got_any` на момент сбоя.
fn read_response(s: &mut TcpStream) -> Result<Response, (io::Error, bool)> {
    let mut r = LineReader::new(s);
    macro_rules! te {
        ($e:expr) => {
            match $e {
                Ok(v) => v,
                Err(e) => return Err((e, r.got_any)),
            }
        };
    }

    let status_line = te!(r.read_line());
    let (status, http10) = te!(parse_status_line(&status_line));
    let mut content_length: Option<u64> = None;
    let mut chunked = false;
    let mut close = http10;
    loop {
        let h = te!(r.read_line());
        if h.is_empty() {
            break;
        }
        let Some(colon) = h.iter().position(|&b| b == b':') else {
            continue;
        };
        let name = h[..colon].to_ascii_lowercase();
        let val = String::from_utf8_lossy(&h[colon + 1..]).trim().to_ascii_lowercase();
        match name.as_slice() {
            b"content-length" => content_length = val.parse::<u64>().ok(),
            b"transfer-encoding" => chunked = val.contains("chunked"),
            b"connection" => close = close || val.contains("close"),
            _ => {}
        }
    }

    let mut body = Vec::new();
    if chunked {
        loop {
            let szline = te!(r.read_line());
            let hexpart: &[u8] = szline
                .split(|&b| b == b';')
                .next()
                .unwrap_or(&[]);
            let sz = te!(parse_hex(hexpart));
            if sz == 0 {
                // трейлеры до пустой строки
                loop {
                    let t = te!(r.read_line());
                    if t.is_empty() {
                        break;
                    }
                }
                break;
            }
            te!(r.read_n(sz, &mut body, BODY_KEEP));
            te!(r.read_line()); // CRLF после чанка
        }
    } else if let Some(n) = content_length {
        te!(r.read_n(n, &mut body, BODY_KEEP));
    } else {
        // Ни длины, ни chunked — тело до закрытия соединения
        close = true;
        loop {
            match r.read_byte() {
                Ok(b) => {
                    if body.len() < BODY_KEEP {
                        body.push(b);
                    }
                }
                Err(e) if e.kind() == ErrorKind::UnexpectedEof => break,
                Err(e) => return Err((e, true)),
            }
        }
    }
    Ok(Response {
        status,
        body,
        close,
    })
}

/// `HTTP/1.x NNN …` → (статус, это HTTP/1.0).
fn parse_status_line(line: &[u8]) -> io::Result<(u16, bool)> {
    let bad = || io::Error::new(ErrorKind::InvalidData, "некорректная статус-строка HTTP");
    let mut it = line.split(|&b| b == b' ').filter(|t| !t.is_empty());
    let ver = it.next().ok_or_else(bad)?;
    if !ver.starts_with(b"HTTP/") {
        return Err(bad());
    }
    let code = it.next().ok_or_else(bad)?;
    let status = std::str::from_utf8(code)
        .ok()
        .and_then(|s| s.parse::<u16>().ok())
        .ok_or_else(bad)?;
    Ok((status, ver == b"HTTP/1.0"))
}

fn parse_hex(b: &[u8]) -> io::Result<u64> {
    let s = std::str::from_utf8(b)
        .map_err(|_| io::Error::new(ErrorKind::InvalidData, "не-UTF8 размер чанка"))?;
    u64::from_str_radix(s.trim(), 16)
        .map_err(|_| io::Error::new(ErrorKind::InvalidData, "некорректный размер HTTP-чанка"))
}

// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::parser::parse_event;
    use std::net::TcpListener;
    use std::thread;

    // --- время ---

    #[test]
    fn civil_days_match_naive_oracle() {
        let leap = |y: i64| (y % 4 == 0 && y % 100 != 0) || y % 400 == 0;
        let mdays = [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
        let mut days = 0i64;
        for y in 1970..2100i64 {
            for m in 1..=12i64 {
                let dm = mdays[(m - 1) as usize] + i64::from(m == 2 && leap(y));
                for d in 1..=dm {
                    assert_eq!(days_from_civil(y, m, d), days, "{y}-{m}-{d}");
                    days += 1;
                }
            }
        }
    }

    #[test]
    fn hour_base() {
        // 2025-11-30 00:00:00 UTC = 1764460800; +16 ч = 1764518400
        assert_eq!(
            hour_base_micros("2025-11-30T16:"),
            Some(1_764_518_400_000_000)
        );
        assert_eq!(hour_base_micros(""), None);
    }

    #[test]
    fn event_ticks() {
        let base = hour_base_micros("2025-11-30T16:");
        assert_eq!(
            event_micros(base, b"06:58.904004"),
            1_764_518_400_000_000 + (6 * 60 + 58) * 1_000_000 + 904_004
        );
        // Деградация: контракт bake-off — ровно 1970-01-01 00:00:00.000000
        assert_eq!(event_micros(None, b"06:58.904004"), 0);
    }

    #[test]
    fn duration_saturates() {
        assert_eq!(parse_u64_saturating(b"0"), 0);
        assert_eq!(parse_u64_saturating(b"17500000000"), 17_500_000_000);
        assert_eq!(parse_u64_saturating(b"18446744073709551615"), u64::MAX);
        assert_eq!(parse_u64_saturating(b"18446744073709551616"), u64::MAX);
        assert_eq!(parse_u64_saturating(b"99999999999999999999999"), u64::MAX);
    }

    // --- RowBinary ---

    fn get_uvarint(b: &[u8], p: &mut usize) -> u64 {
        let mut v = 0u64;
        let mut shift = 0;
        loop {
            let x = b[*p];
            *p += 1;
            v |= u64::from(x & 0x7f) << shift;
            if x & 0x80 == 0 {
                return v;
            }
            shift += 7;
        }
    }

    fn get_str<'a>(b: &'a [u8], p: &mut usize) -> &'a [u8] {
        let n = get_uvarint(b, p) as usize;
        let s = &b[*p..*p + n];
        *p += n;
        s
    }

    #[test]
    fn uvarint_roundtrip() {
        for v in [0u64, 1, 127, 128, 300, 16383, 16384, u64::MAX] {
            let mut buf = Vec::new();
            put_uvarint(&mut buf, v);
            let mut p = 0;
            assert_eq!(get_uvarint(&buf, &mut p), v);
            assert_eq!(p, buf.len());
        }
    }

    #[test]
    fn row_encoding_roundtrip() {
        let mut em = RowEmitter::new();
        em.begin_file(
            hour_base_micros("2025-11-30T21:"),
            b"25113021.log",
            b"input\\rphost_1\\25113021.log",
        );
        // Кавычки-дубли, многострочность с \r\n, число, пустое значение,
        // always-string-поле и дубликат ключа — всё в одном событии
        let ev = b"00:01.000007-007,CALL,0,Usr='x''y',B=\"p\"\"q\",Ctx='line1\r\nline2',N=42,Empty=,Guid=007,N=43";
        assert!(parse_event(&ev[..], &mut em));
        let c = em.take_chunk();
        assert_eq!(c.ends.len(), 1);
        assert_eq!(*c.ends.last().unwrap(), c.data.len());

        let b = &c.data[..];
        let mut p = 0usize;
        let ts = i64::from_le_bytes(b[0..8].try_into().unwrap());
        p += 8;
        assert_eq!(ts, hour_base_micros("2025-11-30T21:").unwrap() + 1_000_007);
        let dur = u64::from_le_bytes(b[p..p + 8].try_into().unwrap());
        p += 8;
        assert_eq!(dur, 7); // "007" → 7
        assert_eq!(get_str(b, &mut p), b"CALL");
        assert_eq!(get_str(b, &mut p), b"0"); // level — текстом
        assert_eq!(get_str(b, &mut p), b"25113021.log");
        assert_eq!(get_str(b, &mut p), b"input\\rphost_1\\25113021.log");
        let n = get_uvarint(b, &mut p);
        assert_eq!(n, 7);
        let pairs: Vec<(Vec<u8>, Vec<u8>)> = (0..n)
            .map(|_| (get_str(b, &mut p).to_vec(), get_str(b, &mut p).to_vec()))
            .collect();
        assert_eq!(pairs[0], (b"Usr".to_vec(), b"x'y".to_vec()));
        assert_eq!(pairs[1], (b"B".to_vec(), b"p\"q".to_vec()));
        assert_eq!(pairs[2], (b"Ctx".to_vec(), b"line1\r\nline2".to_vec()));
        assert_eq!(pairs[3], (b"N".to_vec(), b"42".to_vec()));
        assert_eq!(pairs[4], (b"Empty".to_vec(), b"".to_vec()));
        assert_eq!(pairs[5], (b"Guid".to_vec(), b"007".to_vec()));
        assert_eq!(pairs[6], (b"N".to_vec(), b"43".to_vec())); // дубликат сохранён
        assert_eq!(p, c.data.len());
    }

    #[test]
    fn row_degraded_timestamp_and_no_props() {
        let mut em = RowEmitter::new();
        em.begin_file(hour_base_micros("notadate"), b"notadate.log", b"a\\b\\notadate.log");
        assert!(parse_event(b"59:59.999999-1,EXCP,", &mut em));
        let c = em.take_chunk();
        let b = &c.data[..];
        let mut p = 0usize;
        assert_eq!(i64::from_le_bytes(b[0..8].try_into().unwrap()), 0);
        p += 8;
        assert_eq!(u64::from_le_bytes(b[p..p + 8].try_into().unwrap()), 1);
        p += 8;
        assert_eq!(get_str(b, &mut p), b"EXCP");
        assert_eq!(get_str(b, &mut p), b""); // пустой level
        assert_eq!(get_str(b, &mut p), b"notadate.log");
        assert_eq!(get_str(b, &mut p), b"a\\b\\notadate.log");
        assert_eq!(get_uvarint(b, &mut p), 0); // свойств нет
        assert_eq!(p, c.data.len());
    }

    // --- DSN ---

    #[test]
    fn dsn_parsing() {
        let t = parse_sink_dsn("clickhouse").unwrap();
        assert_eq!(
            (t.host.as_str(), t.db.as_str(), t.table.as_str()),
            ("localhost:8123", "tj_bench", "events")
        );
        let t = parse_sink_dsn("clickhouse:http://127.0.0.1:9999/mydb/mytbl").unwrap();
        assert_eq!(
            (t.host.as_str(), t.db.as_str(), t.table.as_str()),
            ("127.0.0.1:9999", "mydb", "mytbl")
        );
        let t = parse_sink_dsn("clickhouse:http://ch-host/db1").unwrap();
        assert_eq!(
            (t.host.as_str(), t.db.as_str(), t.table.as_str()),
            ("ch-host:8123", "db1", "events")
        );
        assert!(parse_sink_dsn("clickhouse:").is_err());
        assert!(parse_sink_dsn("clickhouse://localhost:9001/tj_bench").is_err()); // native TCP
        assert!(parse_sink_dsn("clickhouse:https://h/db").is_err());
        assert!(parse_sink_dsn("clickhouse:http://h:1/db/t/extra").is_err());
        assert!(parse_sink_dsn("clickhouse:http://h:1/bad-name").is_err());
        assert!(parse_sink_dsn("clickhouse:http://h:notaport/db").is_err());
        assert!(parse_sink_dsn("clickhouse:http://h:1/db?p=1").is_err());
        assert!(parse_sink_dsn("clickhouse:http://user@h:1/db").is_err());
        assert!(parse_sink_dsn("file:out.jsonl").is_err());
    }

    #[test]
    fn url_encoding() {
        assert_eq!(url_encode("SELECT 1"), "SELECT%201");
        assert_eq!(
            url_encode("INSERT INTO a.b (x,y) FORMAT RowBinary"),
            "INSERT%20INTO%20a.b%20%28x%2Cy%29%20FORMAT%20RowBinary"
        );
    }

    // --- батчер ---

    fn mk_chunk(rows: usize, row_size: usize) -> Chunk {
        let mut c = Chunk::new();
        for i in 0..rows {
            c.data.extend(std::iter::repeat_n(i as u8, row_size));
            c.ends.push(c.data.len());
        }
        c
    }

    #[test]
    fn batcher_rows_exact() {
        let mut b = Batcher::new(10, 1 << 20);
        let mut flushed: Vec<(usize, usize)> = Vec::new();
        let mut f = |body: &[u8], rows: usize| {
            flushed.push((body.len(), rows));
            Ok(())
        };
        for _ in 0..3 {
            b.feed(&mk_chunk(7, 5), &mut f).unwrap(); // 21 строка по 5 байт
        }
        b.flush_now(&mut f).unwrap();
        // батчи ровно по 10 строк, хвост — 1
        assert_eq!(flushed, vec![(50, 10), (50, 10), (5, 1)]);
    }

    #[test]
    fn batcher_bytes_bound() {
        let mut b = Batcher::new(1000, 100);
        let mut flushed = Vec::new();
        let mut f = |body: &[u8], rows: usize| {
            flushed.push((body.len(), rows));
            Ok(())
        };
        b.feed(&mk_chunk(7, 30), &mut f).unwrap();
        b.flush_now(&mut f).unwrap();
        // 3 строки по 30 = 90 ≤ 100, четвёртая не помещается
        assert_eq!(flushed, vec![(90, 3), (90, 3), (30, 1)]);
    }

    #[test]
    fn batcher_oversize_row_goes_alone() {
        let mut b = Batcher::new(1000, 100);
        let mut flushed = Vec::new();
        let mut f = |body: &[u8], rows: usize| {
            flushed.push((body.len(), rows));
            Ok(())
        };
        b.feed(&mk_chunk(1, 50), &mut f).unwrap();
        b.feed(&mk_chunk(1, 500), &mut f).unwrap(); // крупнее лимита
        b.feed(&mk_chunk(1, 50), &mut f).unwrap();
        b.flush_now(&mut f).unwrap();
        assert_eq!(flushed, vec![(50, 1), (500, 1), (50, 1)]);
    }

    // --- очередь ---

    #[test]
    fn queue_basics() {
        let q = SinkQueue::new(10, 1);
        assert!(q.push(mk_chunk(1, 4)));
        match q.pop(None) {
            Pop::Chunk(c) => assert_eq!(c.ends.len(), 1),
            _ => panic!("ожидался кусок"),
        }
        // пустая очередь + живой продюсер + прошедший дедлайн → Timeout
        match q.pop(Some(Instant::now())) {
            Pop::Timeout => {}
            _ => panic!("ожидался Timeout"),
        }
        q.producer_done();
        match q.pop(None) {
            Pop::Done => {}
            _ => panic!("ожидался Done"),
        }
        q.set_fatal();
        assert!(!q.push(mk_chunk(1, 4)));
    }

    // --- HTTP-клиент против мини-сервера ---

    /// Мини-сервер: принимает `conns` соединений, на каждом отвечает на
    /// запросы из своего списка `replies`; отдаёт собранные тела запросов.
    fn spawn_server(
        replies_per_conn: Vec<Vec<&'static str>>,
    ) -> (ChTarget, thread::JoinHandle<Vec<Vec<u8>>>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let h = thread::spawn(move || {
            let mut bodies = Vec::new();
            for replies in replies_per_conn {
                let (mut s, _) = listener.accept().unwrap();
                for reply in replies {
                    match read_request(&mut s) {
                        Some((_head, body)) => {
                            bodies.push(body);
                            s.write_all(reply.as_bytes()).unwrap();
                        }
                        None => break,
                    }
                }
                // соединение закрывается по выходу из scope
            }
            bodies
        });
        (
            ChTarget {
                host: format!("127.0.0.1:{port}"),
                db: "tj_bench".into(),
                table: "events".into(),
            },
            h,
        )
    }

    /// Читает запрос целиком (заголовки + тело по Content-Length).
    fn read_request(s: &mut TcpStream) -> Option<(String, Vec<u8>)> {
        let mut buf = Vec::new();
        let mut tmp = [0u8; 4096];
        loop {
            let end = buf
                .windows(4)
                .position(|w| w == b"\r\n\r\n");
            if let Some(pos) = end {
                let head = String::from_utf8_lossy(&buf[..pos]).to_string();
                let cl = head
                    .to_ascii_lowercase()
                    .lines()
                    .find_map(|l| l.strip_prefix("content-length:").map(str::trim).map(str::to_string))
                    .and_then(|v| v.parse::<usize>().ok())
                    .unwrap_or(0);
                let mut body = buf[pos + 4..].to_vec();
                while body.len() < cl {
                    match s.read(&mut tmp) {
                        Ok(0) => return None,
                        Ok(n) => body.extend_from_slice(&tmp[..n]),
                        Err(_) => return None,
                    }
                }
                return Some((head, body));
            }
            match s.read(&mut tmp) {
                Ok(0) => return None,
                Ok(n) => buf.extend_from_slice(&tmp[..n]),
                Err(_) => return None,
            }
        }
    }

    const OK_EMPTY: &str = "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n";

    #[test]
    fn http_insert_keepalive() {
        let (t, h) = spawn_server(vec![vec![OK_EMPTY, OK_EMPTY]]);
        let mut c = ChClient::new(&t);
        c.insert(b"batch-1").unwrap();
        c.insert(b"batch-2").unwrap(); // то же соединение (accept один)
        drop(c);
        let bodies = h.join().unwrap();
        assert_eq!(bodies, vec![b"batch-1".to_vec(), b"batch-2".to_vec()]);
    }

    #[test]
    fn http_error_status_reported() {
        let (t, h) = spawn_server(vec![vec![
            "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 35\r\n\r\nCode: 60. DB::Exception: no table x",
        ]]);
        let mut c = ChClient::new(&t);
        let err = c.insert(b"bad").unwrap_err();
        assert!(err.0.contains("HTTP 500"), "{}", err.0);
        assert!(err.0.contains("DB::Exception"), "{}", err.0);
        drop(c);
        h.join().unwrap();
    }

    #[test]
    fn http_chunked_error_body() {
        let (t, h) = spawn_server(vec![vec![
            "HTTP/1.1 400 Bad Request\r\nTransfer-Encoding: chunked\r\n\r\n6\r\nCode: \r\n3\r\n42.\r\n0\r\n\r\n",
        ]]);
        let mut c = ChClient::new(&t);
        let err = c.insert(b"x").unwrap_err();
        assert!(err.0.contains("Code: 42."), "{}", err.0);
        drop(c);
        h.join().unwrap();
    }

    #[test]
    fn http_connection_close_reconnects() {
        let (t, h) = spawn_server(vec![
            vec!["HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"],
            vec![OK_EMPTY],
        ]);
        let mut c = ChClient::new(&t);
        c.insert(b"a").unwrap();
        c.insert(b"b").unwrap(); // сервер закрыл → новое соединение
        drop(c);
        assert_eq!(h.join().unwrap().len(), 2);
    }

    #[test]
    fn http_stale_keepalive_retries_once() {
        // Соединение 1: один ответ, потом сервер молча закрывает.
        // Соединение 2: обслуживает повтор.
        let (t, h) = spawn_server(vec![vec![OK_EMPTY], vec![OK_EMPTY]]);
        let mut c = ChClient::new(&t);
        c.insert(b"first").unwrap();
        c.insert(b"second").unwrap(); // стухший keep-alive → ретрай на свежем
        drop(c);
        let bodies = h.join().unwrap();
        assert_eq!(bodies, vec![b"first".to_vec(), b"second".to_vec()]);
    }

    #[test]
    fn connect_refused_fails_fast_no_hang() {
        // Порт без слушателя: bind + drop освобождает порт
        let port = {
            let l = TcpListener::bind("127.0.0.1:0").unwrap();
            l.local_addr().unwrap().port()
        };
        let t = ChTarget {
            host: format!("127.0.0.1:{port}"),
            db: "tj_bench".into(),
            table: "events".into(),
        };
        let started = Instant::now();
        let mut c = ChClient::new(&t);
        let err = c.check_ready(&t).unwrap_err();
        assert!(started.elapsed() < CONNECT_TIMEOUT + Duration::from_secs(2), "завис на {:?}", started.elapsed());
        assert!(!err.0.is_empty());
    }
}
