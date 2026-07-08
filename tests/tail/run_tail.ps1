# Оркестратор tail-тестов bake-off (bakeoff-protocol.md §4.5) против живого ClickHouse.
#
#   .\run_tail.ps1 -Agent <exe с режимом --follow> [-Dsn clickhouse] [-KeepData] [-Quick] [-Only S1,S4]
#
# Контракт агента (одинаков для go/rust/cpp):
#   <exe> --follow --input <dir> --sink clickhouse[:<dsn>] --state <dir> --stop-file <path>
#         [--poll-ms 500] [--idle-close-ms 2000] [--threads N] [--stats-json <path>]
# Поведение: первичный скан существующих файлов (с учётом чекпоинтов) → tail; подхват
# нового файла < 2 с; открытие файлов с полным share; событие закрывается следующей
# строкой-маской ИЛИ idle >= idle-close-ms (только если хвост оканчивается \n) ИЛИ
# дренажом на stop; незавершённая СТРОКА (без \n) не эмитится никогда; чекпоинты на файл
# (identity = volume serial + file index) двигаются только после ack вставки в CH
# (min-contiguous), атомарно в --state; усечение (size < offset) или смена identity →
# рестарт с 0; at-least-once после crash допустим (ограниченные дубли), потери — нет;
# по появлению --stop-file — дренаж и exit 0; порог MIN_FILE_SIZE=100 перепроверяется
# по мере роста файла.
#
# Каждый сценарий: TRUNCATE tj_bench.events + свежие каталоги логов/состояния в %TEMP%.
# Харнесс ходит в CH по HTTP (localhost:8123), агент — по своему DSN (native 9001).
#
#   S1 потери/дубли   30 с @ 10000 соб/с, стоп генератора, 10 с дренаж, stop-file.
#   S2 недописанное   хвост без \n не эмитится (>= 5 с), после дописи — ровно 2 записи,
#                     значение свойства склеено целиком.
#   S3 ротация        -RotateAfterSec 10, 25 с: все файлы в CH, потерь счётчиков нет.
#   S4 усечение       1000 событий → truncate до 0 живьём → 500 новых с другим маркером.
#   S5 crash-recovery kill -Force на 10-й секунде, рестарт с тем же --state: потерь 0,
#                     дубли отчётно (at-least-once допустим).
#   S6 латентность    30 с @ 20000 соб/с, p95(now - max(GenMs)) < 5000 мс.
#   S7 sharing        неявно в S1: генератор держит файл открытым весь прогон,
#                     stderr агента в S1 пуст.
#
# -Quick — укороченный прогон для отладки самого харнесса (НЕ зачётный).
# Выход: таблица PASS/FAIL + детали, JSON-отчёт (-OutJson, по умолчанию
# %TEMP%\last_run_<имя агента>.json), exit 1 при любом FAIL, exit 2 — среда не готова.

