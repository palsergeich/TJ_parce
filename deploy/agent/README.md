# Агент-сборщик техжурнала (tj-agent-go): развёртывание

Онлайн-доставка событий ТЖ в ClickHouse: агент следит за каталогами логов
(`--follow` под капотом), нормализует события и вставляет их в продуктовую
`tj.events` (rich-схема) с чекпоинтами «только после подтверждённой вставки»
(ноль потерь, at-least-once на краше). Ставится на каждый сервер 1С.

Сборка (Go 1.22+): `cd agents\go && go build -trimpath -ldflags "-s -w" -o tj-agent-go.exe .\cmd\tj-agent-go`

## 1. Конфигурация

Эталонный пример с комментариями — [agents/go/tj-agent.example.yaml](../../agents/go/tj-agent.example.yaml).
Минимум для боевого сервера:

```yaml
inputs:
  - 'C:\1C_TJ\logs'                # каталог(и) техжурнала из logcfg.xml
sink: 'clickhouse://ch-host:9001/tj?schema=rich&table=events'
state_dir: 'C:\ProgramData\tj-agent\state'
metrics: '0.0.0.0:9101'            # /metrics для Prometheus (пусто = выключен)
log_level: info                    # error | info | debug
```

Правила:

- `schema=rich` обязателен для продуктовой `tj.events` (типизированные колонки
  + `props`; дашборды Grafana читают именно её).
- `state_dir` хранит `checkpoints.json` — прогресс, подтверждённый ClickHouse;
  удалить его = перечитать все логи заново (дубли не появятся только в пустой
  таблице).
- Явные CLI-флаги перекрывают файл: `tj-agent-go --config a.yaml --threads 8`.
- Незнакомый ключ в YAML — ошибка запуска (защита от опечаток), ошибки
  валидации указывают поле: `конфиг ...: поле 'poll_ms': 5 вне диапазона ...`.
- Проверка конфига и конвейера без установки службы:
  `tj-agent-go service run --config C:\tj-agent\tj-agent.yaml` в обычной
  консоли (остановка Ctrl+C — graceful: дренаж, финальный батч, чекпоинт).

## 2. Установка службой Windows

Из консоли **администратора**:

```powershell
# путь конфига фиксируется в команде запуска службы — используйте постоянный каталог
tj-agent-go.exe service install --config C:\tj-agent\tj-agent.yaml
tj-agent-go.exe service start

# остановка (graceful-дренаж: события дочитаны, батч подтверждён, чекпоинт записан)
tj-agent-go.exe service stop
tj-agent-go.exe service uninstall
```

- Имя службы по умолчанию `tj-agent`; несколько агентов на хосте — разные
  `--name` и конфиги: `service install --config b.yaml --name tj-agent-b`.
- Служба ставится с автозапуском (StartType=Automatic), команда запуска —
  `"<exe>" service run --config "<абсолютный путь>"`.
- Сигнал Stop/Shutdown от SCM равнозначен stop-file: тот же graceful-дренаж,
  поэтому `stop_file` в конфиге службы можно не задавать.
- Журнал агента: `log_file` из конфига; если пуст — служба пишет в
  `<state_dir>\tj-agent-go.log` (stderr службы уходит в никуда). Конфиг
  валидируется при install (битый конфиг не даст установить службу).
- Без прав администратора `service install/uninstall/start/stop` честно
  отвечают `Access is denied` с подсказкой — это штатно, поднимите консоль.

## 3. Метрики и Prometheus

Endpoint `/metrics` (текстовый формат Prometheus) включается полем
`metrics: '0.0.0.0:9101'`. Состав — docs/storage-design.md §5:

| Метрика | Тип | Лейблы | Смысл |
|---|---|---|---|
| `tj_agent_read_bytes_total` | counter | `collection` | прочитано сырого ТЖ |
| `tj_agent_events_total` | counter | `collection` | нормализовано событий |
| `tj_agent_parse_errors_total` | counter | `collection` | записей не прошло разбор |
| `tj_agent_lag_seconds` | gauge | `collection` | часы источника − max ts события (главный SLI) |
| `tj_agent_files_open` | gauge | | открытых хэндлов .log-хвостов |
| `tj_ingest_batches_total` | counter | `status=ok\|retried\|failed` | ok — с 1-й попытки, retried — после повторов, failed — каждая неудачная попытка |
| `tj_ingest_rows_total` | counter | | строк подтверждено сервером |
| `tj_ingest_queue_depth` | gauge | | слабов строк в очереди на вставку |
| `tj_ingest_insert_seconds` | histogram | | латентность попытки INSERT |

`collection` — первый сегмент относительного пути файла (совпадает с колонкой
`collection` в `tj.events`). Формат проверен `promtool check metrics`.

Скрейп из Docker-стека проекта: цель `host.docker.internal:9101` в
[deploy/prometheus/prometheus.yml](../prometheus/prometheus.yml) (job
`tj-agents`, раскомментируйте пример; резолв из контейнера tj-prometheus
проверен). Агент обязан слушать `0.0.0.0`, не `127.0.0.1`. Prometheus
перечитывает конфиг при перезапуске контейнера:
`docker compose -f deploy\docker-compose.yml restart prometheus`.

## 4. Алерты (storage-design §5)

Готовые правила для `deploy/prometheus/` (rule-файл подключается в
`prometheus.yml` через `rule_files:`):

```yaml
groups:
  - name: tj-agent
    rules:
      - alert: TjAgentLagHigh
        expr: tj_agent_lag_seconds > 300
        for: 5m
        annotations:
          summary: "Агент отстаёт от техжурнала > 5 минут ({{ $labels.collection }})"
      - alert: TjIngestFailing
        expr: rate(tj_ingest_batches_total{status="failed"}[5m]) > 0
        for: 2m
        annotations:
          summary: "Вставки в ClickHouse падают (агент повторяет с бэкоффом, чекпоинты стоят)"
      - alert: TjAgentDown
        expr: up{job="tj-agents"} == 0
        for: 2m
        annotations:
          summary: "Агент не отвечает на /metrics"
```

Примечания к поведению при недоступном ClickHouse: агент НЕ теряет данные —
батч повторяется с бэкоффом 1→30 с, чтение backpressure'ится, чекпоинты не
двигаются; в метриках это видно как рост `failed`-попыток и `lag_seconds`.

## 5. Как данные попадают на дашборды

Агент пишет в ту же `tj.events`, что и офлайн-импортёр фазы 1, — Grafana
(папка «ТехЖурнал», см. docs/user-guide.md) начинает показывать живые данные
сразу: материализованные представления `mv_agg_*` наполняют роллапы
`agg_minute`/`agg_context`/`agg_query`/`agg_locks` на вставке, drill-down
работает по сырым событиям. Отдельной настройки не требуется — выставьте на
дашборде time range «last 15 minutes» и убедитесь, что `tj_agent_lag_seconds`
единицы секунд.
