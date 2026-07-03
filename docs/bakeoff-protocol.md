# Bake-off Go / Rust / C++ для агента сбора техжурнала 1С: протокол соревнования и тест-стратегия

Инвентаризация корпуса (`E:\TJ_Logs\TJ_Logs`, 175 ГБ, 21 коллекция) выполнена; размеры и конкретные файлы ниже используются в протоколе.

| Коллекция | ГБ | Роль в bake-off |
|---|---|---|
| `_Reference20832` | 39.1 | полный прогон финалистов |
| `LongDB_01` | 33.3 | полный прогон финалистов |
| `Diag` | 30.8 | полный прогон финалистов |
| `Mem` | 28.4 | стресс «один гигантский файл»: `Mem\rphost_47988\25113020.log` = **5.76 ГБ** |
| `QERR_Diag` | 18.0 | стресс длинных Sql/Context: `QERR_Diag\rphost_14952\25113002.log` = 1.39 ГБ |
| `_AccumRgTn14466_AccumRg14438` | 7.7 | альтернативный однородный bench-набор |
| `CallsDiag` 2.78 + `_Reference27041` 1.01 + `EXP` 0.50 + `TLockDiag` 0.35 + `_ReferenceChngR12527` 0.30 + `Diag_86` 0.27 + `_Document15317` 0.21 | **= 5.42 ГБ** | **`bench-medium`** — основной замер (влезает в page cache) |
| `Diag_86` 0.27 + `EXP_86` 0.01 + `CallsDiag_86` 0.01 | **= 0.29 ГБ** | **`bench-smoke`** — CI-регрессия |

---

## 1. Скоуп bake-off: что реализует каждый участник

### 1.1. Обязательные сценарии (одинаковый CLI-контракт у всех трёх)

Каждый агент (`agents/go`, `agents/rust`, `agents/cpp`) обязан поддержать единый интерфейс:

```
tj-agent-{go|rs|cpp} --input <dir> --threads <N> --sink {null|file:<path>|clickhouse:<dsn>}
                     [--follow] [--batch-rows 50000] [--batch-bytes 67108864] [--flush-ms 1000]
                     [--stats-json <path>]
```

**Сценарий A — batch-ingest:** рекурсивный обход каталога `*.log` → нормализация событий ТЖ в записи по спецификации `docs/format-spec.md` (та же семантика, что у `cpp_parse/count_contexts.cpp`, но **без BOM на выходе** и с исправленным `is_number_token` — см. п. 4.1) → батчирование → вставка в ClickHouse.

**Сценарий B — follow/tail:** слежение за растущими файлами (дозапись + появление новых `YYMMDDHH.log` + ротация/усечение), открытие с `FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE`, незавершённое хвостовое событие не эмитится до прихода следующей строки-маски `^\d{2}:\d{2}\.\d{6}-\d+,` или idle-таймаута 2 с; чекпоинт оффсетов только после подтверждённой вставки (min-contiguous offset, идентификация файла по volume serial + file index, не по имени).

**Способ парсинга — свободный выбор участника**, фиксируется в заявке:
- нативный парсер на своём языке (перенос IP: `is_event_start`, сплит по next-event-start, автомат свойств с правилами `''`/`""`, типизация `is_number_token`+always-string, `parse_date_from_filename`);
- либо обвязка существующего C++-ядра: сабпроцесс со stdout-стримингом NDJSON (адаптация (a): `_setmode(_O_BINARY)`, без BOM, статус на stderr) или FFI/DLL (адаптация (c): C ABI `tj_create/tj_add_file/tj_drain/tj_stop/tj_destroy`).
- Разрешено выставить **две конфигурации на язык** (native и wrapped), но в зачёт идёт лучшая одна.

### 1.2. Общее (фиксируется до старта, менять нельзя)

- **Один экземпляр ClickHouse**: `clickhouse/clickhouse-server:24.8` в Docker, конфиг из `deploy/clickhouse/`, рестарт контейнера + `TRUNCATE TABLE` между прогонами.
- **Одна таблица** (динамические свойства нормализуются в `Map` — это часть контракта, чтобы схема не плавала):

