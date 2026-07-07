//! tj-agent-rs — участник bake-off (Rust): нормализатор техжурнала 1С → NDJSON.
//!
//! Два синтаксиса запуска:
//!
//! 1. Контракт golden-раннера (совместим с cpp_parse/count_contexts.exe):
//!    `tj-agent-rs <input_dir> [workers] [output.jsonl] [--no-output]`
//!
//! 2. Контракт bake-off (docs/bakeoff-protocol.md §1.1, batch-режим):
//!    `tj-agent-rs --input <dir> --threads <N>
//!    --sink {null|file:<path>|clickhouse[:<dsn>]}
//!    [--batch-rows N] [--batch-bytes N] [--flush-ms N] [--stats-json <path>]`
//!
//! Формат вывода — docs/format-spec.md v1.0 (rev 3): NDJSON без BOM,
//! LF-терминатор каждой записи. Порядок записей внутри файла = порядок
//! событий в файле при любом числе потоков (жёстче KI-11). Файлы
//! обрабатываются в порядке убывания размера (совместимость с эталонным exe).
//!
//! Exit-коды: 0 — успех; 1 — ошибка аргументов/каталога/записи вывода;
//! 2 — часть входных файлов не удалось прочитать (KI-12).

mod chsink;
mod parser;
mod scanner;
mod walker;

