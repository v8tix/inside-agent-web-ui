#!/usr/bin/env bash
# Builds the quality tools with the Go toolchain this project uses. The tools
# installed globally are often built with an older Go than the module targets,
# and then refuse to analyze it (or, for mockery, to load it).
#
# Usage: scripts/build-tools.sh [--global] [tool ...]
#
#   --global   install where "go install" would with no GOBIN override (GOBIN if
#              set, else GOPATH/bin) so every project on this machine picks them
#              up, instead of this repository's bin/tools.
#
# Tools: golangci-lint staticcheck govulncheck modernize mockery (default: all).
# TOOLS_DIR in .env says where to build them (bin/tools unless changed, and
# unless --global is given), GO the go command. Use them with:
#   PATH="$PWD/bin/tools:$PATH" golangci-lint run ./...
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=lib/process.sh
. scripts/lib/process.sh

global=0
case "${1:-}" in
  --global) global=1; shift ;;
esac

# Settings like any other (.env, written down in .env.example).
GO="$(setting GO)"
[ -n "$GO" ] || GO="go"
if [ "$global" -eq 1 ]; then
  TOOLS_DIR="$("$GO" env GOBIN)"
  [ -n "$TOOLS_DIR" ] || TOOLS_DIR="$("$GO" env GOPATH)/bin"
else
  TOOLS_DIR="$(required_setting TOOLS_DIR)"
  case "$TOOLS_DIR" in /*) ;; *) TOOLS_DIR="$PWD/$TOOLS_DIR" ;; esac
fi
mkdir -p "$TOOLS_DIR"
export GOBIN="$TOOLS_DIR"

# One x/tools for the tools that share it, so they agree on Go 1.27 syntax.
XTOOLS="v0.51.0"
MOCKERY="v2.53.7"

build_golangci_lint() { "$GO" install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0; }
build_staticcheck() { "$GO" install honnef.co/go/tools/cmd/staticcheck@v0.8.2; }
build_govulncheck() { "$GO" install golang.org/x/vuln/cmd/govulncheck@v1.8.0; }
build_modernize() { "$GO" install "golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize@${XTOOLS}"; }

# mockery pins an x/tools too old to load Go 1.27 packages ("package context
# without types"), so it is built in a scratch module against a newer one.
build_mockery() {
  local work
  work="$(mktemp -d)"
  (
    cd "$work"
    "$GO" mod init mockbuild >/dev/null 2>&1
    "$GO" get "github.com/vektra/mockery/v2@${MOCKERY}" "golang.org/x/tools@${XTOOLS}" >/dev/null 2>&1
    "$GO" build -o "$TOOLS_DIR/mockery" github.com/vektra/mockery/v2
  )
  rm -rf "$work"
}

all="golangci-lint staticcheck govulncheck modernize mockery"
for tool in ${*:-$all}; do
  case "$tool" in
    golangci-lint | staticcheck | govulncheck | modernize | mockery) ;;
    *) echo "Unknown tool: $tool (known: $all)" >&2; exit 2 ;;
  esac
  echo "== $tool"
  "build_${tool//-/_}"
done
echo "Built into $TOOLS_DIR"
