#!/usr/bin/env bash
# The quality gate: what must be clean before code is shipped.
#
# Usage: scripts/check.sh [--it] [--quick]
#
#   (default)  formatting, go vet, golangci-lint, staticcheck, modernize,
#              govulncheck, the unit tests with the race detector, the coverage
#              targets, and the tests under it/ that need no Docker. Of these a
#              line for each package is printed, and the tests that were
#              skipped; when they fail, all of their output.
#   --it       all the tests under it/, those that need Docker too (the browser
#              ones also need Node and Chrome; add -short through GOFLAGS to skip
#              those). Only a summary of them is printed. When they fail, all of
#              their output stays in a temporary file, which the gate names.
#   --quick    skip the race detector and the vulnerability scan.
#
# Coverage targets, in .env (unit tests only; generated mocks are left out):
#   COVER_CORE_MIN  the use cases, internal/core and internal/eval/core  (85)
#   COVER_ALL_MIN   everything under internal/ and cmd/                  (70)
#   COVER_PKG_MIN   each package on its own                              (50)
# A package the unit tests cannot reach by design is named in COVER_EXEMPT, and
# the integration tests cover it. The totals can hide a package nobody tests.
# A target is met by the share as it is counted. The share that is printed is cut
# to one decimal, not rounded: 84.96 prints as 84.9 and is below 85.
#
# The linters come from TOOLS_DIR, bin/tools unless .env says otherwise
# (scripts/build-tools.sh builds them with the Go this project uses); GO in .env
# picks another go command.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=lib/process.sh
. scripts/lib/process.sh

run_it=0
quick=0
for arg in "$@"; do
  case "$arg" in
    --it) run_it=1 ;;
    --quick) quick=1 ;;
    -h | --help) sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "check.sh: unknown option '$arg' (see --help)" >&2; exit 2 ;;
  esac
done

# Every setting is in .env (written down in .env.example), not in the environment.
GO="$(setting GO)"
[ -n "$GO" ] || GO="go"
COVER_CORE_MIN="$(required_setting COVER_CORE_MIN)"
COVER_ALL_MIN="$(required_setting COVER_ALL_MIN)"
COVER_PKG_MIN="$(required_setting COVER_PKG_MIN)"
# The composition root and the database adapter need a real PostgreSQL; the two
# programs' main functions and the stylesheet generator have nothing to unit-test.
COVER_EXEMPT="$(required_setting COVER_EXEMPT)"
TOOLS_DIR="$(required_setting TOOLS_DIR)"
case "$TOOLS_DIR" in /*) ;; *) TOOLS_DIR="$PWD/$TOOLS_DIR" ;; esac
export PATH="$TOOLS_DIR:$PATH"

step() { printf '\n== %s\n' "$1"; }
fail() { printf 'check.sh: %s\n' "$1" >&2; exit 1; }
# skipped_tests log name: counts and names the tests that the output of
# "go test -v" in the file log says were skipped; name is what they are called.
# A skipped test did not run, and a green summary must not hide that.
skipped_tests() {
  local skipped
  skipped="$(grep -cE '^\s*--- SKIP' "$1" || true)"
  if [ "${skipped:-0}" -gt 0 ]; then
    printf '%s %s were skipped:\n' "$skipped" "$2"
    grep -E '^\s*--- SKIP' "$1" | sed 's/^ */  /'
  fi
}

for tool in golangci-lint staticcheck modernize govulncheck; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is not installed: run scripts/build-tools.sh"
done

step "gofmt"
unformatted="$(gofmt -l assets cmd internal it)"
[ -z "$unformatted" ] || fail "not formatted (run gofmt -w):
$unformatted"

step "go vet"
"$GO" vet ./...

step "golangci-lint"
golangci-lint run ./...

step "staticcheck"
staticcheck ./...

step "modernize"
modernized="$(modernize ./... 2>&1 || true)"
[ -z "$modernized" ] || fail "modernize has suggestions:
$modernized"

if [ "$quick" -eq 0 ]; then
  step "govulncheck"
  govulncheck ./...
fi

step "unit tests"
profile="$(mktemp)"
trap 'rm -f "$profile"' EXIT
# -count=1 runs every test afresh without emptying the cache for other projects.
if [ "$quick" -eq 1 ]; then
  "$GO" test -count=1 -covermode=atomic -coverprofile="$profile" ./internal/... ./cmd/...
else
  "$GO" test -count=1 -race -covermode=atomic -coverprofile="$profile" ./internal/... ./cmd/...
fi

