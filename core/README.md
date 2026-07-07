# core/ — C++ ядро нормализатора техжурнала 1С

Библиотека + тонкий CLI + C ABI. Извлечено из `cpp_parse/count_contexts.cpp`
(эталон остаётся нетронутым) по плану из
[docs/normalizer-source-map.md](../docs/normalizer-source-map.md), пункты (c)/(d).
Это же — участник C++ в bake-off ([docs/bakeoff-protocol.md](../docs/bakeoff-protocol.md) §1.1).

Формат вывода — [docs/format-spec.md](../docs/format-spec.md) v1.0 rev 3,
**байт-в-байт** с эталоном: golden-гейт `tests/golden/run_golden.ps1` проходит
18 PASS / 0 FAIL / 1 XFAIL (XFAIL — известный KI-1 `mask_inside_quotes`,
воспроизводится сознательно). Вывод побайтно совпадает с Go-агентом
(`agents/go`) на реальных коллекциях корпуса.

## Раскладка

```
core/
├─ include/tj/normalizer.hpp  # публичный API: Config, NormalizerPipeline, RunStats
├─ src/
│  ├─ parser.{hpp,cpp}        # маска события, сплит на события, автомат свойств
│  │                          # ('' / "" / KI-10), типизация чисел (KI-2 strict),
│  │                          # JSON-экранирование — побайтная семантика спеки;
│  │                          # + эмиссия RowBinary (ClickHouse) с ОБЩИМ разбором
│  │                          # заголовка/свойств (политики NdjsonProps/RbProps)
│  ├─ pipeline.cpp            # оконный mmap, байтовый бюджет допуска, воркеры,
│  │                          # писатель (стриминг головного файла)
│  └─ util.{hpp,cpp}          # дата из имени файла, file_path «два предка»
├─ cli/
│  ├─ main.cpp                # tj-agent-cpp: оба CLI-контракта
│  └─ clickhouse_sink.{hpp,cpp} # --sink clickhouse: RowBinary over HTTP (WinHTTP,
│                             # без внешних зависимостей), батчер 50k/64МБ/1000мс
├─ ffi/
│  ├─ tj_ffi.h                # C ABI (tj_create/tj_add_dir/tj_run/…)
│  ├─ tj_ffi.cpp
│  └─ selftest.c              # минимальная проверка ABI на чистом C
└─ CMakeLists.txt             # tj_core (static) + tj-agent-cpp + tj_core_ffi (DLL)
```

## Сборка

Требуются MSVC (VS 2022 Build Tools) и CMake ≥ 3.20. Из корня репозитория:

```bat
call "C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\VC\Auxiliary\Build\vcvars64.bat"
cmake -G "Visual Studio 17 2022" -A x64 -S core -B core\build
cmake --build core\build --config Release
```

Артефакты: `core\build\Release\{tj-agent-cpp.exe, tj_core.lib, tj_core_ffi.dll, tj_ffi_selftest.exe}`.

Флаги релиза — строго по bakeoff-protocol §2.1: `/O2 /GL /arch:AVX2 /DNDEBUG`
+ линк `/LTCG` (плюс не-оптимизационные `/std:c++17 /EHsc /utf-8 /DNOMINMAX
/D_CRT_SECURE_NO_WARNINGS`).

**Известная ловушка:** генератор «NMake Makefiles» на этом репозитории не
работает — кириллица в пути (`E:\git\ТехЖурнал`) ломает try_compile CMake на
записи PDB (LNK1201: response-файлы nmake проходят через OEM-кодировку).
Генератор Visual Studio (msbuild) обрабатывает Unicode-пути корректно — он
и зафиксирован выше.

## CLI (`tj-agent-cpp.exe`)

Оба контракта обязательны и эквивалентны по выводу:

```
# контракт golden-раннера (совместим с cpp_parse/count_contexts.exe)
tj-agent-cpp <input_dir> [workers] [output.jsonl] [--no-output]

# контракт bake-off
tj-agent-cpp --input <dir> [--threads N] [--sink null|file:<path>|clickhouse[:<url>]]
             [--batch-rows N] [--batch-bytes N] [--flush-ms N] [--stats-json <path>]
```

- Данные идут **только в файл/БД** (никогда в stdout). stdout — прогресс и
  успешная сводка (обычный UTF-8 без WriteConsoleW — редирект ничего не
  теряет); stderr — только реальные ошибки (важно для PowerShell 5.1 c
  `$ErrorActionPreference='Stop'`).
