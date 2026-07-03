# Роадмап: рабочее место по производительности 1С (техжурнал)

Цель: из консольного нормализатора (`cpp_parse`, ~3.7 ГБ/с) сделать полноценное рабочее место:
**агент-сборщик → приёмник → хранилище (ClickHouse первым, затем PG/DuckDB/MSSQL) → Grafana → свой UI**.

Решения владельца продукта (2026-07-03):
- СУБД: все четыре за слоем абстракции; первая — ClickHouse; + Grafana и Prometheus-метрики.
- Клиент: сначала Grafana, свой UI вторым этапом.
- Агент: онлайн-слежение (служба) + разовый импорт архива.
- Агентская часть: **bake-off Go vs Rust vs C++** по протоколу [docs/bakeoff-protocol.md](docs/bakeoff-protocol.md), победитель — по цифрам.

---

## Фаза 0 — фундамент (текущая)

- [x] Разобрать репозиторий, убрать старые попытки в `_Архив/`
- [x] Карта исходников нормализатора → [docs/normalizer-source-map.md](docs/normalizer-source-map.md)
- [x] Инвентаризация событий корпуса 175 ГБ → [docs/event-inventory.md](docs/event-inventory.md) (18 типов, ~193 млн событий)
- [x] Проект слоя хранения → [docs/storage-design.md](docs/storage-design.md)
- [x] Протокол bake-off и тест-стратегия → [docs/bakeoff-protocol.md](docs/bakeoff-protocol.md)
- [x] Спецификация формата v1.0 (draft) → [docs/format-spec.md](docs/format-spec.md)
- [ ] `git init` + первый коммит
- [ ] Установить тулчейны: VS Build Tools (MSVC) или MinGW-w64, CMake, rustup (Go 1.26 и Docker уже есть)
- [ ] **Починить критичные баги ядра** (реестр KI в format-spec.md): KI-2 (невалидный JSON у версий вида `8.3.22.1704`), KI-5 (дедлок writer), KI-6/KI-7 (BOM вход/выход), KI-8 (argv)
- [ ] Golden-тесты: сиды уже в `tests/golden/seed/` (8 файлов из разных коллекций) → нарезать кейсы, зафиксировать `expected.jsonl` ПОСЛЕ фиксов KI

## Фаза 1 — быстрая польза: архив → ClickHouse → Grafana

- [ ] `docker compose -f deploy/docker-compose.yml up -d` (схема применяется из `deploy/clickhouse/init/`)
- [ ] Временный импортёр (Python): NDJSON нормализатора → маппинг полей на колонки `tj.events` (правила в storage-design §1) → вставка батчами
- [ ] Залить архив E:\TJ_Logs, проверить объёмы/сжатие против оценок (~12–18×)
- [ ] Datasource ClickHouse в Grafana + первые 3 дашборда: обзор кластера, top Context, долгие DBMSSQL (спеки в storage-design §4)
- [ ] Смоук сравнения периодов на данных 28–30.11.2025

## Фаза 2 — bake-off агентов (Go / Rust / C++)

- [ ] Вынести ядро: `cpp_parse` → `core/` (библиотека + тонкий CLI, C ABI для FFI) — план в normalizer-source-map «Что нужно для превращения в ядро агента»
- [ ] Реализовать трёх участников с единым CLI-контрактом (`--input --threads --sink {null|file|clickhouse} --follow`)
- [ ] Гейт корректности: golden + integration + tail-тесты (допуск к замерам)
- [ ] Серии замеров по протоколу (bench-medium 5.4 ГБ → финалисты на полных 175 ГБ)
- [ ] Решение → `docs/decision-record.md`

## Фаза 3 — продуктизация агента-победителя

- [ ] Follow-режим на живых логах (share-флаги, чекпоинты оффсетов, ротация часа, crash-recovery)
- [ ] Служба Windows / systemd unit, конфиг агента (какие каталоги, куда слать)
- [ ] Прямая вставка в ClickHouse (native TCP) + буфер на диске при недоступности БД
- [ ] `/metrics` Prometheus (список метрик в storage-design §5) + алерты (lag > 5 мин и т.д.)

## Фаза 4 — мульти-БД слой

- [ ] Интерфейс Store (InsertBatch / EnsureSchema / ApplyRetention / Health — storage-design §3)
- [ ] Бэкенд PostgreSQL (ниша: инсталляции до ~20–30 ГБ ТЖ/день)
- [ ] Бэкенд DuckDB («архив на ноутбуке», обмен через Parquet)
- [ ] Бэкенд MS SQL (columnstore)
- [ ] Кросс-бэкенд интеграционные тесты на одном мини-корпусе

## Фаза 5 — свой UI

- [ ] Сервер запросов (API поверх Store) — язык = победитель bake-off
- [ ] SPA: обзор → top-N → drill-down до сырого события; связка Context ↔ SQL; сравнение периодов
- [ ] Управление сбором: статусы агентов, покрытие logcfg (опционально — раздача logcfg.xml)

---

## Открытые вопросы

1. Таймзона: `timestamp` без TZ (локальное время сервера). При нескольких серверах в разных TZ агенту нужен `--tz`/автоопределение. Решить в фазе 3.
2. В корпусе нет DBPOSTGRS/SDBL — схема их поддерживает, но golden-кейсов нет. Нужен лог с PG-инсталляции.
3. Импортёр фазы 1: выбрасывать ли `Mem_86`-подобные пустые коллекции из обхода (209 файлов по 3 байта BOM) — да, фильтр по размеру уже есть (100 байт).
