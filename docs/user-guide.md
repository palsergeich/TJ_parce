# Руководство пользователя

Как развернуть рабочее место, загрузить техжурнал и получать ответы на вопросы о производительности.

## 1. Требования

| Компонент | Зачем | Обязательно |
|---|---|---|
| Windows 10/11 + PowerShell 5.1+ | скрипты импорта/тестов | да |
| Docker Desktop (WSL2) | ClickHouse + Grafana + Prometheus | да |
| Готовый `cpp_parse\build\count_contexts.exe` | нормализация ТЖ | да (лежит в репо после сборки) |
| VS Build Tools 2022 (workload C++) | пересборка ядра | только для разработки |
| Go 1.22+ | Go-агент | только для bake-off |
| Python 3.x | валидация JSON в тестах | желательно |

Память: виртуалка WSL2 по умолчанию забирает 50% RAM под page cache при массовой заливке. Рекомендуется `C:\Users\<вы>\.wslconfig`:

```ini
[wsl2]
memory=10GB
autoMemoryReclaim=gradual
```

(вступает в силу после `wsl --shutdown`; контейнеры перезапустятся).

## 2. Развёртывание стека

```powershell
docker compose -f deploy\docker-compose.yml up -d
```

Поднимаются:

| Сервис | Адрес | Примечание |
|---|---|---|
| ClickHouse 24.8 | HTTP `localhost:8123`, native `localhost:9001` | native на **9001**, т.к. 9000 часто занят. Схема БД (`tj.events` + 4 MV) применяется автоматически при первом старте из `deploy/clickhouse/init/` |
| Grafana 11.1 | `localhost:3000` (admin/admin) | датасорс ClickHouse и дашборды подключаются автоматически (provisioning) |
| Prometheus | `localhost:9090` | скрейп метрик агентов (появятся в фазе 3) |

Проверка: `docker exec tj-clickhouse clickhouse-client --query "SHOW TABLES FROM tj"` — должны быть `events`, `agg_minute`, `agg_context`, `agg_query`, `agg_locks` и 4 `mv_*`.

⚠️ **Грабли, уже собранные за вас** (зашиты в compose/схему, не отключайте):
- `CLICKHOUSE_SKIP_USER_SETUP: 1` — без него пользователь `default` заперт внутри контейнера, и Grafana/импортёр получают `AUTHENTICATION_FAILED`.
- TTL таблицы `tj.events` = 3650 дней. TTL считается **от времени события**: с «обычным» TTL 30 дней импортированный исторический архив удаляется сразу после вставки. Для онлайн-режима сокращайте осознанно: `ALTER TABLE tj.events MODIFY TTL toDateTime(ts) + INTERVAL 30 DAY DELETE`.
- `mem_limit: 6g` у ClickHouse — иначе при заливке он раздувает WSL-виртуалку.

## 3. Нормализация ТЖ

```powershell
cpp_parse\build\count_contexts.exe <каталог_ТЖ> [потоки] [выход.jsonl] [--no-output]
```

- Каталог сканируется рекурсивно, берутся `*.log` ≥ 100 байт; ожидается штатная структура ТЖ `<каталог>\<процесс>_<pid>\YYMMDDHH.log` (дата и час берутся из имени файла).
- `потоки` — суммарный бюджет (читатели+разборщики), 1..1024. Для воспроизводимого порядка событий — `1`.
- Выход — NDJSON: `{"timestamp":"2025-11-30T21:00:53.520012","duration":375000,"event":"CALL","level":1,...все свойства}`. Полный контракт: [format-spec.md](format-spec.md).
- Exit-коды: `0` — успех, `1` — ошибка аргументов/записи, `2` — часть файлов не прочиталась.
- `--no-output` — только парсинг, замер скорости (нужен и 3-й аргумент).

Скорость: сотни МБ/с — ГБ/с на NVMe; 175 ГБ корпус нормализуется за минуты.

## 4. Импорт в ClickHouse

Один файл или каталог с `*.jsonl`:

