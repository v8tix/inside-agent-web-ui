# These targets are just friendly shortcuts for scripts/*.sh, styled after Alex
# Edwards' Makefile for Go projects
# (https://www.alexedwards.net/blog/a-time-saving-makefile-for-your-go-projects).
# Each one hands off to the script that does the real work, so there's a single
# place to read or tweak the logic. For the full set of flags, check the comment
# at the top of the script you're curious about.

web_binary  = agent-web-ui
eval_binary = agent-eval
bin_dir     = bin

# ==================================================================================== #
#
# HELPERS
#
# ==================================================================================== #

## help: print this help message
.PHONY: help
help:
	@echo 'Usage:'
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'

.PHONY: confirm
confirm:
	@echo -n 'Are you sure? [y/N] ' && read ans && [ $${ans:-N} = y ]

.PHONY: no-dirty
no-dirty:
	@test -z "$(shell git status --porcelain)"

# ==================================================================================== #
#
# SETUP
#
# ==================================================================================== #

## install/env: write .env from .env.example, with fresh random passwords (scripts/init-env.sh)
.PHONY: install/env
install/env:
	scripts/init-env.sh

## install/tools: build golangci-lint, staticcheck, govulncheck, modernize and mockery into this repo's own bin/tools, which scripts/check.sh already puts on PATH for the gate (scripts/build-tools.sh)
.PHONY: install/tools
install/tools:
	scripts/build-tools.sh

# Heads up: this overwrites whatever golangci-lint, staticcheck, govulncheck,
# modernize and mockery you already have in this machine's Go bin (go env GOBIN,
# else GOPATH/bin), so every project on your machine picks up these versions, not
# just this one. That's why it's left out of `make install` — other projects might
# expect a different version, so run this one only when you actually want that.
## install/tools/global: build the same tools for this machine instead of just this repo (scripts/build-tools.sh --global)
.PHONY: install/tools/global
install/tools/global:
	scripts/build-tools.sh --global

## install: everything a fresh checkout needs before `make audit` or `make run`
.PHONY: install
install: install/env install/tools

# ==================================================================================== #
#
# QUALITY CONTROL
#
# ==================================================================================== #

## audit: the quality gate - formatting, vet, linters, vulnerabilities, race tests, coverage (scripts/check.sh)
.PHONY: audit
audit:
	scripts/check.sh

## audit/it: the quality gate plus every integration test; needs Docker, Node and Chrome (scripts/check.sh --it)
.PHONY: audit/it
audit/it:
	scripts/check.sh --it

## test: unit tests only, race detector on, no Docker
.PHONY: test
test:
	go clean -testcache
	go test -race ./internal/... ./cmd/...

## test/cover: unit tests with an HTML coverage report
.PHONY: test/cover
test/cover:
	go test -race -coverprofile=/tmp/coverage.out ./internal/... ./cmd/...
	go tool cover -html=/tmp/coverage.out

# ==================================================================================== #
#
# DEVELOPMENT
#
# ==================================================================================== #

## tidy: tidy go.mod/go.sum and gofmt every .go file
.PHONY: tidy
tidy:
	go mod tidy -v
	go fmt ./...

## build: build agent-web-ui and agent-eval into bin/
.PHONY: build
build:
	go build -o=${bin_dir}/${web_binary} ./cmd/web
	go build -o=${bin_dir}/${eval_binary} ./cmd/eval

## up: start PostgreSQL and the test agent only, to run the web server from source
.PHONY: up
up:
	docker compose up -d --wait

## down: stop what `make up` started
.PHONY: down
down:
	docker compose down

## run: run the web server from source against the stack started by `make up` (scripts/run.sh)
.PHONY: run
run:
	scripts/run.sh

## stop: stop the web server started with `make run` (scripts/stop.sh)
.PHONY: stop
stop:
	scripts/stop.sh

## full/up: start the whole stack in Docker - PostgreSQL, the test agent, the web server
.PHONY: full/up
full/up:
	docker compose --profile full up -d --wait

## full/down: stop the stack started with `make full/up` (add ARGS=-v to also delete the database)
.PHONY: full/down
full/down:
	docker compose --profile full down ${ARGS}

# ==================================================================================== #
#
# OPERATIONS
#
# ==================================================================================== #

## push: push to the remote only once the gate passes and the tree is clean
.PHONY: push
push: confirm audit no-dirty
	git push
