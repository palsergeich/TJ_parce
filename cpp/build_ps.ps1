# PowerShell скрипт для сборки C++ версии
$ErrorActionPreference = "Stop"

Write-Host "Building C++ version with optimizations..." -ForegroundColor Cyan
Write-Host ""

# Переходим в папку скрипта
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $scriptDir

# Создаем папку build
if (-not (Test-Path "build")) {
    New-Item -ItemType Directory -Path "build" | Out-Null
}

# Функция поиска MSVC компилятора
function Find-MSVC {
    $paths = @(
        "C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Tools\MSVC",
        "C:\Program Files\Microsoft Visual Studio\2022\Professional\VC\Tools\MSVC",
        "C:\Program Files\Microsoft Visual Studio\2022\Enterprise\VC\Tools\MSVC",
        "C:\Program Files (x86)\Microsoft Visual Studio\2019\Community\VC\Tools\MSVC",
        "C:\Program Files (x86)\Microsoft Visual Studio\2019\Professional\VC\Tools\MSVC",
        "C:\Program Files (x86)\Microsoft Visual Studio\2019\Enterprise\VC\Tools\MSVC"
    )
    
    foreach ($basePath in $paths) {
        if (Test-Path $basePath) {
            $versions = Get-ChildItem -Path $basePath -Directory | Sort-Object Name -Descending
            foreach ($version in $versions) {
                $clPath = Join-Path $version.FullName "bin\Hostx64\x64\cl.exe"
                if (Test-Path $clPath) {
                    return $clPath
                }
            }
        }
    }
    return $null
}

# Функция поиска MinGW
function Find-MinGW {
    $paths = @(
        "C:\msys64\mingw64\bin\g++.exe",
        "C:\mingw64\bin\g++.exe",
        "C:\MinGW\bin\g++.exe"
    )
    
    foreach ($path in $paths) {
        if (Test-Path $path) {
            return $path
        }
    }
    
    # Проверяем PATH
    $gpp = Get-Command g++ -ErrorAction SilentlyContinue
    if ($gpp) {
        return $gpp.Path
    }
    
    return $null
}

# Пробуем найти компилятор
$compiler = $null
$compilerType = $null

Write-Host "Searching for compilers..." -ForegroundColor Yellow

# Сначала пробуем MSVC
$msvcPath = Find-MSVC
if ($msvcPath) {
    Write-Host "Found MSVC: $msvcPath" -ForegroundColor Green
    $compiler = $msvcPath
    $compilerType = "msvc"
} else {
    # Пробуем MinGW
    $mingwPath = Find-MinGW
    if ($mingwPath) {
        Write-Host "Found MinGW: $mingwPath" -ForegroundColor Green
        $compiler = $mingwPath
        $compilerType = "mingw"
    }
}

if (-not $compiler) {
    Write-Host ""
    Write-Host "ERROR: No C++ compiler found!" -ForegroundColor Red
    Write-Host ""
    Write-Host "Please install one of the following:" -ForegroundColor Yellow
    Write-Host "  1. Visual Studio 2019/2022 with C++ tools"
    Write-Host "  2. MinGW-w64 (via MSYS2 or standalone)"
    Write-Host ""
    Write-Host "For MinGW quick install:" -ForegroundColor Cyan
    Write-Host "  - Download MSYS2: https://www.msys2.org/"
    Write-Host "  - Install, then run: pacman -S mingw-w64-x86_64-gcc"
    Write-Host "  - Add to PATH: C:\msys64\mingw64\bin"
    Write-Host ""
    exit 1
}

Write-Host ""
Write-Host "Compiling..." -ForegroundColor Cyan
Write-Host ""

Set-Location build

if ($compilerType -eq "msvc") {
    # MSVC сборка
    $clDir = Split-Path -Parent $compiler
    $env:PATH = "$clDir;$env:PATH"
    
    # Находим Windows SDK
    $sdkBase = "C:\Program Files (x86)\Windows Kits\10"
    if (Test-Path $sdkBase) {
        $sdkVersions = Get-ChildItem -Path "$sdkBase\Include" -Directory | Sort-Object Name -Descending
        if ($sdkVersions) {
            $sdkVer = $sdkVersions[0].Name
            $env:INCLUDE = "$sdkBase\Include\$sdkVer\ucrt;$sdkBase\Include\$sdkVer\shared;$sdkBase\Include\$sdkVer\um;$env:INCLUDE"
            $env:LIB = "$sdkBase\Lib\$sdkVer\ucrt\x64;$sdkBase\Lib\$sdkVer\um\x64;$env:LIB"
        }
    }
    
    $msvcBase = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $clDir))
    $env:INCLUDE = "$msvcBase\include;$env:INCLUDE"
    $env:LIB = "$msvcBase\lib\x64;$env:LIB"
    
    $compileOutput = & cl.exe /nologo /O2 /GL /Oi /Ot /arch:AVX2 /fp:fast /std:c++17 /EHsc `
        /D_CRT_SECURE_NO_WARNINGS /DNOMINMAX `
        /Fecount_contexts.exe ..\count_contexts.cpp `
        /link /LTCG /OPT:REF /OPT:ICF kernel32.lib 2>&1
    
    if ($LASTEXITCODE -ne 0) {
        Write-Host ""
        Write-Host "Compilation failed!" -ForegroundColor Red
        Write-Host $compileOutput
        Set-Location ..
        exit 1
    }
} else {
    # MinGW сборка
    & $compiler -O3 -march=native -mavx2 -flto -std=c++17 -ffast-math `
        -o count_contexts.exe ..\count_contexts.cpp -lstdc++fs -static
    
    if ($LASTEXITCODE -ne 0) {
        Write-Host ""
        Write-Host "Compilation failed!" -ForegroundColor Red
        Set-Location ..
        exit 1
    }
}

Write-Host ""
Write-Host "Build complete!" -ForegroundColor Green
Write-Host "Executable: build\count_contexts.exe" -ForegroundColor Cyan

if (Test-Path "count_contexts.exe") {
    $file = Get-Item "count_contexts.exe"
    Write-Host "Size: $([math]::Round($file.Length / 1MB, 2)) MB" -ForegroundColor Gray
}

Set-Location ..

