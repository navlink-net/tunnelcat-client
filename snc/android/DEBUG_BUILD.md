# Android debug build and emulator run

## Build and install

```
build-debug.bat
```

Builds arm64 + amd64 Go binaries, assembles a debug APK (no signing), and installs it
on any connected device or running emulator via `adb install -r`.

Version label is `yyyyMMddHHmm` (current time). Set `APP_VERSION=<val>` in `.env` or
the environment to pin a specific label.

## Full clean launch sequence

Run these commands in order every time:

```
adb uninstall com.beautysqrl.chat
adb shell am force-stop com.shortnerdcat.snc
adb install -r app\build\outputs\apk\debug\app-debug.apk
adb shell am start -n com.shortnerdcat.snc/.MainActivity
```

Notes:
- Uninstall beautysqrl first — it conflicts with the debug build somehow.
- Force-stop before install so the old process is dead before the new APK lands.
- The emulator is slow on first launch (JVM class verification). Wait ~60 s for the
  UI to appear before assuming something is wrong.

## Watch logs

Stream Go binary output live:

```
adb logcat -s snc-core
```

Or read the current log file directly:

```
adb shell run-as com.shortnerdcat.snc ls files/logs/
adb shell run-as com.shortnerdcat.snc cat files/logs/snc_<name>.log
```

## ADB path (Windows)

`%LOCALAPPDATA%\Android\Sdk\platform-tools\adb.exe`
