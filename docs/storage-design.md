# Слой хранения рабочего места по производительности 1С (техжурнал)

Проектное решение. Основано на инвентаризации реального корпуса 175 ГБ (~193 млн событий за ~50 часов, 18 типов событий, пик — CONN 1.3M, CLSTR 272K, DBMSSQL 255K, CALL 181K). Ключевые факты из инвентаризации, повлиявшие на решения:

- Гигантские записи: TLOCK до ~3.4 МБ на строку (свойство `Locks`), плотность от 61 до 4 624 событий/МБ.
- Длительности до 17.5·10⁹ мкс (~4.9 ч) — только 64-битные целые.
- Свойства смешанных типов (`CALL.Method`, `CLSTR.SessionID`, `TLOCK.WaitConnections` — иногда число, иногда строка/список) — нормализатор обязан приводить или уводить в хвост.
- Имена свойств с двоеточиями (`p:processName`, `t:clientID`) — в колонках используем snake_case-маппинг.
- В корпусе только DBMSSQL, но схема обязана покрывать DBPOSTGRS/DBORACLE/DB2 без миграции (общее поле `dbms` уже есть в данных).
- NDJSON от нормализатора с UTF-8 BOM — читать как utf-8-sig.

---

## 1. ClickHouse — эталонная схема (первая реализация)

### 1.1. Принципы

1. **Одна широкая таблица** `tj.events` для всех типов событий. Не таблица-на-тип: 90% свойств общие (`process`, `OSThread`, `t:*`, `Usr`, `SessionID`, `Context`), сквозные запросы («всё по сеансу 12345 за минуту») — основной сценарий drill-down. Разреженные колонки в ClickHouse почти бесплатны (default-значения сжимаются в ноль).
2. **Горячие колонки типизированы**, длинный хвост — в `Map(LowCardinality(String), String)`. Критерий «горячести»: свойство участвует в WHERE/GROUP BY/ORDER BY дашбордов или присутствует в >30% хотя бы одного массового типа событий.
3. **Хэши считает нормализатор**, не БД: `sql_hash` — от нормализованного текста запроса (параметры → `?`, временные таблицы `#tt123` → `#tt`, числовые литералы → `N`), `context_hash` — от полного контекста, `context_line` — последняя строка контекста (точка кода 1С). Это делает роллапы переносимыми между всеми четырьмя СУБД.
4. **Вставка** — батчи Native/RowBinary по 100–500 тыс. строк или раз в 1–2 секунды, что наступит раньше; `async_insert` не используем (свой батчер быстрее и детерминированнее при 3.7 ГБ/с нормализатора). Параллельные вставки — по числу партиций/шардов, но не более ~8 инсертеров на узел.

### 1.2. DDL основной таблицы

