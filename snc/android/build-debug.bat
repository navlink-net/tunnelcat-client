@echo off
setlocal EnableDelayedExpansion

:: Debug build -- no signing required.
:: Builds arm64 + amd64 Go binaries, packages assembleDebug, installs on any
:: connected device / emulator.

set ROOT=%~dp0..\..
set OUTDIR=%ROOT%\build\android

if exist "%ROOT%\.env" (
    for /f "tokens=1* delims==" %%a in ('findstr /v "^#" "%ROOT%\.env" ^| findstr /v "^$"') do (
        if not "%%a"=="" set %%a=%%b
    )
)

if "!APP_VERSION!" neq "" (
    set VERSION=!APP_VERSION!
) else (
    for /f "tokens=*" %%i in ('powershell -NoProfile -Command "Get-Date -Format yyyyMMddHHmm"') do set VERSION=%%i
    if "!VERSION!"=="" set VERSION=dev
)

echo.
echo === ShortNerdCat Android Debug Build ===
echo Version : !VERSION!
echo Out dir : %OUTDIR%
echo.

if not exist "%OUTDIR%" mkdir "%OUTDIR%"

set LDFLAGS=-s -w -X tunnel_cat/snc/core.Version=!VERSION!

:: 0. Copy shared assets
echo [0/4] Copying assets...
if not exist "app\src\main\res\drawable" mkdir "app\src\main\res\drawable"
copy /y "%ROOT%\snc\win\windows\assets\logo.png" "app\src\main\res\drawable\logo.png" >nul
if errorlevel 1 (echo ERROR: logo.png not found. & exit /b 1)
echo   OK

:: 1a. arm64
echo [1/4] Building snc-core android/arm64...
set GOOS=android
set GOARCH=arm64
set CGO_ENABLED=0
go build -tags with_utls -ldflags="!LDFLAGS!" -o "%OUTDIR%\snc-core.arm64" .\cmd\snc-core\
if errorlevel 1 (echo ERROR: arm64 build failed. & exit /b 1)
echo   OK

:: 1b. amd64 (emulator)
echo [2/4] Building snc-core linux/amd64...
set GOOS=linux
set GOARCH=amd64
go build -tags with_utls -ldflags="!LDFLAGS!" -o "%OUTDIR%\snc-core.amd64" .\cmd\snc-core\
if errorlevel 1 (echo ERROR: amd64 build failed. & exit /b 1)
echo   OK

:: 2. APK
echo [3/4] Packaging debug APK...
if not exist "%~dp0app\src\main\jniLibs\arm64-v8a" mkdir "%~dp0app\src\main\jniLibs\arm64-v8a"
copy /y "%OUTDIR%\snc-core.arm64" "%~dp0app\src\main\jniLibs\arm64-v8a\libsnc_core.so" >nul
if not exist "%~dp0app\src\main\jniLibs\x86_64" mkdir "%~dp0app\src\main\jniLibs\x86_64"
copy /y "%OUTDIR%\snc-core.amd64" "%~dp0app\src\main\jniLibs\x86_64\libsnc_core.so" >nul

call "%~dp0gradlew.bat" assembleDebug --no-daemon -PappVersion=!VERSION!
if errorlevel 1 (echo ERROR: Gradle build failed. & exit /b 1)
echo   OK: app\build\outputs\apk\debug\app-debug.apk

:: 3. Install
echo [4/4] Installing...
set ADB=%LOCALAPPDATA%\Android\Sdk\platform-tools\adb.exe
if not exist "!ADB!" set ADB=adb
"!ADB!" install -r "%~dp0app\build\outputs\apk\debug\app-debug.apk"
if errorlevel 1 (echo WARNING: install failed ^(no device?^). APK is at app\build\outputs\apk\debug\)

echo.
echo Build complete -- version !VERSION!
endlocal