param(
    [Parameter(Mandatory = $true)][string]$Agent,
    [string]$Dsn = 'clickhouse',
    [switch]$KeepData,
    [switch]$Quick,
    [string[]]$Only = @(),
    [string]$OutJson = '',
    [string]$ChHttp = 'http://localhost:8123/',
    [int]$AgentThreads = 0
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

# ------------------------------------------------------------------ конфигурация ---
$script:AgentExe  = (Resolve-Path $Agent).Path
$script:AgentName = [System.IO.Path]::GetFileNameWithoutExtension($script:AgentExe)
$script:SinkSpec  = $Dsn
$script:Table     = 'tj_bench.events'
$script:ChUrl     = $ChHttp
$script:RunStamp  = Get-Date -Format 'yyyyMMdd_HHmmss'
$script:Generator = Join-Path $PSScriptRoot 'generator.ps1'
if (-not (Test-Path $script:Generator)) { throw "не найден генератор: ${script:Generator}" }
if ($OutJson -eq '') { $OutJson = Join-Path $env:TEMP ("last_run_" + $script:AgentName + ".json") }

# Длительности/темпы: зачётные по умолчанию, -Quick — смоук самого харнесса.
if ($Quick) {
    $script:T = @{
        S1Rate = 3000;  S1Dur = 6;  S1Drain = 4
        S3Rate = 3000;  S3Dur = 10; S3Rotate = 4
        S4Seed = 1000;  S4New = 500
        S5Rate = 3000;  S5Dur = 10; S5KillAt = 4
        S6Rate = 5000;  S6Dur = 8;  S6PollSec = 1
        WaitShort = 20; WaitDrain = 45; StopSec = 15
    }
} else {
    $script:T = @{
        S1Rate = 10000; S1Dur = 30; S1Drain = 10
        S3Rate = 5000;  S3Dur = 25; S3Rotate = 10
        S4Seed = 1000;  S4New = 500
        S5Rate = 5000;  S5Dur = 30; S5KillAt = 10
        S6Rate = 20000; S6Dur = 30; S6PollSec = 2
        WaitShort = 60; WaitDrain = 180; StopSec = 30
    }
}

# --------------------------------------------------------------------- ClickHouse ---
function Invoke-CH([string]$Query) {
    $wc = New-Object System.Net.WebClient
    $wc.Encoding = [System.Text.Encoding]::UTF8
    try {
        return $wc.UploadString($script:ChUrl, $Query).TrimEnd("`r", "`n")
    } catch {
        throw ("ClickHouse HTTP недоступен или запрос отвергнут: {0}; запрос: {1}" -f $_.Exception.Message, $Query)
    } finally { $wc.Dispose() }
}

function Get-CHLong([string]$Query) {
    $r = Invoke-CH $Query
    if ($r -eq '') { return 0 }
    return [long]$r
}

function Reset-CH {
    Invoke-CH ("TRUNCATE TABLE " + $script:Table) | Out-Null
    $n = Get-CHLong ("SELECT count() FROM " + $script:Table)
    if ($n -ne 0) { throw "TRUNCATE не обнулил ${script:Table}: count()=$n" }
}

# count() и uniqExact(props['Usr']) по маркерному префиксу одной пробой.
function Get-MarkerStats([string]$Prefix) {
    $q = "SELECT count(), uniqExact(props['Usr']) FROM ${script:Table} " +
         "WHERE startsWith(props['Usr'], '$Prefix') FORMAT TabSeparated"
    $parts = (Invoke-CH $q) -split "`t"
    return [pscustomobject]@{ cnt = [long]$parts[0]; uniq = [long]$parts[1] }
}

function Get-UnixMsNow { return [System.DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() }

# ------------------------------------------------------------- каталоги и агент ---
function New-Work([string]$Sid) {
    $root = Join-Path $env:TEMP ("tj_tail_" + $script:RunStamp + "_" + $Sid)
    if (Test-Path $root) { Remove-Item $root -Recurse -Force }
    $w = [pscustomobject]@{
        Root      = $root
        LogDir    = Join-Path $root 'live'
        StateDir  = Join-Path $root 'state'
        StopFile  = Join-Path $root 'stop.flag'
        StatsJson = Join-Path $root 'stats.json'
        OutLog    = Join-Path $root 'agent.out.log'
        ErrLog    = Join-Path $root 'agent.err.log'
    }
    New-Item -ItemType Directory -Force -Path $w.LogDir, $w.StateDir | Out-Null
    return $w
}

function Start-AgentFollow($Work, [string]$OutSuffix = '') {
    $outLog = $Work.OutLog; $errLog = $Work.ErrLog
    if ($OutSuffix -ne '') {
        $outLog = $Work.OutLog -replace '\.log$', ".$OutSuffix.log"
        $errLog = $Work.ErrLog -replace '\.log$', ".$OutSuffix.log"
    }
    $agentArgs = @('--follow', '--input', $Work.LogDir, '--sink', $script:SinkSpec,
                   '--state', $Work.StateDir, '--stop-file', $Work.StopFile,
                   '--poll-ms', '500', '--idle-close-ms', '2000',
                   '--stats-json', $Work.StatsJson)
    if ($AgentThreads -gt 0) { $agentArgs += @('--threads', "$AgentThreads") }
    $agentArgs = $agentArgs | ForEach-Object { if ($_ -match '\s') { '"' + $_ + '"' } else { $_ } }
    $p = Start-Process -FilePath $script:AgentExe -ArgumentList $agentArgs -PassThru `
                       -NoNewWindow -RedirectStandardOutput $outLog -RedirectStandardError $errLog
    $null = $p.Handle   # кэш хэндла: иначе PS 5.1 теряет ExitCode после выхода процесса
    return $p
}

# Грациозная остановка: stop-file → ожидание выхода → (при таймауте) kill.
function Stop-AgentGraceful($Work, $Proc, [int]$TimeoutSec = 0) {
    if ($TimeoutSec -le 0) { $TimeoutSec = $script:T.StopSec }
    if ($null -eq $Proc) { return [pscustomobject]@{ graceful = $false; exit = $null; msg = 'агент не запускался' } }
    New-Item -ItemType File -Path $Work.StopFile -Force | Out-Null
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while (-not $Proc.HasExited -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 200 }
    if (-not $Proc.HasExited) {
        try { Stop-Process -Id $Proc.Id -Force -Confirm:$false -ErrorAction Stop } catch {}
        return [pscustomobject]@{ graceful = $false; exit = $null
                                  msg = "агент не вышел за $TimeoutSec с после появления stop-file — убит принудительно" }
    }
    $code = $null
    try { $code = $Proc.ExitCode } catch {}
    return [pscustomobject]@{ graceful = $true; exit = $code; msg = '' }
}

function Wait-Until([scriptblock]$Condition, [int]$TimeoutSec, [int]$IntervalMs = 500) {
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        try { if (& $Condition) { return $true } } catch {}
        Start-Sleep -Milliseconds $IntervalMs
    }
    try { return [bool](& $Condition) } catch { return $false }
}

# ------------------------------------------------------------------- отчётность ---
function New-Check([string]$Name, [bool]$Pass, [string]$Detail) {
    $mark = 'FAIL'; if ($Pass) { $mark = 'ok' }
    Write-Host ("    [{0}] {1}: {2}" -f $mark, $Name, $Detail) -ForegroundColor $(if ($Pass) { 'DarkGray' } else { 'Red' })
    return [pscustomobject]@{ check = $Name; pass = $Pass; detail = $Detail }
}

function Read-AgentStderr($Work) {
    $lines = @()
    foreach ($f in (Get-ChildItem $Work.Root -Filter 'agent.err*.log' -ErrorAction SilentlyContinue)) {
        $lines += @(Get-Content $f.FullName -ErrorAction SilentlyContinue)
    }
    return @($lines | Where-Object { $_ -ne '' })
}

# Финализация сценария: остановка агента (если жив), уборка каталогов.
function Complete-Scenario($Work, $Proc, [System.Collections.ArrayList]$Checks, [bool]$ExpectGraceful = $true) {
    $st = Stop-AgentGraceful $Work $Proc
    if ($ExpectGraceful) {
        [void]$Checks.Add((New-Check 'выход по stop-file' ($st.graceful -and $st.exit -eq 0) `
            ("graceful=$($st.graceful) exit=$($st.exit) $($st.msg)")))
    }
    $err = Read-AgentStderr $Work
    if ($err.Count -gt 0) {
        Write-Host ("    stderr агента ({0} строк): {1}" -f $err.Count, (($err | Select-Object -First 3) -join ' | ')) -ForegroundColor DarkYellow
    }
    return $st
}

function Remove-WorkDir($Work, [bool]$ScenarioPass) {
    if ($KeepData -or -not $ScenarioPass) {
        Write-Host ("    данные сохранены: {0}" -f $Work.Root) -ForegroundColor DarkGray
    } else {
        try { Remove-Item $Work.Root -Recurse -Force -Confirm:$false -ErrorAction Stop } catch {}
    }
}

