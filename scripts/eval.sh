#!/usr/bin/env bash
# Runs agent-eval with the settings from .env, and keeps a log of the run.
#
# Usage: scripts/eval.sh [--env <file>] [--rebuild] <command> [flags]
#
#   scripts/eval.sh selftest                   check the evaluator against the scripted agent
#   scripts/eval.sh datasets fetch             download GAIA (needs HF_TOKEN in .env)
#   scripts/eval.sh datasets list              show what was downloaded
#   scripts/eval.sh run --level 1 --limit 5    ask five Level 1 problems
#   scripts/eval.sh report <run-id>            rebuild the summary of a run
#   scripts/eval.sh help                       every command and flag
#
# The options before the command are this script's:
#
#   --env <file>  read the settings from this file (default: .env)
#   --rebuild     build the binary even if it looks up to date
#
# The binary is bin/agent-eval, built when it is missing or when the code is
# newer than it. Output goes to the terminal and to logs/agent-eval-<time>.log
# (BIN_DIR and LOG_DIR in .env say where).
# The exit status is agent-eval's: 0 ok, 1 usage, 2 a required UI check failed,
# 3 accuracy below the minimum or self-test results not as expected,
# 4 infrastructure failure, 130 interrupted.
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
    *) break ;;
  esac
done
if [ $# -eq 0 ]; then
  sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d' >&2
  exit 1
fi

# Where things go, and the go command: settings like any other (.env.example).
BIN="$(required_setting BIN_DIR)/agent-eval"
LOG_DIR="$(required_setting LOG_DIR)"
GO="$(setting GO)"
[ -n "$GO" ] || GO="go"

needs_build() {
  [ "$REBUILD" -eq 1 ] && return 0
  [ -x "$BIN" ] || return 0
  [ -n "$(find cmd internal go.mod go.sum -type f -newer "$BIN" -print -quit)" ]
}
if needs_build; then
  echo "Building $BIN ..."
  "$GO" build -trimpath -o "$BIN" ./cmd/eval
fi

# The settings file is shell syntax (see .env.example). Loading it here makes the
# real environment match it, and lets a variable set by hand still be seen; the
# binary reads the same file itself, so --env is passed on when it is not .env.
if [ -s "$ENV_FILE" ]; then
  load_env "$ENV_FILE"
fi
ARGS=("$@")
if [ "$ENV_FILE" != ".env" ]; then
  ARGS+=(--env "$ENV_FILE")
fi

mkdir -p "$LOG_DIR"
LOG="$LOG_DIR/agent-eval-$(date +%Y%m%d-%H%M%S).log"
echo "Log: $LOG"

# Ctrl-C is for agent-eval, which stops in an orderly way and says how to
# continue. This script and tee must not die first and swallow what it prints.
trap '' INT
set +e
"$BIN" "${ARGS[@]}" 2>&1 | tee -a "$LOG"
status=${PIPESTATUS[0]}
set -e
exit "$status"
