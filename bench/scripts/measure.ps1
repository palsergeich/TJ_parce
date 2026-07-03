# Обёртка одного замера: wall time, CPU%, Peak RSS (поллинг, т.к. после
# выхода процесса PeakWorkingSet64 недоступен). Протокол: docs/bakeoff-protocol.md §3.
#   .\measure.ps1 -Exe agents\go\tj-agent-go.exe -Args '--input','E:\bench\corpus-medium','--threads','16','--sink','null'

param(
    [Parameter(Mandatory)] [string]$Exe,
    [string[]]$Arguments = @(),
    [string]$OutJson = ''
)

$ErrorActionPreference = 'Stop'
$Exe = (Resolve-Path $Exe).Path

$p = Start-Process -FilePath $Exe -ArgumentList $Arguments -PassThru -NoNewWindow
$peak = 0
while (-not $p.HasExited) {
    try {
        $p.Refresh()
        if ($p.PeakWorkingSet64 -gt $peak) { $peak = $p.PeakWorkingSet64 }
    } catch {}
    Start-Sleep -Milliseconds 200
}

$wall = ($p.ExitTime - $p.StartTime).TotalSeconds
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
