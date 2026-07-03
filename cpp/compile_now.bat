@echo off
cd /d "%~dp0"

if not exist "build" mkdir build
cd build

echo Compiling count_contexts.cpp...
echo.

:: Пробуем найти и использовать компилятор
:: Сначала проверяем, есть ли cl.exe в PATH
where cl.exe >nul 2>&1
if %errorlevel% equ 0 (
    echo Using MSVC from PATH...
    cl.exe /nologo /O2 /GL /Oi /Ot /arch:AVX2 /fp:fast /std:c++17 /EHsc /D_CRT_SECURE_NO_WARNINGS /DNOMINMAX /Fecount_contexts.exe ..\count_contexts.cpp /link /LTCG /OPT:REF /OPT:ICF kernel32.lib
    if %errorlevel% equ 0 (
        echo.
        echo Build successful!
        echo Executable: build\count_contexts.exe
        cd ..
        exit /b 0
    )
)

:: Пробуем найти g++
where g++.exe >nul 2>&1
if %errorlevel% equ 0 (
    echo Using MinGW from PATH...
    g++.exe -O3 -march=native -mavx2 -flto -std=c++17 -ffast-math -o count_contexts.exe ..\count_contexts.cpp -lstdc++fs -static
    if %errorlevel% equ 0 (
        echo.
        echo Build successful!
        echo Executable: build\count_contexts.exe
        cd ..
        exit /b 0
    )
)

echo.
echo ERROR: No compiler found in PATH!
echo.
echo Please either:
echo   1. Run build_simple.bat (interactive menu)
echo   2. Run build_msvc.bat (for Visual Studio)
echo   3. Run build_mingw.bat (for MinGW)
echo   4. Add compiler to PATH and try again
echo.
pause
cd ..
exit /b 1

