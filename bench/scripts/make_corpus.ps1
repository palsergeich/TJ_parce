# Собирает bench-корпус ЖЁСТКИМИ ССЫЛКАМИ (hardlink) на файлы источника — данные не копируются.
# Составы корпусов зафиксированы в docs/bakeoff-protocol.md §1.2.
#   .\make_corpus.ps1 -Corpus medium   -> E:\bench\corpus-medium (5.4 ГБ ссылками)
#   .\make_corpus.ps1 -Corpus smoke    -> E:\bench\corpus-smoke  (0.29 ГБ)
#
# Почему hardlink, а не junction: обходчики всех трёх агентов (и PS 5.1 Get-ChildItem)
# пропускают reparse points — junction-корпус выглядит пустым. Hardlink — обычный файл
# (та же дедупликация места), требует одного тома с источником.

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

if ((Get-Item $Source).PSDrive.Name -ne (Split-Path $Target -Qualifier).TrimEnd(':')) {
    throw "Hardlink требует один том: источник $Source и цель $Target на разных дисках"
}

$collections = $compositions[$Corpus]
if (Test-Path $Target) { Remove-Item $Target -Recurse -Force }
New-Item -ItemType Directory -Force -Path $Target | Out-Null

$total = 0; $files = 0
foreach ($c in $collections) {
    $src = Join-Path $Source $c
    if (-not (Test-Path $src)) { throw "Коллекция не найдена: $src" }
    foreach ($f in Get-ChildItem $src -Recurse -File) {
        $rel = $f.FullName.Substring($Source.Length).TrimStart('\')
        $dst = Join-Path $Target $rel
        $dstDir = Split-Path $dst -Parent
        if (-not (Test-Path $dstDir)) { New-Item -ItemType Directory -Force -Path $dstDir | Out-Null }
        New-Item -ItemType HardLink -Path $dst -Target $f.FullName | Out-Null
        $total += $f.Length; $files++
    }
    "{0,-25} готово" -f $c
}

"`nКорпус '$Corpus' собран: $Target"
"Итого: {0:N2} ГБ, {1} файлов (hardlink, место не дублируется)" -f ($total / 1GB), $files
