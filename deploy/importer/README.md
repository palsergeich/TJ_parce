# Импортёр NDJSON-архива в ClickHouse (Phase 1)

`import-jsonl.ps1` потоково заливает выход нормализатора ТЖ (NDJSON,
[docs/format-spec.md](../../docs/format-spec.md)) в таблицу `tj.events`
([deploy/clickhouse/init/001_schema.sql](../clickhouse/init/001_schema.sql))
через HTTP-интерфейс ClickHouse. Файл не парсится на стороне скрипта и не
грузится в память целиком: тело POST стримится (`StreamContent`), каждая
строка читается сервером как одна `String` (`FORMAT JSONAsString`), всё
разложение по колонкам выполняют выражения `JSONExtract*`/`Map` внутри
`INSERT ... SELECT ... FROM input()`. Ведущий UTF-8 BOM (легаси-выход,
KI-7 format-spec) срезается на лету.

Требования: Windows PowerShell 5.1+, доступный HTTP-порт ClickHouse.

## Запуск

```powershell
# один файл
.\import-jsonl.ps1 -Path C:\tj\out\CallsDiag_86.jsonl

# каталог (все *.jsonl, по алфавиту)
.\import-jsonl.ps1 -Path C:\tj\out

# нестандартный сервер/база; посмотреть SQL без загрузки
.\import-jsonl.ps1 -Path C:\tj\out -Url http://ch-host:8123 -Database tj
.\import-jsonl.ps1 -Path C:\tj\out -DryRun
```

Прогресс: `[N/всего] файл: строк за секунды (rows/sec)` + итог. Число строк
на файл считается дельтой `count()` по `events` (заголовок
`X-ClickHouse-Summary` завышен каскадом MV). Ошибка HTTP/ClickHouse →
текст ошибки сервера и `exit 1`.

## Маппинг (кратко)

Полная таблица выражений — в самом скрипте (`-DryRun` печатает итоговый SQL).

| Источник | Колонка | Примечание |
|---|---|---|
| `timestamp` | `ts` | `parseDateTime64BestEffort`; пустой/деградированный (`MM:SS.ssssss`) → эпоха `1970-01-01` |
| `duration` | `duration_us` | |
| `event`, `level` | `event`, `level` | `level` в источнике число или строка — приводится к строке |
| `file_path` | `collection`, `src_path` | `collection` = первый сегмент пути (`\` и `/`) |
| `filename` | `src_file` | `src_line` всегда 0 (см. ограничения) |
| `process`, `p:processName`, `OSThread` | `process`, `process_name`, `os_thread` | |
| `t:clientID` (fallback `ClientID`), `t:connectID`, `SessionID` | `client_id`, `connect_id`, `session_id` | `toUInt32OrZero`; нечисловой `SessionID` (`1,2`, `586(581)`) → 0, оригинал остаётся в `props` |
| `Usr`, `t:applicationName`, `t:computerName`, `AppID` | `usr`, `app_name`, `computer_name`, `app_id` | |
| `DBMS`, `DataBase`, `dbpid`, `Trans`, `Rows`, `RowsAffected` | `dbms`, `db_name`, `db_pid`, `trans`, `rows_ret`, `rows_affected` | |
| `CpuTime`, `Memory`, `MemoryPeak`, `InBytes`, `OutBytes`, `callWait` | `cpu_time_us`, `memory`, `memory_peak`, `in_bytes`, `out_bytes`, `call_wait_us` | |
| `IName`, `MName`, `Func`, `Module` | `iface_name`, `method_name`, `func_name`, `module` | |
| `Context` | `context`, `context_hash`, `context_line` | hash = `cityHash64`; line = последняя непустая строка |
| `Sql` \| `Query` \| `Sdbl` (первый непустой) | `sql_text`, `sql_hash` | hash сырого текста, см. ограничения |
| `planSQLText` | `plan_text` | |
| `Descr` \| `Txt` \| `txt` (первый непустой) | `descr` | |
| `Exception` | `exception` | |
| `Regions`, `WaitConnections`, `Locks`, `DeadlockConnectionIntersections` | `lock_regions`, `lock_wait_conns`, `locks_dump`, `deadlock_graph` | `WaitConnections`: список через запятую → `Array(UInt32)`, нет → `[]` |
| всё остальное | `props` | Map «как есть» (например, `Interface`, `Method`, `CallID`, `first`, `DstClientID`) |

## Известные ограничения

- **`sql_hash` — хэш ненормализованного текста**: литералы/параметры не
  свёрнуты, одинаковые запросы с разными константами дают разные хэши.
  TODO v1.1: хэш нормализованного текста считает нормализатор.
- **`src_line` всегда 0** — нормализатор пока не отдаёт номер строки/смещение
  события в исходном файле (появится в v1.1 как `src_offset`).
- **Метки времени — локальное время сервера-источника**: `timestamp` в NDJSON
  без таймзоны; импортёр пишет его в `DateTime64(..., 'UTC')` «как есть».
  При сравнении с другими источниками учитывайте смещение зоны источника.
- **TTL `tj.events` = 30 суток от времени события**: архив старше месяца
  будет удалён сразу после вставки (парты дропаются целиком). Перед импортом
  исторических коллекций расширьте TTL:
  `ALTER TABLE tj.events MODIFY TTL toDateTime(ts) + INTERVAL 3650 DAY DELETE`.
- Дублирующиеся ключи в одной записи (KI-4 format-spec): в колонки и `props`
  попадает первое значение, а не последнее.
- Файл импортируется одним `INSERT`: при ошибке посреди файла возможна
  частичная вставка без отката — перезапуск после `TRUNCATE`/удаления
  затронутых партиций.