```sql
CREATE DATABASE IF NOT EXISTS tj;

CREATE TABLE tj.events
(
    -- === идентичность события ===
    ts               DateTime64(6, 'UTC')      CODEC(DoubleDelta, ZSTD(1)),
    duration_us      UInt64                    CODEC(T64, ZSTD(1)),
    event            LowCardinality(String),                     -- CALL, DBMSSQL, TLOCK...
    level            LowCardinality(String),                     -- INFO/WARNING/ERROR/...
    collection       LowCardinality(String),                     -- имя набора логов (Diag, Mem, LongDB_01...)
    src_file         LowCardinality(String),                     -- filename (yyMMddHH.log)
    src_path         String                    CODEC(ZSTD(3)),   -- полный путь; нужен для drill-down к сырью
    src_line         UInt32                    CODEC(T64, ZSTD(1)),

    -- === топология кластера / сеанс ===
    process          LowCardinality(String),                     -- rphost/rmngr/ragent
    process_name     LowCardinality(String),                     -- p:processName
    os_thread        UInt32                    CODEC(T64, ZSTD(1)),
    client_id        UInt32                    CODEC(T64, ZSTD(1)),  -- t:clientID (ClientID туда же)
    connect_id       UInt32                    CODEC(T64, ZSTD(1)),  -- t:connectID
    session_id       UInt32                    CODEC(T64, ZSTD(1)),  -- 0 = нет; строковые списки -> props
    usr              LowCardinality(String),
    app_name         LowCardinality(String),                     -- t:applicationName
    computer_name    LowCardinality(String),                     -- t:computerName
    app_id           LowCardinality(String),

    -- === СУБД-слой (DBMSSQL/DBPOSTGRS/...) ===
    dbms             LowCardinality(String),
    db_name          LowCardinality(String),                     -- DataBase
    db_pid           UInt32                    CODEC(T64, ZSTD(1)),
    trans            UInt8,
    rows_ret         UInt64                    CODEC(T64, ZSTD(1)),  -- Rows
    rows_affected    UInt64                    CODEC(T64, ZSTD(1)),

    -- === ресурсы (CALL / Mem) ===
    cpu_time_us      UInt64                    CODEC(T64, ZSTD(1)),
    memory           Int64                     CODEC(T64, ZSTD(1)),  -- бывает отрицательным (дельта)
    memory_peak      Int64                     CODEC(T64, ZSTD(1)),
    in_bytes         UInt64                    CODEC(T64, ZSTD(1)),
    out_bytes        UInt64                    CODEC(T64, ZSTD(1)),
    call_wait_us     UInt64                    CODEC(T64, ZSTD(1)),

    -- === интерфейс вызова (CALL/SCALL) ===
    iface_name       LowCardinality(String),                     -- IName
    method_name      LowCardinality(String),                     -- MName; CALL.Method-строка -> сюда
    func_name        LowCardinality(String),                     -- Func
    module           LowCardinality(String),

    -- === тексты (тяжёлые, отдельные кодеки) ===
    context          String                    CODEC(ZSTD(3)),
    context_hash     UInt64,                                     -- cityHash64 нормализатора
    context_line     LowCardinality(String),                     -- последняя строка Context
    sql_text         String                    CODEC(ZSTD(3)),   -- Sql | Query | Sdbl
    sql_hash         UInt64,                                     -- хэш нормализованного текста
    plan_text        String                    CODEC(ZSTD(6)),   -- planSQLText; очень жирный
    descr            String                    CODEC(ZSTD(3)),   -- Descr/Txt/txt
    exception        LowCardinality(String),

    -- === блокировки (TLOCK/TTIMEOUT/TDEADLOCK) ===
    lock_regions     String                    CODEC(ZSTD(3)),   -- Regions
    lock_wait_conns  Array(UInt32)             CODEC(ZSTD(1)),   -- WaitConnections, распарсенный список
    locks_dump       String                    CODEC(ZSTD(9)),   -- Locks: до 3.4 МБ, максимум сжатия
    deadlock_graph   String                    CODEC(ZSTD(6)),   -- DeadlockConnectionIntersections

    -- === длинный хвост ===
    props            Map(LowCardinality(String), String) CODEC(ZSTD(3)),

    -- === skip-индексы ===
    INDEX idx_usr        usr           TYPE bloom_filter(0.01)      GRANULARITY 4,
    INDEX idx_session    session_id    TYPE bloom_filter(0.01)      GRANULARITY 4,
    INDEX idx_ctx_hash   context_hash  TYPE bloom_filter(0.01)      GRANULARITY 4,
    INDEX idx_sql_hash   sql_hash      TYPE bloom_filter(0.01)      GRANULARITY 4,
    INDEX idx_ctx_tok    context       TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 8,
    INDEX idx_sql_tok    sql_text      TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 8,
    INDEX idx_dur        duration_us   TYPE minmax                  GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (event, ts, process_name, session_id)
TTL toDateTime(ts) + INTERVAL 30 DAY DELETE,
    toDateTime(ts) + INTERVAL 7 DAY TO VOLUME 'cold'   -- если настроен tiered storage; иначе строку убрать
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;
```

