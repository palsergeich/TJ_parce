@echo off
cd /d "%~dp0"

echo Building C++ version with direct compiler call...
echo.
echo This version tries to find and use cl.exe directly without vcvarsall.bat
echo.

:: Список возможных путей к Visual Studio
set "PATHS="
set "PATHS=%PATHS%;C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Tools\MSVC"
set "PATHS=%PATHS%;C:\Program Files\Microsoft Visual Studio\2022\Professional\VC\Tools\MSVC"
set "PATHS=%PATHS%;C:\Program Files\Microsoft Visual Studio\2022\Enterprise\VC\Tools\MSVC"
set "PATHS=%PATHS%;C:\Program Files (x86)\Microsoft Visual Studio\2019\Community\VC\Tools\MSVC"
set "PATHS=%PATHS%;C:\Program Files (x86)\Microsoft Visual Studio\2019\Professional\VC\Tools\MSVC"
set "PATHS=%PATHS%;C:\Program Files (x86)\Microsoft Visual Studio\2019\Enterprise\VC\Tools\MSVC"

set "FOUND=0"

for %%P in (%PATHS%) do (
    if exist "%%P" (
        echo Checking: %%P
        for /f "delims=" %%V in ('dir /b /ad /o-n "%%P" 2^>nul') do (
            set "MSVC_VER=%%V"
            set "MSVC_BASE=%%P\%%V"
            
            if exist "!MSVC_BASE!\bin\Hostx64\x64\cl.exe" (
                echo Found MSVC version: !MSVC_VER!
                
                set "CL_PATH=!MSVC_BASE!\bin\Hostx64\x64"
                set "INCLUDE=!MSVC_BASE!\include;!MSVC_BASE!\atlmfc\include"
                set "LIB=!MSVC_BASE!\lib\x64;!MSVC_BASE!\atlmfc\lib\x64"
                set "PATH=!CL_PATH!;%PATH%"
                
                :: Добавляем Windows SDK
                if exist "C:\Program Files (x86)\Windows Kits\10\Include" (
                    for /f "delims=" %%K in ('dir /b /ad /o-n "C:\Program Files (x86)\Windows Kits\10\Include" 2^>nul') do (
                        set "SDK_VER=%%K"
                        set "SDK_BASE=C:\Program Files (x86)\Windows Kits\10"
                        set "INCLUDE=!INCLUDE!;!SDK_BASE!\Include\!SDK_VER!\ucrt;!SDK_BASE!\Include\!SDK_VER!\shared;!SDK_BASE!\Include\!SDK_VER!\um"
                        set "LIB=!LIB!;!SDK_BASE!\Lib\!SDK_VER!\ucrt\x64;!SDK_BASE!\Lib\!SDK_VER!\um\x64"
                        goto :found_sdk
                    )
                )
                :found_sdk
                
                set "FOUND=1"
                goto :compile
            )
        )
    )
)

if "%FOUND%"=="0" (
    echo ERROR: Could not find Visual Studio MSVC compiler!
    echo.
    echo Searched in:
    for %%P in (%PATHS%) do echo   %%P
    echo.
    echo Please try one of these alternatives:
    echo   1. Install Visual Studio 2019 or 2022 with C++ tools
    echo   2. Use MinGW: build_mingw.bat
    echo.
    pause
    exit /b 1
)

:compile
setlocal enabledelayedexpansion

echo.
echo Using compiler: !CL_PATH!\cl.exe
echo.
echo Compiler version:
cl.exe 2>&1 | findstr "Version"
echo.

if not exist build mkdir build
cd build

echo Compiling with optimizations...
echo Flags: /O2 /GL /Oi /Ot /arch:AVX2 /std:c++17
echo.

cl.exe /nologo /O2 /GL /Oi /Ot /arch:AVX2 /fp:fast /std:c++17 /EHsc ^
    /D_CRT_SECURE_NO_WARNINGS /DNOMINMAX ^
    /Fecount_contexts.exe ..\count_contexts.cpp ^
    /link /LTCG /OPT:REF /OPT:ICF kernel32.lib

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

