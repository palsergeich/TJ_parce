# Обёртка одного замера: wall time, CPU%, Peak RSS. Протокол: docs/bakeoff-protocol.md §3.
#   .\measure.ps1 -Exe agents\go\tj-agent-go.exe -Arguments '--input','E:\bench\corpus-medium','--threads','8','--sink','null'
# Особенности (выяснено на суб-секундных прогонах):
# - StartTime/ExitCode недоступны после выхода процесса без удержания Handle — читаем заранее/держим Handle;
# - поллинг RSS 10 мс (200 мс пропускал пик коротких прогонов; для <0.2 c это всё равно нижняя оценка).

param(
    [Parameter(Mandatory)] [string]$Exe,
    [string[]]$Arguments = @(),
    [string]$OutJson = ''
)

$ErrorActionPreference = 'Stop'
$Exe = (Resolve-Path $Exe).Path

$p = Start-Process -FilePath $Exe -ArgumentList $Arguments -PassThru -NoNewWindow
$null = $p.Handle          # удержание handle: иначе ExitCode/ExitTime/TotalProcessorTime потом не прочитать
$startTime = $p.StartTime  # читаем, пока процесс жив

$peak = 0
while (-not $p.HasExited) {
    try {
        $p.Refresh()
        if ($p.PeakWorkingSet64 -gt $peak) { $peak = $p.PeakWorkingSet64 }
    } catch {}
    Start-Sleep -Milliseconds 10
}
try { $p.Refresh(); if ($p.PeakWorkingSet64 -gt $peak) { $peak = $p.PeakWorkingSet64 } } catch {}

$wall = ($p.ExitTime - $startTime).TotalSeconds
if ($wall -le 0) { $wall = 0.001 }  # защита от деления на ноль на вырожденных прогонах
$cpu  = $p.TotalProcessorTime.TotalSeconds

$result = [PSCustomObject]@{
    exe         = $Exe
    args        = ($Arguments -join ' ')
    wall_s      = [math]::Round($wall, 3)
    cpu_s       = [math]::Round($cpu, 3)
    cpu_pct     = [math]::Round(100 * $cpu / $wall / [int]$env:NUMBER_OF_PROCESSORS, 1)
    peak_rss_mb = [math]::Round($peak / 1MB, 1)
    exit_code   = $p.ExitCode
}

$json = $result | ConvertTo-Json
if ($OutJson) { $json | Out-File $OutJson -Encoding utf8 }
$json
if ($p.ExitCode -ne 0) { exit $p.ExitCode }
