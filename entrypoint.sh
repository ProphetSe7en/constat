#!/bin/bash
# entrypoint.sh — Start constat.sh + web UI

# Set ownership on config directory
PUID=${PUID:-99}
PGID=${PGID:-100}
if [ -d "/config" ]; then
    chown "$PUID:$PGID" /config
    # Fix ownership on existing files
    find /config -maxdepth 1 -type f -exec chown "$PUID:$PGID" {} +
fi

# Set umask for new files
[ -n "$UMASK" ] && umask "$UMASK"

cleanup() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] Shutting down..."
    [ -n "$CONSTAT_PID" ] && kill "$CONSTAT_PID" 2>/dev/null
    [ -n "$UI_PID" ] && kill "$UI_PID" 2>/dev/null
    wait
}

trap cleanup SIGTERM SIGINT

# Start constat.sh in background
/constat.sh &
CONSTAT_PID=$!

# Start web UI (if enabled)
if [ "${UI_ENABLED:-true}" = "true" ]; then
    /constat-ui &
    UI_PID=$!
fi

# Wait for either to exit, then clean up the other
wait -n $CONSTAT_PID ${UI_PID:-}
EXIT_CODE=$?
cleanup
exit $EXIT_CODE