Обоснование ключевых решений:

- **`PARTITION BY toDate(ts)`** — при ~200М строк/день партиция-день даёт быстрый TTL-drop целыми партами (`ttl_only_drop_parts=1`) и удобный period-vs-period.
- **`ORDER BY (event, ts, ...)`** — все дашборд-запросы фильтруют по типу события и времени; `event` первым даёт локальность DBMSSQL/CALL/TLOCK-блоков и лучшее сжатие текстов. Drill-down по `session_id`/`usr` закрывают bloom-фильтры.
- **Смешанные типы**: `session_id`, `client_id` и пр. — числовые; если нормализатор встретил строку (списки в `CLSTR.SessionID`, `TLOCK.WaitConnections`) — в типизированную колонку пишется 0/распарсенный массив, оригинал кладётся в `props['SessionID']`.
- **`locks_dump` с ZSTD(9)** — редкие (10K/сутки), но гигантские значения; высокий уровень сжатия окупается, чтение единичное (drill-down).
- Пер-колоночный TTL для `plan_text`/`locks_dump` (например, `TTL toDateTime(ts) + INTERVAL 14 DAY` на колонке) — опция, если диск дороже, чем повторный сбор.

### 1.3. Материализованные представления

Все MV — `AggregatingMergeTree`/`SummingMergeTree`, TTL 400 дней (агрегаты живут дольше сырья и обслуживают сравнение периодов).

**MV-1. Поминутная сводка по типам событий** — питает обзорный дашборд и таймлайны:

```sql
CREATE TABLE tj.agg_minute
(
    minute         DateTime,
    event          LowCardinality(String),
    process_name   LowCardinality(String),
    computer_name  LowCardinality(String),
    cnt            UInt64,
    dur_sum        UInt64,
    dur_max        SimpleAggregateFunction(max, UInt64),
    dur_q          AggregateFunction(quantilesTDigest(0.5, 0.95, 0.99), UInt64),
    cpu_sum        UInt64,
    mem_peak_max   SimpleAggregateFunction(max, Int64),
    io_bytes       UInt64
)
ENGINE = SummingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (event, minute, process_name, computer_name)
TTL minute + INTERVAL 400 DAY;

CREATE MATERIALIZED VIEW tj.mv_agg_minute TO tj.agg_minute AS
SELECT toStartOfMinute(ts) AS minute, event, process_name, computer_name,
       count() AS cnt, sum(duration_us) AS dur_sum, max(duration_us) AS dur_max,
       quantilesTDigestState(0.5, 0.95, 0.99)(duration_us) AS dur_q,
       sum(cpu_time_us) AS cpu_sum, max(memory_peak) AS mem_peak_max,
       sum(in_bytes + out_bytes) AS io_bytes
FROM tj.events GROUP BY minute, event, process_name, computer_name;
```

**MV-2. Роллап по контекстам (top-N Context)** — «кто съел кластер»:

```sql
CREATE TABLE tj.agg_context
(
    period         DateTime,                       -- 5 минут
    event          LowCardinality(String),
    context_hash   UInt64,
    usr            LowCardinality(String),
    context_line   SimpleAggregateFunction(anyLast, String),
    cnt            UInt64,
    dur_sum        UInt64,
    cpu_sum        UInt64,
    mem_sum        Int64,
    io_sum         UInt64
)
ENGINE = SummingMergeTree
PARTITION BY toYYYYMM(period)
ORDER BY (event, period, context_hash, usr)
TTL period + INTERVAL 400 DAY;

CREATE MATERIALIZED VIEW tj.mv_agg_context TO tj.agg_context AS
SELECT toStartOfFiveMinutes(ts) AS period, event, context_hash, usr,
       anyLast(context_line) AS context_line,
       count() AS cnt, sum(duration_us) AS dur_sum, sum(cpu_time_us) AS cpu_sum,
       sum(memory) AS mem_sum, sum(in_bytes + out_bytes) AS io_sum
FROM tj.events
WHERE context_hash != 0
GROUP BY period, event, context_hash, usr;
```