use std::fs;
use std::fs::File;
use std::io::{BufWriter, Write};
use std::path::{Path, MAIN_SEPARATOR};
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering::Relaxed};
use std::sync::{Condvar, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use walker::{find_log_files, FileMeta};

/// Порог сброса готового NDJSON писателю (паритет с core chunk_bytes).
const FLUSH_BYTES: usize = 4 << 20;
/// Байтовый бюджет допуска на воркера; итог = max(64 МБ × workers, 256 МБ).
const BUDGET_PER_WORKER: u64 = 64 << 20;
const BUDGET_FLOOR: u64 = 256 << 20;

/// Счётчики статистики, разделяемые между потоками.
#[derive(Default)]
pub struct Stats {
    pub events: AtomicU64,
    pub parse_skips: AtomicU64,
    pub small_skips: AtomicU64,
    pub failed: AtomicU64,
    pub bytes: AtomicU64,
}

struct Config {
    input: String,
    workers: usize,
    output: String, // путь к NDJSON; пуст при null_sink
    null_sink: bool,
    clickhouse: Option<chsink::ChTarget>,
    batch_rows: usize,
    batch_bytes: usize,
    flush_ms: u64,
    stats_json: String,
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    std::process::exit(run(&args));
}

fn usage() {
    eprint!(
        "Использование:\n  \
         tj-agent-rs <input_dir> [workers] [output.jsonl] [--no-output]\n  \
         tj-agent-rs --input <dir> [--threads N] [--sink null|file:<path>|clickhouse[:<dsn>]]\n           \
         [--batch-rows N] [--batch-bytes N] [--flush-ms N] [--stats-json <path>]\n\
         DSN ClickHouse: http://host[:порт][/база[/таблица]], по умолчанию\n\
         http://localhost:8123/tj_bench/events (HTTP-порт, пользователь default)\n"
    );
}

fn run(args: &[String]) -> i32 {
    let cfg = match parse_args(args) {
        Some(cfg) => cfg,
        None => return 1,
    };

    match fs::metadata(&cfg.input) {
        Err(_) => {
            eprintln!("Ошибка: директория не существует: {}", cfg.input);
            return 1;
        }
        Ok(md) if !md.is_dir() => {
            eprintln!("Ошибка: указанный путь не является директорией: {}", cfg.input);
            return 1;
        }
        Ok(_) => {}
    }

    let stats = Stats::default();
    let files = find_log_files(Path::new(&cfg.input), &stats);
    if files.is_empty() {
        // Подходящих файлов нет → выходной файл НЕ создаётся, exit 0 (§6)
        println!("Не найдено .log файлов для обработки");
        write_stats_json(&cfg, &stats, 0, cfg.clickhouse.as_ref().map(|_| 0));
        return 0;
    }

    let start = Instant::now();

    if let Some(target) = &cfg.clickhouse {
        return run_clickhouse(&cfg, target, &files, &stats, start);
    }

    // Выход открываем до разбора: пустой (но существующий) файл — валидный
    // результат, если все события отфильтрованы (как у эталонного exe).
    let mut out: Option<BufWriter<File>> = None;
    if !cfg.null_sink {
        if let Some(dir) = Path::new(&cfg.output).parent() {
            if !dir.as_os_str().is_empty() && dir != Path::new(".") {
                if let Err(e) = fs::create_dir_all(dir) {
                    eprintln!(
                        "Ошибка: не удалось создать директории для файла {}: {}",
                        cfg.output, e
                    );
                    return 1;
                }
            }
        }
        match File::create(&cfg.output) {
            Ok(f) => out = Some(BufWriter::with_capacity(4 << 20, f)),
            Err(e) => {
                eprintln!("Ошибка: не удалось открыть файл для записи {}: {}", cfg.output, e);
                return 1;
            }
        }
    }

    // Параллельный разбор по файлам, запись строго в порядке files
    // (детерминизм не зависит от workers — требование v1.1 формата, §5).
    //
    // Архитектура памяти — зеркало core/src/pipeline.cpp:
    //   - воркеры берут файлы строго по возрастанию индекса; готовый NDJSON
    //     копится кусками ≤ FLUSH_BYTES в слоте файла;
    //   - ГОЛОВНОЙ файл (i == files_written — его очередь писаться) допускается
    //     безусловно и СТРИМИТСЯ: writer забирает куски по мере готовности,
    //     целиком вывод в RAM не живёт;
    //   - НЕголовные файлы допускаются по байтовому бюджету: сумма размеров
    //     допущенных-но-не-записанных ≤ max(64 МБ × workers, 256 МБ). Файл
    //     крупнее остатка бюджета ждёт, пока не станет головным (и тогда
    //     стримится без буферизации);
    //   - writer выдаёт куски файла i только после файла i-1 → порядок стабилен,
    //     вывод байт-идентичен при любом --threads.
    //
    // Дедлок невозможен: допуск головного файла НЕ зависит от бюджета, а когда
    // writer ждёт непринятый головной файл, все предыдущие уже записаны — есть
    // свободный воркер, который его возьмёт.
    let n_files = files.len();
    let budget = (BUDGET_PER_WORKER * cfg.workers as u64).max(BUDGET_FLOOR);

    #[derive(Default)]
    struct Slot {
        chunks: Vec<Vec<u8>>, // готовые куски NDJSON (целые записи с '\n')
        done: bool,
        charged: u64, // списано с бюджета при допуске (0 у головного)
    }
    struct PipeState {
        slots: Vec<Slot>,
        next_job: usize,       // следующий невыданный файл
        files_written: usize,  // писатель ждёт этот индекс (головной)
        admitted_bytes: u64,   // сумма charged допущенных неголовных файлов
    }
    let state = Mutex::new(PipeState {
        slots: (0..n_files).map(|_| Slot::default()).collect(),
        next_job: 0,
        files_written: 0,
        admitted_bytes: 0,
    });
    let cv_workers = Condvar::new(); // будит воркеров (сдвиг головы / возврат бюджета)
    let cv_writer = Condvar::new(); // будит писателя (кусок готов / файл завершён)

    let mut writer_failed = false;
    thread::scope(|s| {
        for _ in 0..cfg.workers {
            let (state, cv_workers, cv_writer) = (&state, &cv_workers, &cv_writer);
            let stats = &stats;
            let files = &files;
            s.spawn(move || {
                let mut read_buf: Vec<u8> = Vec::new(); // переиспользуется между файлами
                loop {
                    let job = {
                        let mut st = state.lock().unwrap();
                        loop {
                            if st.next_job >= n_files {
                                break None;
                            }
                            let j = st.next_job;
                            let is_head = j == st.files_written;
                            if is_head || st.admitted_bytes + files[j].size <= budget {
                                st.next_job = j + 1;
                                if !is_head {
                                    st.admitted_bytes += files[j].size;
                                    st.slots[j].charged = files[j].size;
                                }
                                break Some(j);
                            }
                            st = cv_workers.wait(st).unwrap();
                        }
                    };
                    let Some(i) = job else { break };
                    // Допуск изменил next_job/бюджет — соседи переоценивают предикат
                    cv_workers.notify_all();

                    process_file(&files[i], stats, &mut read_buf, |chunk| {
                        state.lock().unwrap().slots[i].chunks.push(chunk);
                        cv_writer.notify_all();
                    });

                    state.lock().unwrap().slots[i].done = true;
                    cv_writer.notify_all();
                }
            });
        }

        // Writer — текущий поток: строго в порядке files, кусками
        for i in 0..n_files {
            loop {
                let (got, done) = {
                    let mut st = state.lock().unwrap();
                    loop {
                        if st.slots[i].done || !st.slots[i].chunks.is_empty() {
                            break (std::mem::take(&mut st.slots[i].chunks), st.slots[i].done);
                        }
                        st = cv_writer.wait(st).unwrap();
                    }
                };
                if let Some(w) = out.as_mut() {
                    if !writer_failed {
                        for chunk in &got {
                            if let Err(e) = w.write_all(chunk) {
                                eprintln!(
                                    "Ошибка записи в файл (диск полон?): {}: {}",
                                    cfg.output, e
                                );
                                writer_failed = true;
                                break;
                            }
                        }
                    }
                }
                drop(got); // куски освобождаются ДО возврата бюджета
                if done {
                    break;
                }
            }
            {
                let mut st = state.lock().unwrap();
                let charged = std::mem::take(&mut st.slots[i].charged);
                st.admitted_bytes -= charged;
                st.files_written = i + 1; // голова сдвинулась
            }
            cv_workers.notify_all();
        }
    });

    if let Some(mut w) = out {
        if let Err(e) = w.flush() {
            if !writer_failed {
                eprintln!("Ошибка записи в файл (диск полон?): {}: {}", cfg.output, e);
                writer_failed = true;
            }
        }
        // Закрытие файла (drop) — ошибки закрытия после успешного flush
        // на Windows не несут данных, паритет с Go-агентом сохраняется
    }

    let elapsed = start.elapsed().as_secs_f64();
    report_stats(&cfg, &stats, files.len(), elapsed);
    write_stats_json(&cfg, &stats, files.len(), None);

    if writer_failed {
        eprintln!("ОШИБКА: запись результатов не удалась, вывод неполный");
        return 1;
    }
    if stats.failed.load(Relaxed) > 0 {
        eprintln!("ВНИМАНИЕ: часть файлов не обработана (см. счётчик ошибок)");
        return 2;
    }
    0
}

fn default_workers() -> usize {
    thread::available_parallelism()
        .map(std::num::NonZeroUsize::get)
        .unwrap_or(1)
        .clamp(1, 1024)
}

fn parse_args(args: &[String]) -> Option<Config> {
    let mut cfg = Config {
        input: String::new(),
        workers: default_workers(),
        output: String::new(),
        null_sink: false,
        clickhouse: None,
        batch_rows: chsink::DEFAULT_BATCH_ROWS,
        batch_bytes: chsink::DEFAULT_BATCH_BYTES,
        flush_ms: chsink::DEFAULT_FLUSH_MS,
        stats_json: String::new(),
    };
    if args.is_empty() {
        usage();
        return None;
    }
    if args[0].starts_with("--") {
        return parse_flag_args(args, cfg);
    }

    // Позиционный контракт golden-раннера
    cfg.input = args[0].clone();
    if args.len() >= 2 {
        match args[1].parse::<usize>() {
            Ok(w) if (1..=1024).contains(&w) => cfg.workers = w,
            _ => {
                eprintln!("Ошибка: workers должен быть целым числом от 1 до 1024");
                return None;
            }
        }
    }
    if args.len() >= 3 {
        cfg.output = args[2].clone();
    } else {
        match std::env::current_dir() {
            Ok(cwd) => cfg.output = cwd.join("result.jsonl").to_string_lossy().into_owned(),
            Err(e) => {
                eprintln!("Ошибка определения текущей директории: {e}");
                return None;
            }
        }
    }
    if args.len() >= 4 {
        if let "--no-output" | "--no-write" | "--dry-run" = args[3].as_str() {
            cfg.null_sink = true;
            cfg.output.clear();
        }
    }
    Some(cfg)
}

fn parse_flag_args(args: &[String], mut cfg: Config) -> Option<Config> {
    let mut sink = String::new();
    // Значение флага args[i]; None + сообщение, если значения нет
    let next = |i: usize| -> Option<&String> {
        let v = args.get(i + 1);
        if v.is_none() {
            eprintln!("Ошибка: у флага {} нет значения", args[i]);
        }
        v
    };
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--input" => {
                cfg.input = next(i)?.clone();
                i += 1;
            }
            "--threads" => {
                match next(i)?.parse::<usize>() {
                    Ok(w) if (1..=1024).contains(&w) => cfg.workers = w,
                    _ => {
                        eprintln!("Ошибка: --threads должен быть целым числом от 1 до 1024");
                        return None;
                    }
                }
                i += 1;
            }
            "--sink" => {
                sink = next(i)?.clone();
                i += 1;
            }
            "--stats-json" => {
                cfg.stats_json = next(i)?.clone();
                i += 1;
            }
            // Политика батчей ClickHouse-синка (bakeoff-protocol §1.2);
            // для file/null принимаются и игнорируются
            "--batch-rows" => {
                match next(i)?.parse::<usize>() {
                    Ok(v) if (1..=100_000_000).contains(&v) => cfg.batch_rows = v,
                    _ => {
                        eprintln!("Ошибка: --batch-rows должен быть целым числом от 1 до 100000000");
                        return None;
                    }
                }
                i += 1;
            }
            "--batch-bytes" => {
                match next(i)?.parse::<usize>() {
                    Ok(v) if (1..=(16 << 30)).contains(&v) => cfg.batch_bytes = v,
                    _ => {
                        eprintln!("Ошибка: --batch-bytes должен быть целым числом от 1 до 17179869184");
                        return None;
                    }
                }
                i += 1;
            }
            "--flush-ms" => {
                match next(i)?.parse::<u64>() {
                    Ok(v) if (1..=3_600_000).contains(&v) => cfg.flush_ms = v,
                    _ => {
                        eprintln!("Ошибка: --flush-ms должен быть целым числом от 1 до 3600000");
                        return None;
                    }
                }
                i += 1;
            }
            "--follow" => {
                eprintln!("Ошибка: --follow пока не реализован (фаза 3)");
                return None;
            }
            other => {
                eprintln!("Ошибка: неизвестный флаг {other}");
                usage();
                return None;
            }
        }
        i += 1;
    }
    if cfg.input.is_empty() {
        eprintln!("Ошибка: обязателен --input <dir>");
        return None;
    }
    if sink == "null" {
        cfg.null_sink = true;
    } else if let Some(path) = sink.strip_prefix("file:") {
        if path.is_empty() {
            eprintln!("Ошибка: пустой путь в --sink file:<path>");
            return None;
        }
        cfg.output = path.to_string();
    } else if sink == "clickhouse" || sink.starts_with("clickhouse:") {
        match chsink::parse_sink_dsn(&sink) {
            Ok(t) => cfg.clickhouse = Some(t),
            Err(e) => {
                eprintln!("Ошибка: {e}");
                return None;
            }
        }
    } else if sink.is_empty() {
        eprintln!("Ошибка: обязателен --sink {{null|file:<path>|clickhouse[:<dsn>]}}");
        return None;
    } else {
        eprintln!("Ошибка: неизвестный sink \"{sink}\"");
        return None;
    }
    Some(cfg)
}

