# PowerShell скрипт для запуска C++ версии с правильной кодировкой UTF-8

# Устанавливаем кодировку вывода на UTF-8
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8

# Переходим в директорию скрипта
Set-Location $PSScriptRoot

# Ищем исполняемый файл
$executable = $null
if (Test-Path "build\Release\count_contexts.exe") {
    $executable = "build\Release\count_contexts.exe"
} elseif (Test-Path "build\count_contexts.exe") {
    $executable = "build\count_contexts.exe"
} else {
    Write-Host "Executable not found! Please build first." -ForegroundColor Red
    Write-Host ""
    Write-Host "Try one of these:"
    Write-Host "  - build_simple.bat  (interactive menu)"
    Write-Host "  - build_msvc.bat    (Visual Studio)"
    Write-Host "  - build_mingw.bat   (MinGW-w64)"
    Write-Host "  - build.bat         (CMake)"
    Read-Host "Press Enter to exit"
    exit 1
}

# Проверяем аргументы
if ($args.Count -eq 0) {
    Write-Host "Usage: .\run_cpp.ps1 `"path\to\logs`" [num_workers]" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "Example: .\run_cpp.ps1 `"C:\TJ_Logs\TJ_Logs`" 16"
    Read-Host "Press Enter to exit"
    exit 1
}

Write-Host "Running C++ version..." -ForegroundColor Green
Write-Host ""

# Запускаем программу
if ($args.Count -eq 1) {
    & $executable $args[0]
} else {
    & $executable $args[0] $args[1]
}

Read-Host "Press Enter to exit"

