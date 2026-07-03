@echo off
cd /d "%~dp0"

echo Building C++ version with AVX2 optimizations...
echo.

if not exist build mkdir build
cd build

echo Configuring with CMake...
cmake .. -G "Visual Studio 16 2019" -A x64
if errorlevel 1 (
    echo.
    echo CMake configuration failed!
    echo Make sure you have Visual Studio 2019 or newer installed.
    pause
    exit /b 1
)

echo.
echo Building Release version...
cmake --build . --config Release -j
if errorlevel 1 (
    echo.
    echo Build failed!
    pause
    exit /b 1
)

echo.
echo Build complete!
echo Executable: build\Release\count_contexts.exe
echo.
pause

