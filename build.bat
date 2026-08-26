@echo off
:: build.bat — macOS client build entry point for Windows.
:: Delegates to build.sh via bash (WSL or Git Bash).
::
:: Usage:
::   build.bat                    — build + notarize
::   build.bat --skip-notarize    — build DMG only, skip notarization
::   build.bat --version 1.2.3    — set explicit version string
::
:: Requires bash on PATH (Git Bash or WSL).
:: The actual compilation and packaging run on macOS;
:: use this bat to trigger the build on a remote Mac:
::
::   set MAC_BUILDER=user@macmini.local
::   ssh %MAC_BUILDER% "cd ~/REPO/shortnerdcat && bash build.sh %*"
::
bash "%~dp0build.sh" %*
