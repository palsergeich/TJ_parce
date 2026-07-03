# ТехЖурнал — рабочее место по производительности 1С

Платформа анализа технологического журнала 1С: высокопроизводительный нормализатор (C++, AVX2, memory-mapped I/O, **до 3.7 ГБ/с**) + агент-сборщик + хранилище + дашборды.

Проверено на реальном корпусе: **175 ГБ ТЖ, 7100 файлов, ~193 млн событий — нормализация за 44 секунды.**

## Архитектура (целевая)

```
[Сервер 1С]                        [Сервер анализа]                  [Пользователь]
агент-сборщик                   →  ClickHouse (первый бэкенд)     →  Grafana (этап 1)
 ├─ онлайн-хвост каталогов ТЖ      PostgreSQL / DuckDB / MSSQL       свой UI (этап 2)
 ├─ ядро-нормализатор (C++)        (за слоем абстракции Store)
 ├─ разовый импорт архива          Prometheus ← /metrics агентов
 └─ NDJSON → батчи → БД
```

Язык агента выбирается соревнованием **Go vs Rust vs C++** на реальном корпусе — протокол в [docs/bakeoff-protocol.md](docs/bakeoff-protocol.md).

## Структура репозитория

| Путь | Что |
|------|-----|
| `cpp_parse/` | **Рабочий нормализатор** (события ТЖ → NDJSON). Будет распилен в `core/` |
| `cpp/` | Старая версия — счётчик уникальных Context (донор quote-parity логики) |
| `core/` | (цель) ядро-нормализатор: библиотека + CLI + C ABI |
| `agents/{go,rust,cpp}/` | участники bake-off |
| `server/` | будущий API-сервер |
| `deploy/` | docker-compose: ClickHouse + Grafana + Prometheus, схема БД |
| `bench/` | скрипты и результаты замеров |
| `tests/` | golden / integration / tail / fuzz |
| `docs/` | проектная документация (см. ниже) |
| `_Архив/` | старые эксперименты (Python/Go/Rust-версии счётчика) |

## Документация

- [ROADMAP.md](ROADMAP.md) — план по фазам, текущий статус
- [docs/format-spec.md](docs/format-spec.md) — **спецификация NDJSON** (источник истины формата)
- [docs/normalizer-source-map.md](docs/normalizer-source-map.md) — карта исходников ядра, найденные баги, план адаптации
- [docs/event-inventory.md](docs/event-inventory.md) — инвентаризация 18 типов событий реального корпуса
- [docs/storage-design.md](docs/storage-design.md) — схема ClickHouse + адаптации PG/DuckDB/MSSQL, дашборды, метрики
- [docs/bakeoff-protocol.md](docs/bakeoff-protocol.md) — протокол соревнования агентов и тест-стратегия

## Быстрый старт (что работает уже сейчас)

Нормализовать каталог с ТЖ в NDJSON:

```cmd
cpp_parse\build\count_contexts.exe <каталог_с_ТЖ> <потоки> <выход.jsonl>
```

Пример: `cpp_parse\build\count_contexts.exe E:\TJ_Logs\TJ_Logs 16 result.jsonl`

Поднять стек хранения/визуализации:

```cmd
docker compose -f deploy\docker-compose.yml up -d
```

ClickHouse: `localhost:8123` (HTTP) / `localhost:9001` (native; хост-порт 9001, т.к. 9000 на этой машине занят), Grafana: `localhost:3000` (admin/admin), Prometheus: `localhost:9090`.

Схема БД (`tj.events` + 4 агрегатных MV) применяется автоматически при первом старте и **проверена на живом ClickHouse 24.8** — вставка и все агрегаты работают.

## Тестовые данные

Референсный корпус: `E:\TJ_Logs\TJ_Logs` (175 ГБ, 21 коллекция, 2025-11-28…30).
Сиды для golden-тестов: `tests/golden/seed/` (8 небольших сырых логов из разных коллекций).
