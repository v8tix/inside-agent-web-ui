#!/usr/bin/env bash
# Stops the agent-web-ui that scripts/run.sh started. Safe to run twice.
#
# Usage: scripts/stop.sh [--env <file>]
#
# It sends SIGTERM, which lets running replies finish saving, waits as long as
# APP_SHUTDOWN_GRACE (.env, 10s; a part of a second counts as a whole one) plus 2
# seconds, and only then uses SIGKILL.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=lib/process.sh
. scripts/lib/process.sh

ENV_FILE=".env"
while [ $# -gt 0 ]; do
  case "$1" in
    --env) use_settings_file "${2:?--env needs a file name}"; shift 2 ;;
    -h | --help) sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d'; exit 0 ;;
    *) echo "Unknown option: $1 (try --help)" >&2; exit 2 ;;
  esac
done

PID_FILE="$(required_setting LOG_DIR)/agent-web-ui.pid"

# The grace period as the server reads it; the server's own ten seconds when the
# settings have none or it is no duration.
grace="$(setting APP_SHUTDOWN_GRACE)"
grace_seconds="$(duration_seconds "$grace")" || grace_seconds=10
WAIT=$((grace_seconds + 2))

# The pid file first. Without it, whatever listens on the port counts, but only
# when it really is this program.
PID=""
if [ -f "$PID_FILE" ]; then
  PID="$(cat "$PID_FILE")"
  # The pid may have been given to another process since the file was written.
  is_process "$PID" agent-web-ui || PID=""
fi
if [ -z "$PID" ]; then
  PORT="$(setting APP_HTTP_ADDR)"
  PORT="${PORT##*:}"
  case "$PORT" in "" | *[!0-9]*) PORT=8300 ;; esac # an address without a port: the program's own
  CANDIDATE="$(lsof -ti "tcp:${PORT}" -sTCP:LISTEN 2>/dev/null | head -n 1 || true)"
  if [ -n "$CANDIDATE" ] && ps -o command= -p "$CANDIDATE" 2>/dev/null | grep -q "agent-web-ui"; then
    PID="$CANDIDATE"
  fi
fi

if [ -z "$PID" ]; then
  rm -f "$PID_FILE"
  echo "agent-web-ui is not running."
  exit 0
fi

echo "Stopping agent-web-ui (pid $PID) ..."
kill -TERM "$PID" 2>/dev/null || true
for _ in $(seq 1 $((WAIT * 5))); do
  kill -0 "$PID" 2>/dev/null || { rm -f "$PID_FILE"; echo "Stopped."; exit 0; }
  sleep 0.2
done
echo "It did not stop within ${WAIT}s; killing it." >&2
kill -KILL "$PID" 2>/dev/null || true
rm -f "$PID_FILE"
echo "Killed."