# Ручная сборка события ТЖ (для S2): маска ММ:СС.мммммм-Дл, + свойства.
function Get-TJEventLine([string]$Mmss, [long]$Dur, [string]$Props) {
    return ('{0}-{1},CALL,1,{2}' -f $Mmss, $Dur, $Props)
}

function Get-CurrentHourLogName { return (Get-Date).ToString('yyMMddHH') + '.log' }

function New-ScenarioResult([string]$Sid, [string]$Title, $Checks, $Notes, $Sw) {
    $pass = (@($Checks | Where-Object { -not $_.pass }).Count -eq 0) -and (@($Checks).Count -gt 0)
    return [pscustomobject]@{
        id = $Sid; title = $Title; pass = $pass
        duration_s = [math]::Round($Sw.Elapsed.TotalSeconds, 1)
        checks = @($Checks); notes = @($Notes)
    }
}

function Get-P95([double[]]$Xs) {
    $s = @($Xs | Sort-Object)
    if ($s.Count -eq 0) { return [double]::NaN }
    $idx = [int][math]::Ceiling(0.95 * $s.Count) - 1
    if ($idx -lt 0) { $idx = 0 }
    return [double]$s[$idx]
}

# Запуск генератора в фоне (S5/S6): отдельный процесс через Start-Job,
# в -ArgumentList только скаляры (правило репо: массивы через powershell -File не гонять).
function Start-GeneratorJob([string]$LogDir, [int]$Rate, [int]$DurationSec) {
    return Start-Job -ScriptBlock {
        param($Gen, $Dir, $Rate, $Sec)
        & $Gen -Dir $Dir -Rate $Rate -DurationSec $Sec
    } -ArgumentList $script:Generator, $LogDir, $Rate, $DurationSec
}

function Receive-GeneratorJob($Job, [int]$TimeoutSec) {
    $done = Wait-Job -Job $Job -Timeout $TimeoutSec
    if ($null -eq $done) {
        Stop-Job $Job; Remove-Job $Job -Force
        return $null
    }
    $out = @(Receive-Job $Job -ErrorAction SilentlyContinue)
    Remove-Job $Job -Force
    $jsonLine = $out | Where-Object { "$_" -match '^\{' } | Select-Object -Last 1
    if ($null -eq $jsonLine) { return $null }
    return ("$jsonLine" | ConvertFrom-Json)
}

# ---------------------------------------------------------------------------- S1 ---
function Invoke-S1 {
    $sid = 'S1'; $title = 'нет потерь и дублей под нагрузкой'
    Write-Host ("`n=== {0}: {1} ({2} c @ {3} соб/с) ===" -f $sid, $title, $script:T.S1Dur, $script:T.S1Rate) -ForegroundColor Cyan
    $checks = New-Object System.Collections.ArrayList
    $notes  = New-Object System.Collections.ArrayList
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $w = New-Work $sid; $proc = $null
    try {
        Reset-CH
        $proc = Start-AgentFollow $w
        Start-Sleep -Seconds 1

        $gen = (& $script:Generator -Dir $w.LogDir -Rate $script:T.S1Rate -DurationSec $script:T.S1Dur |
                Select-Object -Last 1) | ConvertFrom-Json
        $total = [long]$gen.total
        [void]$notes.Add("генератор: $total событий, фактический темп $($gen.actual_rate)/с")

        Start-Sleep -Seconds $script:T.S1Drain    # протокол: стоп генератора → дренаж → stop-file

        [void]$checks.Add((New-Check 'агент дожил до stop-file' (-not $proc.HasExited) `
            $(if ($proc.HasExited) { "агент завершился преждевременно (exit=$($proc.ExitCode))" } else { 'жив' })))

        $st = Stop-AgentGraceful $w $proc
        [void]$checks.Add((New-Check 'выход по stop-file: graceful, exit 0' `
            ($st.graceful -and $st.exit -eq 0) ("graceful=$($st.graceful) exit=$($st.exit) $($st.msg)")))

        $m = Get-MarkerStats 'tail_'
        $loss = $total - $m.uniq
        $dups = $m.cnt - $m.uniq
        $dupLimit = [long][math]::Floor($total * 0.0001)   # 0.01%
        [void]$checks.Add((New-Check 'потери = 0' ($loss -eq 0) `
            ("записано=$total, uniqExact=$($m.uniq), потеряно=$loss")))
        # строгое равенство count()==max(counter) достижимо только при 0 дублей;
        # при дублях в допуске 0.01% превышение count() ровно на их число — не потеря
        [void]$checks.Add((New-Check 'count() == max(counter) (с поправкой на дубли в допуске)' `
            (($m.cnt -eq [long]$gen.last) -or ($loss -eq 0 -and $dups -le $dupLimit)) `
            ("count()=$($m.cnt), max(counter)=$($gen.last), дублей=$dups")))
        [void]$checks.Add((New-Check 'дубли: 0 (допуск <= 0.01% при заявленном at-least-once)' `
            ($dups -le $dupLimit) ("count=$($m.cnt), uniq=$($m.uniq), дублей=$dups, допуск=$dupLimit")))
        if ($dups -gt 0 -and $dups -le $dupLimit) {
            [void]$notes.Add("ВНИМАНИЕ: $dups дублей в допуске — засчитывается только при заявленном at-least-once с дедупом")
        }

        # артефакты для S7 (sharing): генератор держал файл открытым (FileShare.Read) весь прогон
        $errLines = Read-AgentStderr $w
        $script:S1Artifacts = @{
            ran = $true
            pass = (@($checks | Where-Object { -not $_.pass }).Count -eq 0)
            stderr = $errLines
        }
    } catch {
        [void]$checks.Add((New-Check 'исключение сценария' $false $_.Exception.Message))
    } finally {
        if ($null -ne $proc -and -not $proc.HasExited) { try { Stop-Process -Id $proc.Id -Force -Confirm:$false } catch {} }
    }
    $res = New-ScenarioResult $sid $title $checks $notes $sw
    Remove-WorkDir $w $res.pass
    return $res
}