```sql
CREATE TABLE tj.events (
    timestamp  DateTime64(6),
    duration   UInt64,
    event      LowCardinality(String),
    level      LowCardinality(String),
    filename   String,
    file_path  String,
    props      Map(LowCardinality(String), String)
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (event, timestamp);
```

- **Клиент CH** — любой официальный для языка (clickhouse-go v2, clickhouse-rs, clickhouse-cpp), протокол native TCP; настройки вставки одинаковы: `async_insert=0`, серверные значения по умолчанию.
- **Единая политика батчей:** 50 000 строк ИЛИ 64 МБ ИЛИ 1000 мс — что наступит раньше.
- **Корпус замера:** `bench-medium` (5.42 ГБ, состав в таблице выше, собирается junction-ссылками скриптом `bench\scripts\make_corpus.ps1` — данные не копируются). **Полный корпус 175 ГБ — только для двух финалистов** (один прогон end-to-end + один null-sink).
- **Гейт корректности:** к замерам допускается только агент, прошедший 100% golden-суиты (п. 4.1) и tail-теста (п. 4.5). Скорость неправильного парсера не измеряется.

### 1.3. Критерии победителя (объявить заранее)

| Критерий | Вес |
|---|---|
| End-to-end throughput (bench-medium → CH), медиана MB/s | 35% |
| Parse-only throughput (`--sink null`) | 20% |
| Peak RSS на bench-medium и на файле 5.76 ГБ | 15% |
| Tail-режим: корректность (0 потерь/дублей) + p95 задержки append→queryable | 15% |
| Полный корпус 175 ГБ (только финалисты): устойчивость, деградация MB/s к bench-medium | 10% |
| Сопровождаемость: LOC, зависимостей, время сборки, простота FFI/деплоя | 5% |

---

## 2. Правила честности

### 2.1. Версии и сборка (файл `bench/versions.lock.md`, фиксируется коммитом)

| | Тулчейн | Флаги релиза |
|---|---|---|
| Go | Go 1.24.x (точный патч зафиксировать) | `go build -trimpath -ldflags "-s -w"`; `GOMAXPROCS=$N`; **PGO запрещён** в раунде 1 (у C++/Rust его нет «из коробки» бесплатно; либо PGO всем во втором раунде) |
| Rust | stable 1.8x (точная версия) | `cargo build --release` + в `Cargo.toml`: `lto = "fat"`, `codegen-units = 1`, `panic = "abort"`; `RUSTFLAGS="-C target-cpu=x86-64-v3"` |
| C++ | MSVC 19.4x (VS 2022) или clang-cl 18 — один компилятор на весь раунд | `/O2 /GL /arch:AVX2 /DNDEBUG` + линк `/LTCG` (CMake: `-DCMAKE_BUILD_TYPE=Release -DCMAKE_INTERPROCEDURAL_OPTIMIZATION=ON`) |

- Целевая микроархитектура одинаковая: AVX2 (`x86-64-v3`) всем; кто не использует SIMD — его выбор.
- Аллокаторы: разрешены любые публичные пакеты экосистемы (mimalloc и т.п.), версии пиннятся в lock-файлах. Это честно — экосистема часть языка.
- Хэши коммитов трёх агентов на момент замера фиксируются в `bench/results/<дата>/manifest.json`.

### 2.2. Бюджет ресурсов

- `N = число физических ядер` (узнать: `(Get-CimInstance Win32_Processor | Measure-Object NumberOfCores -Sum).Sum`). Всем `--threads $N`. Внутреннее распределение читатели/парсеры — на усмотрение участника (это тоже предмет соревнования), но суммарный бюджет одинаков.
- Один и тот же NVMe для корпуса и данных CH; корпус read-only.

### 2.3. Гигиена окружения (скрипт `bench\scripts\prepare_host.ps1`)

```powershell
powercfg /setactive 8c5e7fda-e8bf-4a96-9a85-a6e23a8c635c   # High performance
Add-MpPreference -ExclusionPath 'E:\TJ_Logs','E:\bench'     # Defender вне замера
# Пауза Windows Update, закрыть браузеры/IDE; не трогать машину во время прогонов
```

