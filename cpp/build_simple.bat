@echo off
cd /d "%~dp0"

echo ========================================
echo C++ Context Counter - Build Script
echo ========================================
echo.
echo This script will attempt to build the C++ version.
echo.
echo Choose your compiler:
echo   1. MSVC (Visual Studio) - Recommended for Windows
echo   2. MSVC Direct (if option 1 fails)
echo   3. MinGW-w64 (GCC) - Alternative for Windows
echo   4. CMake (requires CMake installed)
echo   Q. Quit
echo.

choice /C 1234Q /N /M "Select option (1/2/3/4/Q): "

if errorlevel 5 goto :EOF
if errorlevel 4 goto cmake_build
if errorlevel 3 goto mingw_build
if errorlevel 2 goto msvc_direct_build
if errorlevel 1 goto msvc_build

:msvc_build
echo.
echo Building with MSVC...
call build_msvc.bat
goto :EOF

:msvc_direct_build
echo.
echo Building with MSVC (direct method)...
call build_direct.bat
goto :EOF

:mingw_build
echo.
echo Building with MinGW...
call build_mingw.bat
goto :EOF

:cmake_build
echo.
echo Building with CMake...
call build.bat
goto :EOF

