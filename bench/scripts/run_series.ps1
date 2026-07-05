# Официальная серия null-sink на bench-medium: 3 участника x 3 прогона с ротацией порядка.
# Протокол: docs/bakeoff-protocol.md §2.4 (ротация нейтрализует тепловой дрейф и фон).
param(
    [string]$Corpus = 'E:\bench\corpus-medium',
    [int]$Threads = 8,
    [string]$OutDir = "E:\git\ТехЖурнал\bench\results\2026-07-04-official"
)

$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$measure = "E:\git\ТехЖурнал\bench\scripts\measure.ps1"

$contenders = @{
    go   = @{ exe = 'E:\git\ТехЖурнал\agents\go\tj-agent-go.exe' }
    rust = @{ exe = 'E:\git\ТехЖурнал\agents\rust\target\release\tj-agent-rs.exe' }
    cpp  = @{ exe = 'E:\git\ТехЖурнал\core\build\Release\tj-agent-cpp.exe' }
}

$rotations = @(@('go','rust','cpp'), @('rust','cpp','go'), @('cpp','go','rust'))
$runs = @{ go = @(); rust = @(); cpp = @() }
$events = @{}

$round = 0
foreach ($rotation in $rotations) {
    $round++
    foreach ($name in $rotation) {
        $c = $contenders[$name]
        $stats = Join-Path $env:TEMP "stats_$name.json"
        $out = & $measure -Exe $c.exe -Arguments @('--input', $Corpus, '--threads', "$Threads", '--sink', 'null', '--stats-json', $stats)
        $r = ($out | Out-String | ConvertFrom-Json)
        if ($r.exit_code -ne 0) { throw "$name exit $($r.exit_code) в раунде $round" }
        $runs[$name] += $r
        $s = Get-Content $stats -Raw | ConvertFrom-Json
        if (-not $events.ContainsKey($name)) { $events[$name] = $s.events }
        elseif ($events[$name] -ne $s.events) { throw "$name events расходятся между прогонами" }
        "{0,-5} r{1}: wall={2,6:N3}s rss={3,6:N1}MB cpu={4,5:N1}% events={5}" -f $name, $round, $r.wall_s, $r.peak_rss_mb, $r.cpu_pct, $s.events
    }
}

# Контроль полноты: все участники обязаны насчитать одинаково
$vals = $events.Values | Sort-Object -Unique
if ($vals.Count -ne 1) { throw "СЧЁТЧИКИ СОБЫТИЙ РАСХОДЯТСЯ: $($events | ConvertTo-Json -Compress)" }
""
"События (все совпали): $($vals[0])"

$corpusBytes = (Get-ChildItem $Corpus -Recurse -File | Measure-Object Length -Sum).Sum
$summary = foreach ($name in @('cpp','rust','go')) {
    $walls = $runs[$name] | ForEach-Object { $_.wall_s } | Sort-Object
    $median = $walls[1]
    $spread = ($walls[2] - $walls[0]) / $median
    [PSCustomObject]@{
        contender  = $name
        median_s   = $median
        mbps       = [math]::Round($corpusBytes / 1MB / $median, 0)
        spread_pct = [math]::Round(100 * $spread, 1)
        rss_mb_max = [math]::Round(($runs[$name] | Measure-Object peak_rss_mb -Maximum).Maximum, 0)
        cpu_pct    = [math]::Round(($runs[$name] | Measure-Object cpu_pct -Average).Average, 1)
        valid      = if ($spread -le 0.10) { 'да' } else { 'НЕТ (>10%)' }
    }
}
$summary | Format-Table -AutoSize
$summary | ConvertTo-Json | Out-File (Join-Path $OutDir 'medium_null_summary.json') -Encoding utf8
$runs | ConvertTo-Json -Depth 4 | Out-File (Join-Path $OutDir 'medium_null_runs.json') -Encoding utf8
"Сохранено в $OutDir"
