@echo off
cd /d "%~dp0"

echo Building C++ version with MinGW-w64...
echo.

:: Проверяем наличие g++
where g++ >nul 2>&1
if errorlevel 1 (
    echo ERROR: g++ not found!
    echo.
    echo MinGW-w64 is not installed or not in PATH.
    echo.
    echo === QUICK INSTALL ===
    echo.
    echo Option 1: MSYS2 (recommended)
    echo   1. Download: https://www.msys2.org/
    echo   2. Install to C:\msys64
    echo   3. Open "MSYS2 MinGW 64-bit" from Start Menu
    echo   4. Run: pacman -S mingw-w64-x86_64-gcc
    echo   5. Add to PATH: C:\msys64\mingw64\bin
    echo.
    echo Option 2: Standalone
    echo   1. Download: https://github.com/niXman/mingw-builds-binaries/releases
    echo   2. Get: x86_64-*-release-posix-seh-ucrt-*.7z
    echo   3. Extract to C:\mingw64
    echo   4. Add to PATH: C:\mingw64\bin
    echo.
    echo See INSTALL_MINGW.md for detailed instructions.
    echo.
    pause
    exit /b 1
)

echo Found g++:
g++ --version | findstr "g++"
echo.

if not exist build mkdir build
cd build

echo Compiling with optimizations: -O3 -march=native -mavx2 -flto
echo.

g++ -O3 -march=native -mavx2 -flto -std=c++17 -ffast-math ^
    -o count_contexts.exe ..\count_contexts.cpp -lstdc++fs -static

if errorlevel 1 (
    echo.
    echo Compilation failed!
    cd ..
    pause
    exit /b 1
)

echo.
echo Build complete!
echo Executable: build\count_contexts.exe
echo Size:
dir count_contexts.exe | findstr "count_contexts.exe"
echo.

cd ..
pause

