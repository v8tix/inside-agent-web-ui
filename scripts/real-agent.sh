#!/usr/bin/env bash
# Starts a real agent for the Web UI and the evaluator to talk to: a sample agent
# from the adk-training-go repository, on Gemini.
#
# Usage: scripts/real-agent.sh [--env <file>] [start|stop|status]     (default: start)
#
#   --env <file>  read the settings from this file (default: .env)
#
# Every setting is in .env, written down in .env.example (section "Real agent"):
# the sample, the port, the ADK it is built against, and the rest. What .env lacks
# comes from .env.example. The environment is not looked at.
#
# It builds the sample into bin/real-agent, starts it in the background, and waits
# until it lists its application. The process id is kept in logs/real-agent.pid and
# the output goes to logs/real-agent-<time>.log. Stop it with `stop`.
#
# The model is Gemini, and there is no setting for it. The sample can run other
# models, one on this computer among them, which takes much of its memory; the
# script names the model itself (MODEL_TYPE), so the sample never picks another
# from its own .env or from the shell. Only the model settings reach the agent:
# the rest of .env (passwords, tokens) stays here.
#
# The agent needs a Gemini key: put it in .env as GOOGLE_AI_STUDIO_API_KEY and
# the script hands it to the agent. Without one the agent looks for it itself, in
# the .env of the adk-training-go checkout it runs from and in the environment
# this script runs in. The script never prints it. Every message goes to Google
# with that key and counts against its quota.
#
# A sample built with an ADK launcher before 2.5 listens on every network
# interface and cannot be told otherwise, so anyone who can reach this machine
# could use the model (and the key) through the agent's port. The script looks at
# where the agent really listens and stops it unless REAL_AGENT_ALLOW_LAN=1 says
# that is acceptable: a network that is yours alone, or a firewall that blocks
# the port from outside. (From 2.5 the launcher listens on this computer only.)
# REAL_AGENT_ADK=v2.5.0 builds the sample against that ADK instead of the one its
# go.mod pins, from a copy of go.mod and go.sum, so the sample's files stay as
# they are and the agent listens on this computer only.
#
# The launcher also ends a reply that has been streaming for two minutes, which
# a long answer with tool calls can take; the page then shows it cut off.
# REAL_AGENT_SSE_TIMEOUT raises that limit.
#
# Then point the Web UI at it, in .env:
#   AGENT_BASE_URL=http://localhost:9094/api   AGENT_REQUEST_TIMEOUT=600s
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=lib/process.sh
. scripts/lib/process.sh

ENV_FILE=".env"
while [ $# -gt 0 ]; do
  case "$1" in
    --env) use_settings_file "${2:?--env needs a file name}"; shift 2 ;;
    -h | --help) sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) break ;;
  esac
done

ROOT="$PWD"
ADK_TRAINING_DIR="$(required_setting ADK_TRAINING_DIR)"
REAL_AGENT="$(required_setting REAL_AGENT)"
REAL_AGENT_APP="$(required_setting REAL_AGENT_APP)"
REAL_AGENT_PORT="$(required_setting REAL_AGENT_PORT)"
REAL_AGENT_SSE_TIMEOUT="$(required_setting REAL_AGENT_SSE_TIMEOUT)"
REAL_AGENT_ADK="$(setting REAL_AGENT_ADK)"
REAL_AGENT_ALLOW_LAN="$(setting REAL_AGENT_ALLOW_LAN)"
# Not named like the variables the agent reads: a shell variable that was exported
# keeps that when it is assigned, and an empty setting would then replace, for the
# agent, a key or a model that the environment already gives it.
gemini_model="$(setting GEMINI_MODEL)"
gemini_key="$(setting GOOGLE_AI_STUDIO_API_KEY)"
GO="$(setting GO)"
[ -n "$GO" ] || GO="go"

