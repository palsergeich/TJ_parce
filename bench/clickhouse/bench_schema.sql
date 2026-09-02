-- Контрактная таблица e2e-серии bake-off (docs/bakeoff-protocol.md §1.2).
-- Отдельная БД: продакшн tj.events (121.5 млн строк) не участвует в замерах.
CREATE DATABASE IF NOT EXISTS tj_bench;

CREATE TABLE IF NOT EXISTS tj_bench.events
(
    timestamp  DateTime64(6),
    duration   UInt64,
    event      LowCardinality(String),
    level_num  LowCardinality(String),  -- важность из заголовка ТЖ (rev 4)
    level      LowCardinality(String),  -- свойство события level=INFO|DEBUG
    filename   String,
    file_path  String,
    -- Значение — все вхождения ключа в порядке события (format-spec §4.5 rev 4)
    props      Map(LowCardinality(String), Array(String))
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (event, timestamp);
