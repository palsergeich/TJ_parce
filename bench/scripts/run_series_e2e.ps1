# E2E-серия bake-off: normalize + insert в ClickHouse (tj_bench.events).
# 3 участника x 3 прогона с ротацией; TRUNCATE перед каждым прогоном; сверка count().
# ClickHouse обязан работать (он — часть измеряемой системы); Grafana/Prometheus глушим.
param(
    [string]$Corpus = 'E:\bench\corpus-medium',
    [int]$Threads = 8,
    [long]$ExpectedEvents = 8648884,
    [string]$OutDir = "E:\git\ТехЖурнал\bench\results\2026-07-06-e2e"
)

$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$measure = "E:\git\ТехЖурнал\bench\scripts\measure.ps1"

$contenders = @{
    go   = @{ exe = 'E:\git\ТехЖурнал\agents\go\tj-agent-go.exe' }
    rust = @{ exe = 'E:\git\ТехЖурнал\agents\rust\target\release\tj-agent-rs.exe' }
    cpp  = @{ exe = 'E:\git\ТехЖурнал\core\build\Release\tj-agent-cpp.exe' }
}

function Invoke-CH([string]$q) {
    $r = docker exec tj-clickhouse clickhouse-client --query $q
    if ($LASTEXITCODE -ne 0) { throw "clickhouse-client failed: $q" }
    return $r
}

$rotations = @(@('go','rust','cpp'), @('rust','cpp','go'), @('cpp','go','rust'))
$runs = @{ go = @(); rust = @(); cpp = @() }

$round = 0
foreach ($rotation in $rotations) {
    $round++
    foreach ($name in $rotation) {
        $c = $contenders[$name]
        Invoke-CH "TRUNCATE TABLE tj_bench.events" | Out-Null
        $stats = Join-Path $env:TEMP "e2e_$name.json"
        $out = & $measure -Exe $c.exe -Arguments @('--input', $Corpus, '--threads', "$Threads", '--sink', 'clickhouse', '--stats-json', $stats)
        $r = ($out | Out-String | ConvertFrom-Json)
        if ($r.exit_code -ne 0) { throw "$name exit $($r.exit_code) в раунде $round" }
        $rows = [long](Invoke-CH "SELECT count() FROM tj_bench.events")
        if ($rows -ne $ExpectedEvents) { throw "${name}: в БД $rows строк, ожидалось $ExpectedEvents (раунд $round)" }
        $runs[$name] += $r
        "{0,-5} r{1}: wall={2,7:N3}s rss={3,7:N1}MB cpu={4,5:N1}% rows={5} OK" -f $name, $round, $r.wall_s, $r.peak_rss_mb, $r.cpu_pct, $rows
    }
}

$corpusBytes = (Get-ChildItem $Corpus -Recurse -File | Measure-Object Length -Sum).Sum
$summary = foreach ($name in @('cpp','rust','go')) {
    $walls = $runs[$name] | ForEach-Object { $_.wall_s } | Sort-Object
    $median = $walls[1]
    $spread = ($walls[2] - $walls[0]) / $median
    [PSCustomObject]@{
        contender  = $name
        median_s   = $median
        mbps       = [math]::Round($corpusBytes / 1MB / $median, 0)
        krows_s    = [math]::Round($ExpectedEvents / 1000 / $median, 0)
        spread_pct = [math]::Round(100 * $spread, 1)
        rss_mb_max = [math]::Round(($runs[$name] | Measure-Object peak_rss_mb -Maximum).Maximum, 0)
        valid      = if ($spread -le 0.10) { 'да' } else { 'НЕТ (>10%)' }
    }
}
$summary | Format-Table -AutoSize
$summary | ConvertTo-Json | Out-File (Join-Path $OutDir 'medium_e2e_summary.json') -Encoding utf8
$runs | ConvertTo-Json -Depth 4 | Out-File (Join-Path $OutDir 'medium_e2e_runs.json') -Encoding utf8
Invoke-CH "TRUNCATE TABLE tj_bench.events" | Out-Null
"Сохранено в $OutDir"