step "coverage"
# A profile line is  file:from,to statements count  and a block is covered when
# its count is above zero. A floor is compared with the share as it is counted;
# the share that is printed is cut to one decimal, so that a number on the screen
# never reaches a floor that the share did not. awk leaves the locale of the
# machine aside, so the numbers read the same everywhere: 84.9, never 84,9.
totals="$(LC_ALL=C awk -F'[: ]' -v core_min="$COVER_CORE_MIN" -v all_min="$COVER_ALL_MIN" '
  function meets(covered, statements, min) { return ((statements ? 100 * covered / statements : 0) >= min + 0) }
  function shown(covered, statements) { return statements ? int(1000 * covered / statements) / 10 : 0 }
  NR == 1 || $1 ~ /\/mocks\// { next }
  { all += $3; if ($4 > 0) allHit += $3 }
  $1 ~ /\/internal\/core\// || $1 ~ /\/internal\/eval\/core\// { core += $3; if ($4 > 0) coreHit += $3 }
  END { printf "%.1f %d %.1f %d\n", shown(coreHit, core), meets(coreHit, core, core_min), shown(allHit, all), meets(allHit, all, all_min) }
' "$profile")" || fail "could not read the coverage profile"
read -r core_pct core_met all_pct all_met <<<"$totals"
[ -n "${core_pct:-}" ] && [ -n "${all_met:-}" ] || fail "could not read the coverage profile (got: '$totals')"
printf 'use cases (internal/core, internal/eval/core):  %s%% (target %s%%)\n' "$core_pct" "$COVER_CORE_MIN"
printf 'everything under internal/ and cmd/, no mocks: %s%% (target %s%%)\n' "$all_pct" "$COVER_ALL_MIN"
[ "$core_met" -eq 1 ] || fail "the use cases are below ${COVER_CORE_MIN}%"
[ "$all_met" -eq 1 ] || fail "internal/ and cmd/ are below ${COVER_ALL_MIN}%"

# Each package on its own: a total can be met while one package has no tests.
low="$(LC_ALL=C awk -F'[: ]' -v min="$COVER_PKG_MIN" -v exempt="$COVER_EXEMPT" '
  function shown(covered, statements) { return statements ? int(1000 * covered / statements) / 10 : 0 }
  BEGIN { n = split(exempt, e, " "); for (i = 1; i <= n; i++) skip[e[i]] = 1 }
  NR == 1 || $1 ~ /\/mocks\// { next }
  {
    pkg = $1; sub(/^.*agent-web-ui\//, "", pkg); sub(/\/[^\/]*$/, "", pkg)
    total[pkg] += $3; if ($4 > 0) hit[pkg] += $3
  }
  END {
    for (pkg in total) {
      pct = total[pkg] ? 100 * hit[pkg] / total[pkg] : 0
      if (!(pkg in skip) && pct < min) printf "  %s: %.1f%%\n", pkg, shown(hit[pkg], total[pkg])
    }
  }
' "$profile" | sort)" || fail "could not work out the coverage of each package"
if [ -n "$low" ]; then
  fail "these packages are below ${COVER_PKG_MIN}% (add tests, or name the package in COVER_EXEMPT with a reason):
$low"
fi
printf 'every package is at %s%% or above, except the ones named in COVER_EXEMPT\n' "$COVER_PKG_MIN"

if [ "$run_it" -eq 0 ]; then
  step "tests that need no Docker"
  # Under it/, the tests that read the repository or run its scripts and start no
  # container: the scripts, the settings and the ignore files (it/scripts), and the
  # table of routes the security tests go by (the tests of it/security named
  # TestRoutes_). With --it they run among all the others, below.
  # They run with -v, since a test that was skipped says so only then. Their
  # output goes to a file: a line for each package and the skipped tests are
  # printed when they pass, and all of it when they fail.
  no_docker_log="$(mktemp)"
  trap 'rm -f "$profile" "$no_docker_log"' EXIT
  if ! { "$GO" test -count=1 -v ./it/scripts/ && "$GO" test -count=1 -v -run '^TestRoutes_' ./it/security/; } >"$no_docker_log" 2>&1; then
    cat "$no_docker_log" >&2
    fail "the tests that need no Docker failed: all of their output is above"
  fi
  grep -E '^(ok|\?) ' "$no_docker_log" || true
  skipped_tests "$no_docker_log" "tests that need no Docker"
fi

if [ "$run_it" -eq 1 ]; then
  step "integration tests"
  # Their output is long, so it goes to a file and only its summary is printed.
  it_log="$(mktemp)"
  trap 'rm -f "$profile" "$it_log"' EXIT
  if ! "$GO" test -count=1 -v ./it/... >"$it_log" 2>&1; then
    grep -E '^(\s*--- FAIL|FAIL|panic)' "$it_log" | head -60 >&2 || true
    # The reasons are in the file, so it stays when the gate ends.
    trap 'rm -f "$profile"' EXIT
    fail "the integration tests failed. The lines above only name the tests; the reasons are in their output, which stays in $it_log"
  fi
  grep -E '^(ok|\?) ' "$it_log"
  skipped_tests "$it_log" "integration tests"
fi

if [ "$quick" -eq 1 ]; then
  printf '\nThe quick checks passed (the race detector and the vulnerability scan were left out).\n'
else
  printf '\nAll checks passed.\n'
fi