**MV-3. Роллап по нормализованным запросам СУБД** — top SQL, «долгие DBMSSQL/DBPOSTGRS»:

```sql
CREATE TABLE tj.agg_query
(
    hour           DateTime,
    dbms           LowCardinality(String),
    db_name        LowCardinality(String),
    sql_hash       UInt64,
    sql_sample     SimpleAggregateFunction(anyLast, String),   -- нормализованный текст
    context_sample SimpleAggregateFunction(anyLast, String),
    cnt            UInt64,
    dur_sum        UInt64,
    dur_max        SimpleAggregateFunction(max, UInt64),
    dur_q          AggregateFunction(quantilesTDigest(0.5, 0.95, 0.99), UInt64),
    rows_sum       UInt64
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(hour)
ORDER BY (dbms, hour, sql_hash)
TTL hour + INTERVAL 400 DAY;

CREATE MATERIALIZED VIEW tj.mv_agg_query TO tj.agg_query AS
SELECT toStartOfHour(ts) AS hour, dbms, db_name, sql_hash,
       anyLast(sql_text) AS sql_sample, anyLast(context_line) AS context_sample,
       count() AS cnt, sum(duration_us) AS dur_sum, max(duration_us) AS dur_max,
       quantilesTDigestState(0.5, 0.95, 0.99)(duration_us) AS dur_q,
       sum(rows_ret) AS rows_sum
FROM tj.events
WHERE event IN ('DBMSSQL','DBPOSTGRS','DBORACLE','DB2','SDBL') AND sql_hash != 0
GROUP BY hour, dbms, db_name, sql_hash;
```

**MV-4. Блокировки/ожидания** — питает TLOCK/TDEADLOCK-панели:

```sql
CREATE MATERIALIZED VIEW tj.mv_agg_locks
ENGINE = SummingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (minute, event, usr, lock_regions_short)
TTL minute + INTERVAL 400 DAY
AS SELECT
    toStartOfMinute(ts) AS minute, event, usr,
    substring(lock_regions, 1, 128) AS lock_regions_short,
    count() AS cnt, sum(duration_us) AS wait_us_sum,
    max(duration_us) AS wait_us_max
FROM tj.events
WHERE event IN ('TLOCK','TTIMEOUT','TDEADLOCK')
GROUP BY minute, event, usr, lock_regions_short;
```

Граф «кто кого ждёт» строится по сырью: `lock_wait_conns` (жертва → массив connectID виновников) join-ится на `events` по `connect_id` в окне ±duration — сырьё за 30 дней это позволяет, MV для этого не нужен.

---

## 2. Адаптация схемы под остальные бэкенды

Логическая схема (имена/типы колонок, роллапы) едина; отличается физика.

### 2.1. PostgreSQL

- Таблица `tj_events` с теми же колонками; хвост — `props jsonb`. `LowCardinality` → `text` (+ при желании справочники не городить — не окупается).
- **Декларативное партиционирование** `PARTITION BY RANGE (ts)` по суткам; создание партиций — обязанность `EnsureSchema` (или pg_partman). Retention = `DROP TABLE` партиции.
- Индексы: BRIN по `ts` (дёшево при append-only), btree `(event, ts)`, btree `(sql_hash)`, `(context_hash)`, `(session_id, ts)`, GIN по `props jsonb_path_ops` — только если реально нужен поиск по хвосту.
- Роллапы MV-1..4 — обычные таблицы, наполняемые инкрементальным джобом раз в минуту (`INSERT ... ON CONFLICT DO UPDATE` по ключу периода); у PG нет инкрементальных matview.
- Вставка — `COPY BINARY` батчами; на 200М строк/день PG — это нижняя граница применимости, честно документируем: PG-бэкенд для инсталляций до ~20–30 ГБ ТЖ/день.
- `wal_level=minimal` не трогаем, но `synchronous_commit=off` для ingest-сессий — да.

