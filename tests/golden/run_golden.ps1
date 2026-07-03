# Golden-раннер: гоняет нормализатор по каждому кейсу и сравнивает вывод с эталоном побайтно.
#
#   .\run_golden.ps1                       # проверить дефолтным exe (cpp_parse\build)
#   .\run_golden.ps1 -Agent <путь к exe>   # проверить другого участника (bake-off)
#   .\run_golden.ps1 -Regen                # перегенерировать expected.jsonl (после смены спеки!)
#
# Контракт агента: <exe> <input_dir> <workers> <output.jsonl>  (см. docs/format-spec.md §5:
# запуск с workers=1 обязателен для детерминизма порядка записей).
# Кейс с файлом XFAIL — известное расхождение (KI): несовпадение НЕ валит прогон,
# а совпадение помечается как UNEXPECTED PASS (пора убрать XFAIL).

param(
    [string]$Agent = (Join-Path $PSScriptRoot '..\..\cpp_parse\build\count_contexts.exe'),
    [switch]$Regen
)

$ErrorActionPreference = 'Stop'
$cases = Join-Path $PSScriptRoot 'cases'
if (-not (Test-Path $cases)) { throw "Нет каталога cases/ — сначала запустите make_cases.ps1" }
$Agent = (Resolve-Path $Agent).Path

$tmp = Join-Path $env:TEMP ("golden_" + [System.Diagnostics.Process]::GetCurrentProcess().Id)
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

$pass = 0; $fail = 0; $xfail = 0; $upass = 0; $failed = @()

foreach ($case in Get-ChildItem $cases -Directory | Sort-Object Name) {
    $inputDir = Join-Path $case.FullName 'input'
    $expected = Join-Path $case.FullName 'expected.jsonl'
    $isXfail  = Test-Path (Join-Path $case.FullName 'XFAIL')
    $actual   = Join-Path $tmp ($case.Name + '.jsonl')

    # Нормализатор пишет прогресс в stdout — глушим, важен только файл и exit-код
    & $Agent $inputDir 1 $actual *> $null
    $code = $LASTEXITCODE
    if ($code -ne 0) {
        Write-Host ("[ERR ] {0}: exit-код {1}" -f $case.Name, $code) -ForegroundColor Red
        $fail++; $failed += $case.Name
        continue
    }
    if (-not (Test-Path $actual)) {
        # Пустой вход (всё отфильтровано) — считаем выводом нулевой длины
        [System.IO.File]::WriteAllBytes($actual, [byte[]]@())
    }

    if ($Regen) {
        if ($isXfail) {
            Write-Host ("[SKIP] {0}: XFAIL — эталон не перегенерируется (фиксация после починки KI)" -f $case.Name) -ForegroundColor DarkYellow
        } else {
            Copy-Item $actual $expected -Force
            Write-Host ("[GEN ] {0}: {1} байт" -f $case.Name, (Get-Item $expected).Length)
        }
        continue
    }

    if (-not (Test-Path $expected)) {
        if ($isXfail) {
            # У XFAIL-кейса эталона может не быть до починки KI — это известное состояние
            Write-Host ("[XFAIL] {0}: эталон появится после починки KI" -f $case.Name) -ForegroundColor DarkYellow
            $xfail++
        } else {
            Write-Host ("[MISS] {0}: нет expected.jsonl (запустите -Regen)" -f $case.Name) -ForegroundColor Yellow
            $fail++; $failed += $case.Name
        }
        continue
    }

    $a = [System.IO.File]::ReadAllBytes($actual)
    $e = [System.IO.File]::ReadAllBytes($expected)
    $equal = ($a.Length -eq $e.Length) -and ([System.Linq.Enumerable]::SequenceEqual($a, $e))

    if ($equal) {
        if ($isXfail) { Write-Host ("[UPASS] {0}: XFAIL неожиданно прошёл — уберите маркер" -f $case.Name) -ForegroundColor Magenta; $upass++ }
        else          { Write-Host ("[PASS] {0}" -f $case.Name) -ForegroundColor Green; $pass++ }
    } else {
        if ($isXfail) { Write-Host ("[XFAIL] {0} (известное расхождение)" -f $case.Name) -ForegroundColor DarkYellow; $xfail++ }
        else {
            Write-Host ("[FAIL] {0}: actual {1} байт vs expected {2} байт" -f $case.Name, $a.Length, $e.Length) -ForegroundColor Red
            Write-Host ("       diff: fc.exe /b `"{0}`" `"{1}`"" -f $actual, $expected)
            $fail++; $failed += $case.Name
        }
    }
}

Write-Host ''
Write-Host ("Итого: PASS={0} FAIL={1} XFAIL={2} UNEXPECTED-PASS={3}" -f $pass, $fail, $xfail, $upass)
if ($fail -gt 0) { Write-Host ("Провалены: {0}" -f ($failed -join ', ')) -ForegroundColor Red; exit 1 }
exit 0
