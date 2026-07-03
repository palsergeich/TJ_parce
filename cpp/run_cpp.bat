@echo off
cd /d "%~dp0"

REM Устанавливаем кодировку консоли на UTF-8
chcp 65001 >nul 2>&1

if exist "build\Release\count_contexts.exe" (
    set "EXECUTABLE=build\Release\count_contexts.exe"
) else if exist "build\count_contexts.exe" (
    set "EXECUTABLE=build\count_contexts.exe"
) else (
    echo Executable not found! Please build first.
    echo.
    echo Try one of these:
    echo   - build_simple.bat  (interactive menu)
    echo   - build_msvc.bat    (Visual Studio)
    echo   - build_mingw.bat   (MinGW-w64)
    echo   - build.bat         (CMake)
    pause
    exit /b 1
)

if "%~1"=="" (
    echo Usage: run_cpp.bat "path\to\logs" [num_workers]
    echo.
    echo Example: run_cpp.bat "C:\TJ_Logs\TJ_Logs" 16
    pause
    exit /b 1
)

echo Running C++ version...
echo.

if "%~2"=="" (
    %EXECUTABLE% "%~1"
) else (
    %EXECUTABLE% "%~1" %2
)

pause