### 2.2. DuckDB (встраиваемый, локальный анализ на ноутбуке)

- Один файл `tj.duckdb`, та же таблица; хвост — `MAP(VARCHAR, VARCHAR)` (или `JSON`), `event` — `ENUM`.
- Партиционирование не нужно; вместо него — **экспорт/импорт Parquet**: `COPY tj_events TO 'events/' (FORMAT PARQUET, PARTITION_BY (date, event), COMPRESSION ZSTD)`. Это же — формат обмена «снял срез на сервере → унёс на ноутбук».
- MV заменяются обычными VIEW поверх сырья: на ноутбучных объёмах (единицы–десятки ГБ) DuckDB агрегирует сырьё быстрее, чем стоит поддержка роллапов.
- Вставка — appender API или `INSERT ... FROM read_json_auto()` прямо из NDJSON нормализатора.

### 2.3. MS SQL Server

- Таблица с **clustered columnstore index**; партиционная функция по дням (`PARTITION FUNCTION pfTjDay(datetime2)`), retention = `TRUNCATE ... WITH (PARTITIONS ...)` / switch-out.
- Хвост — `props nvarchar(max)` с JSON + при необходимости computed columns `JSON_VALUE(...) PERSISTED` под конкретные фильтры.
- Тексты (`sql_text`, `plan_text`, `locks_dump`) — `nvarchar(max)`; помнить, что LOB в columnstore хранится вне сегментов — гигантские `Locks` сюда ложатся терпимо.
- Вставка — `SqlBulkCopy` батчами **≥102 400 строк**, чтобы попадать напрямую в compressed rowgroups, минуя deltastore.
- Роллапы — таблицы + джоб (indexed views с columnstore и такими агрегатами не дружат).

---

## 3. Абстракция хранилища

Язык-нейтральный контракт; показан как Go. Батч — **колоночный** (SoA), чтобы драйверы ClickHouse Native / DuckDB appender / SqlBulkCopy не переупаковывали строки.

```go
// Dialect — идентификатор бэкенда для UI-слоя (генерация SQL).
type Dialect string
const (
    DialectClickHouse Dialect = "clickhouse"
    DialectPostgres   Dialect = "postgres"
    DialectDuckDB     Dialect = "duckdb"
    DialectMSSQL      Dialect = "mssql"
)

// EventBatch — нормализованные события, колоночная раскладка.
// Все слайсы одной длины N; отсутствие значения = zero value + при
// необходимости оригинал в Props[i].
type EventBatch struct {
    TS          []time.Time // микросекундная точность
    DurationUS  []uint64
    Event       []string
    Level       []string
    Collection  []string
    SrcFile     []string
    SrcPath     []string
    Process     []string
    ProcessName []string
    OSThread    []uint32
    ClientID    []uint32
    ConnectID   []uint32
    SessionID   []uint32
    Usr         []string
    AppName     []string
    ComputerName []string
    // ... остальные горячие колонки 1:1 со схемой (dbms, cpu_time_us,
    //     memory, in/out_bytes, context, context_hash, sql_text, sql_hash,
    //     plan_text, lock_*, ...)
    Props       []map[string]string // длинный хвост
}

type InsertStats struct {
    Rows      int
    Bytes     int64         // объём, ушедший в сеть/на диск
    Elapsed   time.Duration
}

type Health struct {
    OK          bool
    LatencyMS   float64
    Version     string
    PendingMigrations int
    Detail      string
}

// Store — единственный контракт слоя хранения.
type Store interface {
    // Идемпотентно приводит схему к текущей версии кода: БД, таблицы,
    // партиции (PG/MSSQL — заранее на N дней вперёд), MV/роллапы, индексы.
    EnsureSchema(ctx context.Context) error

    // Явные версионированные миграции (up-only). EnsureSchema вызывает
    // Migrate до последней версии; отдельный метод нужен CLI-инструменту.
    Migrate(ctx context.Context, toVersion int) error

    // Батчевая вставка. Реализация обязана быть atomic-or-retryable:
    // либо весь батч, либо ошибка (вызывающий ретраит тот же батч —
    // допускается at-least-once, дедупликация не требуется).
    InsertBatch(ctx context.Context, b *EventBatch) (InsertStats, error)

    // Escape hatch для UI: параметризованный запрос на диалекте бэкенда.
    // Только чтение; реализация ставит read-only-сессию/квоты.
    Query(ctx context.Context, sql string, args ...any) (RowIterator, error)

    // Retention: удалить данные старше указанных горизонтов.
    ApplyRetention(ctx context.Context, raw time.Duration, aggregates time.Duration) error

    Dialect() Dialect
    Health(ctx context.Context) Health
    Close() error
}

type RowIterator interface {
    Next() bool
    Scan(dest ...any) error
    Columns() []string
    Err() error
    Close() error
}
```

