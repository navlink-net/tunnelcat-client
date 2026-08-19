@echo off
setlocal EnableDelayedExpansion

:: ── ShortNerdCat Android client deploy script ────────────────────────────────
::
:: Builds the release APK and uploads it to the arbiter.
::
:: Usage:
::   deploy.bat [--skip-build] [--password <keystore-password>]
::
:: Optional env var overrides:
::   set ARBITER_URL=https://navlink.net
::   set UPLOAD_KEY=<key>
::   set SNC_SIGN_PASSWORD=<keystore-password>

set SCRIPT_DIR=%~dp0
set APK_SRC=%SCRIPT_DIR%app\build\outputs\apk\release\app-release.apk
if "!ARBITER_URL!"=="" (
  echo ERROR: set ARBITER_URL to your own arbiter, e.g. https://your-arbiter-host
  exit /b 1
)
if "!UPLOAD_KEY!"=="" (
  echo ERROR: set UPLOAD_KEY to your own arbiter's admin upload key
  exit /b 1
)
set SKIP_BUILD=0

:: VERSION is computed once here so the local artifact and the upload stamp match.
for /f "tokens=*" %%i in ('powershell -NoProfile -Command "Get-Date -Format yyyyMMddHHmm"') do set VERSION=%%i
if "!VERSION!"=="" set VERSION=dev

set APK_OUT=%SCRIPT_DIR%shortnerdcat-%VERSION%.apk

:parse_args
if "%~1"=="" goto parse_end
if not "%~1"=="--skip-build" goto try_password
set SKIP_BUILD=1
shift
goto parse_args
:try_password
if not "%~1"=="--password" goto unknown_arg
set SNC_SIGN_PASSWORD=%~2
shift
shift
goto parse_args
:unknown_arg
echo Unknown option: %~1
exit /b 1
:parse_end

echo.
echo === ShortNerdCat Android Deploy ===
echo Version: !VERSION!
echo.

:: ── 1. Build release APK ─────────────────────────────────────────────────────
if "!SKIP_BUILD!"=="0" (
    echo [1/3] Building release APK...
    call "%SCRIPT_DIR%build.bat"
    if errorlevel 1 (echo ERROR: build failed. & exit /b 1)
) else (
    echo [1/3] Skipping build.
)

if not exist "!APK_SRC!" (echo ERROR: APK not found at !APK_SRC! & exit /b 1)

:: ── 2. Copy to versioned filename ────────────────────────────────────────────
echo [2/3] Copying to shortnerdcat-!VERSION!.apk...
copy /Y "!APK_SRC!" "!APK_OUT!" >nul
if errorlevel 1 (echo ERROR: copy failed. & exit /b 1)

:: ── 3. Upload to arbiter ──────────────────────────────────────────────────────
echo [3/3] Uploading version !VERSION! to !ARBITER_URL!...
for /f %%h in ('curl -sk -L -H "Expect:" -w "%%{http_code}" -o NUL -X POST "!ARBITER_URL!/admin/downloads/upload" -H "Authorization: Bearer !UPLOAD_KEY!" -F "binary_type=android" -F "version=!VERSION!" -F "file=@!APK_OUT!;filename=shortnerdcat.apk"') do set HTTP_STATUS=%%h
echo HTTP !HTTP_STATUS!
if "!HTTP_STATUS!"=="200" goto apk_ok
if "!HTTP_STATUS!"=="303" goto apk_ok
echo ERROR: upload failed (HTTP !HTTP_STATUS!). & exit /b 1
:apk_ok

del "!APK_OUT!" >nul 2>&1

echo.
echo Deploy complete: !ARBITER_URL!/download
endlocal