# Absolute, because the sample is built and run from other directories.
absolute() { case "$1" in /*) echo "$1" ;; *) echo "$ROOT/$1" ;; esac; }
# Two steps each: inside a nested substitution, a failure of required_setting would
# not stop the script.
BIN_DIR="$(required_setting BIN_DIR)"
BIN_DIR="$(absolute "$BIN_DIR")"
LOG_DIR="$(required_setting LOG_DIR)"
LOG_DIR="$(absolute "$LOG_DIR")"
BIN="$BIN_DIR/real-agent"
PID_FILE="$LOG_DIR/real-agent.pid"
URL="http://127.0.0.1:${REAL_AGENT_PORT}/api"

# The pid may have been given to another process since the file was written.
running() { [ -f "$PID_FILE" ] && is_process "$(cat "$PID_FILE")" real-agent; }

stop_agent() {
  local pid
  pid="$(cat "$PID_FILE")"
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.2; done
  kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
  rm -f "$PID_FILE"
}

# build_sample builds the sample into $BIN. With REAL_AGENT_ADK it is built against
# that version of the ADK, using a copy of the sample's go.mod and go.sum (go
# -modfile): the sample's own files are not touched.
build_sample() {
  if [ -z "$REAL_AGENT_ADK" ] || [ "$REAL_AGENT_ADK" = sample ]; then
    (cd "$ADK_TRAINING_DIR" && "$GO" build -trimpath -o "$BIN" "./cmd/$REAL_AGENT")
    return
  fi
  local work status=0
  work="$(mktemp -d)"
  cp "$ADK_TRAINING_DIR/go.mod" "$work/alt.mod"
  if [ -f "$ADK_TRAINING_DIR/go.sum" ]; then cp "$ADK_TRAINING_DIR/go.sum" "$work/alt.sum"; fi
  # tidy adds the checksums that the newer version's own dependencies need.
  (
    cd "$ADK_TRAINING_DIR" &&
      "$GO" get -modfile="$work/alt.mod" "google.golang.org/adk/v2@$REAL_AGENT_ADK" &&
      "$GO" mod tidy -modfile="$work/alt.mod" &&
      "$GO" build -modfile="$work/alt.mod" -trimpath -o "$BIN" "./cmd/$REAL_AGENT"
  ) || status=$?
  rm -rf "$work"
  return "$status"
}

case "${1:-start}" in
  status)
    if running; then echo "The agent is running (pid $(cat "$PID_FILE")) at $URL"; else echo "The agent is not running."; fi
    exit 0
    ;;
  stop)
    if ! running; then echo "The agent is not running."; rm -f "$PID_FILE"; exit 0; fi
    stop_agent
    echo "The agent is stopped."
    exit 0
    ;;
  start) ;;
  *) echo "Unknown command: $1 (try --help)" >&2; exit 2 ;;
esac

if running; then
  echo "The agent is already running (pid $(cat "$PID_FILE")) at $URL" >&2
  exit 1
fi
[ -f "$ADK_TRAINING_DIR/cmd/$REAL_AGENT/main.go" ] ||
  { echo "There is no sample 'cmd/$REAL_AGENT' in $ADK_TRAINING_DIR (set ADK_TRAINING_DIR or REAL_AGENT)." >&2; exit 1; }

# What the agent is told about its model, and where it runs from: the sample's
# checkout, whose .env may hold the key when this repository's does not. The
# sample's .env may also name another model: MODEL_TYPE, set here, decides.
mkdir -p "$LOG_DIR" "$BIN_DIR"
model_env=(MODEL_TYPE=gemini)
if [ -n "$gemini_model" ]; then model_env+=("GEMINI_MODEL=$gemini_model"); fi
model_name="Gemini${gemini_model:+ ($gemini_model)}"

if [ "$REAL_AGENT_ADK" = sample ]; then against=""; else against="${REAL_AGENT_ADK:+ against ADK $REAL_AGENT_ADK}"; fi
echo "Building the $REAL_AGENT sample into $BIN$against ..."
build_sample

log="$LOG_DIR/real-agent-$(date +%Y%m%d-%H%M%S).log"
(
  cd "$ADK_TRAINING_DIR"
  # The key goes in the agent's environment, not its arguments, which any user of
  # this machine can read with ps. Unset here, the agent finds its own.
  if [ -n "$gemini_key" ]; then export GOOGLE_AI_STUDIO_API_KEY="$gemini_key"; fi
  env "${model_env[@]}" nohup "$BIN" web --port="$REAL_AGENT_PORT" api --sse-write-timeout="$REAL_AGENT_SSE_TIMEOUT" >"$log" 2>&1 &
  echo $! >"$PID_FILE"
)

for _ in $(seq 1 100); do
  if ! running; then
    echo "The agent stopped while starting. Its log ($log), last lines:" >&2
    tail -n 20 "$log" >&2
    rm -f "$PID_FILE"
    exit 1
  fi
  if curl -fsS -m 2 "$URL/list-apps" 2>/dev/null | grep -q "\"$REAL_AGENT_APP\""; then
    scope="$(listening_scope "$(cat "$PID_FILE")" "$REAL_AGENT_PORT")"
    if [ "$scope" != loopback ]; then
      if [ "$REAL_AGENT_ALLOW_LAN" != 1 ]; then
        stop_agent
        if [ "$scope" = unknown ]; then
          {
            echo "Cannot tell where the agent listens (lsof is missing, or does not see the port), so it has been"
            echo "stopped: it could be open to the whole network. Install lsof and start it again."
            echo "Or, if this network is yours alone, or a firewall blocks port $REAL_AGENT_PORT from outside,"
            echo "set REAL_AGENT_ALLOW_LAN=1 in $ENV_FILE and start it again."
          } >&2
        else
          {
            echo "The agent listens on every network interface (the ADK launcher cannot be told otherwise),"
            echo "so anyone who can reach this machine could use your model, and your key, through port $REAL_AGENT_PORT."
            echo "It has been stopped. Build the sample against an ADK that listens on this computer only,"
            echo "without changing its go.mod: set REAL_AGENT_ADK=v2.5.0 in $ENV_FILE and start it again."
            echo "Or, if this network is yours alone, or a firewall blocks port $REAL_AGENT_PORT from outside,"
            echo "set REAL_AGENT_ALLOW_LAN=1 in $ENV_FILE and start it again."
          } >&2
        fi
        exit 1
      fi
      if [ "$scope" = unknown ]; then
        echo "Warning: cannot tell where the agent listens (REAL_AGENT_ALLOW_LAN=1)." >&2
      else
        echo "Warning: the agent listens on the network, not only on this computer (REAL_AGENT_ALLOW_LAN=1)." >&2
      fi
    fi
    echo "The agent is ready (pid $(cat "$PID_FILE")) at $URL, serving \"$REAL_AGENT_APP\" on $model_name."
    echo "Log: $log"
    exit 0
  fi
  sleep 0.3
done
echo "The agent did not list \"$REAL_AGENT_APP\" within 30 seconds. Its log ($log), last lines:" >&2
tail -n 20 "$log" >&2
exit 1
