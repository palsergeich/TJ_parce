-- Схема хранилища ТЖ для ClickHouse (эталонный бэкенд).
-- Полное обоснование: docs/storage-design.md

CREATE DATABASE IF NOT EXISTS tj;

CREATE TABLE IF NOT EXISTS tj.events
(
    -- === идентичность события ===
    ts               DateTime64(6, 'UTC')      CODEC(DoubleDelta, ZSTD(1)),
    duration_us      UInt64                    CODEC(T64, ZSTD(1)),
    event            LowCardinality(String),
    level            LowCardinality(String),
    collection       LowCardinality(String),
    src_file         LowCardinality(String),
    src_path         String                    CODEC(ZSTD(3)),
    src_line         UInt32                    CODEC(T64, ZSTD(1)),

    -- === топология кластера / сеанс ===
    process          LowCardinality(String),
    process_name     LowCardinality(String),
    os_thread        UInt32                    CODEC(T64, ZSTD(1)),
    client_id        UInt32                    CODEC(T64, ZSTD(1)),
    connect_id       UInt32                    CODEC(T64, ZSTD(1)),
    session_id       UInt32                    CODEC(T64, ZSTD(1)),
    usr              LowCardinality(String),
    app_name         LowCardinality(String),
    computer_name    LowCardinality(String),
    app_id           LowCardinality(String),

    -- === СУБД-слой ===
    dbms             LowCardinality(String),
    db_name          LowCardinality(String),
    db_pid           UInt32                    CODEC(T64, ZSTD(1)),
    trans            UInt8,
    rows_ret         UInt64                    CODEC(T64, ZSTD(1)),
    rows_affected    UInt64                    CODEC(T64, ZSTD(1)),

    -- === ресурсы ===
    cpu_time_us      UInt64                    CODEC(T64, ZSTD(1)),
    memory           Int64                     CODEC(T64, ZSTD(1)),
    memory_peak      Int64                     CODEC(T64, ZSTD(1)),
    in_bytes         UInt64                    CODEC(T64, ZSTD(1)),
    out_bytes        UInt64                    CODEC(T64, ZSTD(1)),
    call_wait_us     UInt64                    CODEC(T64, ZSTD(1)),

    -- === интерфейс вызова ===
    iface_name       LowCardinality(String),
    method_name      LowCardinality(String),
    func_name        LowCardinality(String),
    module           LowCardinality(String),

    -- === тексты ===
    context          String                    CODEC(ZSTD(3)),
    context_hash     UInt64,
    context_line     LowCardinality(String),
    sql_text         String                    CODEC(ZSTD(3)),
    sql_hash         UInt64,
    plan_text        String                    CODEC(ZSTD(6)),
    descr            String                    CODEC(ZSTD(3)),
    exception        LowCardinality(String),

    -- === блокировки ===
    lock_regions     String                    CODEC(ZSTD(3)),
    lock_wait_conns  Array(UInt32)             CODEC(ZSTD(1)),
    locks_dump       String                    CODEC(ZSTD(9)),
    deadlock_graph   String                    CODEC(ZSTD(6)),

    -- === длинный хвост ===
    props            Map(LowCardinality(String), String) CODEC(ZSTD(3)),

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
-- TTL отсчитывается от времени СОБЫТИЯ, а не от времени вставки. С коротким TTL
-- импорт исторического архива (сценарий фазы 1) удалялся бы сразу после вставки
-- (проверено на живой БД: парты 2025-11 дропнулись в ту же секунду, а агрегаты
-- в MV остались — рассинхрон). Поэтому по умолчанию — «не удалять» (10 лет).
-- Для онлайн-режима с ретенцией сырья 30 суток (docs/storage-design.md §6):
--   ALTER TABLE tj.events MODIFY TTL toDateTime(ts) + INTERVAL 30 DAY DELETE
TTL toDateTime(ts) + INTERVAL 3650 DAY DELETE
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

-- === MV-1: поминутная сводка ===
CREATE TABLE IF NOT EXISTS tj.agg_minute
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

CREATE MATERIALIZED VIEW IF NOT EXISTS tj.mv_agg_minute TO tj.agg_minute AS
SELECT toStartOfMinute(ts) AS minute, event, process_name, computer_name,
       count() AS cnt, sum(duration_us) AS dur_sum, max(duration_us) AS dur_max,
       quantilesTDigestState(0.5, 0.95, 0.99)(duration_us) AS dur_q,
       sum(cpu_time_us) AS cpu_sum, max(memory_peak) AS mem_peak_max,
       sum(in_bytes + out_bytes) AS io_bytes
FROM tj.events GROUP BY minute, event, process_name, computer_name;

-- === MV-2: роллап по контекстам ===
CREATE TABLE IF NOT EXISTS tj.agg_context
(
    period         DateTime,
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

CREATE MATERIALIZED VIEW IF NOT EXISTS tj.mv_agg_context TO tj.agg_context AS
SELECT toStartOfFiveMinutes(ts) AS period, event, context_hash, usr,
       anyLast(context_line) AS context_line,
       count() AS cnt, sum(duration_us) AS dur_sum, sum(cpu_time_us) AS cpu_sum,
       sum(memory) AS mem_sum, sum(in_bytes + out_bytes) AS io_sum
FROM tj.events
WHERE context_hash != 0
GROUP BY period, event, context_hash, usr;

-- === MV-3: роллап по нормализованным запросам СУБД ===
CREATE TABLE IF NOT EXISTS tj.agg_query
(
    hour           DateTime,
    dbms           LowCardinality(String),
    db_name        LowCardinality(String),
    sql_hash       UInt64,
    sql_sample     SimpleAggregateFunction(anyLast, String),
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

CREATE MATERIALIZED VIEW IF NOT EXISTS tj.mv_agg_query TO tj.agg_query AS
SELECT toStartOfHour(ts) AS hour, dbms, db_name, sql_hash,
       anyLast(sql_text) AS sql_sample, anyLast(context_line) AS context_sample,
       count() AS cnt, sum(duration_us) AS dur_sum, max(duration_us) AS dur_max,
       quantilesTDigestState(0.5, 0.95, 0.99)(duration_us) AS dur_q,
       sum(rows_ret) AS rows_sum
FROM tj.events
WHERE event IN ('DBMSSQL','DBPOSTGRS','DBORACLE','DB2','SDBL') AND sql_hash != 0
GROUP BY hour, dbms, db_name, sql_hash;

-- === MV-4: блокировки/ожидания ===
CREATE TABLE IF NOT EXISTS tj.agg_locks
(
    minute             DateTime,
    event              LowCardinality(String),
    usr                LowCardinality(String),
    lock_regions_short String,
    cnt                UInt64,
    wait_us_sum        UInt64,
    wait_us_max        SimpleAggregateFunction(max, UInt64)
)
ENGINE = SummingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (minute, event, usr, lock_regions_short)
TTL minute + INTERVAL 400 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS tj.mv_agg_locks TO tj.agg_locks AS
SELECT
    toStartOfMinute(ts) AS minute, event, usr,
    substring(lock_regions, 1, 128) AS lock_regions_short,
    count() AS cnt, sum(duration_us) AS wait_us_sum,
    max(duration_us) AS wait_us_max
FROM tj.events
WHERE event IN ('TLOCK','TTIMEOUT','TDEADLOCK')
GROUP BY minute, event, usr, lock_regions_short;