### 2.4. Протокол прогрева и повторов

- **Прогрев page cache** перед каждой серией (bench-medium 5.4 ГБ < RAM):

```powershell
Get-ChildItem E:\bench\corpus-medium -Recurse -Filter *.log | ForEach-Object {
  $fs=[IO.File]::OpenRead($_.FullName); $buf=New-Object byte[] 4MB
  while($fs.Read($buf,0,$buf.Length) -gt 0){}; $fs.Close() }
```

- **3 прогона на конфигурацию, берём медиану.** Если разброс (max−min)/median > 10% — серия невалидна, повторить.
- **Ротация порядка** участников между сериями (Go→Rust→C++, Rust→C++→Go, C++→Go→Rust) — нейтрализует тепловой дрейф и фоновые эффекты.
- Между end-to-end прогонами: `docker restart tj-clickhouse` + `TRUNCATE tj.events` + ожидание `SELECT 1`.

### 2.5. Разделение parse-only и end-to-end, изоляция БД

1. **`--sink null`** (аналог `--no-output` в cpp_parse): чистая скорость discovery+parse+сериализация. Это главная «языковая» метрика.
2. **`--sink clickhouse`**: полный тракт.
3. **Потолок БД измеряется отдельно**, до соревнования: предварительно нормализованный NDJSON заливается напрямую:

```powershell
Get-Content E:\bench\pregenerated\medium.jsonl -Raw |
  docker exec -i tj-clickhouse clickhouse-client --query "INSERT INTO tj.events FORMAT JSONEachRow"
```

Если все три агента упираются в этот потолок (end-to-end результаты в пределах ±5% друг от друга и ≥90% потолка), end-to-end не дискриминирует — решает parse-only, а в отчёте фиксируется «bottleneck = ClickHouse». 4. Контроль полноты: после каждого end-to-end прогона `SELECT count() FROM tj.events` обязан совпадать с эталонным числом событий корпуса (получено golden-инструментом); расхождение = дисквалификация прогона.

---

## 3. Метрики и их снятие на Windows

| Метрика | Определение | Как снимаем |
|---|---|---|
| Wall time | старт→exit процесса | `hyperfine` (есть под Windows: `winget install hyperfine`) или `Measure-Command` |
| MB/s | байты корпуса / wall | константа корпуса из manifest / время |
| events/s | `SELECT count()` (e2e) или счётчик из `--stats-json` (null) / wall | обязательный `--stats-json` у агентов: `{"events":N,"files":M,"skips":K,"bytes":B}` |
| Peak RSS | max WorkingSet за жизнь процесса | поллинг `PeakWorkingSet64` (см. скрипт) |
| CPU% | TotalProcessorTime / wall / N ядер | тот же скрипт |
| Аллокации | «если дёшево» | Go: дамп `runtime.MemStats` (Mallocs, TotalAlloc) в `--stats-json`; Rust: счётный `#[global_allocator]`-обёртка за фичей `alloc-stats` (в зачётных прогонах выключена); C++: опционально mimalloc stats |
| CH-сторона | длительности INSERT, InsertedRows/s | `system.query_log`, `system.events` после прогона |

**`bench\scripts\measure.ps1`** (обёртка одного прогона; PeakWorkingSet64 читается поллингом, т.к. после выхода процесса свойство недоступно):

```powershell
param([string]$Exe, [string[]]$Args)
$p = Start-Process -FilePath $Exe -ArgumentList $Args -PassThru -NoNewWindow
$peak = 0
while (-not $p.HasExited) {
    try { $p.Refresh(); if ($p.PeakWorkingSet64 -gt $peak) { $peak = $p.PeakWorkingSet64 } } catch {}
    Start-Sleep -Milliseconds 200
}
$wall = ($p.ExitTime - $p.StartTime).TotalSeconds
$cpu  = $p.TotalProcessorTime.TotalSeconds
[PSCustomObject]@{ wall_s=$wall; cpu_s=$cpu;
  cpu_pct=[math]::Round(100*$cpu/$wall/$env:NUMBER_OF_PROCESSORS,1);
  peak_rss_mb=[math]::Round($peak/1MB,1); exit=$p.ExitCode } | ConvertTo-Json
```

