@echo off
cd /d "%~dp0"

echo Found MSVC compiler and Windows SDK directly.
echo MSVC Version: 14.44.35207
echo SDK Version: 10.0.26100.0
echo.

:: Установка путей
set "MSVC_PATH=C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Tools\MSVC\14.44.35207"
set "SDK_PATH=C:\Program Files (x86)\Windows Kits\10"
set "SDK_VER=10.0.26100.0"

set "CL_PATH=%MSVC_PATH%\bin\Hostx64\x64"

:: Добавляем в PATH
set "PATH=%CL_PATH%;%PATH%"

:: Устанавливаем INCLUDE
set "INCLUDE=%MSVC_PATH%\include;%MSVC_PATH%\atlmfc\include"
set "INCLUDE=%INCLUDE%;%SDK_PATH%\Include\%SDK_VER%\ucrt"
set "INCLUDE=%INCLUDE%;%SDK_PATH%\Include\%SDK_VER%\shared"
set "INCLUDE=%INCLUDE%;%SDK_PATH%\Include\%SDK_VER%\um"

:: Устанавливаем LIB
set "LIB=%MSVC_PATH%\lib\x64;%MSVC_PATH%\atlmfc\lib\x64"
set "LIB=%LIB%;%SDK_PATH%\Lib\%SDK_VER%\ucrt\x64"
set "LIB=%LIB%;%SDK_PATH%\Lib\%SDK_VER%\um\x64"

if not exist "build" mkdir build
cd build

echo Compiling...
cl.exe /nologo /O2 /GL /Oi /Ot /arch:AVX2 /fp:fast /std:c++17 /EHsc /D_CRT_SECURE_NO_WARNINGS /DNOMINMAX /Fecount_contexts.exe ..\count_contexts.cpp /link /LTCG /OPT:REF /OPT:ICF kernel32.lib

if %errorlevel% equ 0 (
    echo.
    echo Build successful!
    echo Executable: build\count_contexts.exe
    echo.
    echo Trying to run check...
    count_contexts.exe ..\..
) else (
    echo.
    echo Build failed!
)

cd ..
pause

