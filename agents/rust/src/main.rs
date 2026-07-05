//! tj-agent-rs — участник bake-off (Rust): нормализатор техжурнала 1С → NDJSON.
//!
//! Два синтаксиса запуска:
//!
//! 1. Контракт golden-раннера (совместим с cpp_parse/count_contexts.exe):
//!    `tj-agent-rs <input_dir> [workers] [output.jsonl] [--no-output]`
//!
//! 2. Контракт bake-off (docs/bakeoff-protocol.md §1.1, batch-режим):
//!    `tj-agent-rs --input <dir> --threads <N> --sink {null|file:<path>}
//!    [--stats-json <path>]`
//!
//! Формат вывода — docs/format-spec.md v1.0 (rev 3): NDJSON без BOM,
//! LF-терминатор каждой записи. Порядок записей внутри файла = порядок
//! событий в файле при любом числе потоков (жёстче KI-11). Файлы
//! обрабатываются в порядке убывания размера (совместимость с эталонным exe).
//!
//! Exit-коды: 0 — успех; 1 — ошибка аргументов/каталога/записи вывода;
//! 2 — часть входных файлов не удалось прочитать (KI-12).

mod parser;
mod walker;

use std::fs;
use std::fs::File;
use std::io::{BufWriter, Write};
use std::path::{Path, MAIN_SEPARATOR};
use std::sync::atomic::{AtomicU64, Ordering::Relaxed};
use std::sync::{mpsc, Arc, Mutex};
use std::thread;
use std::time::Instant;

use walker::{find_log_files, FileMeta};

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
         tj-agent-rs --input <dir> [--threads N] [--sink null|file:<path>] [--stats-json <path>]\n"
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
        write_stats_json(&cfg, &stats, 0);
        return 0;
    }

    let start = Instant::now();

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
    // Окно допуска (bounded admission window, паттерн Go-агента) ограничивает
    // число файлов «разобран, но ещё не записан»: без него, пока writer ждёт
    // медленный головной файл (сортировка по убыванию размера ставит самый
    // большой первым), воркеры разобрали бы весь остальной корпус и их
    // NDJSON-буферы копились бы в памяти без ограничения (на корпусе 175 ГБ —
    // гарантированный OOM). Допуск строго в порядке files, поэтому дедлок
    // невозможен: writer всегда ждёт файл, который уже допущен и обрабатывается.
    let window = cfg.workers * 2; // ≥ workers, иначе простой; ×2 — запас на перекрытие записи
    let (admit_tx, admit_rx) = mpsc::sync_channel::<()>(window);
    let (job_tx, job_rx) = mpsc::channel::<(usize, mpsc::SyncSender<Vec<u8>>)>();
    let mut res_txs: Vec<mpsc::SyncSender<Vec<u8>>> = Vec::with_capacity(files.len());
    let mut res_rxs: Vec<mpsc::Receiver<Vec<u8>>> = Vec::with_capacity(files.len());
    for _ in &files {
        let (tx, rx) = mpsc::sync_channel::<Vec<u8>>(1);
        res_txs.push(tx);
        res_rxs.push(rx);
    }
    // mpsc — single-consumer; общий Receiver превращаем в MPMC через Mutex
    let job_rx = Arc::new(Mutex::new(job_rx));

    let mut writer_failed = false;
    thread::scope(|s| {
        // Диспетчер: место в окне занимает перед выдачей задания,
        // освобождает его writer после записи буфера
        s.spawn(move || {
            for (i, tx) in res_txs.into_iter().enumerate() {
                if admit_tx.send(()).is_err() || job_tx.send((i, tx)).is_err() {
                    break;
                }
            }
            // job_tx дропается здесь → воркеры получают Err и завершаются
        });

        for _ in 0..cfg.workers {
            let job_rx = Arc::clone(&job_rx);
            let stats = &stats;
            let files = &files;
            s.spawn(move || loop {
                // Блокирующий recv под мьютексом безопасен: writer и диспетчер
                // этот мьютекс не берут, а заданий без допуска всё равно нет
                let job = job_rx.lock().unwrap().recv();
                match job {
                    Ok((i, tx)) => {
                        let buf = process_file(&files[i], stats);
                        // Err = writer уже упал и дропнул приёмник — буфер не нужен
                        let _ = tx.send(buf);
                    }
                    Err(_) => break,
                }
            });
        }

        // Writer — текущий поток: строго в порядке files
        for rx in &res_rxs {
            let buf = rx.recv().unwrap_or_default();
            if let Some(w) = out.as_mut() {
                if !writer_failed {
                    if let Err(e) = w.write_all(&buf) {
                        eprintln!("Ошибка записи в файл (диск полон?): {}: {}", cfg.output, e);
                        writer_failed = true;
                    }
                }
            }
            drop(buf); // буфер освобождается ДО освобождения места в окне
            let _ = admit_rx.recv();
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
    write_stats_json(&cfg, &stats, files.len());

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
            "--batch-rows" | "--batch-bytes" | "--flush-ms" => {
                // Параметры батчирования не влияют на file/null-sink — принимаем и игнорируем
                next(i)?;
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
    } else if sink.starts_with("clickhouse:") {
        eprintln!("Ошибка: --sink clickhouse пока не реализован (фаза 2, e2e-серия)");
        return None;
    } else if sink.is_empty() {
        eprintln!("Ошибка: обязателен --sink {{null|file:<path>}}");
        return None;
    } else {
        eprintln!("Ошибка: неизвестный sink \"{sink}\"");
        return None;
    }
    Some(cfg)
}

/// Читает файл целиком и возвращает готовый NDJSON-буфер его событий.
/// Чтение целиком — осознанный паритет с Go-агентом; общий объём памяти
/// ограничен окном допуска (window × максимальный файл).
fn process_file(fm: &FileMeta, s: &Stats) -> Vec<u8> {
    let data = match fs::read(&fm.path) {
        Ok(data) => data,
        Err(e) => {
            s.failed.fetch_add(1, Relaxed);
            eprintln!("Ошибка открытия файла: {}: {}", fm.path.display(), e);
            return Vec::new();
        }
    };
    s.bytes.fetch_add(data.len() as u64, Relaxed);

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

    let mut buf = Vec::with_capacity(data.len() + data.len() / 4 + 4096);
    let mut events = 0u64;
    let mut skips = 0u64;
    parser::split_events(&data, |ev| {
        if parser::append_event(&mut buf, ev, &fm.date_prefix, &filename_esc, &file_path_esc) {
            events += 1;
        } else {
            skips += 1;
        }
    });
    s.events.fetch_add(events, Relaxed);
    s.parse_skips.fetch_add(skips, Relaxed);
    buf
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
/// (json.Marshal сортирует ключи map).
fn write_stats_json(cfg: &Config, s: &Stats, n_files: usize) {
    if cfg.stats_json.is_empty() {
        return;
    }
    let parse_skips = s.parse_skips.load(Relaxed);
    let small_skips = s.small_skips.load(Relaxed);
    let json = format!(
        "{{\"bytes\":{},\"events\":{},\"failed_files\":{},\"files\":{},\"parse_skips\":{},\"skips\":{},\"small_file_skips\":{}}}\n",
        s.bytes.load(Relaxed),
        s.events.load(Relaxed),
        s.failed.load(Relaxed),
        n_files,
        parse_skips,
        parse_skips + small_skips,
        small_skips
    );
    if let Err(e) = fs::write(&cfg.stats_json, json) {
        eprintln!("Ошибка записи --stats-json {}: {}", cfg.stats_json, e);
    }
}