- `--stats-json` — контракт bakeoff-protocol §3:
  `{"bytes":…,"events":…,"failed_files":…,"files":…,"parse_skips":…,"skips":…,"small_file_skips":…}`;
  при `--sink clickhouse` добавляется `"inserted_rows":…`.
- `--batch-rows/--batch-bytes/--flush-ms` настраивают батчер clickhouse-sink
  (для file/null принимаются и игнорируются); `--follow` пока не реализован
  (фаза 3) и даёт ошибку.
- Exit-коды: `0` успех; `1` ошибка аргументов/каталога/записи вывода/вставки
  в ClickHouse (KI-5: ошибка писателя фатальна, без дедлока); `2` часть
  входных файлов не прочитана (KI-12).
- Аргументы принимаются широкими (`wmain`) и конвертируются в UTF-8 —
  кириллические пути работают на любой системной локали.

### `--sink clickhouse[:<url>]`

События вставляются в ClickHouse по HTTP-интерфейсу
(`POST /?query=INSERT INTO <db>.<table> FORMAT RowBinary`) через системный
**WinHTTP** — внешних зависимостей нет. Кодирует строки само ядро на уровне
разобранного события (`tj::parse::append_event_rowbinary`) — готовый NDJSON
не пересобирается и не перечитывается.

- URL: `http://host[:port][/<db>.<table>]`; по умолчанию
  `http://localhost:8123/tj_bench.events`. Пример:
  `--sink clickhouse:http://localhost:8123/tj_bench.events_cpp`.
- Схема колонок (bench-таблица `tj_bench.events`): `timestamp DateTime64(6)`
  (µs с эпохи; деградированный файл без даты в имени → `1970-01-01`),
  `duration UInt64`, `event`/`level` `LowCardinality(String)` (level числовой
  и так десятичный текст), `filename`/`file_path String`,
  `props Map(LowCardinality(String), String)` — ВСЕ свойства события, значения
  текстом без JSON-экранирования (кавычки `''`/`""` развёрнуты, многострочные
  значения сохраняют реальные `\r\n`).
- Батч-политика: флаш при `--batch-rows` (50 000) ИЛИ `--batch-bytes` (64 МБ)
  ИЛИ `--flush-ms` (1000 мс) с прошлого флаша; остаток отправляется в конце.
- Префлайт при старте (INSERT с пустым телом) проверяет сервер и таблицу ДО
  разбора корпуса: неверный порт/хост/таблица — быстрый exit 1, не зависание.
- Замер (Diag_86, 271.75 МБ, 1 256 605 событий, `--threads 8`, локальный
  ClickHouse 24.8 в Docker): ~2.3 с, **~550 тыс. строк/с**; `count()`,
  `sum(duration)`, `min/max(timestamp)` и гистограмма по `event` совпадают
  с NDJSON-выводом до строки/байта.

## Гарантии и архитектура

- **Детерминизм (v1.1 §5, жёстче KI-11):** порядок записей внутри файла =
  порядок событий в файле; файлы — по убыванию размера; вывод байт-идентичен
  при **любом** числе потоков: воркеры берут файлы строго по возрастанию
  индекса, писатель выдаёт файлы по порядку.
- **Байтовый бюджет допуска** (модель agents/go и agents/rust, бэкпорт):
  ГОЛОВНОЙ файл (его очередь писаться) допускается безусловно и **стримится**
  чанками `chunk_bytes` (4 МБ) с ограниченной очередью (16 чанков — медленный
  приёмник не копит вывод головы); НЕголовной файл допускается, только если
  его размер помещается в остаток бюджета `max(64 МБ × workers, 256 МБ)`
  (`Config::admission_budget_bytes`), и буферизуется до своей очереди; файл
  крупнее остатка ждёт, пока сам станет головой. Бюджет возвращается
  писателем после выдачи файла на любом пути, включая ошибки. Дедлок
  невозможен: допуск головы никогда не блокируется на бюджете. Прежнее окно
  «2×workers файлов» буферизовало целиком вывод любого неголовного файла:
  на стрессе 6.04+1.95 ГБ (`Mem\rphost_47988`, 8 потоков, null-sink) peak RSS
  был **2368 МБ**; с байтовым бюджетом — **73 МБ** (медиана wall 14.6 с
  против 11.5 с у старого окна на тёплом кэше: второй файл больше бюджета и
  разбирается после головы; на холодном кэше последовательное чтение диску
  только удобнее — у Go/Rust с той же моделью 9.7/10.7 с).
