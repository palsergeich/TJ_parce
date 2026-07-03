# Карта документации

| Документ | Для кого / зачем |
|---|---|
| [user-guide.md](user-guide.md) | **Пользователь/инженер**: развернуть, загрузить ТЖ, работать с дашбордами, SQL-рецепты, обслуживание |
| [format-spec.md](format-spec.md) | **Разработчик агента/парсера**: контракт NDJSON (источник истины), правила разбора и типизации, реестр known issues (KI). По этой спеке пишутся все участники bake-off |
| [storage-design.md](storage-design.md) | **Архитектор/DBA**: схема ClickHouse с обоснованием, адаптации PostgreSQL/DuckDB/MSSQL, интерфейс Store, спецификации дашбордов, метрики Prometheus, ретенция и сайзинг |
| [bakeoff-protocol.md](bakeoff-protocol.md) | **Разработчик**: правила соревнования Go/Rust/C++ (скоуп, честность, метрики, критерии победы) и тест-стратегия продукта (golden/fuzz/integration/tail/перф-регрессия) |
| [event-inventory.md](event-inventory.md) | **Аналитик**: 18 типов событий реального корпуса 175 ГБ — свойства, заполненность, типы, аномалии данных |
| [normalizer-source-map.md](normalizer-source-map.md) | **Разработчик ядра**: карта конвейера cpp_parse по строкам, найденные баги, план превращения в библиотеку (FFI, tail-режим, метрики) |
| [../ROADMAP.md](../ROADMAP.md) | **Все**: план по фазам и текущий статус |
| [../deploy/importer/README.md](../deploy/importer/README.md) | Маппинг полей NDJSON → колонки ClickHouse, ограничения импортёра |