/// Пайплайн `--sink clickhouse`: воркеры разбирают файлы в RowBinary-куски
/// (тот же сканер и автомат свойств, что у file-синка, но эмиттер —
/// `chsink::RowEmitter`, без сборки/повторного разбора JSON), синк в главном
/// потоке собирает батчи 50 000 строк / 64 МБ / 1000 мс и вставляет по HTTP.
///
/// Порядок строк: батчи уходят по мере готовности («flow as parsed»), без
/// глобального межфайлового упорядочивания (СУБД оно не нужно — сортировка
/// задаётся ORDER BY таблицы); события ОДНОГО файла попадают в поток вставки
/// в исходном порядке файла (файл разбирается одним воркером, куски
/// выкладываются последовательно, батчер границы кусков сохраняет).
fn run_clickhouse(
    cfg: &Config,
    target: &chsink::ChTarget,
    files: &[FileMeta],
    stats: &Stats,
    start: Instant,
) -> i32 {
    let mut client = chsink::ChClient::new(target);
    // Fail-fast до разбора: неверный порт/БД/таблица → внятная ошибка, exit 1
    if let Err(e) = client.check_ready(target) {
        eprintln!("Ошибка ClickHouse ({}): {e}", target.host);
        return 1;
    }

    // Обратное давление: очередь ограничена двумя батчами по байтам
    let queue_cap = cfg.batch_bytes.saturating_mul(2).max(16 << 20);
    let queue = chsink::SinkQueue::new(queue_cap, cfg.workers);
    let next_job = AtomicUsize::new(0);
    let mut inserted_rows = 0u64;
    let mut batches = 0u64;
    let mut sink_err: Option<chsink::ChError> = None;

    thread::scope(|s| {
        for _ in 0..cfg.workers {
            let (queue, next_job) = (&queue, &next_job);
            s.spawn(move || {
                let mut read_buf: Vec<u8> = Vec::new();
                let mut em = chsink::RowEmitter::new();
                loop {
                    let i = next_job.fetch_add(1, Relaxed);
                    if i >= files.len() {
                        break;
                    }
                    if !process_file_ch(&files[i], stats, &mut read_buf, &mut em, queue) {
                        break; // вставка фатально упала — разбор бессмыслен
                    }
                }
                queue.producer_done();
            });
        }

        // Синк — текущий поток
        let res = chsink::run_sink(
            &mut client,
            &queue,
            cfg.batch_rows,
            cfg.batch_bytes,
            Duration::from_millis(cfg.flush_ms),
            &mut inserted_rows,
            &mut batches,
        );
        if let Err(e) = res {
            queue.set_fatal(); // разбудить воркеров, ждущих место в очереди
            sink_err = Some(e);
        }
    });

    let elapsed = start.elapsed().as_secs_f64();
    report_stats(cfg, stats, files.len(), elapsed);
    let rows_per_sec = if elapsed > 0.0 {
        inserted_rows as f64 / elapsed
    } else {
        0.0
    };
    println!(
        "ClickHouse: вставлено {inserted_rows} строк в {}.{} ({batches} батчей, {rows_per_sec:.0} строк/с)",
        target.db, target.table
    );
    write_stats_json(cfg, stats, files.len(), Some(inserted_rows));

    if let Some(e) = sink_err {
        eprintln!("ОШИБКА ClickHouse ({}): {e}", target.host);
        eprintln!("ОШИБКА: вставка результатов не удалась, данные в БД неполные");
        return 1;
    }
    if stats.failed.load(Relaxed) > 0 {
        eprintln!("ВНИМАНИЕ: часть файлов не обработана (см. счётчик ошибок)");
        return 2;
    }
    0
}