Замечания для Rust/C++:

- **Rust**: `trait Store` c `async fn` (или `#[async_trait]`); `EventBatch` — struct из `Vec<...>`; вместо `Query`-итератора — `Stream<Item = Row>`. Ошибки — `thiserror`-enum с вариантом `Retryable(bool)`.
- **C++**: абстрактный класс с виртуальными методами, батч — struct с `std::vector`; владение итератором — `std::unique_ptr<RowIterator>`. Коды ошибок — `expected<T, StoreError>`.
- Инвариант для всех реализаций: `InsertBatch` не парсит и не нормализует — только маппит колонки батча в физическую схему. Вся логика типов/хэшей — в нормализаторе, один раз.

---

## 4. Grafana

### 4.1. Датасорсы

- **ClickHouse**: официальный `grafana-clickhouse-datasource` (Grafana Labs + ClickHouse Inc.). Не Altinity-плагин — официальный лучше поддерживает macros (`$__timeFilter`, `$__interval`) и HTTP/native.
- PostgreSQL, MSSQL — встроенные датасорсы Grafana.
- DuckDB — вне Grafana-сценария (локальный анализ), но при нужде — `motherduck-duckdb-datasource`.
- Дашборды поставляем как JSON-провижининг с переменной `$datasource`, чтобы один комплект работал над CH и PG (SQL-скетчи ниже — ClickHouse-диалект).

### 4.2. Дашборды (6 штук)

**D1. Обзор кластера (Cluster Overview)** — источник: `tj.agg_minute`.
- Stat-ряд: события/сек, ошибок EXCP/мин, активных `process_name`, p99 длительности CALL.
- Timeseries «события по типам»: `SELECT minute, event, sum(cnt) FROM tj.agg_minute WHERE $__timeFilter(minute) GROUP BY minute, event`.
- Timeseries «p50/p95/p99 CALL»: `SELECT minute, quantilesTDigestMerge(0.5,0.95,0.99)(dur_q) FROM tj.agg_minute WHERE event='CALL' AND $__timeFilter(minute) GROUP BY minute`.
- Переменные: `$computer_name`, `$process_name` (из `agg_minute`).

**D2. Топ контекстов (Top Contexts)** — источник: `tj.agg_context`.
- Table top-20 по суммарной длительности:
```sql
SELECT context_line, sum(dur_sum)/1e6 AS sec_total, sum(cnt) AS calls,
       sum(cpu_sum)/1e6 AS cpu_sec, sum(io_sum) AS io_bytes
FROM tj.agg_context WHERE $__timeFilter(period) AND event = '$event'
GROUP BY context_hash, context_line ORDER BY sec_total DESC LIMIT 20
```
- Те же топы с сортировкой по `cpu_sum`, `mem_sum`, `io_sum` (переключатель-переменная `$metric`).
- Data link из строки → D6 (drill-down) с `var-context_hash`.

