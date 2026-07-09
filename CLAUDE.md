# CLAUDE.md — контекст проекта для ИИ-ассистента

Рабочее место по производительности 1С на основе техжурнала (ТЖ). Владелец — инженер по
производительности 1С, общение на русском, стиль «делай» (автономно, спрашивать только
развилки, которые нельзя решить из кода/доков). GitHub: https://github.com/palsergeich/TJ_parce
(push в `master` после каждого законченного куска, коммиты на русском с телом-объяснением).

## Состояние (2026-07-09)

Фазы 0–2 закрыты, фаза 3 почти закрыта. **Bake-off Go/Rust/C++ завершён: агент — Go** (90.2 >
C++ 88.2 > Rust 86.3, [docs/decision-record.md](docs/decision-record.md)). Работает полный контур:
онлайн-агент / архивный импорт → ClickHouse (121.5 млн событий загружено) → 5 дашбордов Grafana.
Актуальные статусы и хвосты — в [ROADMAP.md](ROADMAP.md) (фаза 3: дисковый буфер, SCM-тест службы,
дашборд «Агент»; фаза 4: мульти-БД Store — PG/DuckDB/MSSQL; фаза 5: свой UI).

## Компоненты

| Путь | Что | Роль после bake-off |
|---|---|---|
| `agents/go/` | **Продуктовый агент** (Go, ch-go): batch+follow, rich-схема, служба Windows, /metrics | развивается (фаза 3+) |
| `core/` | C++ ядро: tj_core lib + tj-agent-cpp CLI + C ABI (FFI) | эталон формата + библиотека |
| `agents/rust/` | Rust-участник (std-only) | законсервирован (дифф-фаззинг) |
| `cpp_parse/` | Исходный нормализатор | замороженный golden-эталон; НЕ трогать |
| `deploy/` | docker-compose (CH+Grafana+Prometheus), схема БД, импортёр, дашборды | прод |
| `tests/` | golden (byte-exact) / tail (7 сценариев) / integration | гейты |
| `bench/` | скрипты серий + результаты всех замеров | история решений |

## Команды

```powershell
# Стек (CH http 8123 / native 9001 (!не 9000), Grafana 3000 admin/admin)
docker compose -f deploy\docker-compose.yml up -d

# Go-агент: сборка и гейты
cd agents\go; go build -trimpath -ldflags "-s -w" -o tj-agent-go.exe ./cmd/tj-agent-go
go vet ./...; go test ./...

# Golden-гейт (ОБЯЗАТЕЛЕН после любых правок парсеров; НИКОГДА -Regen без смены спеки)
powershell -File tests\golden\run_golden.ps1 -Agent <exe>   # ожидание: 18 PASS, 0 FAIL, 1 XFAIL(KI-1)

# Tail-серия (форграунд! фоновые задачи без сети к localhost)
powershell -File tests\tail\run_tail.ps1 -Agent <exe>       # 7/7 PASS

# Rust: cargo в %USERPROFILE%\.cargo\bin (НЕ в PATH); cargo build --release
# C++: cmake ТОЛЬКО генератор "Visual Studio 17 2022" (NMake ломается на кириллице пути);
#      cl-флаги с /DNOMINMAX обязательно; vcvars64 в стандартном месте BuildTools
```

## Жёсткие правила (нарушение = регрессия)

1. **Формат** = [docs/format-spec.md](docs/format-spec.md) (rev 3, единственный источник истины,
   реестр KI там же). Три реализации байт-эквивалентны; любое изменение формата — через спеку,
   перегенерацию goldens (`-Regen`) и синхронно во всех трёх парсерах.
2. **Данные**: `tj.events` — 121 485 342 продакшн-строки, НЕ вставлять/не трункейтить в тестах.
   Тесты — в `tj_bench.*` или персональных БД/таблицах (создать → проверить → DROP).
   Хэши: cityHash64 (go-faster/city == ClickHouse бит-в-бит) — менять нельзя, сломается
   непрерывность context_hash/sql_hash с загруженными данными.
3. **Follow-контракт**: чекпоинт персистится СРАЗУ после каждого ack вставки (периодический
   даёт до 25% дублей на kill -9); идентичность файла = volume serial + file index;
   размер живого файла — ТОЛЬКО через открытый хэндл (NTFS лениво обновляет метаданные
   каталога для удерживаемых писателем файлов — агенты слепнут); idle-close 2 с только для
   `\n`-терминированного хвоста, незавершённая строка не эмитится никогда.
4. **PowerShell 5.1**: все .ps1 репозитория — UTF-8 **с BOM** (иначе кириллица ломает парсер);
   `"${var}:"` а не `"$var:"`; вложенные скрипты звать in-process (`& $script`), не `powershell -File`
   (массивы аргументов расплющиваются); после Start-Process кэшировать `$p.Handle`, иначе
   ExitCode/ExitTime недоступны.
5. **Бенчи**: корпуса — hardlink-зеркала (`bench\scripts\make_corpus.ps1`), НЕ junction
   (обходчики пропускают reparse points); Defender сканирует свежесобранный exe на первом
   прогоне (без прав администратора исключения не поставить) — первый прогон новой сборки
   выбрасывать/прогревать; серии с ротацией порядка, медианы из 3, разброс ≤10%;
   на полном корпусе 175 ГБ скорости участников несравнимы без сброса page cache.
6. **ClickHouse**: `CLICKHOUSE_SKIP_USER_SETUP: 1` обязателен (иначе default заперт в
   контейнере); TTL считается от времени СОБЫТИЯ (короткий TTL молча удаляет исторический
   архив сразу после вставки — дефолт 3650 дней); `mem_limit: 6g` + `.wslconfig` (10 ГБ)
   против раздувания WSL-виртуалки page cache'ем.

## Тестовые данные

Референсный корпус: `E:\TJ_Logs\TJ_Logs` — 175 ГБ, 21 коллекция, 2025-11-28…30, ~121.5 млн
событий, 18 типов (инвентаризация: [docs/event-inventory.md](docs/event-inventory.md)).
Ловушки: TLOCK-записи до 3.4 МБ, длительности до 4.9 ч (64-бит), смешанные типы полей,
BOM в начале каждого файла, папки `*_86` частично пустые (BOM-файлы по 3 байта).
Генератор живого ТЖ: `tests\tail\generator.ps1` (держит файл открытым — как rphost).

## Карта документации

[docs/README.md](docs/README.md) — по ролям. Ключевое: [user-guide](docs/user-guide.md)
(эксплуатация), [format-spec](docs/format-spec.md) (формат), [storage-design](docs/storage-design.md)
(схема БД/дашборды/метрики/Store-интерфейс для фазы 4), [bakeoff-protocol](docs/bakeoff-protocol.md)
(методика замеров), [decision-record](docs/decision-record.md) (финал bake-off + долги),
`bench/results/*/README.md` (все серии с цифрами и находками).
