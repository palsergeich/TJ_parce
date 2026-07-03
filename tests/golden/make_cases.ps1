# Генератор golden-кейсов: синтетические краевые случаи + реальные сиды из seed/.
# Запускать один раз (или после изменения набора кейсов). Идемпотентен.
# Файлы создаются с точным контролем байтов (кодировка, BOM, EOL, размер).

$ErrorActionPreference = 'Stop'
$golden = $PSScriptRoot
$cases  = Join-Path $golden 'cases'
New-Item -ItemType Directory -Force -Path $cases | Out-Null

$utf8 = New-Object System.Text.UTF8Encoding $false

function New-Case([string]$name, [string]$fileName, [string]$content, [switch]$Bom, [string]$subdir = 'rphost_1') {
    $dir = Join-Path $cases "$name\input\$subdir"
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $bytes = $utf8.GetBytes($content)
    if ($Bom) { $bytes = [byte[]](0xEF, 0xBB, 0xBF) + $bytes }
    [System.IO.File]::WriteAllBytes((Join-Path $dir $fileName), $bytes)
}

# Все файлы >= 100 байт (MIN_FILE_SIZE), иначе нормализатор их пропустит.
$pad = ",Pad=" + ('x' * 80)

# --- Типизация чисел (KI-2): версии/диапазоны/кривые дроби -> строки ---
New-Case 'version_token' '25113010.log' @"
00:01.000001-100,EXCP,0,AppVersion=8.3.22.1704,Ver=1.2.3,Num=42,Neg=-5,Float=0.5,BadF1=.5,BadF2=0.,Range=1-2,Zero=0,LeadZero=007,Guid=123456
"@

# --- Ведущие нули длительности (KI-2b) ---
New-Case 'leading_zero_duration' '25113011.log' @"
00:02.000002-007,CALL,1,Memory=10$pad
00:03.000003-0,CALL,1,Memory=11$pad
"@

# --- BOM на входе (KI-6): содержимое идентично no_bom, вывод обязан совпадать ---
$bomContent = @"
00:04.000004-10,CALL,1,Usr=test$pad
00:05.000005-20,CALL,2,Usr=test2$pad
"@
New-Case 'bom_input' '25113012.log' $bomContent -Bom
New-Case 'no_bom'    '25113012.log' $bomContent

# --- Кавычки: удвоение, незакрытая, многострочность ---
New-Case 'quotes_doubling' '25113013.log' @"
00:06.000006-30,SDBL,0,Sql='SELECT ''x'' FROM t',Descr="he said ""hi""",Tail=ok
00:07.000007-40,EXCP,0,Descr='unterminated value
second line of the same event
00:08.000008-50,CALL,1,Usr=next$pad
"@

# --- Эвристика одинарной кавычки (KI-10): 'abc'extra глотает следующее свойство ---
New-Case 'single_quote_heuristic' '25113014.log' @"
00:09.000009-50,TEST,0,Val='abc'extra,Next=1$pad
"@

# --- Пустые значения ---
New-Case 'empty_value' '25113015.log' @"
00:10.000010-60,TEST,0,A=,B=1,C=$pad
"@

# --- Дубликаты ключей и свойство с именем базового поля ---
New-Case 'dup_keys' '25113016.log' @"
00:11.000011-70,TEST,0,X=1,X=2,event=fake$pad
"@

# --- Имя файла не по маске: timestamp деградирует до MM:SS ---
New-Case 'bad_filename' 'notadate.log' @"
00:12.000012-80,TEST,0,A=1$pad
"@

# --- Заголовки без уровня / без второй запятой ---
New-Case 'short_header' '25113017.log' @"
00:13.000013-5,EXCP$pad
00:14.000014-6,EXCP,
00:15.000015-7,EXCP,0$pad
"@

# --- Граница MIN_FILE_SIZE: 99 байт пропускается, 100/101 обрабатываются ---
$dir99 = Join-Path $cases 'min_size_boundary\input\rphost_1'
New-Item -ItemType Directory -Force -Path $dir99 | Out-Null
foreach ($target in 99, 100, 101) {
    $prefix = "00:16.000016-1,TEST,0,Pad="   # 26 байт
    $padLen = $target - $prefix.Length - 1   # -1 под завершающий LF
    $content = $prefix + ('y' * $padLen) + "`n"
    $bytes = $utf8.GetBytes($content)
    if ($bytes.Length -ne $target) { throw "min_size_boundary: got $($bytes.Length), want $target" }
    # Разные имена-часы, чтобы отличать записи в выводе
    [System.IO.File]::WriteAllBytes((Join-Path $dir99 ("251130{0:D2}.log" -f ($target - 80))), $bytes)
}

# --- KI-1 (expected-fail): маска внутри кавычек ложно режет событие ---
New-Case 'mask_inside_quotes' '25113018.log' @"
00:17.000017-90,QERR,0,Descr='line one
10:00.000000-5,fake inside quotes
line three',After=1$pad
"@
Set-Content -Path (Join-Path $cases 'mask_inside_quotes\XFAIL') -Value 'KI-1: quote-parity guard not implemented yet' -Encoding ascii

# --- Реальные сиды: seed/<Коллекция>__<процесс>__<YYMMDDHH>.log -> cases/real_<коллекция>/input/<процесс>/<YYMMDDHH>.log ---
Get-ChildItem (Join-Path $golden 'seed') -File | ForEach-Object {
    $parts = $_.Name -split '__'
    if ($parts.Count -ne 3) { Write-Warning "seed не по маске: $($_.Name)"; return }
    $caseName = 'real_' + $parts[0].ToLower()
    $dir = Join-Path $cases "$caseName\input\$($parts[1])"
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    Copy-Item $_.FullName (Join-Path $dir $parts[2]) -Force
}

Write-Host "Кейсы созданы в $cases"
Get-ChildItem $cases -Directory | Select-Object -ExpandProperty Name
