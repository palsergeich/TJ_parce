@echo off
cd /d "%~dp0"

echo Building C++ version with MSVC directly (without CMake)...
echo.

:: Ищем Visual Studio
set "VSWHERE=%ProgramFiles(x86)%\Microsoft Visual Studio\Installer\vswhere.exe"

if exist "%VSWHERE%" (
    echo Found vswhere.exe, searching for Visual Studio...
    for /f "usebackq tokens=*" %%i in (`"%VSWHERE%" -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath`) do (
        set "VSINSTALLDIR=%%i"
    )
)

if not defined VSINSTALLDIR (
    echo Visual Studio not found with vswhere, trying common paths...
    
    if exist "C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvarsall.bat" (
        set "VSINSTALLDIR=C:\Program Files\Microsoft Visual Studio\2022\Community"
    ) else if exist "C:\Program Files (x86)\Microsoft Visual Studio\2019\Community\VC\Auxiliary\Build\vcvarsall.bat" (
        set "VSINSTALLDIR=C:\Program Files (x86)\Microsoft Visual Studio\2019\Community"
    ) else (
        echo.
        echo ERROR: Visual Studio not found!
        echo.
        echo Please install Visual Studio 2019 or newer with C++ tools.
        echo Or use MinGW version: build_mingw.bat
        echo.
        pause
        exit /b 1
    )
)

echo Found Visual Studio at: %VSINSTALLDIR%
echo.

:: Инициализируем окружение MSVC
echo Initializing MSVC environment...
call "%VSINSTALLDIR%\VC\Auxiliary\Build\vcvarsall.bat" x64 >nul 2>&1
if errorlevel 1 (
    echo Warning: vcvarsall.bat failed, trying direct compiler path...
    
    :: Пробуем найти компилятор напрямую
    set "CL_PATH=%VSINSTALLDIR%\VC\Tools\MSVC"
    
    for /f "delims=" %%i in ('dir /b /ad /o-n "%CL_PATH%" 2^>nul') do (
        if exist "%CL_PATH%\%%i\bin\Hostx64\x64\cl.exe" (
            set "COMPILER_DIR=%CL_PATH%\%%i\bin\Hostx64\x64"
            set "PATH=%COMPILER_DIR%;%PATH%"
            goto :compiler_found
        )
    )
    
    echo ERROR: Could not find cl.exe
    echo.
    echo Visual Studio with C++ tools is not installed or not found.
    echo.
    echo === SOLUTIONS ===
    echo.
    echo Option 1: Install Visual Studio (recommended for best performance)
    echo   1. Download: https://visualstudio.microsoft.com/downloads/
    echo   2. Choose "Desktop development with C++" during installation
    echo.
    echo Option 2: Use MinGW-w64 (easier, faster to install)
    echo   1. See INSTALL_MINGW.md for instructions
    echo   2. Then run: build_mingw.bat
    echo.
    echo Quick MinGW install:
    echo   - Download MSYS2: https://www.msys2.org/
    echo   - Install, then run: pacman -S mingw-w64-x86_64-gcc
    echo   - Add to PATH: C:\msys64\mingw64\bin
    echo.
    pause
    exit /b 1
    
    :compiler_found
    echo Found compiler at: %COMPILER_DIR%
)

echo.
echo Compiling with optimizations: /O2 /GL /arch:AVX2 /std:c++17 /EHsc
echo.

if not exist build mkdir build
cd build

cl.exe /O2 /GL /Oi /Ot /arch:AVX2 /fp:fast /std:c++17 /EHsc /D_CRT_SECURE_NO_WARNINGS ^
    /Fecount_contexts.exe ..\count_contexts.cpp /link /LTCG /OPT:REF /OPT:ICF

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
echo.

cd ..
pause