- **Ограниченная память на гигантских файлах:** вход отображается не целиком,
  а скользящим окном `map_bytes` (64 МБ по умолчанию), окно всегда покрывает
  текущее событие (у whole-file-mmap файл 5.76 ГБ давал 8 ГБ WorkingSet).
- **Сохранённые квирки эталона** (обязательны для байт-точности): KI-1
  ложный сплит маской внутри кавычек (golden `mask_inside_quotes` — XFAIL),
  KI-10 эвристика закрытия `'`, несимметрия `''`/`""`, KI-3 UTF-8 не
  валидируется, KI-4 дубликаты ключей.
- **Исправления из cpp_parse сохранены:** строгая грамматика чисел и
  канонизация duration (KI-2), пропуск входного BOM (KI-6), без выходного BOM
  (KI-7), валидация workers (KI-8), exit-коды и дренаж при ошибке писателя
  (KI-5/KI-12). POSIX-ветка mmap компилируема (KI-9) — но собиралась и
  тестировалась только Windows-сборка.
- Ограничение оконного сканера: строка-кандидат маски события длиной больше
  64 КБ (гвардейская зона окна) теоретически может классифицироваться иначе,
  чем при сплошном отображении. В ТЖ маска — ~15–25 байт; на корпусе
  расхождений с Go-агентом нет.

## Использование как библиотеки

```cpp
#include <tj/normalizer.hpp>

tj::Config cfg;
cfg.workers = 16;                      // 0 = все аппаратные потоки
cfg.on_error = [](const std::string& m) { std::fprintf(stderr, "%s\n", m.c_str()); };

tj::NormalizerPipeline pipe(cfg);
pipe.add_dir(L"E:/TJ_Logs/Diag");      // можно несколько каталогов

tj::RunStats st = pipe.run(
    // одна NDJSON-запись БЕЗ '\n'; вызывается на потоке run(),
    // строго в детерминированном порядке
    [&](const char* rec, std::size_t len) { sink.write(rec, len); },
    // телеметрия завершения файла (опционально)
    [&](const tj::FileCompletion& fc) { log_file(fc.path, fc.events, fc.ok); });
```

`run()` блокирует вызывающий поток и сам является «писателем»: приёмник
никогда не вызывается конкурентно. Null-sink — передать пустой `RecordFn`.

Для ClickHouse есть второй режим — `run_rowbinary(ChunkFn, FileFn)`: события
кодируются в RowBinary на уровне разбора (тот же автомат заголовка/свойств,
что и NDJSON — множество принятых событий совпадает по построению), приёмник
получает чанки целых строк с их количеством:

```cpp
tj::RunStats st = pipe.run_rowbinary(
    [&](const char* data, std::size_t len, std::uint64_t rows) {
        sink.append(data, len, rows); // напр. tj_cli::ClickHouseSink
    });
```

## C ABI (FFI) — статус

Собирается `tj_core_ffi.dll` (+ импорт-библиотека) и `tj_ffi_selftest.exe`
(чистый C: create → add_dir → run → stats → destroy; проходит). Обёртка тонкая:
все исключения перехватываются на границе, **abort никогда не вызывается**,
текст последней ошибки — `tj_last_error()`. Строки — UTF-8. Sink-колбэк
вызывается только на потоке `tj_run()`.

```c
tj_pipeline* p = tj_create(NULL /*дефолты*/, my_sink, user_data);
tj_add_dir(p, "E:/TJ_Logs/Diag");
int rc = tj_run(p);            /* 0 ок; 1 фатально; 2 часть файлов не прочитана */
tj_stats st; tj_get_stats(p, &st);
tj_destroy(p);
```

Поле конфига `tj_config.admission_budget_mb` (бывш. `admission_window`) —
байтовый бюджет допуска в МБ, `0` = auto (`max(64 МБ × workers, 256 МБ)`).

Не реализовано (по плану фаз): `tj_follow`/`tj_stop` (tail-режим),
`tj_drain`, RowBinary-режим в C ABI, привязка Go (cgo) / Rust — заголовок
к этому готов.