**D3. Запросы СУБД (DB Queries)** — источник: `tj.agg_query` + сырьё.
- Table top-SQL по `dur_sum` за период, колонки: `sql_sample` (обрезанный), cnt, total sec, p95 (`quantilesTDigestMerge`), max sec, rows, `context_sample`.
- Timeseries по выбранному `$sql_hash` — история конкретного запроса (регрессии планов).
- Table «долгие сырые запросы > $threshold»:
```sql
SELECT ts, duration_us/1e6 AS sec, db_name, usr, session_id,
       substring(sql_text,1,300) AS sql, context_line
FROM tj.events
WHERE event IN ('DBMSSQL','DBPOSTGRS') AND $__timeFilter(ts)
  AND duration_us > $threshold_us
ORDER BY duration_us DESC LIMIT 100
```

**D4. Блокировки и взаимоблокировки (Locks)** — `tj.agg_locks` (наполняется mv_agg_locks) + сырьё.
- Timeseries ожиданий TLOCK (сумма и max wait) по минутам; аннотации TDEADLOCK/TTIMEOUT (`SELECT ts, usr, context_line FROM tj.events WHERE event='TDEADLOCK'`).
- Table «кто кого ждёт»: жертва (usr, session_id, context_line, wait sec) + виновники через `arrayJoin(lock_wait_conns)` join на events по `connect_id`.
- Top lock_regions по суммарному ожиданию.

**D5. Ошибки (Errors: EXCP/QERR/ATTN)** — сырьё.
- Timeseries счётчиков по типам; top-20 `exception`; таблица последних EXCP с `descr`, `usr`, `context_line`; отдельная таблица QERR с `Query` (из `sql_text`).

**D6. Drill-down / сравнение периодов** — сырьё.
- Таблица сырых событий с полным набором фильтров-переменных: `$usr`, `$session_id`, `$process_name`, `$computer_name`, `$event`, `$context_hash` — прямой ответ на «от агрегата к сырому событию».
- Сравнение периодов: два timeseries-запроса к `agg_minute`, второй с `minute + INTERVAL $offset` (переменная `$offset`: 1 day / 7 day), панель-таблица дельт top-контекстов «период A vs B» (два подзапроса к `agg_context`, FULL JOIN по `context_hash`, колонка diff %).

---

## 5. Метрики Prometheus

Экспорт в формате Prometheus (endpoint `/metrics`) у двух компонентов.

**Агент/нормализатор (на каждом сервере 1С):**

| Метрика | Тип | Лейблы | Смысл |
|---|---|---|---|
| `tj_agent_read_bytes_total` | counter | `collection` | прочитано сырого ТЖ |
| `tj_agent_events_total` | counter | `collection`, `event` | нормализовано событий |
| `tj_agent_parse_errors_total` | counter | `collection` | битые строки/записи |
| `tj_agent_lag_seconds` | gauge | `collection` | now − ts последнего обработанного события (главный SLI) |
| `tj_agent_files_open` | gauge | | активных .log-хвостов |
| `tj_agent_normalize_seconds` | histogram | | время нормализации батча |

**Ingest-сервер / загрузчик в БД:**

| Метрика | Тип | Лейблы | Смысл |
|---|---|---|---|
| `tj_ingest_batches_total` | counter | `backend`, `status=ok\|retried\|failed` | вставки |
| `tj_ingest_rows_total` | counter | `backend` | вставлено строк |
| `tj_ingest_bytes_total` | counter | `backend` | байт ушло в БД |
| `tj_ingest_queue_depth` | gauge | | батчей в очереди на вставку |
| `tj_ingest_queue_bytes` | gauge | | байт в очереди (backpressure-триггер) |
| `tj_ingest_insert_seconds` | histogram | `backend` | латентность InsertBatch |
| `tj_ingest_end_to_end_lag_seconds` | gauge | | now − max(ts) вставленных событий |
| `tj_store_healthy` | gauge (0/1) | `backend` | результат Health() |
| `tj_store_migrations_pending` | gauge | `backend` | |

