<#
.SYNOPSIS
    Импорт NDJSON-архива нормализатора ТЖ в ClickHouse (Phase 1).

.DESCRIPTION
    Потоково заливает *.jsonl (формат docs/format-spec.md) в tj.events через
    HTTP-интерфейс ClickHouse. Каждая строка файла читается сервером как одна
    String-колонка `json` (FORMAT JSONAsString), всё разложение по колонкам
    делает сам ClickHouse выражениями JSONExtract*/Map — скрипт файл не парсит
    и в память целиком не грузит (StreamContent), поэтому годится и для
    многогигабайтных файлов.

    Ведущий UTF-8 BOM (легаси-выход нормализатора, KI-7) срезается на лету.

.PARAMETER Path
    Файл *.jsonl или каталог с файлами *.jsonl (обязательный).

.PARAMETER Url
    Адрес HTTP-интерфейса ClickHouse. По умолчанию http://localhost:8123.

.PARAMETER Database
    Имя базы данных. По умолчанию tj.

.PARAMETER DryRun
    Напечатать итоговый INSERT-запрос и выйти, ничего не загружая.

.EXAMPLE
    .\import-jsonl.ps1 -Path C:\tj\out\CallsDiag_86.jsonl
    .\import-jsonl.ps1 -Path C:\tj\out -Url http://ch-host:8123 -Database tj
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Path,

    [string]$Url = 'http://localhost:8123',

    [string]$Database = 'tj',

    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# INSERT-запрос: одна строка NDJSON -> колонка json -> Map всех пар key/value
# (JSONExtractKeysAndValues конвертирует числа JSON в строки, null отбрасывает),
# из которого раскладываются горячие колонки; остаток уходит в props.
# Соответствие колонок свойствам ТЖ — по DDL deploy/clickhouse/init/001_schema.sql.
# ---------------------------------------------------------------------------
$insertSqlTemplate = @'
INSERT INTO {DB}.events
(
    ts, duration_us, event, level, collection, src_file, src_path, src_line,
    process, process_name, os_thread, client_id, connect_id, session_id, usr,
    app_name, computer_name, app_id,
    dbms, db_name, db_pid, trans, rows_ret, rows_affected,
    cpu_time_us, memory, memory_peak, in_bytes, out_bytes, call_wait_us,
    iface_name, method_name, func_name, module,
    context, context_hash, context_line,
    sql_text, sql_hash, plan_text, descr, exception,
    lock_regions, lock_wait_conns, locks_dump, deadlock_graph,
    props
)
WITH
    CAST(JSONExtractKeysAndValues(json, 'String'), 'Map(String, String)') AS m,
    m['Context'] AS v_context,
    -- первый непустой из Sql | Query | Sdbl
    if(m['Sql'] != '', m['Sql'], if(m['Query'] != '', m['Query'], m['Sdbl'])) AS v_sql,
    m['timestamp'] AS v_ts
