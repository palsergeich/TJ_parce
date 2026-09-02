-- 003_rev4_dup_keys.sql — миграция «формат rev 4: повторяющиеся ключи»
-- (docs/format-spec.md §4.5, KI-4 закрыт).
--
-- Что меняется и почему:
--   1. Поле заголовка ТЖ и свойство события с именем level — РАЗНЫЕ величины.
--      До rev 4 они попадали в один ключ NDJSON, приёмник брал первое, и текст
--      уровня (INFO|DEBUG|WARNING) не сохранялся ни в одной строке. Позиционная
--      важность переезжает в level_num, колонка level освобождается под текст.
--      У строк, загруженных до миграции, текста нет и взять его неоткуда —
--      level обнуляется, чтобы семантика колонки была одинаковой везде.
--   2. props становится Map(String, Array(String)): значение — все вхождения
--      ключа в порядке события. Повторы, лежавшие в старых строках плоско
--      (одинаковые ключи в одной Map), группируются.
--
-- Свежая установка: 001_schema.sql уже содержит целевые типы, этот файл
-- идемпотентен и ничего не делает.
--
-- Эталонная установка (2026-09-02) этой миграцией НЕ пользовалась: схема tj
-- пересоздана, а 121.5 млн строк переразобраны заново из корпуса
-- E:\TJ_Logs\TJ_Logs. Так честнее — level текстовый в старых строках взять
-- было неоткуда, миграция оставила бы его пустым навсегда.
-- Этот файл остаётся для установок, где исходные логи не сохранены и
-- переразбор невозможен.
--
-- Существующий сервер (init-скрипты на непустом volume НЕ перезапускаются):
--   docker cp deploy\clickhouse\init\003_rev4_dup_keys.sql tj-clickhouse:/tmp/
--   docker exec tj-clickhouse clickhouse-client -n --queries-file /tmp/003_rev4_dup_keys.sql
--
-- Мутации переписывают затронутые колонки во всех партах. На корпусе 121.5 млн
-- строк / 5.92 ГиБ это минуты; следить за system.mutations.is_done.
--
-- ВНИМАНИЕ дашбордам: обращения к level, где имелась в виду важность,
-- переводить на level_num.

ALTER TABLE tj.events
    ADD COLUMN IF NOT EXISTS level_num LowCardinality(String) AFTER event;

-- Одна мутация: присваивания видят значения ДО мутации, поэтому перенос
-- важности и очистка текста происходят согласованно.
ALTER TABLE tj.events
    UPDATE level_num = level, level = '' WHERE 1;

ALTER TABLE tj.events
    ADD COLUMN IF NOT EXISTS props_v4 Map(LowCardinality(String), Array(String)) CODEC(ZSTD(3));

-- Группировка повторов: ключи — по первому вхождению (arrayDistinct сохраняет
-- порядок), значение — все вхождения этого ключа в исходном порядке.
ALTER TABLE tj.events
    UPDATE props_v4 = CAST(
        (arrayDistinct(mapKeys(props)),
         arrayMap(k -> arrayFilter((v, kk) -> kk = k, mapValues(props), mapKeys(props)),
                  arrayDistinct(mapKeys(props)))),
        'Map(LowCardinality(String), Array(String))')
    WHERE 1;

-- Выполнять только после is_done = 1 у мутации выше:
--   SELECT * FROM system.mutations WHERE table = 'events' AND is_done = 0;
ALTER TABLE tj.events DROP COLUMN IF EXISTS props;
ALTER TABLE tj.events RENAME COLUMN props_v4 TO props;
