@echo off
cd /d "%~dp0"

echo Compiling count_contexts.cpp...
echo.

if not exist build mkdir build
cd build

:: Пробуем g++ (MinGW)
where g++ >nul 2>&1
if %errorlevel% equ 0 (
    echo Using g++ (MinGW-w64)...
    g++ -O3 -march=native -mavx2 -flto -std=c++17 -ffast-math -o count_contexts.exe ..\count_contexts.cpp -lstdc++fs -static
    if %errorlevel% equ 0 (
        echo.
        echo Build successful!
        echo Executable: build\count_contexts.exe
        cd ..
        exit /b 0
    )
)

:: Пробуем cl.exe (MSVC)
where cl.exe >nul 2>&1
if %errorlevel% equ 0 (
    echo Using cl.exe (MSVC)...
    cl.exe /O2 /GL /Oi /Ot /arch:AVX2 /fp:fast /std:c++17 /EHsc /D_CRT_SECURE_NO_WARNINGS /Fecount_contexts.exe ..\count_contexts.cpp /link /LTCG /OPT:REF /OPT:ICF
    if %errorlevel% equ 0 (
        echo.
        echo Build successful!
        echo Executable: build\count_contexts.exe
        cd ..
        exit /b 0
    )
)

echo.
echo ERROR: No compiler found!
echo.
echo Please install one of:
echo   1. MinGW-w64 (g++)
echo   2. Visual Studio with C++ tools
echo.
cd ..
pause
exit /b 1

