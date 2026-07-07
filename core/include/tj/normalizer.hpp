// tj/normalizer.hpp — публичный API ядра нормализатора техжурнала 1С.
//
// Извлечено из cpp_parse/count_contexts.cpp (см. docs/normalizer-source-map.md,
// раздел «Что нужно для превращения в ядро агента», пункты (c)/(d)).
// Формат вывода — docs/format-spec.md v1.0 rev 3, байт-в-байт с эталоном.
//
// Детерминизм (требование v1.1, жёстче KI-11): порядок записей внутри файла =
// порядок событий в файле; файлы — по убыванию размера; вывод байт-идентичен
// при любом числе потоков. Память ограничена БАЙТОВЫМ бюджетом допуска
// (модель agents/go и agents/rust): неголовные файлы допускаются к разбору,
// только если их размер помещается в остаток бюджета max(64 МБ × workers,
// 256 МБ); головной файл допускается безусловно и стримится чанками.
#pragma once

#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <functional>
#include <string>
#include <vector>

namespace tj {

// Конфигурация конвейера.
struct Config {
    // Число рабочих потоков-разборщиков. 0 → std::thread::hardware_concurrency().
    unsigned workers = 0;
    // Байтовый бюджет допуска: суммарный размер НЕголовных файлов в состоянии
    // «разбирается/разобран, но не записан». 0 → max(64 МБ × workers, 256 МБ).
    // Головной файл (его очередь писаться) допускается вне бюджета и стримится
    // с ограниченной очередью чанков; файл крупнее остатка бюджета ждёт, пока
    // сам станет головным. Это и есть ограничитель памяти конвейера.
    std::uint64_t admission_budget_bytes = 0;
    // Порог (байт), при котором разобранный NDJSON-буфер файла передаётся
    // писателю, не дожидаясь конца файла — головной (самый большой) файл
    // стримится, а не копится целиком в RAM.
    std::size_t chunk_bytes = 4u << 20;
    // Размер окна memory-mapping входного файла. Файлы НЕ отображаются
    // целиком (файл 5.76 ГБ давал бы 5.76 ГБ WorkingSet): окно скользит по
    // файлу, всегда покрывая текущее событие. 0 → 64 МБ.
    std::uint64_t map_bytes = 64u << 20;
    // Колбэк ошибок (открытие файла, обход каталога). Вызывается из рабочих
    // потоков и потока вызывающего; может быть пустым (ошибки только в счётчиках).
    std::function<void(const std::string&)> on_error;
};

// Итог обработки одного файла (колбэк завершения файла).
struct FileCompletion {
    std::string path;          // полный путь, UTF-8
    std::uint64_t events = 0;      // выданных записей
    std::uint64_t parse_skips = 0; // отброшенных кандидатов (format-spec §6)
    std::uint64_t bytes = 0;       // размер файла
    bool ok = true;                // false: не удалось открыть/замапить (KI-12)
};

// Суммарная статистика прогона (контракт --stats-json, bakeoff-protocol §3).
struct RunStats {
    std::uint64_t files = 0;            // файлов-кандидатов (≥100 байт, *.log)
    std::uint64_t events = 0;
    std::uint64_t parse_skips = 0;
    std::uint64_t small_file_skips = 0; // файлов < 100 байт
    std::uint64_t failed_files = 0;     // не открылись / ошибки обхода
    std::uint64_t bytes = 0;            // прочитанных байт
};

class NormalizerPipeline {
public:
    // Приёмник записей: одна NDJSON-строка БЕЗ завершающего '\n'.
    // Данные валидны только на время вызова. Вызывается на потоке,
    // вызвавшем run(), строго в детерминированном порядке.
    using RecordFn = std::function<void(const char* data, std::size_t len)>;
    // Колбэк завершения файла (телеметрия). Тоже на потоке run(), в порядке файлов.
    using FileFn = std::function<void(const FileCompletion&)>;
    // Приёмник RowBinary-чанков (run_rowbinary): data/len — целое число строк
    // RowBinary (ClickHouse), rows — сколько строк в чанке. Данные валидны
    // только на время вызова; вызывается на потоке run_rowbinary(), строго
    // в детерминированном порядке.
    using ChunkFn = std::function<void(const char* data, std::size_t len, std::uint64_t rows)>;

    explicit NormalizerPipeline(Config cfg = {});

    // Рекурсивный поиск *.log размером ≥ 100 байт (format-spec §6).
    // Возвращает число добавленных файлов; можно вызывать несколько раз.
    std::size_t add_dir(const std::filesystem::path& dir);

    std::size_t file_count() const { return files_.size(); }

    // Разбор всех добавленных файлов. on_record может быть пустым (null-sink).
    // Исключение из on_record/on_file останавливает выдачу (конвейер корректно
    // дренируется) и пробрасывается вызывающему после остановки потоков.
    RunStats run(const RecordFn& on_record, const FileFn& on_file = {});

    // То же, но события кодируются в ClickHouse RowBinary (см. parser.hpp,
    // append_event_rowbinary: timestamp DateTime64(6) µs, duration UInt64,
    // event/level/filename/file_path String, props Map(String,String)) и
    // выдаются чанками с числом строк. Семантика ошибок — как у run().
    RunStats run_rowbinary(const ChunkFn& on_chunk, const FileFn& on_file = {});

private:
    RunStats run_impl(bool rowbinary, const RecordFn& on_record,
                      const ChunkFn& on_chunk, const FileFn& on_file);

    struct FileTask {
        std::filesystem::path path;
        std::uint64_t size = 0;
        std::string date_prefix; // "20YY-MM-DDTHH:" или ""
    };

    Config cfg_;
    std::vector<FileTask> files_;
    std::uint64_t small_file_skips_ = 0;
    std::uint64_t failed_walk_ = 0;
};

} // namespace tj