# ---------------------------------------------------------------------------- S2 ---
function Invoke-S2 {
    $sid = 'S2'; $title = 'незавершённое событие не эмитится до дописи'
    Write-Host ("`n=== {0}: {1} ===" -f $sid, $title) -ForegroundColor Cyan
    $checks = New-Object System.Collections.ArrayList
    $notes  = New-Object System.Collections.ArrayList
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $w = New-Work $sid; $proc = $null; $wr = $null
    try {
        Reset-CH
        # затравка: один файл, 5 полных событий (>100 байт — порог MIN_FILE_SIZE пройден), BOM + CRLF
        $procDir = Join-Path $w.LogDir 'rphost_1'
        New-Item -ItemType Directory -Force -Path $procDir | Out-Null
        $file = Join-Path $procDir (Get-CurrentHourLogName)
        $seed = New-Object System.IO.StreamWriter($file, $false, (New-Object System.Text.UTF8Encoding $true))
        for ($i = 1; $i -le 5; $i++) {
            $seed.Write((Get-TJEventLine ('00:0{0}.000000' -f $i) (1000 + $i) "Usr=tail_seed$i,GenMs=$(Get-UnixMsNow),Memory=$i"))
            $seed.Write("`r`n")
        }
        $seed.Flush(); $seed.Close()

        $proc = Start-AgentFollow $w
        $ok = Wait-Until { (Get-CHLong "SELECT count() FROM ${script:Table}") -eq 5 } $script:T.WaitShort
        $cnt0 = Get-CHLong "SELECT count() FROM ${script:Table}"
        [void]$checks.Add((New-Check 'первичный скан: 5 затравочных событий в CH' $ok `
            ("count()=$cnt0, ожидалось 5 за $($script:T.WaitShort) с" + $(if (-not $ok) { ' — агент не тянет файл?' } else { '' }))))

        if ($ok) {
            # дозапись ЧАСТИ события без завершающего \n (обрыв посреди значения свойства)
            $fs = New-Object System.IO.FileStream($file, [System.IO.FileMode]::Append,
                    [System.IO.FileAccess]::Write, [System.IO.FileShare]::Read)
            $wr = New-Object System.IO.StreamWriter($fs, (New-Object System.Text.UTF8Encoding $false))
            $wr.Write((Get-TJEventLine '00:10.000001' 777 "Usr=tail_inc1,GenMs=$(Get-UnixMsNow),Memory=42,Descr='halfA_ha"))
            $wr.Flush()

            Start-Sleep -Seconds 5   # > idle-close-ms (2000): хвост БЕЗ \n эмитить нельзя
            $cnt1 = Get-CHLong "SELECT count() FROM ${script:Table}"
            [void]$checks.Add((New-Check 'хвост без \n не эмитится (ждали 5 с > idle-close 2 с)' ($cnt1 -eq 5) `
                ("count()=$cnt1, ожидалось по-прежнему 5")))

            # дописываем хвост значения + ещё одно полное событие
            $wr.Write("lfB_assembled'"); $wr.Write("`r`n")
            $wr.Write((Get-TJEventLine '00:11.000001' 888 "Usr=tail_inc2,GenMs=$(Get-UnixMsNow),Memory=43"))
            $wr.Write("`r`n")
            $wr.Flush()

            $ok2 = Wait-Until { (Get-CHLong "SELECT count() FROM ${script:Table}") -eq 7 } $script:T.WaitShort
            $cnt2 = Get-CHLong "SELECT count() FROM ${script:Table}"
            [void]$checks.Add((New-Check 'после дописи ровно 2 новые записи' $ok2 `
                ("count()=$cnt2, ожидалось 7 за $($script:T.WaitShort) с")))

            Start-Sleep -Seconds 3
            $cnt3 = Get-CHLong "SELECT count() FROM ${script:Table}"
            [void]$checks.Add((New-Check 'лишние записи не появились (стабильно 7)' ($cnt3 -eq 7) ("count()=$cnt3")))

            $descr = Invoke-CH ("SELECT props['Descr'] FROM ${script:Table} WHERE props['Usr'] = 'tail_inc1' FORMAT TabSeparated")
            [void]$checks.Add((New-Check 'значение свойства склеено целиком через границу дозаписи' `
                ($descr -eq 'halfA_halfB_assembled') ("props['Descr']='$descr', ожидалось 'halfA_halfB_assembled'")))
            $inc2 = Get-CHLong "SELECT count() FROM ${script:Table} WHERE props['Usr'] = 'tail_inc2'"
            [void]$checks.Add((New-Check 'второе полное событие дошло' ($inc2 -eq 1) ("записей tail_inc2: $inc2")))
        }

        $st = Stop-AgentGraceful $w $proc
        [void]$checks.Add((New-Check 'выход по stop-file: graceful, exit 0' `
            ($st.graceful -and $st.exit -eq 0) ("graceful=$($st.graceful) exit=$($st.exit) $($st.msg)")))
    } catch {
        [void]$checks.Add((New-Check 'исключение сценария' $false $_.Exception.Message))
    } finally {
        if ($null -ne $wr) { try { $wr.Close() } catch {} }
        if ($null -ne $proc -and -not $proc.HasExited) { try { Stop-Process -Id $proc.Id -Force -Confirm:$false } catch {} }
    }
    $res = New-ScenarioResult $sid $title $checks $notes $sw
    Remove-WorkDir $w $res.pass
    return $res
}

# ---------------------------------------------------------------------------- S3 ---
function Invoke-S3 {
    $sid = 'S3'; $title = 'ротация: подхват нового файла, потерь нет'
    Write-Host ("`n=== {0}: {1} ({2} c, ротация каждые {3} c) ===" -f $sid, $title, $script:T.S3Dur, $script:T.S3Rotate) -ForegroundColor Cyan
    $checks = New-Object System.Collections.ArrayList
    $notes  = New-Object System.Collections.ArrayList
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $w = New-Work $sid; $proc = $null
    try {
        Reset-CH
        $proc = Start-AgentFollow $w
        Start-Sleep -Seconds 1

        $gen = (& $script:Generator -Dir $w.LogDir -Rate $script:T.S3Rate -DurationSec $script:T.S3Dur `
                -RotateAfterSec $script:T.S3Rotate | Select-Object -Last 1) | ConvertFrom-Json
        $total = [long]$gen.total
        $genFiles = @($gen.files | ForEach-Object { [System.IO.Path]::GetFileName($_) })
        [void]$notes.Add("генератор: $total событий в $($genFiles.Count) файлах: $($genFiles -join ', ')")
        [void]$checks.Add((New-Check 'ротация состоялась (файлов >= 2)' ($genFiles.Count -ge 2) `
            ("файлов у генератора: $($genFiles.Count)")))

        [void](Wait-Until { (Get-MarkerStats 'tail_').uniq -ge $total } $script:T.WaitDrain)

        [void]$checks.Add((New-Check 'агент дожил до stop-file' (-not $proc.HasExited) `
            $(if ($proc.HasExited) { "агент завершился преждевременно (exit=$($proc.ExitCode))" } else { 'жив' })))
        $st = Stop-AgentGraceful $w $proc
        [void]$checks.Add((New-Check 'выход по stop-file: graceful, exit 0' `
            ($st.graceful -and $st.exit -eq 0) ("graceful=$($st.graceful) exit=$($st.exit) $($st.msg)")))

        $m = Get-MarkerStats 'tail_'
        $loss = $total - $m.uniq; $dups = $m.cnt - $m.uniq
        [void]$checks.Add((New-Check 'потери счётчиков = 0 (сквозь ротацию)' ($loss -eq 0) `
            ("записано=$total, uniqExact=$($m.uniq), потеряно=$loss")))
        [void]$checks.Add((New-Check 'дубли: 0 (допуск <= 0.01%)' ($dups -le [long][math]::Floor($total * 0.0001)) `
            ("count=$($m.cnt), uniq=$($m.uniq), дублей=$dups")))

        # какие файлы реально видны в CH + непрерывность счётчиков на границах
        $pos = 'tail_'.Length + 1
        $rowsRaw = Invoke-CH ("SELECT filename, min(toInt64OrZero(substring(props['Usr'], $pos))) AS mn, " +
            "max(toInt64OrZero(substring(props['Usr'], $pos))) AS mx, count() AS c FROM ${script:Table} " +
            "WHERE startsWith(props['Usr'], 'tail_') GROUP BY filename ORDER BY mn FORMAT TabSeparated")
        $rows = @(@($rowsRaw -split "`n") | Where-Object { $_ -ne '' } | ForEach-Object {
            $p = $_ -split "`t"
            [pscustomobject]@{ file = $p[0]; mn = [long]$p[1]; mx = [long]$p[2]; cnt = [long]$p[3] }
        })
        $chFiles = @($rows | ForEach-Object { $_.file })
        $missing = @($genFiles | Where-Object { $chFiles -notcontains $_ })
        [void]$checks.Add((New-Check 'события всех файлов ротации присутствуют в CH' ($missing.Count -eq 0) `
            ("в CH: [$($chFiles -join ', ')]; отсутствуют: [$($missing -join ', ')]")))
        $contig = $true; $detail = @()
        for ($i = 0; $i -lt $rows.Count; $i++) {
            $detail += ("{0}: {1}..{2} ({3})" -f $rows[$i].file, $rows[$i].mn, $rows[$i].mx, $rows[$i].cnt)
            if ($i -gt 0 -and $rows[$i].mn -ne ($rows[$i - 1].mx + 1)) { $contig = $false }
        }
        if ($rows.Count -eq 0) { $detail = @('в CH нет строк с маркером tail_') }
        [void]$checks.Add((New-Check 'счётчики непрерывны на границах файлов (подхват без пропуска)' `
            ($contig -and $rows.Count -ge 2) ($detail -join '; ')))
    } catch {
        [void]$checks.Add((New-Check 'исключение сценария' $false $_.Exception.Message))
    } finally {
        if ($null -ne $proc -and -not $proc.HasExited) { try { Stop-Process -Id $proc.Id -Force -Confirm:$false } catch {} }
    }
    $res = New-ScenarioResult $sid $title $checks $notes $sw
    Remove-WorkDir $w $res.pass
    return $res
}

# ---------------------------------------------------------------------------- S4 ---
function Invoke-S4 {
    $sid = 'S4'; $title = 'усечение файла до 0 на живом агенте'
    Write-Host ("`n=== {0}: {1} ===" -f $sid, $title) -ForegroundColor Cyan
    $checks = New-Object System.Collections.ArrayList
    $notes  = New-Object System.Collections.ArrayList
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $w = New-Work $sid; $proc = $null
    try {
        Reset-CH
        $proc = Start-AgentFollow $w
        Start-Sleep -Seconds 1

        # волна 1: 1000 событий tail_*
        $gen1 = (& $script:Generator -Dir $w.LogDir -Rate $script:T.S4Seed -DurationSec 1 |
                 Select-Object -Last 1) | ConvertFrom-Json
        $ok = Wait-Until { (Get-MarkerStats 'tail_').uniq -ge [long]$gen1.total } $script:T.WaitShort
        $m1 = Get-MarkerStats 'tail_'
        [void]$checks.Add((New-Check "волна 1 в CH ($($gen1.total) событий)" $ok `
            ("uniq=$($m1.uniq) из $($gen1.total) за $($script:T.WaitShort) с" + $(if (-not $ok) { ' — агент не вставляет?' } else { '' }))))

        if ($ok) {
            # волна 2: генератор пересоздаёт файл ТОГО ЖЕ часа (FileMode.Create → усечение до 0),
            # 500 событий с другим маркером; новый размер заведомо меньше старого оффсета
            $gen2 = (& $script:Generator -Dir $w.LogDir -Rate $script:T.S4New -DurationSec 1 -MarkerPrefix 'trunc_' |
                     Select-Object -Last 1) | ConvertFrom-Json
            $lastFile1 = @($gen1.files)[-1]; $firstFile2 = @($gen2.files)[0]
            $sameFile = ($firstFile2 -eq $lastFile1)
            [void]$checks.Add((New-Check 'усечён тот же файл (час не сменился)' $sameFile `
                ("волна1: $lastFile1; волна2: $firstFile2" + $(if (-not $sameFile) { ' — граница часа попала в сценарий, перезапустите S4' } else { '' }))))

            $ok2 = Wait-Until { (Get-MarkerStats 'trunc_').uniq -ge [long]$gen2.total } $script:T.WaitShort
            $m2 = Get-MarkerStats 'trunc_'
            [void]$checks.Add((New-Check "новые $($gen2.total) после усечения в CH (рестарт с offset 0)" $ok2 `
                ("uniq=$($m2.uniq), count=$($m2.cnt) из $($gen2.total) за $($script:T.WaitShort) с")))
            [void]$checks.Add((New-Check 'без дублей в новой волне' ($m2.cnt -eq $m2.uniq) `
                ("count=$($m2.cnt), uniq=$($m2.uniq)")))

            $m1b = Get-MarkerStats 'tail_'
            [void]$checks.Add((New-Check 'старая волна осталась как была (без повторной вставки)' `
                ($m1b.cnt -eq [long]$gen1.total -and $m1b.uniq -eq [long]$gen1.total) `
                ("count=$($m1b.cnt), uniq=$($m1b.uniq), ожидалось $($gen1.total)")))
        }

        [void]$checks.Add((New-Check 'агент не упал после усечения' (-not $proc.HasExited) `
            $(if ($proc.HasExited) { "агент мёртв (exit=$($proc.ExitCode))" } else { 'жив' })))
        $st = Stop-AgentGraceful $w $proc
        [void]$checks.Add((New-Check 'выход по stop-file: graceful, exit 0' `
            ($st.graceful -and $st.exit -eq 0) ("graceful=$($st.graceful) exit=$($st.exit) $($st.msg)")))
    } catch {
        [void]$checks.Add((New-Check 'исключение сценария' $false $_.Exception.Message))
    } finally {
        if ($null -ne $proc -and -not $proc.HasExited) { try { Stop-Process -Id $proc.Id -Force -Confirm:$false } catch {} }
    }
    $res = New-ScenarioResult $sid $title $checks $notes $sw
    Remove-WorkDir $w $res.pass
    return $res
}