/// Разбор одного файла в RowBinary-куски для ClickHouse-синка (аналог
/// `process_file`, но без JSON). Возвращает `false` при фатальной ошибке
/// вставки — воркеру пора останавливаться.
fn process_file_ch(
    fm: &FileMeta,
    s: &Stats,
    read_buf: &mut Vec<u8>,
    em: &mut chsink::RowEmitter,
    queue: &chsink::SinkQueue,
) -> bool {
    let mut f = match File::open(&fm.path) {
        Ok(f) => f,
        Err(e) => {
            s.failed.fetch_add(1, Relaxed);
            eprintln!("Ошибка открытия файла: {}: {}", fm.path.display(), e);
            return true;
        }
    };

    let filename = fm
        .path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let file_path = rel_file_path(&fm.path);
    em.begin_file(
        chsink::hour_base_micros(&fm.date_prefix),
        filename.as_bytes(),
        file_path.as_bytes(),
    );

    let mut events = 0u64;
    let mut skips = 0u64;
    let mut alive = true;
    let res = scanner::scan_events(&mut f, read_buf, |ev| {
        if !alive {
            return; // фатал: дочитываем без разбора (scan_events не прерывается)
        }
        if parser::parse_event(ev, em) {
            events += 1;
        } else {
            skips += 1;
        }
        if em.chunk_bytes() >= chsink::CHUNK_TARGET_BYTES {
            alive = queue.push(em.take_chunk());
        }
    });
    match res {
        Ok(total) => {
            s.bytes.fetch_add(total, Relaxed);
        }
        Err(e) => {
            // Уже выданные события остаются (KI-12, exit 2 — паритет с file-синком)
            s.failed.fetch_add(1, Relaxed);
            eprintln!("Ошибка чтения файла: {}: {}", fm.path.display(), e);
        }
    }
    if alive && em.chunk_bytes() > 0 {
        alive = queue.push(em.take_chunk());
    }
    s.events.fetch_add(events, Relaxed);
    s.parse_skips.fetch_add(skips, Relaxed);
    alive
}

