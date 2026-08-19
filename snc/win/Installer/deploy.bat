@echo off
setlocal EnableDelayedExpansion

:: ── SncInstaller deploy script ──────────────────────────────────────────────
::
:: Publishes the WPF installer (self-contained single-file EXE), zips it, and
:: uploads to the arbiter under binary_type=windows-installer -- a separate
:: slot from the raw client (binary_type=windows, handled by ..\deploy.bat).
:: The installer does NOT bundle the client -- it downloads the latest client
:: build at runtime via /api/downloads/info, so its own DEPLOY_VERSION here just
:: needs to be fresher than whatever pre-installer client's core.Version is
:: currently running, to trigger the OTA hand-off in tunnel_cat/snc/core/updater.go.
::
:: Usage:
::   deploy.bat [--skip-build]
::
:: Optional env var overrides:
::   set ARBITER_URL=https://navlink.net
::   set UPLOAD_KEY=<key>

set SCRIPT_DIR=%~dp0
set PROJ_DIR=%SCRIPT_DIR%SncInstaller
set PUBLISH_DIR=%PROJ_DIR%\bin\Release\net9.0-windows\win-x64\publish

for /f %%v in ('powershell -NoProfile -Command "Get-Date -Format yyyyMMddHHmm"') do set DEPLOY_VERSION=%%v

set EXE=%PUBLISH_DIR%\SncInstaller.exe
set ZIP=%SCRIPT_DIR%shortnerdcat-installer-%DEPLOY_VERSION%.zip
if "!ARBITER_URL!"=="" (
  echo ERROR: set ARBITER_URL to your own arbiter, e.g. https://your-arbiter-host
  exit /b 1
)
if "!UPLOAD_KEY!"=="" (
  echo ERROR: set UPLOAD_KEY to your own arbiter's admin upload key
  exit /b 1
)
set "SKIP_BUILD=0"

:parse_args
if "%~1"=="--skip-build" (set "SKIP_BUILD=1" & shift & goto parse_args)
if not "%~1"=="" (echo Unknown option: %~1 & exit /b 1)

echo.
echo === SncInstaller Deploy ===
echo Version: !DEPLOY_VERSION!
echo.

:: ── 1. Publish ────────────────────────────────────────────────────────────────
if "!SKIP_BUILD!"=="0" (
    echo [1/3] Publishing...
    dotnet publish "%PROJ_DIR%\SncInstaller.csproj" -c Release
    if errorlevel 1 (echo ERROR: publish failed. & exit /b 1)
) else (
    echo [1/3] Skipping build.
)

if not exist "!EXE!" (echo ERROR: !EXE! not found. & exit /b 1)

:: Same staleness guard as ..\deploy.bat -- --skip-build must only reuse an
:: EXE built moments ago, not some older leftover publish output.
if "!SKIP_BUILD!"=="1" (
    for /f %%t in ('powershell -NoProfile -Command "(Get-Item '!EXE!').LastWriteTime.ToString('yyyyMMddHHmm')"') do set EXE_MTIME=%%t
    for /f %%d in ('powershell -NoProfile -Command "[int][math]::Abs(([datetime]::ParseExact('!DEPLOY_VERSION!','yyyyMMddHHmm',$null) - [datetime]::ParseExact('!EXE_MTIME!','yyyyMMddHHmm',$null)).TotalMinutes)"') do set EXE_AGE_MIN=%%d
    if !EXE_AGE_MIN! GTR 15 (
        echo ERROR: --skip-build with a stale !EXE! -- last built !EXE_MTIME!, but this deploy is labeled !DEPLOY_VERSION! ^(!EXE_AGE_MIN! min apart^).
        echo        Re-run without --skip-build, or only use --skip-build to retry the SAME build's upload.
        exit /b 1
    )
)

:: ── 2. Package into ZIP ───────────────────────────────────────────────────────
echo [2/3] Packaging shortnerdcat-installer-%DEPLOY_VERSION%.zip...
if exist "!ZIP!" del "!ZIP!"
powershell -NoProfile -Command "Compress-Archive -Force -Path '!EXE!' -DestinationPath '!ZIP!'"
if errorlevel 1 (echo ERROR: zip failed. & exit /b 1)

:: ── 3. Upload to arbiter ──────────────────────────────────────────────────────
echo [3/3] Uploading version !DEPLOY_VERSION! to !ARBITER_URL!...
for /f %%h in ('curl -sk -L -H "Expect:" -w "%%{http_code}" -o NUL -X POST "!ARBITER_URL!/admin/downloads/upload" -H "Authorization: Bearer !UPLOAD_KEY!" -F "binary_type=windows-installer" -F "version=!DEPLOY_VERSION!" -F "file=@!ZIP!;filename=shortnerdcat-installer.zip"') do set HTTP_STATUS=%%h
echo HTTP !HTTP_STATUS!
if "!HTTP_STATUS!"=="200" goto ok
if "!HTTP_STATUS!"=="303" goto ok
echo ERROR: upload failed (HTTP !HTTP_STATUS!). & exit /b 1
:ok

del "!ZIP!" >nul 2>&1

echo.
echo Deploy complete: !ARBITER_URL!/download
endlocal
