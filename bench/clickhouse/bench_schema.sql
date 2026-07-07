-- Контрактная таблица e2e-серии bake-off (docs/bakeoff-protocol.md §1.2).
-- Отдельная БД: продакшн tj.events (121.5 млн строк) не участвует в замерах.
CREATE DATABASE IF NOT EXISTS tj_bench;

CREATE TABLE IF NOT EXISTS tj_bench.events
(
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