Алерты по умолчанию: `tj_agent_lag_seconds > 300`, `tj_ingest_queue_bytes > 0.8 * limit`, `rate(tj_ingest_batches_total{status="failed"}[5m]) > 0`, `tj_store_healthy == 0`.

Долгосрочные бизнес-метрики (events/sec по кластеру) в Prometheus не дублируем — они уже в `agg_minute` и видны в Grafana из ClickHouse.

---

## 6. Retention/ILM и оценка объёмов

### 6.1. Политика (по умолчанию, настраиваемая)

| Слой | Горизонт | Механизм |
|---|---|---|
| Сырые события (`tj.events`), горячий диск | 7 дней | TTL `TO VOLUME 'cold'` (если tiered) |
| Сырые события, холодный диск | 30 дней | TTL DELETE, drop целых партиций |
| Тяжёлые колонки (`plan_text`, `locks_dump`) | 14 дней (опция) | column TTL |
| Роллапы (`agg_minute`, `agg_context`, `agg_query`, `agg_locks`) | 400 дней | TTL по периоду |
| Архив (опция) | без ограничения | выгрузка партиций в Parquet (ZSTD) на S3/NAS перед drop; читается DuckDB |

Принцип: **drill-down до сырья гарантирован 30 дней; тренды и period-vs-period — 13 месяцев; всё старше — Parquet-архив по требованию.** Для PG/MSSQL то же самое реализуется drop/switch-out партиций через `ApplyRetention`.

### 6.2. Оценка размеров (эталонный корпус 175 ГБ/сутки, ~190М событий/сутки)

Ожидаемые коэффициенты сжатия ClickHouse для ТЖ-данных (сильно повторяющиеся тексты, монотонное время, узкие словари):

- `ts` (DoubleDelta+ZSTD): ~1–2 байта/значение.
- LowCardinality-колонки (`event`, `usr`, `process_name`, `computer_name`, ...): 0.1–0.5 байта/значение.
- Числовые (T64+ZSTD): 0.5–2 байта/значение.
- `context`/`sql_text` (ZSTD3, схожие тексты лежат рядом благодаря ORDER BY по event): 10–25×.
- `plan_text`, `locks_dump` (ZSTD6/9): 20–40× (планы и дампы блокировок чрезвычайно шаблонны).

Итого консервативно **12–18× к сырому логу**: 175 ГБ/сутки → **~10–15 ГБ/сутки** в ClickHouse. Бюджет диска на инсталляцию-эталон:

| Компонент | Объём |
|---|---|
| Сырьё 30 дней | ~300–450 ГБ |
| Роллапы 400 дней | ~30–60 ГБ (агрегаты ~0.1–0.15 ГБ/сутки) |
| Запас на merge (×1.3) + ZooKeeper/логи | ~100–150 ГБ |
| **Итого один узел CH** | **~0.5–0.7 ТБ NVMe** (+ холодный том по желанию) |

Скорость вставки: одиночный узел ClickHouse на NVMe штатно принимает 0.5–1.5 М строк/с в Native-формате — против ~2 200 строк/с среднего потока эталона (190М/сутки) запас ×200+; узким местом будет не БД, а сеть/батчер, поэтому догрузка исторических 175 ГБ выполняется за часы. Для PG-бэкенда тот же корпус — ~1.5–2.5× хуже по сжатию (~25–40 ГБ/сутки с индексами) и предел по вставке ~50–100 К строк/с через COPY — что и фиксирует его нишу «малые инсталляции». MSSQL columnstore по сжатию близок к CH (×8–15), DuckDB+Parquet(ZSTD) — ×15–25 на срезах.
