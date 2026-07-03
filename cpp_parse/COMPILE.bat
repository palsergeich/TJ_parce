@echo off
chcp 65001 >nul 2>&1
cd /d "%~dp0"

echo ========================================
echo Компиляция count_contexts.cpp
echo ========================================
echo.

if not exist build mkdir build
cd build

:: Пробуем g++ (MinGW)
where g++ >nul 2>&1
if %errorlevel% equ 0 (
    echo [OK] Найден g++ (MinGW-w64)
    echo Компиляция с оптимизациями...
    echo.
    g++ -O3 -flto -std=c++17 -ffast-math -o count_contexts.exe ..\count_contexts.cpp -lstdc++fs -static
    if %errorlevel% equ 0 (
        echo.
        echo ========================================
        echo [УСПЕХ] Компиляция завершена!
        echo ========================================
        echo Исполняемый файл: build\count_contexts.exe
        if exist count_contexts.exe (
            for %%A in (count_contexts.exe) do echo Размер: %%~zA байт
        )
        echo.
        cd ..
        pause
        exit /b 0
    ) else (
        echo.
        echo [ОШИБКА] Компиляция не удалась!
        cd ..
        pause
        exit /b 1
    )
)

:: Пробуем cl.exe (MSVC)
where cl.exe >nul 2>&1
if %errorlevel% equ 0 (
    echo [OK] Найден cl.exe (MSVC)
    echo Компиляция с оптимизациями...
    echo.
    cl.exe /O2 /GL /Oi /Ot /arch:AVX2 /fp:fast /std:c++17 /EHsc /D_CRT_SECURE_NO_WARNINGS /Fecount_contexts.exe ..\count_contexts.cpp /link /LTCG /OPT:REF /OPT:ICF
    if %errorlevel% equ 0 (
        echo.
        echo ========================================
        echo [УСПЕХ] Компиляция завершена!
        echo ========================================
        echo Исполняемый файл: build\count_contexts.exe
        if exist count_contexts.exe (
            for %%A in (count_contexts.exe) do echo Размер: %%~zA байт
        )
        echo.
        cd ..
        pause
        exit /b 0
    ) else (
        echo.
        echo [ОШИБКА] Компиляция не удалась!
        cd ..
        pause
        exit /b 1
    )
)

echo.
echo ========================================
echo [ОШИБКА] Компилятор не найден!
echo ========================================
echo.
echo Установите один из компиляторов:
echo.
echo 1. MinGW-w64 (g++):
echo    - Скачайте MSYS2: https://www.msys2.org/
echo    - Установите, затем: pacman -S mingw-w64-x86_64-gcc
echo    - Добавьте в PATH: C:\msys64\mingw64\bin
echo.
echo 2. Visual Studio с C++ инструментами:
echo    - Скачайте: https://visualstudio.microsoft.com/downloads/
echo    - Выберите "Desktop development with C++"
echo.
echo См. README.md для подробных инструкций.
echo.
cd ..
pause
exit /b 1

