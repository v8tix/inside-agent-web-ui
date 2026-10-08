# Helpers for the scripts that start and stop processes and read their settings.
# Source this file; it prints nothing and changes nothing by itself.

# load_env <file>: puts the settings of the file (shell syntax, see .env.example)
# in the environment. A name without a directory is read from the current one:
# the shell's own "." would look for it on the PATH first, and a file of that name
# in a directory on the PATH would be read instead.
load_env() {
  local file="${1:?load_env needs a file}"
  case "$file" in */*) ;; *) file="./$file" ;; esac
  set -a
  # shellcheck disable=SC1090
  . "$file"
  set +a
}

# setting <NAME>: one setting, for the script that asks. It is what the settings
# file says (ENV_FILE, .env unless --env names another), or else what .env.example
# says; an empty value counts as unset, and a setting that neither file has comes
# out empty. The environment is not looked at: the settings live in these files.
# Each file is read in a shell of its own (read_setting) and only the one value
# comes out, so a script can use its own settings without handing every secret in
# the file (passwords, tokens) to the programs it starts. A file that cannot be
# read is an error, not a quiet fallback to the example.
setting() {
  local name="${1:?setting needs a name}" file value
  for file in "${ENV_FILE:-.env}" "${ENV_EXAMPLE:-.env.example}"; do
    [ -f "$file" ] || continue
    value="$(read_setting "$file" "$name")" || return 1
    if [ -n "$value" ]; then
      printf '%s' "$value"
      return 0
    fi
  done
  return 0
}

# required_setting <NAME>: setting, for one that must have a value. It is an error
# when neither file gives one (the example always does, so this means the example
# is missing or lost the key).
required_setting() {
  local value
  value="$(setting "${1:?required_setting needs a name}")" || return 1
  if [ -z "$value" ]; then
    echo "$1 is not set in ${ENV_FILE:-.env} or ${ENV_EXAMPLE:-.env.example}" >&2
    return 1
  fi
  printf '%s' "$value"
}

# read_setting <file> <NAME>: the value NAME has once the file is read, or nothing.
# The file is read as strictly as the scripts run (set -eu): a line that fails, an
# unset $VARIABLE or a value with a space that is not quoted is an error here, not a
# value that quietly goes missing. The shell that reads it is a bash of its own,
# started for that: it has the environment, and none of the variables the script
# has only set for itself. A syntax error ends the shell that reads the file before
# it can say anything, and a subshell would not do: where its caller handles the
# failure, as here, bash from 4.3 on does not let a line that fails end it. The
# failure is reported out here. What the shell prints about a bad line is thrown
# away: it can hold a piece of a secret. The shell is started without the file
# that BASH_ENV names: it is there to read the settings file, and nothing else.
read_setting() {
  local file="${1:?read_setting needs a file}" name="${2:?read_setting needs a name}" value
  case "$file" in */*) ;; *) file="./$file" ;; esac
  # The text in single quotes is the program of the other shell.
  # shellcheck disable=SC2016
  value="$(BASH_ENV='' "$BASH" -euc '
    unset "$2" # only the file may answer, not a variable somebody exported
    . "$1" >/dev/null 2>&1
    printf "%s" "${!2-}"
  ' read_setting "$file" "$name" 2>/dev/null)" || { echo "cannot read the settings in $1: is there a syntax error?" >&2; return 1; }
  printf '%s' "$value"
}

# use_settings_file <file>: the settings file the script was told to read (--env).
# A name with no file behind it is a mistake in the call, and reading the example in
# its place would be a lie: stop.sh would say "not running" about a server started
# from another file. The script ends, with status 2.
use_settings_file() {
  local file="${1:?use_settings_file needs a file}"
  if [ ! -f "$file" ]; then
    echo "${0##*/}: no settings file '$file'" >&2
    exit 2
  fi
  ENV_FILE="$file"
}

# duration_seconds <duration>: the whole seconds in a duration, written the way the
# programs read one (Go's time.ParseDuration): a number and a unit, as many times
# as it takes, such as 10s, 500ms, 1.5s, 1m30s or 1h. The units are h, m, s, ms, us
# (also written with the micro sign) and ns. A part of a second counts as a whole
# one, so waiting that many seconds is never shorter than the duration. It prints
# nothing and fails for what is no such duration, and for one that is not longer
# than zero, which is no time to wait. awk is told to leave the locale of the
# machine aside: in one that writes a decimal comma it would read 1.5 as 1. And
# it gets the duration through the environment, as it is written: given with -v,
# awk reads a backslash in it as the start of an escape.
duration_seconds() {
  DURATION="${1-}" LC_ALL=C awk 'BEGIN {
    d = ENVIRON["DURATION"]
    ns["h"] = 3600e9; ns["m"] = 60e9; ns["s"] = 1e9; ns["ms"] = 1e6; ns["us"] = 1e3; ns["ns"] = 1
    ns["\302\265s"] = 1e3; ns["\316\274s"] = 1e3 # us with the micro sign, and with the Greek mu
    gsub(/^[ \t]+|[ \t]+$/, "", d)
    sub(/^\+/, "", d)
    total = 0
    while (d != "") {
      number = d; sub(/[^0-9.].*$/, "", number)
      if (number !~ /^[0-9]*\.?[0-9]*$/ || number !~ /[0-9]/) exit 1
      d = substr(d, length(number) + 1)
      unit = d; sub(/[0-9.].*$/, "", unit)
      if (!(unit in ns)) exit 1
      d = substr(d, length(unit) + 1)
      total += number * ns[unit]
    }
    # In nanoseconds, the smallest part a program counts. The fraction added is
    # for the rounding of the arithmetic, and is far below one of them.
    total = int(total + 0.001)
    if (total < 1) exit 1
    seconds = int(total / 1e9)
    if (seconds * 1e9 < total) seconds++
    printf "%d\n", seconds
  }'
}

# is_process <pid> <name>: succeeds when a process with that pid is running and
# its command line contains name. A pid file outlives the process it was written
# for, and the number can be given to another one; signalling by pid alone could
# hit an unrelated process.
is_process() {
  local pid="${1:-}" name="${2:?is_process needs a name}"
  [ -n "$pid" ] && ps -o command= -p "$pid" 2>/dev/null | grep -qF -- "$name"
}

# listening_scope <pid> <port>: where the process listens on the TCP port.
# Prints loopback (this computer only), all (any other address, which is
# reachable from the network), or unknown (not listening, or lsof cannot tell).
# LSOF names another lsof, for tests.
listening_scope() {
  local pid="${1:?listening_scope needs a pid}" port="${2:?listening_scope needs a port}"
  local listeners
  listeners="$("${LSOF:-lsof}" -nP -a -p "$pid" -iTCP:"$port" -sTCP:LISTEN 2>/dev/null |
    awk '/\(LISTEN\)/ { print $(NF-1) }')" || listeners=""
  if [ -z "$listeners" ]; then
    echo unknown
    return 0
  fi
  local address
  while IFS= read -r address; do
    case "$address" in
      127.0.0.1:* | "[::1]:"* | localhost:*) ;;
      *) echo all; return 0 ;;
    esac
  done <<<"$listeners"
  echo loopback
}
