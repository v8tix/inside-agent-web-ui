#!/usr/bin/env bash
# Starts agent-web-ui in the background and waits until it is ready.
#
# Usage: scripts/run.sh [--env <file>] [--rebuild]
#
#   --env <file>  read the settings from this file (default: .env)
#   --rebuild     build the binary even if it looks up to date
#
# The binary is bin/agent-web-ui, built when it is missing or when the code is
# newer than it. The process id is kept in logs/agent-web-ui.pid and the output
# goes to logs/agent-web-ui-<time>.log (BIN_DIR and LOG_DIR in .env say where).
# Stop it with scripts/stop.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=lib/process.sh
. scripts/lib/process.sh

ENV_FILE=".env"
REBUILD=0
while [ $# -gt 0 ]; do
  case "$1" in
    --env) use_settings_file "${2:?--env needs a file name}"; shift 2 ;;
    --rebuild) REBUILD=1; shift ;;
    -h | --help) sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d'; exit 0 ;;
    *) echo "Unknown option: $1 (try --help)" >&2; exit 2 ;;
  esac
done

# Where things go, and the go command: settings like any other (.env.example).
BIN="$(required_setting BIN_DIR)/agent-web-ui"
LOG_DIR="$(required_setting LOG_DIR)"
PID_FILE="$LOG_DIR/agent-web-ui.pid"
GO="$(setting GO)"
[ -n "$GO" ] || GO="go"

if [ ! -s "$ENV_FILE" ]; then
  echo "There are no settings in $ENV_FILE." >&2
  echo "Create them with: scripts/init-env.sh   (or install -m 600 .env.example .env, and edit it)" >&2
  exit 1
fi

if [ -f "$PID_FILE" ] && is_process "$(cat "$PID_FILE")" agent-web-ui; then
  echo "agent-web-ui is already running (pid $(cat "$PID_FILE")). Stop it first: scripts/stop.sh" >&2
  exit 1
fi

# Build when asked, when there is no binary, or when the code changed since.
needs_build() {
  [ "$REBUILD" -eq 1 ] && return 0
  [ -x "$BIN" ] || return 0
  [ -n "$(find cmd internal assets go.mod go.sum -type f -newer "$BIN" -print -quit)" ]
}
if needs_build; then
  echo "Building $BIN ..."
  "$GO" build -trimpath -o "$BIN" ./cmd/web
fi

# The settings file is shell syntax (see .env.example); real environment
# variables set before this script runs are overwritten by it, as documented
# there, so a one-off override goes into a copy passed with --env.
load_env "$ENV_FILE"
# The server reads the same file itself, so --env is passed on when it is not
# .env: a key the file lacks or leaves empty is then not taken from .env.
set --
if [ "$ENV_FILE" != ".env" ]; then
  set -- --env "$ENV_FILE"
fi

ADDR="$(setting APP_HTTP_ADDR)"
PORT="${ADDR##*:}"
case "$PORT" in "" | *[!0-9]*) PORT=8300 ;; esac # an address without a port: the program's own
READY_URL="http://127.0.0.1:${PORT}/readyz"

mkdir -p "$LOG_DIR"
LOG="$LOG_DIR/agent-web-ui-$(date +%Y%m%d-%H%M%S).log"
nohup "$BIN" "$@" >"$LOG" 2>&1 &
PID=$!
echo "$PID" >"$PID_FILE"
echo "Started agent-web-ui (pid $PID), log: $LOG"

fail() {
  echo "$1" >&2
  echo "--- last 20 lines of $LOG" >&2
  tail -n 20 "$LOG" >&2 || true
  kill "$PID" 2>/dev/null || true
  rm -f "$PID_FILE"
  exit 1
}

for _ in $(seq 1 20); do
  if ! kill -0 "$PID" 2>/dev/null; then
    rm -f "$PID_FILE"
    echo "agent-web-ui stopped right after it started." >&2
    echo "--- last 20 lines of $LOG" >&2
    tail -n 20 "$LOG" >&2 || true
    exit 1
  fi
  if curl -fsS -o /dev/null --max-time 2 "$READY_URL" 2>/dev/null; then
    echo "Ready: http://localhost:${PORT}/"
    exit 0
  fi
  sleep 1
done
fail "agent-web-ui did not become ready within 20 seconds ($READY_URL). Is the database up and the agent running?"