/// Разбирает файл потоково: чтение кусками через `scanner::scan_events`
/// (файл целиком в RAM не живёт), готовый NDJSON сбрасывается в `flush`
/// порциями ≈ FLUSH_BYTES. `read_buf` — переиспользуемый буфер чтения воркера.
fn process_file(fm: &FileMeta, s: &Stats, read_buf: &mut Vec<u8>, mut flush: impl FnMut(Vec<u8>)) {
    let mut f = match File::open(&fm.path) {
        Ok(f) => f,
        Err(e) => {
            s.failed.fetch_add(1, Relaxed);
            eprintln!("Ошибка открытия файла: {}: {}", fm.path.display(), e);
            return;
        }
    };

    let filename = fm
        .path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let file_path = rel_file_path(&fm.path);
    let mut filename_esc = Vec::new();
    parser::append_escaped(&mut filename_esc, filename.as_bytes());
    let mut file_path_esc = Vec::new();
    parser::append_escaped(&mut file_path_esc, file_path.as_bytes());

    let mut out = Vec::with_capacity(FLUSH_BYTES + (64 << 10));
    let mut events = 0u64;
    let mut skips = 0u64;
    let res = scanner::scan_events(&mut f, read_buf, |ev| {
        if parser::append_event(&mut out, ev, &fm.date_prefix, &filename_esc, &file_path_esc) {
            events += 1;
        } else {
            skips += 1;
        }
        if out.len() >= FLUSH_BYTES {
            let full = std::mem::replace(&mut out, Vec::with_capacity(FLUSH_BYTES + (64 << 10)));
            flush(full);
        }
    });
    match res {
        Ok(total) => {
            s.bytes.fetch_add(total, Relaxed);
        }
        Err(e) => {
            // Ошибка чтения посреди файла: уже выданные события остаются
            // (паритет с core), файл считается ошибочным (KI-12, exit 2)
            s.failed.fetch_add(1, Relaxed);
            eprintln!("Ошибка чтения файла: {}: {}", fm.path.display(), e);
        }
    }
    if !out.is_empty() {
        flush(out);
    }
    s.events.fetch_add(events, Relaxed);
    s.parse_skips.fetch_add(skips, Relaxed);
}