```powershell
powershell -File deploy\importer\import-jsonl.ps1 -Path events.jsonl
# параметры: -Url http://localhost:8123  -Database tj  -DryRun
```

Импортёр потоково гонит NDJSON в ClickHouse; весь маппинг «поле ТЖ → колонка» выполняется на стороне БД (`JSONExtract`), горячие поля — в типизированные колонки, остальное — в `props Map(String,String)`. Скорость: 100–180 тыс. строк/с. Подробности маппинга: [deploy/importer/README.md](../deploy/importer/README.md).

**Большой архив** грузите по коллекциям, удаляя промежуточные NDJSON (полный корпус 175 ГБ → ~200 ГБ временных файлов, если делать разом):

```powershell
foreach ($c in Get-ChildItem D:\TJ_Logs -Directory) {
    cpp_parse\build\count_contexts.exe $c.FullName 16 "$env:TEMP\$($c.Name).jsonl"
    powershell -File deploy\importer\import-jsonl.ps1 -Path "$env:TEMP\$($c.Name).jsonl"
    Remove-Item "$env:TEMP\$($c.Name).jsonl"
}
```

Проверка после импорта:

```sql
SELECT count(), formatReadableSize(sum(bytes_on_disk))
FROM system.parts WHERE database='tj' AND table='events' AND active;
SELECT event, count() FROM tj.events GROUP BY event ORDER BY 2 DESC;
```

## 5. Дашборды

Grafana → папка «ТехЖурнал». Стартовый период дашбордов выставлен на демо-архив (28.11–01.12.2025) — под свои данные меняйте time range. Время отображается «как в логах» (таймзона UTC = локальное время сервера-источника).

| Дашборд | Что смотреть |
|---|---|
| **Производительность: обзор** | Первый экран: занятость кластера (среднее число занятых потоков = сумма времени CALL в минуту / 60 с), доля СУБД, p50/p95/p99 отклика, всплески ожиданий на блокировках, топ контекстов/пользователей по времени |
| **Серверные вызовы (CALL)** | Разбор «у пользователей тормозит»: перцентили отклика, топ методов (`MName`) и пользователей по суммарному времени, гистограмма длительностей, пиковая память по процессам, худшие вызовы списком |
| **СУБД (DBMSSQL)** | Разбор «база тупит»: топ запросов по суммарному времени с текстом, **топ строк кода 1С по порождаемому времени СУБД** (`context_line`), долгие запросы, QERR, профиль по базам |
| **Блокировки** | TLOCK/TTIMEOUT/TDEADLOCK: таймлайн ожиданий, жертвы, области, места в коде; в таблице таймаутов — `connect_id` виновников для drill-down |
| **Ошибки** | Тренды и топы QERR/EXCP, последние ошибки с контекстом |

Фильтры сверху: информационная база (`process_name`), пользователь (`usr`), где уместно — база СУБД.

## 6. SQL-рецепты (drill-down)

Подключение: `docker exec -it tj-clickhouse clickhouse-client` или любой клиент на `localhost:8123`/`9001`.

**От агрегата к сырым событиям контекста:**

```sql
SELECT ts, usr, session_id, duration_us/1e6 AS sec, context
FROM tj.events
WHERE context_hash = cityHash64('<полный текст контекста>')  -- или известный hash из agg_context
ORDER BY duration_us DESC LIMIT 20;
```

**Вся активность сеанса вокруг проблемного момента:**

```sql
SELECT ts, event, duration_us/1e6 AS sec, method_name, left(sql_text,120) AS sql, context_line
FROM tj.events
WHERE session_id = 1583 AND ts BETWEEN '2025-11-30 16:00:00' AND '2025-11-30 16:10:00'
ORDER BY ts;
```

**Кто держал блокировку (по connect_id виновника из TTIMEOUT):**

```sql
SELECT ts, event, usr, duration_us/1e6 AS sec, method_name, context_line
FROM tj.events
WHERE connect_id = 10511
  AND ts BETWEEN toDateTime('2025-11-30 12:00:00') - INTERVAL 60 SECOND
             AND toDateTime('2025-11-30 12:00:00') + INTERVAL 60 SECOND
ORDER BY ts;
```

