@echo off
setlocal EnableDelayedExpansion

:: ── ShortNerdCat Android client build script ────────────────────────────────
::
:: Outputs:
::   android-client\out\snc-core.arm64   — Go binary for Android aarch64
::   android-client\app\build\outputs\apk\release\app-release.apk  — APK (if Gradle project exists)
::
:: Prerequisites:
::   go (module-aware build, GOOS=android supported since Go 1.16)
::   Android NDK is NOT required — CGO_ENABLED=0
::   For APK: Android SDK with Gradle wrapper at android-client\gradlew.bat
::
:: Note: 32-bit arm (armv7) is intentionally not built — it requires CGO
::   (Android NDK), and all devices since Android 5+ support arm64.
::
:: Signing: uses d:\REPO\tf38key.jks (alias tf38key, PKCS12).
::   Pass SNC_SIGN_PASSWORD as env var or the script will prompt for it.

set ROOT=%~dp0..\..
set OUTDIR=%ROOT%\build\android

:: Load variables from .env if present (gitignored — keeps secrets out of the repo).
if exist "%ROOT%\.env" (
    for /f "tokens=1* delims==" %%a in ('findstr /v "^#" "%ROOT%\.env" ^| findstr /v "^$"') do (
        if not "%%a"=="" set %%a=%%b
    )
)

:: Command-line args override .env values.
:parse_args
if not "%~1"=="--password" goto parse_end
set SNC_SIGN_PASSWORD=%~2
shift
shift
goto parse_args
:parse_end
if not "%~1"=="" (echo Unknown option: %~1 & exit /b 1)

if "!APP_VERSION!" neq "" (
    set VERSION=!APP_VERSION!
) else (
    for /f "tokens=*" %%i in ('powershell -NoProfile -Command "Get-Date -Format yyyyMMddHHmm"') do set VERSION=%%i
    if "!VERSION!"=="" (echo ERROR: could not compute build version ^(Get-Date failed^) -- refusing to fall back to a placeholder & exit /b 1)
)

echo.
echo === ShortNerdCat Android Build ===
echo Version : !VERSION!
echo Out dir : %OUTDIR%
echo.

if not exist "%OUTDIR%" mkdir "%OUTDIR%"

:: ── 0. Copy shared assets from win-client ───────────────────────────────────
echo [0/2] Copying assets...
if not exist "app\src\main\res\drawable" mkdir "app\src\main\res\drawable"
copy /y "%ROOT%\snc\win\windows\assets\logo.png" "app\src\main\res\drawable\logo.png" >nul
if errorlevel 1 (
    echo ERROR: logo.png not found at snc\win\windows\assets\logo.png
    exit /b 1
)
echo   OK: logo.png copied

:: ── 1a. Go binary: arm64 (primary, all modern Android) ─────────────────────
echo [1/4] Building snc-core for android/arm64...
set GOOS=android
set GOARCH=arm64
set CGO_ENABLED=0
go build -tags with_utls -ldflags="-s -w -X tunnel_cat/snc/core.Version=!VERSION!" ^
    -o "%OUTDIR%\snc-core.arm64" ^
    .\cmd\snc-core\
if errorlevel 1 (
    echo ERROR: arm64 build failed.
    exit /b 1
)
echo   OK: %OUTDIR%\snc-core.arm64

