#!/usr/bin/env bash
# Checks a change before anyone is asked to review it: the title of the pull
# request and the size of the change.
#
# Usage: scripts/pr-check.sh [--env <file>] [--base <branch>] [--title <text>]
#
#   --env <file>     read the settings from this file (default: .env)
#   --base <branch>  what the change goes into (default: PR_BASE in .env, main)
#   --title <text>   the title of the pull request; without it only the size is checked
#
# A title is a prefix, a colon and what changed, such as "fix: stop dropping the
# partial reply": the prefix is one of PR_PREFIXES, with a (scope) after it if you
# like, and the title is at most PR_TITLE_MAX characters. A title that is not fails
# the check.
# The size is what the branch changes against the base, as a pull request shows it.
# Over PR_MAX_LINES lines or PR_MAX_FILES files only warns: the limits say when to
# split a change, and a generated or deleted file can be big for good reason.
# The exit status is 0 when the title is fine or not given, 1 when it is wrong, and
# 2 when the call or the branches are wrong.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=lib/process.sh
. scripts/lib/process.sh

usage_error() { echo "pr-check: $1 (try --help)" >&2; exit 2; }

ENV_FILE=".env"
base=""
title=""
while [ $# -gt 0 ]; do
  case "$1" in
    --env) [ $# -ge 2 ] || usage_error "--env needs a file name"; use_settings_file "$2"; shift 2 ;;
    --base) [ $# -ge 2 ] || usage_error "--base needs a branch"; base="$2"; shift 2 ;;
    --title) [ $# -ge 2 ] || usage_error "--title needs the title"; title="$2"; shift 2 ;;
    -h | --help) sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) usage_error "unknown option: $1" ;;
  esac
done

[ -n "$base" ] || base="$(required_setting PR_BASE)"
prefixes="$(required_setting PR_PREFIXES)"
title_max="$(required_setting PR_TITLE_MAX)"
max_lines="$(required_setting PR_MAX_LINES)"
max_files="$(required_setting PR_MAX_FILES)"
for limit in "$title_max" "$max_lines" "$max_files"; do
  case "$limit" in "" | *[!0-9]*) echo "pr-check: PR_TITLE_MAX, PR_MAX_LINES and PR_MAX_FILES must be whole numbers (got '$limit')" >&2; exit 2 ;; esac
done

status=0
if [ -n "$title" ]; then
  # "feat", or "feat(web)", then a colon, a space and something.
  pattern="^($(printf '%s' "$prefixes" | tr -s ' ' '|'))(\\([^)]+\\))?: .+"
  if ! [[ "$title" =~ $pattern ]]; then
    echo "title: '$title' needs a prefix and a colon, one of: $prefixes (for example 'fix: stop dropping the partial reply')" >&2
    status=1
  elif [ "${#title}" -gt "$title_max" ]; then
    echo "title: ${#title} characters, over the limit of $title_max" >&2
    status=1
  else
    echo "title: ok (${#title} of $title_max characters)"
  fi
fi

if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
  echo "pr-check: there is no branch or commit called '$base' to compare with" >&2
  exit 2
fi
numstat="$(git diff --numstat "$base...HEAD")" || { echo "pr-check: cannot compare HEAD with '$base'" >&2; exit 2; }
# Added plus deleted lines, and the number of files. A binary file shows "-" for
# both, which awk reads as 0.
read -r lines files <<<"$(printf '%s\n' "$numstat" | awk 'NF { l += $1 + $2; f++ } END { print l + 0, f + 0 }')"
printf 'size:  %s lines changed in %s files (guide: %s lines, %s files)\n' "$lines" "$files" "$max_lines" "$max_files"
if [ "$lines" -gt "$max_lines" ] || [ "$files" -gt "$max_files" ]; then
  echo "warning: that is more than a reviewer can take in at once; consider splitting it" >&2
fi
exit "$status"