/// «Ровно два уровня предков» фактического пути (format-spec §3):
/// `<коллекция>\<process_pid>\<файл>.log`. Отсутствующий предок даёт пустую
/// часть — композиция повторяет семантику fs::path::operator/ эталона.
fn rel_file_path(path: &Path) -> String {
    let parent = path.parent();
    let grandparent = parent.and_then(Path::parent);
    let base = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    cpp_join(
        &cpp_join(&path_filename(grandparent), &path_filename(parent)),
        &base,
    )
}

/// Аналог fs::path::filename(): для корня диска / "." / "" возвращает "",
/// для пути, оканчивающегося на "..", возвращает ".." (fs::path("..").filename()
/// == ".."; filepath.Base в Go тоже даёт ".."). Path::file_name для ".."
/// возвращает None — прямое его использование теряло компонент ".." в
/// file_path при относительном входе вида `..\logs` (расходилось и с C++, и с Go).
fn path_filename(p: Option<&Path>) -> String {
    use std::path::Component;
    match p.and_then(|p| p.components().next_back()) {
        Some(Component::Normal(n)) => n.to_string_lossy().into_owned(),
        Some(Component::ParentDir) => "..".to_string(),
        _ => String::new(),
    }
}

/// Семантика fs::path::operator/ для относительных компонентов.
fn cpp_join(p: &str, x: &str) -> String {
    if x.is_empty() {
        if p.is_empty() {
            return String::new();
        }
        if !p.ends_with(MAIN_SEPARATOR) {
            return format!("{p}{MAIN_SEPARATOR}");
        }
        return p.to_string();
    }
    if p.is_empty() {
        return x.to_string();
    }
    if p.ends_with(MAIN_SEPARATOR) {
        return format!("{p}{x}");
    }
    format!("{p}{MAIN_SEPARATOR}{x}")
}