:: ── 1b. Go binary: armv7 (32-bit ARM) ───────────────────────────────────────
:: Some ultra-budget devices ship a 32-bit-only system image despite a
:: 64-bit-capable chip (e.g. Poco C51 / Redmi A-series on Unisoc T612) --
:: without this, they get "app not compatible with this device" at install
:: since the APK has no matching ABI. android/arm requires external (cgo)
:: linking in the Go toolchain (unlike android/arm64), so this needs the NDK.
echo [2/4] Building snc-core for android/arm (armv7, 32-bit)...
if "!ANDROID_NDK_HOME!"=="" (
    set NDK_ROOT=%LOCALAPPDATA%\Android\Sdk\ndk
    if exist "!NDK_ROOT!" (
        for /f "delims=" %%v in ('dir /b /ad /o-n "!NDK_ROOT!" 2^>nul') do (
            if "!ANDROID_NDK_HOME!"=="" set ANDROID_NDK_HOME=!NDK_ROOT!\%%v
        )
    )
)
if "!ANDROID_NDK_HOME!"=="" (
    echo ERROR: Android NDK not found. Set ANDROID_NDK_HOME or install it via Android Studio's SDK Manager.
    exit /b 1
)
set NDK_CC=!ANDROID_NDK_HOME!\toolchains\llvm\prebuilt\windows-x86_64\bin\armv7a-linux-androideabi26-clang.cmd
set NDK_CXX=!ANDROID_NDK_HOME!\toolchains\llvm\prebuilt\windows-x86_64\bin\armv7a-linux-androideabi26-clang++.cmd
if not exist "!NDK_CC!" (
    echo ERROR: NDK clang not found at !NDK_CC! ^(ANDROID_NDK_HOME=!ANDROID_NDK_HOME!^)
    exit /b 1
)
if not exist "!NDK_CXX!" (
    echo ERROR: NDK clang++ not found at !NDK_CXX! ^(ANDROID_NDK_HOME=!ANDROID_NDK_HOME!^)
    exit /b 1
)
echo   NDK: !ANDROID_NDK_HOME!
set GOOS=android
set GOARCH=arm
set GOARM=7
set CGO_ENABLED=1
set CC=!NDK_CC!
set CXX=!NDK_CXX!
go build -tags with_utls -ldflags="-s -w -X tunnel_cat/snc/core.Version=!VERSION!" ^
    -o "%OUTDIR%\snc-core.armv7" ^
    .\cmd\snc-core\
if errorlevel 1 (
    echo ERROR: armv7 build failed.
    exit /b 1
)
set CGO_ENABLED=0
set CC=
set CXX=
set GOARM=
echo   OK: %OUTDIR%\snc-core.armv7

:: ── 1c. Go binary: amd64 (x86_64 emulator — uses linux/amd64 target) ────────
echo [3/4] Building snc-core for linux/amd64 (emulator)...
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
go build -tags with_utls -ldflags="-s -w -X tunnel_cat/snc/core.Version=!VERSION!" ^
    -o "%OUTDIR%\snc-core.amd64" ^
    .\cmd\snc-core\
if errorlevel 1 (
    echo ERROR: amd64 build failed.
    exit /b 1
)
echo   OK: %OUTDIR%\snc-core.amd64

:: ── 4. APK via Gradle ────────────────────────────────────────────────────────
echo [4/4] APK...

:: Ask for signing password if not already set in the environment.
if "!SNC_SIGN_PASSWORD!"=="" (
    set /p SNC_SIGN_PASSWORD="Keystore password (d:\REPO\tf38key.jks): "
)
if "!SNC_SIGN_PASSWORD!"=="" (
    echo ERROR: signing password is required.
    exit /b 1
)

:: Copy Go binaries into jniLibs as .so so Android installs them with exec permission.
if not exist "%~dp0app\src\main\jniLibs\arm64-v8a" mkdir "%~dp0app\src\main\jniLibs\arm64-v8a"
copy /y "%OUTDIR%\snc-core.arm64" "%~dp0app\src\main\jniLibs\arm64-v8a\libsnc_core.so" >nul
if not exist "%~dp0app\src\main\jniLibs\armeabi-v7a" mkdir "%~dp0app\src\main\jniLibs\armeabi-v7a"
copy /y "%OUTDIR%\snc-core.armv7" "%~dp0app\src\main\jniLibs\armeabi-v7a\libsnc_core.so" >nul
if not exist "%~dp0app\src\main\jniLibs\x86_64" mkdir "%~dp0app\src\main\jniLibs\x86_64"
copy /y "%OUTDIR%\snc-core.amd64" "%~dp0app\src\main\jniLibs\x86_64\libsnc_core.so" >nul

call "%~dp0gradlew.bat" assembleRelease --no-daemon -PappVersion=!VERSION!
if errorlevel 1 (
    echo ERROR: Gradle build failed.
    exit /b 1
)
echo   OK: app\build\outputs\apk\release\app-release.apk

:done
echo.
echo Build complete.
endlocal