# ---------------------------------------------------------------------------- S5 ---
function Invoke-S5 {
    $sid = 'S5'; $title = 'crash-recovery: kill -Force и рестарт с тем же --state'
    Write-Host ("`n=== {0}: {1} ({2} c @ {3} соб/с, kill на {4}-й c) ===" -f $sid, $title, $script:T.S5Dur, $script:T.S5Rate, $script:T.S5KillAt) -ForegroundColor Cyan
    $checks = New-Object System.Collections.ArrayList
    $notes  = New-Object System.Collections.ArrayList
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $w = New-Work $sid; $proc = $null; $proc2 = $null; $job = $null
    try {
        Reset-CH
        $proc = Start-AgentFollow $w 'a'
        Start-Sleep -Seconds 1

        $job = Start-GeneratorJob $w.LogDir $script:T.S5Rate $script:T.S5Dur
        $genStarted = Wait-Until { Test-Path (Join-Path $w.LogDir 'timeline.csv') } 30 200
        [void]$checks.Add((New-Check 'фоновый генератор стартовал' $genStarted `
            $(if ($genStarted) { 'timeline.csv появился' } else { 'timeline.csv не появился за 30 с' })))

        Start-Sleep -Seconds $script:T.S5KillAt
        $wasAlive = ($null -ne $proc -and -not $proc.HasExited)
        [void]$checks.Add((New-Check 'агент был жив к моменту kill' $wasAlive `
            $(if ($wasAlive) { 'жив' } else { "агент уже завершился (exit=$($proc.ExitCode)) — падение до kill?" })))
        if ($wasAlive) {
            Stop-Process -Id $proc.Id -Force -Confirm:$false
            [void](Wait-Until { $proc.HasExited } 10 100)
            [void]$notes.Add("kill -Force на ~$($script:T.S5KillAt)-й секунде записи, рестарт с тем же --state")
        }

        # рестарт с тем же CLI: тот же --state, тот же --input, stop-file ещё не существует
        $proc2 = Start-AgentFollow $w 'b'

        $gen = Receive-GeneratorJob $job ($script:T.S5Dur + 90); $job = $null
        $genOk = ($null -ne $gen)
        [void]$checks.Add((New-Check 'генератор отработал в фоне' $genOk `
            $(if ($genOk) { "$($gen.total) событий, темп $($gen.actual_rate)/с" } else { 'нет JSON-сводки от генератора (job упал/завис)' })))

        if ($genOk) {
            $total = [long]$gen.total
            [void](Wait-Until { (Get-MarkerStats 'tail_').uniq -ge $total } $script:T.WaitDrain)

            [void]$checks.Add((New-Check 'агент (рестарт) дожил до stop-file' ($null -ne $proc2 -and -not $proc2.HasExited) `
                $(if ($null -ne $proc2 -and $proc2.HasExited) { "рестартованный агент завершился преждевременно (exit=$($proc2.ExitCode))" } else { 'жив' })))
            $st = Stop-AgentGraceful $w $proc2
            [void]$checks.Add((New-Check 'выход по stop-file: graceful, exit 0' `
                ($st.graceful -and $st.exit -eq 0) ("graceful=$($st.graceful) exit=$($st.exit) $($st.msg)")))

            $m = Get-MarkerStats 'tail_'
            $loss = $total - $m.uniq; $dups = $m.cnt - $m.uniq
            [void]$checks.Add((New-Check 'потери = 0 (все счётчики на месте после crash+restart)' ($loss -eq 0) `
                ("записано=$total, uniqExact=$($m.uniq), потеряно=$loss")))
            # at-least-once допустим: дубли только отчётно, порога нет (ждём «мало»)
            $dupPct = 0.0; if ($total -gt 0) { $dupPct = [math]::Round(100.0 * $dups / $total, 3) }
            [void]$checks.Add((New-Check 'дубли отчётно (at-least-once допустим)' $true `
                ("count=$($m.cnt), uniq=$($m.uniq), дублей=$dups ($dupPct%)")))
            [void]$notes.Add("дублей после crash-recovery: $dups ($dupPct%) — заявка агента должна покрывать at-least-once")
            if ($dupPct -gt 1.0) { [void]$notes.Add("ВНИМАНИЕ: дублей больше 1% — «ограниченные дубли» под вопросом, чекпоинты редкие?") }
        } else {
            $st = Stop-AgentGraceful $w $proc2
            [void]$checks.Add((New-Check 'выход по stop-file: graceful, exit 0' `
                ($st.graceful -and $st.exit -eq 0) ("graceful=$($st.graceful) exit=$($st.exit) $($st.msg)")))
        }
    } catch {
        [void]$checks.Add((New-Check 'исключение сценария' $false $_.Exception.Message))
    } finally {
        if ($null -ne $job) { try { Stop-Job $job; Remove-Job $job -Force } catch {} }
        foreach ($p in @($proc, $proc2)) {
            if ($null -ne $p -and -not $p.HasExited) { try { Stop-Process -Id $p.Id -Force -Confirm:$false } catch {} }
        }
    }
    $res = New-ScenarioResult $sid $title $checks $notes $sw
    Remove-WorkDir $w $res.pass
    return $res
}

