/* tj_ffi.h — C ABI ядра нормализатора техжурнала 1С (tj_core).
 *
 * Тонкая обёртка над tj::NormalizerPipeline для встраивания в Go (cgo),
 * Rust (FFI) и другие хосты. Все строки — UTF-8. Все исключения C++
 * перехватываются на границе ABI: библиотека НИКОГДА не вызывает abort
 * и не выпускает исключения в хост; текст последней ошибки — tj_last_error().
 *
 * Потоковая модель: tj_run() блокирует вызывающий поток; sink-колбэк и
 * колбэк завершения файла вызываются ТОЛЬКО на потоке, вызвавшем tj_run(),
 * строго в детерминированном порядке (файлы по убыванию размера, события
 * в порядке файла) — при любом числе потоков конфигурации.
 *
 * Типичный сценарий:
 *   tj_pipeline* p = tj_create(NULL, my_sink, ud);   // NULL cfg = дефолты
 *   tj_add_dir(p, "E:/TJ_Logs/Diag");                // можно несколько раз
 *   int rc = tj_run(p);                              // 0 | 1 | 2
 *   tj_get_stats(p, &st);
 *   tj_destroy(p);
 */
#ifndef TJ_FFI_H
#define TJ_FFI_H

#include <stddef.h>
#include <stdint.h>

#if defined(_WIN32)
#  if defined(TJ_FFI_BUILD)
#    define TJ_API __declspec(dllexport)
#  elif defined(TJ_FFI_STATIC)
#    define TJ_API
#  else
#    define TJ_API __declspec(dllimport)
#  endif
#else
#  define TJ_API __attribute__((visibility("default")))
#endif

#ifdef __cplusplus
extern "C" {
#endif

typedef struct tj_pipeline tj_pipeline; /* непрозрачный хэндл */

/* Одна NDJSON-запись БЕЗ завершающего '\n'; record не NUL-терминирован и
 * валиден только на время вызова. Колбэк не должен вызывать longjmp/panic
 * сквозь границу ABI. */
typedef void (*tj_sink_fn)(void* user_data, const char* record, size_t len);

/* Завершение файла (телеметрия): ok=0 — файл не удалось открыть/замапить. */
typedef void (*tj_file_fn)(void* user_data, const char* utf8_path,
                           uint64_t events, uint64_t parse_skips,
                           uint64_t bytes, int32_t ok);

typedef struct tj_config {
    uint32_t workers;             /* 0 = число аппаратных потоков */
    uint32_t admission_budget_mb; /* 0 = auto: max(64 МБ × workers, 256 МБ).
                                     Байтовый бюджет допуска: суммарный размер
                                     НЕголовных файлов «разобран, но не записан»;
                                     головной файл стримится вне бюджета */
    uint32_t chunk_bytes;         /* 0 = 4 МБ; порог передачи буфера писателю */
    uint32_t map_bytes;           /* 0 = 64 МБ; окно mmap входного файла (файлы не
                                     отображаются целиком — резидентность ограничена) */
} tj_config;

typedef struct tj_stats {
    uint64_t files;
    uint64_t events;
    uint64_t parse_skips;
    uint64_t small_file_skips;
    uint64_t failed_files;
    uint64_t bytes;
} tj_stats;

/* Создаёт конвейер. cfg может быть NULL (дефолты). sink может быть NULL
 * (null-sink: только разбор и счётчики). file_cb может быть NULL.
 * NULL при нехватке памяти. */
TJ_API tj_pipeline* tj_create(const tj_config* cfg, tj_sink_fn sink, void* user_data);

/* Необязательный колбэк завершения файла. Вызывать до tj_run(). 0 = ок. */
TJ_API int32_t tj_set_file_callback(tj_pipeline* p, tj_file_fn cb);

/* Рекурсивно добавляет *.log (>= 100 байт) из каталога. Возвращает число
 * добавленных файлов (>= 0) либо -1 (каталог не существует / ошибка —
 * см. tj_last_error). Можно вызывать несколько раз. */
TJ_API int32_t tj_add_dir(tj_pipeline* p, const char* utf8_dir);

/* Разбирает все добавленные файлы, выдаёт записи в sink.
 * 0 — успех; 1 — фатальная ошибка (см. tj_last_error);
 * 2 — часть файлов не удалось прочитать (KI-12, счётчик failed_files). */
TJ_API int32_t tj_run(tj_pipeline* p);

/* Статистика последнего tj_run (контракт --stats-json). 0 = ок. */
TJ_API int32_t tj_get_stats(const tj_pipeline* p, tj_stats* out);

/* Текст последней ошибки (UTF-8, "" если ошибок не было). Указатель валиден
 * до следующего вызова любой tj_*-функции с этим хэндлом. */
TJ_API const char* tj_last_error(const tj_pipeline* p);

/* Уничтожает конвейер. p может быть NULL. Не вызывать во время tj_run. */
TJ_API void tj_destroy(tj_pipeline* p);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* TJ_FFI_H */
