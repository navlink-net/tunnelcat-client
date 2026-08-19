#!/bin/bash
# CFBundleExecutable launcher: resolves the real binary next to this script
# and exec's it, preserving all arguments.

LOG=/tmp/snc_debug.log
echo "$(date): ShortNerdCat.sh started pid=$$ uid=$(id -u) args=$*" >> "$LOG" 2>&1

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARY="$SCRIPT_DIR/shortnerdcat"
echo "$(date): SCRIPT_DIR=$SCRIPT_DIR BINARY=$BINARY exists=$(test -x "$BINARY" && echo yes || echo NO)" >> "$LOG" 2>&1

if [[ ! -x "$BINARY" ]]; then
    echo "$(date): ERROR binary not found" >> "$LOG" 2>&1
    osascript -e 'display alert "ShortNerdCat" message "Binary not found: '"$BINARY"'" as critical'
    exit 1
fi

echo "$(date): exec binary uid=$(id -u)" >> "$LOG" 2>&1
exec "$BINARY" "$@"