# ---------------------------------------------------------------------------- S6 ---
function Invoke-S6 {
    $sid = 'S6'; $title = 'латентность append -> queryable'
    Write-Host ("`n=== {0}: {1} ({2} c @ {3} соб/с, опрос каждые {4} c) ===" -f $sid, $title, $script:T.S6Dur, $script:T.S6Rate, $script:T.S6PollSec) -ForegroundColor Cyan
    $checks = New-Object System.Collections.ArrayList
    $notes  = New-Object System.Collections.ArrayList
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $w = New-Work $sid; $proc = $null; $job = $null
    try {
        Reset-CH
        $proc = Start-AgentFollow $w
        Start-Sleep -Seconds 1

        $job = Start-GeneratorJob $w.LogDir $script:T.S6Rate $script:T.S6Dur
        $genStarted = Wait-Until { Test-Path (Join-Path $w.LogDir 'timeline.csv') } 30 200
        [void]$checks.Add((New-Check 'фоновый генератор стартовал' $genStarted `
            $(if ($genStarted) { 'timeline.csv появился' } else { 'timeline.csv не появился за 30 с' })))

        # опрос лага, пока генератор пишет: lag = now_ms - max(GenMs в CH)
        $samples = @()
        $qMax = "SELECT max(toUInt64OrZero(props['GenMs'])) FROM ${script:Table} WHERE startsWith(props['Usr'], 'tail_')"
        if ($genStarted) {
            $t0 = Get-UnixMsNow
            $sampleDeadline = (Get-Date).AddSeconds($script:T.S6Dur + 5)
            while ((Get-Date) -lt $sampleDeadline -and $job.State -eq 'Running') {
                Start-Sleep -Seconds $script:T.S6PollSec
                $mx = [long]0
                try { $mx = Get-CHLong $qMax } catch {}
                $lagBase = $t0; if ($mx -gt 0) { $lagBase = $mx }
                $samples += [double]((Get-UnixMsNow) - $lagBase)
            }
        }

        $gen = Receive-GeneratorJob $job ($script:T.S6Dur + 90); $job = $null
        $genOk = ($null -ne $gen)
        $rateOk = $false
        if ($genOk) { $rateOk = ([double]$gen.actual_rate -ge 0.95 * $script:T.S6Rate) }
        [void]$checks.Add((New-Check "генератор удержал темп >= 95% от $($script:T.S6Rate)/с" ($genOk -and $rateOk) `
            $(if ($genOk) { "фактически $($gen.actual_rate)/с" } else { 'нет сводки генератора — замер латентности невалиден' })))

        [void]$checks.Add((New-Check 'замеров латентности достаточно (>= 3)' ($samples.Count -ge 3) `
            ("выборок: $($samples.Count)")))
        if ($samples.Count -ge 3) {
            $p95 = Get-P95 $samples
            $smin = [math]::Round(($samples | Measure-Object -Minimum).Minimum, 0)
            $smax = [math]::Round(($samples | Measure-Object -Maximum).Maximum, 0)
            [void]$checks.Add((New-Check 'p95 задержки append->queryable < 5000 мс' ($p95 -lt 5000) `
                ("p95=$([math]::Round($p95,0)) мс (min=$smin, max=$smax, n=$($samples.Count))")))
            [void]$notes.Add("выборки лага, мс: " + (($samples | ForEach-Object { [math]::Round($_, 0) }) -join ', '))
        }

        if ($genOk) {
            $total = [long]$gen.total
            [void](Wait-Until { (Get-MarkerStats 'tail_').uniq -ge $total } $script:T.WaitDrain)
            $st = Stop-AgentGraceful $w $proc
            [void]$checks.Add((New-Check 'выход по stop-file: graceful, exit 0' `
                ($st.graceful -and $st.exit -eq 0) ("graceful=$($st.graceful) exit=$($st.exit) $($st.msg)")))
            $m = Get-MarkerStats 'tail_'
            [void]$checks.Add((New-Check 'потери = 0 (контроль полноты после дренажа)' (($total - $m.uniq) -eq 0) `
                ("записано=$total, uniqExact=$($m.uniq)")))
        } else {
            $st = Stop-AgentGraceful $w $proc
            [void]$checks.Add((New-Check 'выход по stop-file: graceful, exit 0' `
                ($st.graceful -and $st.exit -eq 0) ("graceful=$($st.graceful) exit=$($st.exit) $($st.msg)")))
        }
    } catch {
        [void]$checks.Add((New-Check 'исключение сценария' $false $_.Exception.Message))
    } finally {
        if ($null -ne $job) { try { Stop-Job $job; Remove-Job $job -Force } catch {} }
        if ($null -ne $proc -and -not $proc.HasExited) { try { Stop-Process -Id $proc.Id -Force -Confirm:$false } catch {} }
    }
    $res = New-ScenarioResult $sid $title $checks $notes $sw
    Remove-WorkDir $w $res.pass
    return $res
}

# ---------------------------------------------------------------------------- S7 ---
function Invoke-S7 {
    $sid = 'S7'; $title = 'sharing: файл открыт генератором на запись весь S1'
    Write-Host ("`n=== {0}: {1} ===" -f $sid, $title) -ForegroundColor Cyan
    $checks = New-Object System.Collections.ArrayList
    $notes  = New-Object System.Collections.ArrayList
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $a = $script:S1Artifacts
    [void]$checks.Add((New-Check 'S1 выполнялся (генератор держал файл открытым с FileShare.Read)' $a.ran `
        $(if ($a.ran) { 'да' } else { 'S1 не запускался (возможно, отфильтрован -Only) — sharing не проверен' })))
    if ($a.ran) {
        [void]$checks.Add((New-Check 'S1 прошёл при открытом на запись файле' $a.pass `
            $(if ($a.pass) { 'да' } else { 'S1 провален — см. его проверки' })))
        $errPatterns = '(?i)error|failed|denied|sharing|violation|exception|panic|fatal'
        $badLines = @($a.stderr | Where-Object { $_ -match $errPatterns })
        [void]$checks.Add((New-Check 'stderr агента в S1 без ошибок' ($badLines.Count -eq 0) `
            $(if ($badLines.Count -eq 0) { "строк stderr всего: $(@($a.stderr).Count), ошибочных: 0" }
              else { "ошибочные строки: " + (($badLines | Select-Object -First 3) -join ' | ') })))
    }
    return (New-ScenarioResult $sid $title $checks $notes $sw)
}

# ================================================================== главный цикл ===
Write-Host ""
Write-Host ("tail-харнесс bake-off §4.5: агент {0}" -f $script:AgentExe) -ForegroundColor White
Write-Host ("  sink агента: --sink {0}; проверки харнесса: {1} (таблица {2})" -f $script:SinkSpec, $script:ChUrl, $script:Table)
if ($Quick) { Write-Host "  РЕЖИМ -Quick: укороченные длительности, прогон НЕ зачётный" -ForegroundColor Yellow }

try {
    $chVer = Invoke-CH 'SELECT version()'
    $tblOk = Invoke-CH ("EXISTS TABLE " + $script:Table)
    if ($tblOk -ne '1') { throw "таблица ${script:Table} не существует" }
    Write-Host ("  ClickHouse {0}, таблица {1} на месте" -f $chVer, $script:Table)
} catch {
    Write-Host ("СРЕДА НЕ ГОТОВА: {0}" -f $_.Exception.Message) -ForegroundColor Red
    Write-Host "нужен живой контейнер tj-clickhouse (HTTP localhost:8123) с таблицей tj_bench.events"
    exit 2
}

$script:S1Artifacts = @{ ran = $false; pass = $false; stderr = @() }
$allScenarios = @('S1', 'S2', 'S3', 'S4', 'S5', 'S6', 'S7')
$toRun = $allScenarios
if ($Only.Count -gt 0) { $toRun = @($allScenarios | Where-Object { $Only -contains $_ }) }

$results = @()
foreach ($sid in $toRun) {
    $results += & ("Invoke-" + $sid)
}

# ------------------------------------------------------------------- итоги/отчёт ---
Write-Host "`n================================ ИТОГИ ================================" -ForegroundColor White
$table = $results | ForEach-Object {
    $failedChecks = @($_.checks | Where-Object { -not $_.pass })
    [pscustomobject]@{
        'Сценарий' = $_.id
        'Итог'     = $(if ($_.pass) { 'PASS' } else { 'FAIL' })
        'Время, с' = $_.duration_s
        'Что'      = $_.title
        'Провалено' = $(if ($failedChecks.Count -eq 0) { '' } else { ($failedChecks | ForEach-Object { $_.check }) -join '; ' })
    }
}
$table | Format-Table -AutoSize -Wrap | Out-String -Width 200 | Write-Host

foreach ($r in @($results | Where-Object { -not $_.pass })) {
    Write-Host ("--- {0} FAIL, детали:" -f $r.id) -ForegroundColor Red
    foreach ($c in @($r.checks | Where-Object { -not $_.pass })) {
        Write-Host ("    {0}: {1}" -f $c.check, $c.detail) -ForegroundColor Red
    }
}

$passCount = @($results | Where-Object { $_.pass }).Count
$failCount = @($results | Where-Object { -not $_.pass }).Count
$report = [pscustomobject]@{
    agent        = $script:AgentName
    agent_exe    = $script:AgentExe
    dsn          = $script:SinkSpec
    quick        = [bool]$Quick
    host         = $env:COMPUTERNAME
    started_utc  = $script:RunStamp
    finished_utc = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    pass         = $passCount
    fail         = $failCount
    overall_pass = ($failCount -eq 0 -and $passCount -gt 0)
    scenarios    = $results
}
# UTF-8 С BOM: иначе PS 5.1 Get-Content без -Encoding прочитает русские названия проверок как cp1251-мусор
$reportJson = $report | ConvertTo-Json -Depth 6
[System.IO.File]::WriteAllText($OutJson, $reportJson, (New-Object System.Text.UTF8Encoding $true))
Write-Host ("JSON-отчёт: {0}" -f $OutJson)
Write-Host ("PASS={0} FAIL={1}" -f $passCount, $failCount) -ForegroundColor $(if ($failCount -eq 0) { 'Green' } else { 'Red' })

if ($failCount -gt 0) { exit 1 }
exit 0