**Сравнение периодов (вчера vs сегодня) по контекстам:**

```sql
SELECT a.context_line,
       round(a.dur/1e6,1) AS today_s, round(b.dur/1e6,1) AS yesterday_s,
       round(100*(a.dur - b.dur)/greatest(b.dur,1), 0) AS diff_pct
FROM (SELECT context_hash, anyLast(context_line) context_line, sum(dur_sum) dur
      FROM tj.agg_context WHERE period >= '2025-11-30' GROUP BY context_hash) a
FULL JOIN (SELECT context_hash, sum(dur_sum) dur
      FROM tj.agg_context WHERE period >= '2025-11-29' AND period < '2025-11-30'
      GROUP BY context_hash) b USING context_hash
ORDER BY a.dur DESC LIMIT 30;
```

**Свойства из «длинного хвоста» (всё, что не легло в колонки):**

```sql
SELECT props['Interface'] AS iface, count()
FROM tj.events WHERE event = 'CALL' GROUP BY iface ORDER BY 2 DESC LIMIT 10;
```

## 7. Тесты

```powershell
# Собрать входы кейсов (однократно) и прогнать byte-exact сравнение
powershell -File tests\golden\make_cases.ps1
powershell -File tests\golden\run_golden.ps1                     # эталонное C++ ядро
powershell -File tests\golden\run_golden.ps1 -Agent agents\go\tj-agent-go.exe
# После осознанного изменения формата: перегенерация эталонов
powershell -File tests\golden\run_golden.ps1 -Regen
```

19 кейсов: 12 синтетических краевых (BOM, кавычки, типизация чисел, границы размера...) + 7 из реальных логов. Кейс с маркером `XFAIL` — известное расхождение (реестр KI в [format-spec.md](format-spec.md)).

## 8. Пересборка ядра

```cmd
"C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\VC\Auxiliary\Build\vcvars64.bat"
cd cpp_parse\build
cl.exe /O2 /GL /Oi /Ot /arch:AVX2 /fp:fast /std:c++17 /EHsc /D_CRT_SECURE_NO_WARNINGS /DNOMINMAX ^
   /Fecount_contexts.exe ..\count_contexts.cpp /link /LTCG /OPT:REF /OPT:ICF
```

`/DNOMINMAX` обязателен (windows.h ломает `std::min/max`). После пересборки — обязательно `run_golden.ps1`.

## 9. Обслуживание

- **Ретенция**: по умолчанию сырьё живёт 3650 дней, агрегаты 400. Онлайн-режим: `ALTER TABLE tj.events MODIFY TTL ...` (см. §2). Дисковый бюджет: ~1 ГБ на ~20 млн событий.
- **Полная очистка данных**: `TRUNCATE TABLE tj.events` + `TRUNCATE` всех `tj.agg_*` (агрегаты сами не очищаются!).
- **Снести и пересоздать всё**: `docker compose down -v` (удалит и данные), затем `up -d` — схема применится заново.
- **Память хоста**: см. §1 про `.wslconfig`; разово кэш виртуалки сбрасывается `wsl -d docker-desktop sh -c "sync && echo 3 > /proc/sys/vm/drop_caches"`.
- **Кодировки**: PowerShell-скрипты репозитория — UTF-8 **с BOM** (PS 5.1 иначе читает их как ANSI); NDJSON и логи — UTF-8 без BOM.

## 10. Известные ограничения

- `timestamp` — локальное время сервера-источника без таймзоны (мульти-TZ кластеры — фаза 3).
- `sql_hash` пока считается от сырого текста запроса (нормализация параметров/врем. таблиц — v1.1).
- Порядок записей между файлами не гарантируется; внутри файла — гарантируется Go-агентом всегда, эталонным exe только при `потоки=1`.
- Онлайн-режим (слежение за растущими логами) — фаза 3; сейчас только разовый импорт.
- Полный реестр расхождений формата: [format-spec.md](format-spec.md) §7.
