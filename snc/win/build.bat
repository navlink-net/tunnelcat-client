@echo off
setlocal enabledelayedexpansion

:: Always run from the repo root regardless of where the script was launched from.
cd /d "%~dp0..\.."

:: Load variables from .env if present (gitignored — keeps secrets out of the repo).
if exist ".env" (
    for /f "tokens=1* delims==" %%a in ('findstr /v "^#" .env ^| findstr /v "^$"') do (
        if not "%%a"=="" set %%a=%%b
    )
)

:: ── ShortNerdCat build script ─────────────────────────────────────────────────
::
:: Usage:
::   build.bat          — release build  (no console window)
::   build.bat debug    — debug build    (console window visible, logs to stderr)
::
:: Output: shortnerdcat.exe in the repo root.

set GO="C:\Program Files\Go\bin\go.exe"
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0

:: Fetch wintun.dll if missing
echo [1/3] Fetching wintun.dll...
%GO% run ./snc/win/tools/fetch_wintun/
if errorlevel 1 (
    echo ERROR: failed to fetch wintun.dll
    exit /b 1
)

:: Version stamp: YYYYMMDDHHmm (locale-independent via PowerShell).
:: When called from deploy.bat, VERSION is already set there — use it so the
:: baked-in binary version matches exactly what gets uploaded to the arbiter.
if not defined VERSION (
    for /f %%v in ('powershell -NoProfile -Command "Get-Date -Format yyyyMMddHHmm"') do set VERSION=%%v
)
set VERPATH=tunnel_cat/snc/core.Version

:: Determine link flags
if /i "%1"=="debug" (
    set LDFLAGS=-X %VERPATH%=%VERSION%
    echo [2/3] Building DEBUG binary ^(console visible^)...
) else (
    set LDFLAGS=-H windowsgui -X %VERPATH%=%VERSION%
    echo [2/3] Building RELEASE binary ^(no console^)...
)

%GO% build -tags with_utls -o shortnerdcat.exe -ldflags="!LDFLAGS!" ./snc/win/cmd/shortnerdcat/
if errorlevel 1 (
    echo ERROR: build failed
    exit /b 1
)

echo [3/3] Done: shortnerdcat.exe
