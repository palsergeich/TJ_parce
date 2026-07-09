-- 002_sql_norm.sql — миграция «Нормализация SQL» (docs/sql-normalization.md).
--
-- Свежая установка: файл выполняется автоматически (docker-entrypoint-initdb.d
-- прогоняет init-скрипты по порядку имён; 001 уже содержит эти объекты —
-- здесь всё идемпотентно: IF NOT EXISTS).
--
-- Существующий сервер: init-скрипты НЕ перезапускаются на непустом volume —
-- применить вручную (только аддитивные операции, данные не трогаются):
--   docker cp deploy\clickhouse\init\002_sql_norm.sql tj-clickhouse:/tmp/
--   docker exec tj-clickhouse clickhouse-client -n --queries-file /tmp/002_sql_norm.sql
--
-- До применения миграции агент со schema=rich и включённой нормализацией
-- (по умолчанию) упадёт на вставке: неизвестные колонки. Обход на время
-- миграции — sql_norm: false в конфиге агента.
--
-- Семантика колонок:
--   sql_norm_hash — cityHash64 нормы (литералы → '?', IN/VALUES-списки
--                   схлопнуты, хвост "p_N: значение" вырезан); 0 — событие
--                   без sql_text либо нормализация выключена;
--   param_count   — число извлечённых параметров с насыщением 65535
--                   (реальная длина всегда length(sql_params));
--   sql_params    — значения параметров, позиционно: i-й элемент = i-й '?'
--                   нормы ДО схлопывания списков (для схлопнутого IN — все
--                   элементы списка). Норм-текст не хранится: детерминированно
--                   восстанавливается нормализатором из sql_text.

ALTER TABLE tj.events ADD COLUMN IF NOT EXISTS sql_norm_hash UInt64 AFTER sql_hash;
ALTER TABLE tj.events ADD COLUMN IF NOT EXISTS param_count UInt16 CODEC(T64, ZSTD(1)) AFTER sql_norm_hash;
ALTER TABLE tj.events ADD COLUMN IF NOT EXISTS sql_params Array(String) CODEC(ZSTD(3)) AFTER param_count;

ALTER TABLE tj.events ADD INDEX IF NOT EXISTS idx_sqlnorm_hash sql_norm_hash TYPE bloom_filter(0.01) GRANULARITY 4;

-- Роллап по нормализованным запросам. Ключ — хэш нормы + бакет числа
-- параметров (план зависит от длины IN-списка: 500 элементов ≠ 5).
-- Счётчики — SimpleAggregateFunction(sum, ...): плоские UInt64 в
-- AggregatingMergeTree теряют значения при слияниях (подтверждено на живом
-- agg_query: sum(cnt) 9.76 млн < 14.96 млн событий). Старые agg_query /
-- mv_agg_query не трогаются — параллельная работа до миграции дашбордов.
CREATE TABLE IF NOT EXISTS tj.agg_query2
(
    hour           DateTime,
    dbms           LowCardinality(String),
    db_name        LowCardinality(String),
    sql_norm_hash  UInt64,
    param_bucket   LowCardinality(String),
    sql_sample     SimpleAggregateFunction(anyLast, String),
    context_sample SimpleAggregateFunction(anyLast, String),
    cnt            SimpleAggregateFunction(sum, UInt64),
    dur_sum        SimpleAggregateFunction(sum, UInt64),
    dur_max        SimpleAggregateFunction(max, UInt64),
    dur_q          AggregateFunction(quantilesTDigest(0.5, 0.95, 0.99), UInt64),
    rows_sum       SimpleAggregateFunction(sum, UInt64),
    params_avg     AggregateFunction(avg, UInt16)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(hour)
ORDER BY (dbms, hour, sql_norm_hash, param_bucket)
TTL hour + INTERVAL 400 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS tj.mv_agg_query2 TO tj.agg_query2 AS
SELECT
    toStartOfHour(ts) AS hour,
    dbms,
    anyLast(db_name) AS db_name,
    sql_norm_hash,
    multiIf(param_count = 0, '0',
            param_count = 1, '1',
            param_count <= 10, '2-10',
            param_count <= 100, '11-100',
            param_count <= 1000, '101-1000',
            '1000+') AS param_bucket,
    anyLast(sql_text) AS sql_sample,
    anyLast(context_line) AS context_sample,
    count() AS cnt,
    sum(duration_us) AS dur_sum,
    max(duration_us) AS dur_max,
    quantilesTDigestState(0.5, 0.95, 0.99)(duration_us) AS dur_q,
    sum(rows_ret) AS rows_sum,
    avgState(param_count) AS params_avg
FROM tj.events
WHERE sql_norm_hash != 0
GROUP BY hour, dbms, sql_norm_hash, param_bucket;