Пример серии через hyperfine (wall) + measure.ps1 (RSS/CPU — отдельным прогоном, чтобы поллинг не влиял на hyperfine):

```powershell
hyperfine --warmup 1 --runs 3 --export-json bench\results\2026-07-03\go_null.json `
  'agents\go\tj-agent-go.exe --input E:\bench\corpus-medium --threads 16 --sink null'
```

Все результаты складываются в `bench/results/<дата>/{lang}_{scenario}.json`; сводка генерируется `bench\scripts\report.py`.

---

## 4. Тест-стратегия продукта

### 4.0. Сначала — заморозка спецификации

`docs/format-spec.md` — единственный источник истины формата NDJSON. Перед созданием golden-файлов зафиксировать решения по известным багам ядра (иначе golden закрепит мусор):

- `is_number_token`: **строгая JSON-грамматика чисел**; `8.3.22.1704`, `1-2`, `.5`, `0.` — строки (текущее ядро выдаёт невалидный JSON — это баг, не спецификация).
- Входной BOM: **пропускается**, первое событие BOM-файла не теряется.
- Выходной BOM: **отсутствует** (jq/ClickHouse/Elastic его не переваривают).
- Writer-failure: фатальная ошибка с ненулевым exit-кодом, не дедлок.
- False-split события маской внутри кавычек: зафиксировать как known-issue с expected-fail golden-кейсом до починки (перенос кавычко-чётности из `cpp/count_contexts.cpp`).

### 4.1. Golden-тесты нормализатора (`tests/golden/`)

Структура: `tests/golden/cases/<имя>/input/*.log` + `expected.jsonl`. **Правило байт-стабильности:** каждый кейс — один файл, прогон с `--threads 1` (или флагом детерминизма); порядок записей = порядок событий в файле; сравнение побайтовое после нормализации EOL. Для мультифайловых кейсов харнесс сортирует вывод по `(file_path, порядковый номер в файле)`.

Обязательные кейсы (реальные фрагменты вырезать из корпуса, обезличить):

| Кейс | Источник/содержание |
|---|---|
| `basic_call` | типовые CALL/SCALL из `CallsDiag\rphost_*\*.log` |
| `multiline_context` | событие TLOCK/TDEADLOCK с многострочным `Context` из `TLockDiag` |
| `excp_multiline` | EXCP с многострочным описанием из `EXP` |
| `long_sql` | DBMSSQL с большим `Sql` из `QERR_Diag\rphost_14952\25113002.log` |
| `quotes_doubling` | `''`→`'`, `""`→`\"`, незакрытая кавычка до конца события |
| `single_quote_heuristic` | `'val'extra` и кавычка+`\r` (текущая эвристика строки 1062) |
| `bom_input` / `no_bom` | одинаковое содержимое с/без EF BB BF — вывод обязан совпадать |
| `bad_filename` | `notadate.log` → пустой date_prefix |
| `version_token` | `AppVersion=8.3.22.1704` → строка (валидный JSON) |
| `empty_value`, `dup_keys`, `prop_named_event` | краевые случаи ключей |
| `min_size_boundary` | файлы 99/100/101 байт (порог MIN_FILE_SIZE) |
| `mask_inside_quotes` | строка-маска внутри кавычек (expected-fail до фикса) |

Один и тот же golden-раннер (`tests/golden/run_golden.ps1 -Agent <exe>`) гоняется против всех трёх агентов и против эталонного `core` CLI — это входной билет в bake-off.

### 4.2. Property-based / fuzz (`tests/fuzz/`)

- **Дифференциальный фаззинг** — главный приём при трёх реализациях: генератор мутирует валидные события (сиды — golden-inputs), скармливает всем трём парсерам, сравнивает выводы; любое расхождение = находка.
- Нативные фаззеры: C++ — libFuzzer (`clang-cl -fsanitize=fuzzer,address` на функцию parse_event), Go — `go test -fuzz=FuzzParseEvent`, Rust — `cargo fuzz run parse_event`.
- Инварианты (property-тесты): (1) не падает ни на каких байтах; (2) каждая выданная строка — валидный JSON (проверка `serde_json`/`encoding/json`); (3) консервация: число совпадений маски во входе == записи + учтённые skips; (4) идемпотентность правил кавычек на round-trip.
- Обязательные злые входы: обрыв события на полуслове, невалидный UTF-8, NUL-байты, файл из одного BOM, строка 16 МБ без `\n`, `Context` на 10 МБ, CRLF/LF-смесь, месяц «13» в имени файла.

### 4.3. Интеграционный тест с ClickHouse (`tests/integration/`)

```powershell
docker compose -f deploy/docker-compose.yml up -d clickhouse
# init-скрипт deploy/clickhouse/init/001_schema.sql создаёт tj.events
tests\integration\run.ps1 -Agent agents\rust\target\release\tj-agent-rs.exe
```

Мини-корпус `tests/integration/corpus/` (~50 МБ, срез `EXP_86`+`CallsDiag_86`): агент заливает → assert `SELECT count()` == эталон, `SELECT * WHERE ...` для 20 заранее известных записей (сверка timestamp/duration/props побайтно), затем повторный прогон в пустую таблицу для проверки воспроизводимости.

### 4.4. Перф-регрессия (smoke)

- Корпус `bench-smoke` = `Diag_86`+`EXP_86`+`CallsDiag_86` (~0.29 ГБ), null-sink, 3 прогона, медиана.
- Базлайн в `bench/baselines.json` (per-agent MB/s). **Порог: падение >15% от базлайна = fail**, 10–15% = warning. Запуск nightly (не на каждый коммит — шумно), на выделенной машине с prepare_host.ps1.

### 4.5. Tail-mode тест (`tests/tail/`)

Харнесс `tests/tail/generator.ps1` пишет события в `live\rphost_1\<YYMMDDHH>.log` с уникальным маркером `Usr='tail_<counter>'`:

1. **No loss / no dup:** 60 с записи по 10–50 тыс. соб/с (пачки + FlushFileBuffers), агент в `--follow --sink clickhouse`; стоп генератора, 10 с дренаж, стоп агента. Assert: `count() == max(counter)`, `count(DISTINCT Usr) == count()`. Допуск: потерь **0**; дублей 0 (exactly-once) либо ≤0.01% при заявленном at-least-once с дедупом на `ReplacingMergeTree`.
2. **Незавершённое событие:** записать событие без завершающего перевода строки/следующей маски, подождать 5 с (не должно эмититься раньше idle-таймаута как обрезок), дописать хвост + новое событие → ровно одна корректная запись.
3. **Ротация:** переход часа — генератор закрывает `25113021.log`, создаёт `25113022.log`; assert подхват нового файла < 2 с, старый дочитан до конца.
4. **Усечение/пересоздание:** truncate файла до 0 → агент стартует с offset 0 без падения.
5. **Crash-recovery:** `Stop-Process -Force` агента посреди записи, рестарт → инвариант п.1 сохраняется (чекпоинты по подтверждённым вставкам).
6. **Латентность:** p95 (время от append до появления в `SELECT`) < 5 с при 50 тыс. соб/с.
7. **Sharing:** генератор держит файл открытым на запись весь тест (ловит регресс `FILE_SHARE_READ`-only).

### 4.6. Стресс-тесты на реальном корпусе

- `Mem\rphost_47988\25113020.log` (5.76 ГБ, один файл): контроль Peak RSS — для mmap-подхода файл маппится целиком, лимит: RSS-заявка агента документируется, WorkingSet не должен вызывать своп.
- `Mem` целиком (28.4 ГБ, 8 прогонов не нужны — 1 прогон): деградация MB/s относительно bench-medium ≤ 20% (холодный кэш, NVMe).

---

## 5. Структура репозитория

```
tj-platform/
├─ core/                        # C++ нормализатор: библиотека + CLI
│  ├─ include/tj/               # публичные заголовки (NormalizerPipeline, config)
│  ├─ src/                      # перенос из cpp_parse/count_contexts.cpp, распил main()
│  ├─ ffi/                      # C ABI: tj_create/tj_add_file/tj_follow/tj_drain/tj_stop/tj_destroy
│  ├─ cli/                      # тонкий main() поверх библиотеки (замена count_contexts)
│  ├─ fuzz/                     # libFuzzer-цели
│  └─ CMakeLists.txt
├─ agents/
│  ├─ go/                       # go.mod; cmd/tj-agent-go; internal/{parser|coreffi|sink|follow}
│  ├─ rust/                     # Cargo.toml; src/; профиль release с lto=fat
│  └─ cpp/                      # агент поверх core/ как библиотеки
├─ server/                      # будущий API/бэкенд запросов (пока заглушка + README)
├─ deploy/
│  ├─ docker-compose.yml        # clickhouse:24.8 + prometheus + grafana
│  ├─ clickhouse/init/001_schema.sql
│  ├─ prometheus/prometheus.yml # скрейп агентских /metrics (или textfile-exporter)
│  └─ grafana/dashboards/ingest.json
├─ bench/
│  ├─ scripts/                  # prepare_host.ps1, make_corpus.ps1 (junctions),
│  │                            # warmup.ps1, measure.ps1, run_series.ps1, report.py
│  ├─ corpus/                   # ТОЛЬКО манифесты/junctions на E:\TJ_Logs, не данные
│  │  ├─ medium.manifest.json   # 7 коллекций, 5.42 ГБ (состав — п.1.2)
│  │  └─ smoke.manifest.json    # 0.29 ГБ
│  ├─ versions.lock.md
│  ├─ baselines.json
│  └─ results/<YYYY-MM-DD>/     # manifest.json + {lang}_{scenario}.json
├─ tests/
│  ├─ golden/cases/<name>/{input/,expected.jsonl} + run_golden.ps1
│  ├─ fuzz/                     # диф-фаззер + сиды
│  ├─ integration/              # corpus/ (~50 МБ) + run.ps1
│  └─ tail/                     # generator.ps1 + asserts.sql + run.ps1
└─ docs/
   ├─ format-spec.md            # замороженная спецификация NDJSON (п.4.0)
   ├─ bakeoff-protocol.md       # этот протокол: правила, веса, команды
   └─ decision-record.md        # итог: победитель, цифры, обоснование
```

Фрагмент `deploy/docker-compose.yml`:

```yaml
services:
  clickhouse:
    image: clickhouse/clickhouse-server:24.8
    container_name: tj-clickhouse
    ports: ["8123:8123", "9000:9000"]
    volumes:
      - ./clickhouse/init:/docker-entrypoint-initdb.d
      - ch-data:/var/lib/clickhouse
    ulimits: { nofile: 262144 }
  prometheus:
    image: prom/prometheus:v2.53.0
    volumes: [ "./prometheus:/etc/prometheus" ]
    ports: ["9090:9090"]
  grafana:
    image: grafana/grafana:11.1.0
    ports: ["3000:3000"]
    volumes: [ "./grafana/dashboards:/var/lib/grafana/dashboards" ]
volumes: { ch-data: }
```

### Порядок проведения (сводно)

1. Заморозить `format-spec.md` + golden-суиту (включая фиксы is_number_token/BOM) — 1 неделя.
2. Гейт: все агенты проходят golden + integration + tail — без этого к бенчу не допускаются.
3. Серия 1: `bench-medium`, null-sink, 3×, медиана. Серия 2: `bench-medium` → ClickHouse, 3×. Серия 3: tail-нагрузка 50 тыс. соб/с, латентность+корректность. Серия 4: стресс 5.76-ГБ файл (RSS).
4. Два лидера по взвешенной сумме → полный корпус 175 ГБ (1× e2e + 1× null-sink каждому).
5. Итог в `docs/decision-record.md`: таблица метрик, потолок ClickHouse, выбранный победитель и (важно) — решение native vs wrapped-core, поскольку допущены оба пути.
