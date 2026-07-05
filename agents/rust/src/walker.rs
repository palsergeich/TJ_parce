//! Рекурсивный поиск `*.log` файлов размером ≥ MIN_FILE_SIZE.
//!
//! Порядок обхода воспроизводит `filepath.WalkDir` Go-агента: внутри каталога
//! записи (файлы и подкаталоги вперемешку) сортируются лексикографически по
//! имени, обход в глубину. Итоговый список сортируется по размеру по убыванию
//! стабильной сортировкой — при равных размерах сохраняется порядок обхода
//! (format-spec §5: межфайловый порядок при равных размерах не специфицирован,
//! но golden-кейсы не содержат файлов равного размера).

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::atomic::Ordering::Relaxed;

use crate::parser::{date_from_filename, MIN_FILE_SIZE};
use crate::Stats;

pub struct FileMeta {
    pub path: PathBuf,
    pub size: u64,
    /// "20YY-MM-DDTHH:" из имени файла или "" (аномалия — см. format-spec §3)
    pub date_prefix: String,
}

pub fn find_log_files(root: &Path, s: &Stats) -> Vec<FileMeta> {
    let mut files = Vec::new();
    walk(root, s, &mut files);
    // sort_by_key в Rust стабильна — паритет с sort.SliceStable Go-агента
    files.sort_by_key(|f| std::cmp::Reverse(f.size));
    files
}

fn walk(dir: &Path, s: &Stats, out: &mut Vec<FileMeta>) {
    let rd = match fs::read_dir(dir) {
        Ok(rd) => rd,
        Err(e) => {
            eprintln!("Ошибка обхода директорий: {}: {}", dir.display(), e);
            s.failed.fetch_add(1, Relaxed);
            return;
        }
    };
    let mut entries: Vec<fs::DirEntry> = Vec::new();
    for ent in rd {
        match ent {
            Ok(e) => entries.push(e),
            Err(e) => {
                eprintln!("Ошибка обхода директорий: {}: {}", dir.display(), e);
                s.failed.fetch_add(1, Relaxed);
            }
        }
    }
    entries.sort_by_key(fs::DirEntry::file_name);

    for e in entries {
        let path = e.path();
        let ft = match e.file_type() {
            Ok(ft) => ft,
            Err(err) => {
                eprintln!("Ошибка чтения атрибутов {}: {}", path.display(), err);
                s.failed.fetch_add(1, Relaxed);
                continue;
            }
        };
        if ft.is_dir() {
            walk(&path, s, out);
            continue;
        }
        if !ft.is_file() {
            continue; // симлинки и спецфайлы пропускаются (паритет с d.Type().IsRegular())
        }
        let name_os = e.file_name();
        let name = name_os.to_string_lossy();
        // Суффикс .log; имя вида ".log" (точка в позиции 0) — не лог-файл
        if !name.ends_with(".log") || name.rfind('.') == Some(0) {
            continue;
        }
        let md = match e.metadata() {
            Ok(md) => md,
            Err(err) => {
                eprintln!("Ошибка чтения атрибутов {}: {}", path.display(), err);
                s.failed.fetch_add(1, Relaxed);
                continue;
            }
        };
        if md.len() < MIN_FILE_SIZE {
            s.small_skips.fetch_add(1, Relaxed);
            continue;
        }
        out.push(FileMeta {
            path,
            size: md.len(),
            date_prefix: date_from_filename(&name),
        });
    }
}
