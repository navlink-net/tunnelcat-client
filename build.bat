@echo off
:: build.bat — macOS client build entry point for Windows.
:: Delegates to snc\mac\build.sh via bash (WSL or Git Bash), since the actual
:: compilation, codesigning, and notarization must run on macOS.
::
:: Usage:
::   build.bat                    — build + notarize
::   build.bat --skip-notarize    — build DMG only, skip notarization
::   build.bat --version 1.2.3    — set explicit version string
::
:: Requires bash on PATH (Git Bash or WSL). To trigger the build on a
:: remote Mac instead of running it locally:
::
::   set MAC_BUILDER=user@your-mac-host
::   ssh %MAC_BUILDER% "cd /path/to/tunnelcat-client && bash snc/mac/build.sh %*"
::
bash "%~dp0snc\mac\build.sh" %*