fn report_stats(cfg: &Config, s: &Stats, n_files: usize, sec: f64) {
    let mb = s.bytes.load(Relaxed) as f64 / (1024.0 * 1024.0);
    let speed = if sec > 0.0 { mb / sec } else { 0.0 };
    // На успешном пути сводка в stdout, как у эталонного exe: golden-раннер
    // (PowerShell 5.1, $ErrorActionPreference='Stop') трактует stderr
    // native-команды под редиректом как ошибку. stderr — только для ошибок.
    println!(
        "Файлов: {} (ошибок открытия: {}, пропущено <{} байт: {}) | Событий: {} | parse_skips: {} | {:.2} МБ за {:.3} с ({:.1} МБ/с, workers={})",
        n_files,
        s.failed.load(Relaxed),
        parser::MIN_FILE_SIZE,
        s.small_skips.load(Relaxed),
        s.events.load(Relaxed),
        s.parse_skips.load(Relaxed),
        mb,
        sec,
        speed,
        cfg.workers
    );
}

/// Контракт bakeoff-protocol §3: {"events":N,"files":M,"skips":K,"bytes":B}
/// плюс расшифровка skips отдельными полями (приёмник обязан игнорировать
/// незнакомые). Ключи в алфавитном порядке — байт-в-байт с Go-агентом
/// (json.Marshal сортирует ключи map). `inserted` задан только у
/// clickhouse-синка — добавляет поле `inserted_rows` (подтверждённые вставки).
fn write_stats_json(cfg: &Config, s: &Stats, n_files: usize, inserted: Option<u64>) {
    if cfg.stats_json.is_empty() {
        return;
    }
    let parse_skips = s.parse_skips.load(Relaxed);
    let small_skips = s.small_skips.load(Relaxed);
    let inserted_part = match inserted {
        Some(n) => format!("\"inserted_rows\":{n},"),
        None => String::new(),
    };
    let json = format!(
        "{{\"bytes\":{},\"events\":{},\"failed_files\":{},\"files\":{},{}\"parse_skips\":{},\"skips\":{},\"small_file_skips\":{}}}\n",
        s.bytes.load(Relaxed),
        s.events.load(Relaxed),
        s.failed.load(Relaxed),
        n_files,
        inserted_part,
        parse_skips,
        parse_skips + small_skips,
        small_skips
    );
    if let Err(e) = fs::write(&cfg.stats_json, json) {
        eprintln!("Ошибка записи --stats-json {}: {}", cfg.stats_json, e);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rel_path_two_ancestors() {
        let s = MAIN_SEPARATOR;
        assert_eq!(
            rel_file_path(Path::new(&format!("E:{s}logs{s}Mem{s}rphost_1{s}25113021.log"))),
            format!("Mem{s}rphost_1{s}25113021.log")
        );
    }

    #[test]
    fn rel_path_dotdot_ancestor_preserved() {
        // Вход `..` (cwd внутри коллекции): C++ (fs::path::filename) и Go
        // (filepath.Base) оба дают компонент ".." — Rust обязан совпасть
        let s = MAIN_SEPARATOR;
        assert_eq!(
            rel_file_path(Path::new(&format!("..{s}inner{s}25113021.log"))),
            format!("..{s}inner{s}25113021.log")
        );
    }

    #[test]
    fn rel_path_missing_ancestors() {
        let s = MAIN_SEPARATOR;
        // Один предок: пустой grandparent не даёт ведущего разделителя
        assert_eq!(
            rel_file_path(Path::new(&format!("inner{s}25113021.log"))),
            format!("inner{s}25113021.log")
        );
        // Без предков (файл в cwd)
        assert_eq!(rel_file_path(Path::new("25113021.log")), "25113021.log");
    }
}