SELECT
    -- timestamp без таймзоны (локальное время источника); деградированный
    -- 'MM:SS.ssssss' (файл с нецифровым префиксом имени) и пустой -> эпоха.
    -- parseDateTime64BestEffortOrNull деградированный НЕ отвергает (выдумывает
    -- дату), поэтому формат проверяется явно.
    if(match(v_ts, '^\\d{4}-\\d{2}-\\d{2}T'),
       coalesce(parseDateTime64BestEffortOrNull(v_ts, 6, 'UTC'), toDateTime64(0, 6, 'UTC')),
       toDateTime64(0, 6, 'UTC'))                                        AS ts,
    toUInt64OrZero(m['duration'])                                        AS duration_us,
    m['event']                                                           AS event,
    -- level в источнике бывает и числом, и строкой; в Map оба уже строки
    m['level']                                                           AS level,
    -- коллекция = первый сегмент file_path (разделители и '\', и '/')
    splitByChar('\\', replaceAll(m['file_path'], '/', '\\'))[1]          AS collection,
    m['filename']                                                        AS src_file,
    m['file_path']                                                       AS src_path,
    toUInt32(0)                                                          AS src_line,
    m['process']                                                         AS process,
    m['p:processName']                                                   AS process_name,
    toUInt32OrZero(m['OSThread'])                                        AS os_thread,
    toUInt32OrZero(if(m['t:clientID'] != '', m['t:clientID'], m['ClientID'])) AS client_id,
    toUInt32OrZero(m['t:connectID'])                                     AS connect_id,
    -- смешанный тип: списки/скобки ('1,2', '586(581)') дают 0 и остаются в props
    toUInt32OrZero(m['SessionID'])                                       AS session_id,
    m['Usr']                                                             AS usr,
    m['t:applicationName']                                               AS app_name,
    m['t:computerName']                                                  AS computer_name,
    m['AppID']                                                           AS app_id,
    m['DBMS']                                                            AS dbms,
    m['DataBase']                                                        AS db_name,
    toUInt32OrZero(m['dbpid'])                                           AS db_pid,
    toUInt8OrZero(m['Trans'])                                            AS trans,
    toUInt64OrZero(m['Rows'])                                            AS rows_ret,
    toUInt64OrZero(m['RowsAffected'])                                    AS rows_affected,
    toUInt64OrZero(m['CpuTime'])                                         AS cpu_time_us,
    toInt64OrZero(m['Memory'])                                           AS memory,
    toInt64OrZero(m['MemoryPeak'])                                       AS memory_peak,
    toUInt64OrZero(m['InBytes'])                                         AS in_bytes,
    toUInt64OrZero(m['OutBytes'])                                        AS out_bytes,
    toUInt64OrZero(m['callWait'])                                        AS call_wait_us,
    m['IName']                                                           AS iface_name,
    m['MName']                                                           AS method_name,
    m['Func']                                                            AS func_name,
    m['Module']                                                          AS module,
    v_context                                                            AS context,
    if(v_context = '', 0, cityHash64(v_context))                         AS context_hash,
    -- последняя непустая строка контекста (CR выбрасываем, режем по LF)
    arrayLast(x -> x != '', splitByChar('\n', replaceAll(v_context, '\r', ''))) AS context_line,
    v_sql                                                                AS sql_text,
    -- TODO v1.1: хэш НОРМАЛИЗОВАННОГО текста запроса считает нормализатор;
    -- пока хэшируется сырой текст (литералы/параметры не свёрнуты).
    if(v_sql = '', 0, cityHash64(v_sql))                                 AS sql_hash,
    m['planSQLText']                                                     AS plan_text,
    -- первый непустой из Descr | Txt | txt
    if(m['Descr'] != '', m['Descr'], if(m['Txt'] != '', m['Txt'], m['txt'])) AS descr,
    m['Exception']                                                       AS exception,
    m['Regions']                                                         AS lock_regions,
    -- WaitConnections: '7,9,11' | одиночное число -> массив; нет -> []
    if(m['WaitConnections'] = '',
       CAST([] AS Array(UInt32)),
       arrayMap(x -> toUInt32OrZero(trimBoth(x)), splitByChar(',', m['WaitConnections']))) AS lock_wait_conns,
    m['Locks']                                                           AS locks_dump,
    m['DeadlockConnectionIntersections']                                 AS deadlock_graph,
    -- Хвост props: всё, что НЕ разложено по горячим колонкам.
    -- Исключение: SessionID, не распарсившийся в число (session_id=0),
    -- сохраняется в props, чтобы значение не потерялось.
    mapFilter((k, v) ->
        (k NOT IN (
            'timestamp','duration','event','level','filename','file_path',
            'process','p:processName','OSThread','t:clientID','ClientID','t:connectID',
            'SessionID','Usr','t:applicationName','t:computerName','AppID',
            'DBMS','DataBase','dbpid','Trans','Rows','RowsAffected',
            'CpuTime','Memory','MemoryPeak','InBytes','OutBytes','callWait',
            'IName','MName','Func','Module',
            'Context','Sql','Query','Sdbl','planSQLText','Descr','Txt','txt','Exception',
            'Regions','WaitConnections','Locks','DeadlockConnectionIntersections'
        ))
        OR (k = 'SessionID' AND toUInt32OrZero(v) = 0 AND v != '' AND v != '0'),
        m)                                                               AS props
FROM input('json String')
FORMAT JSONAsString
'@

$insertSql = $insertSqlTemplate.Replace('{DB}', $Database)

if ($DryRun) {
    Write-Output $insertSql
    exit 0
}

# ---------------------------------------------------------------------------
# Сбор списка файлов
# ---------------------------------------------------------------------------
if (Test-Path -LiteralPath $Path -PathType Container) {
    $files = @(Get-ChildItem -LiteralPath $Path -Filter '*.jsonl' -File | Sort-Object Name)
} elseif (Test-Path -LiteralPath $Path -PathType Leaf) {
    $files = @(Get-Item -LiteralPath $Path)
} else {
    Write-Error "Path not found: $Path"
    exit 1
}
if ($files.Count -eq 0) {
    Write-Error "No *.jsonl files found in: $Path"
    exit 1
}

# ---------------------------------------------------------------------------
# HTTP-клиент: запрос уходит в query-string URL, тело POST — сырые байты NDJSON
# ---------------------------------------------------------------------------
Add-Type -AssemblyName System.Net.Http

$client = New-Object System.Net.Http.HttpClient
$client.Timeout = [TimeSpan]::FromSeconds(3700)   # чуть больше max_execution_time

# Скалярный запрос к ClickHouse (для count()); возвращает строку ответа.
function Invoke-ChScalar {
    param([string]$Query)
    $content = New-Object System.Net.Http.StringContent($Query)
    $resp = $client.PostAsync($Url.TrimEnd('/') + '/', $content).GetAwaiter().GetResult()
    try {
        $body = $resp.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        if (-not $resp.IsSuccessStatusCode) {
            Write-Error ("ClickHouse HTTP {0} on query '{1}':`n{2}" -f
                [int]$resp.StatusCode, $Query, $body) -ErrorAction Continue
            exit 1
        }
        return $body.Trim()
    }
    finally { $resp.Dispose() }
}

$fullUrl = $Url.TrimEnd('/') + '/?query=' + [uri]::EscapeDataString($insertSql) +
           '&max_execution_time=3600' +
           '&date_time_input_format=best_effort'

$totalRows  = [long]0
$totalBytes = [long]0
$done       = 0
$sw         = [System.Diagnostics.Stopwatch]::StartNew()

# Число вставленных строк считаем дельтой count() по events: заголовок
# X-ClickHouse-Summary (read_rows/written_rows) завышен каскадом MV
# (по одному проходу на каждое материализованное представление).
$countQuery = 'SELECT count() FROM {DB}.events'.Replace('{DB}', $Database)
$prevCount  = [long](Invoke-ChScalar $countQuery)

foreach ($f in $files) {
    $fileSw = [System.Diagnostics.Stopwatch]::StartNew()
    $fs = $null
    $resp = $null
    try {
        # Открываем поток и срезаем ведущий UTF-8 BOM (EF BB BF), если есть:
        # заглядываем в первые 3 байта, при отсутствии BOM возвращаемся на 0.
        $fs = New-Object System.IO.FileStream(
            $f.FullName,
            [System.IO.FileMode]::Open,
            [System.IO.FileAccess]::Read,
            [System.IO.FileShare]::Read,
            1048576)
        $bom = New-Object byte[] 3
        $n = $fs.Read($bom, 0, 3)
        if (-not ($n -eq 3 -and $bom[0] -eq 0xEF -and $bom[1] -eq 0xBB -and $bom[2] -eq 0xBF)) {
            $fs.Position = 0
        }

        $content = New-Object System.Net.Http.StreamContent($fs, 1048576)
        $content.Headers.ContentType =
            New-Object System.Net.Http.Headers.MediaTypeHeaderValue('application/octet-stream')

        $resp = $client.PostAsync($fullUrl, $content).GetAwaiter().GetResult()
        $body = $resp.Content.ReadAsStringAsync().GetAwaiter().GetResult()

        if (-not $resp.IsSuccessStatusCode) {
            Write-Error ("ClickHouse HTTP {0} on file '{1}':`n{2}" -f
                [int]$resp.StatusCode, $f.FullName, $body) -ErrorAction Continue
            exit 1
        }

        # Точное число вставленных событий = дельта count() по events
        # (count() на MergeTree читается из метаданных, это дёшево).
        $newCount = [long](Invoke-ChScalar $countQuery)
        $fileRows = $newCount - $prevCount
        $prevCount = $newCount

        $fileSw.Stop()
        $done++
        $totalRows += $fileRows
        $totalBytes += $f.Length

        $rate = if ($fileSw.Elapsed.TotalSeconds -gt 0 -and $fileRows -gt 0) {
            [math]::Round($fileRows / $fileSw.Elapsed.TotalSeconds)
        } else { 0 }
        Write-Output ("[{0}/{1}] {2}: {3} rows in {4:n1}s ({5} rows/sec)" -f
            $done, $files.Count, $f.Name, $fileRows,
            $fileSw.Elapsed.TotalSeconds, $rate)
    }
    finally {
        if ($null -ne $resp) { $resp.Dispose() }
        if ($null -ne $fs)   { $fs.Dispose() }
    }
}

$sw.Stop()
$client.Dispose()

$totalRate = if ($sw.Elapsed.TotalSeconds -gt 0) {
    [math]::Round($totalRows / $sw.Elapsed.TotalSeconds)
} else { 0 }
Write-Output ("Done: {0} file(s), {1} rows, {2:n1} MB in {3:n1}s ({4} rows/sec)" -f
    $done, $totalRows, ($totalBytes / 1MB), $sw.Elapsed.TotalSeconds, $totalRate)
exit 0
