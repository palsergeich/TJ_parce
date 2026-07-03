# Собирает bench-корпус junction-ссылками (данные не копируются).
# Составы корпусов зафиксированы в docs/bakeoff-protocol.md §1.2.
#   .\make_corpus.ps1 -Corpus medium   -> E:\bench\corpus-medium (5.4 ГБ ссылками)
#   .\make_corpus.ps1 -Corpus smoke    -> E:\bench\corpus-smoke  (0.29 ГБ)

param(
    [ValidateSet('medium', 'smoke')]
    [string]$Corpus = 'medium',
    [string]$Source = 'E:\TJ_Logs\TJ_Logs',
    [string]$Target = "E:\bench\corpus-$Corpus"
)

$ErrorActionPreference = 'Stop'

$compositions = @{
    medium = @('CallsDiag', '_Reference27041', 'EXP', 'TLockDiag', '_ReferenceChngR12527', 'Diag_86', '_Document15317')
    smoke  = @('Diag_86', 'EXP_86', 'CallsDiag_86')
}

$collections = $compositions[$Corpus]
New-Item -ItemType Directory -Force -Path $Target | Out-Null

$total = 0
foreach ($c in $collections) {
    $src = Join-Path $Source $c
    if (-not (Test-Path $src)) { throw "Коллекция не найдена: $src" }
    $link = Join-Path $Target $c
    if (Test-Path $link) { (Get-Item $link).Delete() }  # удалить старый junction, не содержимое
    New-Item -ItemType Junction -Path $link -Target $src | Out-Null
    $size = (Get-ChildItem $src -Recurse -File | Measure-Object Length -Sum).Sum
    $total += $size
    "{0,-25} {1,10:N1} МБ" -f $c, ($size / 1MB)
}

"`nКорпус '$Corpus' собран: $Target"
"Итого: {0:N2} ГБ в {1} коллекциях (junction-ссылки, место не занято)" -f ($total / 1GB), $collections.Count
