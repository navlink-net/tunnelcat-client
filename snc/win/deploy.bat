@echo off
setlocal EnableDelayedExpansion

:: ── ShortNerdCat Windows client deploy script ─────────────────────────────────
::
:: Builds the release EXE, packages into ZIP, uploads to arbiter via HTTP.
::
:: Usage:
::   deploy.bat [--skip-build]
::
:: Optional env var overrides:
::   set ARBITER_URL=https://navlink.net
::   set UPLOAD_KEY=<key>

set SCRIPT_DIR=%~dp0

:: VERSION is computed once here and passed to build.bat via env so both the
:: binary and the archive share an identical stamp.
for /f %%v in ('powershell -NoProfile -Command "Get-Date -Format yyyyMMddHHmm"') do set VERSION=%%v

set EXE=%SCRIPT_DIR%..\..\shortnerdcat.exe
set WINTUN=%SCRIPT_DIR%..\..\wintun.dll
set ZIP=%SCRIPT_DIR%..\..\shortnerdcat-%VERSION%.zip
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
echo === ShortNerdCat Windows Deploy ===
echo Version: !VERSION!
echo.

:: ── 1. Build ──────────────────────────────────────────────────────────────────
if "!SKIP_BUILD!"=="0" (
    echo [1/3] Building...
    call "%SCRIPT_DIR%build.bat"
    if errorlevel 1 (echo ERROR: build failed. & exit /b 1)
) else (
    echo [1/3] Skipping build.
)

if not exist "!EXE!" (echo ERROR: !EXE! not found. & exit /b 1)

:: --skip-build reuses whatever .exe is already on disk, but this VERSION was
:: computed fresh just now (line 19) -- if that .exe was actually built at some
:: EARLIER time (a stale reuse across separate deploy.bat invocations, not a
:: same-run retry), its baked-in tunnel_cat/snc/core.Version (via build.bat's
:: -ldflags) is older than the label this run is about to upload it under.
:: Real sibling bug, 2026-08-10 (Android): deploy.sh --skip-build uploaded an
:: already-built APK under a VERSION that didn't match what was actually
:: baked into it -- the file's *own* version never advanced, so every device
:: that "updated" saw no change and the client nagged forever. Windows has no
:: aapt-equivalent to read the baked Go Version back out of a compiled .exe,
:: so this uses the file's own last-write-time as a proxy: a genuine
:: same-session --skip-build re-upload (e.g. retrying after a failed curl)
:: happens within seconds of the real build, not stale by design.
if "!SKIP_BUILD!"=="1" (
    for /f %%t in ('powershell -NoProfile -Command "(Get-Item '!EXE!').LastWriteTime.ToString('yyyyMMddHHmm')"') do set EXE_MTIME=%%t
    for /f %%d in ('powershell -NoProfile -Command "[int][math]::Abs(([datetime]::ParseExact('!VERSION!','yyyyMMddHHmm',$null) - [datetime]::ParseExact('!EXE_MTIME!','yyyyMMddHHmm',$null)).TotalMinutes)"') do set EXE_AGE_MIN=%%d
    if !EXE_AGE_MIN! GTR 15 (
        echo ERROR: --skip-build with a stale !EXE! -- last built !EXE_MTIME!, but this deploy is labeled !VERSION! ^(!EXE_AGE_MIN! min apart^).
        echo        Uploading it under a fresher label than what's actually baked inside it will silently break OTA
        echo        for every client that "updates" to it -- see shortnerdcat-client-versioning skill for the Android incident this mirrors.
        echo        Re-run without --skip-build, or only use --skip-build to retry the SAME build's upload.
        exit /b 1
    )
)

:: ── 2. Package into ZIP ───────────────────────────────────────────────────────
:: wintun.dll must ship alongside the exe -- it's a real runtime dependency
:: (loaded via LoadLibrary by the underlying wireguard-go tun package, not
:: go:embed'd into the binary despite build.bat's fetch_wintun step writing a
:: second copy to internal/wintun/ "ready for go:embed" -- nothing in this
:: repo actually embeds it). Confirmed missing from every zip actually
:: deployed so far, 2026-08-10: every user who downloaded and ran the live
:: navlink.net/download/zip would fail to establish a VPN tunnel with no
:: wintun.dll next to the exe, unless one happened to already be on their
:: machine from something else.
if not exist "!WINTUN!" (echo ERROR: !WINTUN! not found -- run build.bat first, or fetch it via snc/win/tools/fetch_wintun. & exit /b 1)
echo [2/3] Packaging shortnerdcat-%VERSION%.zip...
if exist "!ZIP!" del "!ZIP!"
powershell -NoProfile -Command "Compress-Archive -Force -Path '!EXE!','!WINTUN!' -DestinationPath '!ZIP!'"
if errorlevel 1 (echo ERROR: zip failed. & exit /b 1)

:: ── 3. Upload to arbiter ──────────────────────────────────────────────────────
echo [3/3] Uploading version !VERSION! to !ARBITER_URL!...
for /f %%h in ('curl -sk -L -H "Expect:" -w "%%{http_code}" -o NUL -X POST "!ARBITER_URL!/admin/downloads/upload" -H "Authorization: Bearer !UPLOAD_KEY!" -F "binary_type=windows" -F "version=!VERSION!" -F "file=@!ZIP!;filename=shortnerdcat.zip"') do set HTTP_STATUS=%%h
echo HTTP !HTTP_STATUS!
if "!HTTP_STATUS!"=="200" goto win_ok
if "!HTTP_STATUS!"=="303" goto win_ok
echo ERROR: upload failed (HTTP !HTTP_STATUS!). & exit /b 1
:win_ok

del "!ZIP!" >nul 2>&1

echo.
echo Deploy complete: !ARBITER_URL!/download
endlocal
