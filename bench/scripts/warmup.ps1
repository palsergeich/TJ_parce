# Прогрев page cache перед серией (bench-medium 5.4 ГБ < RAM).
param([string]$Corpus = 'E:\bench\corpus-medium')

$ErrorActionPreference = 'Stop'
$buf = New-Object byte[] (4MB)
$files = Get-ChildItem $Corpus -Recurse -Filter *.log -File
$total = 0
foreach ($f in $files) {
    $fs = [IO.File]::OpenRead($f.FullName)
    while ($fs.Read($buf, 0, $buf.Length) -gt 0) {}
    $fs.Close()
    $total += $f.Length
}
"Прогрето: {0:N2} ГБ в {1} файлах" -f ($total / 1GB), $files.Count
