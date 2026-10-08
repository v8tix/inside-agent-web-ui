# inside-agent-web-ui

[![CI](https://github.com/v8tix/inside-agent-web-ui/actions/workflows/ci.yml/badge.svg)](https://github.com/v8tix/inside-agent-web-ui/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A ChatGPT-style web UI for any agent that speaks the ADK REST API, plus
**agent-eval** — a second program that drives that UI in a real browser just like
a person would, asks it GAIA problems, and scores both the agent's answers and the
UI itself. The binaries keep their original names, `agent-web-ui` and `agent-eval`.

- **Web UI** (`cmd/web`): built in Go, with server-rendered HTML, HTMX and
  Server-Sent Events, Bootstrap 5, and PostgreSQL. You get sign-in, conversations,
  streamed replies, Stop, Regenerate, rename and delete, a light/dark theme, and
  it works well on keyboard and phone.
- **Test agent** (`cmd/test-agent`): a real ADK REST server wrapped around a
  scripted model, so you can run and test the UI with no LLM and no cost.
- **agent-eval** (`cmd/eval`): the evaluator — see [Evaluating an agent](#evaluating-an-agent).

Both programs live in one Go module, `github.com/v8tix/inside-agent-web-ui` (Go
1.27), built as hexagonal services: `internal/core` doesn't know anything about
HTTP, SQL or browsers, and the adapters under `internal/infrastructure` and
`internal/eval/infrastructure` plug into its ports.

## Prerequisites

The scripts are bash, so you'll need macOS or Linux (Windows works through WSL).

| Tool | Version | Needed for |
|------|---------|------------|
| [Go](https://go.dev/dl/) | 1.27+ | both programs |
| [Docker](https://www.docker.com/) | any recent | PostgreSQL, the test agent, the full stack |
| [Node.js](https://nodejs.org/) (for `npx`) | 20.19+, 22.12+ or 23+ | `agent-eval`'s browser tests; older fails at once, naming the Node it found |
| Chrome | any recent | `agent-eval`, headless |
| `make` | any | the [Makefile](#makefile) targets below |

Once those are installed, `make install` takes care of the rest: it writes your
`.env` file and builds this project's own copies of the quality tools it
relies on, since the ones installed globally are often compiled with an older
Go than this module and then refuse to analyze it (mockery goes further and
just fails to load it) — see [Makefile](#makefile) for which tools and where
they go.

## Quick start

```sh
scripts/init-env.sh                   # writes .env with fresh random passwords
docker compose --profile full up -d --wait   # PostgreSQL, the test agent, the web server
open http://localhost:8300            # sign in with ADMIN_EMAIL / ADMIN_PASSWORD from .env
docker compose --profile full down    # stop it (add -v to delete the database)
```

Or `make full/up` and `make full/down` — see [Makefile](#makefile).

Want to run the web server from source instead of Docker? Start just the other
two services (`docker compose up -d --wait`, or `make up`) and run
`scripts/run.sh` (or `make run`). `scripts/stop.sh` (`make stop`) stops the
server; `docker compose down` (`make down`) stops the other two services.

To point this at your own agent, set `AGENT_BASE_URL` and `AGENT_APP_NAME` in
`.env`. Every setting is explained in `.env.example`, and a real environment
variable always wins over what's in `.env`.

By default, the `.env` that `scripts/init-env.sh` writes has the web server
listening on `127.0.0.1:8300` — this computer only (`APP_HTTP_ADDR`). Leave that
key out and it listens on every interface (`:8300`), which is what the container
needs. Keep in mind the page is plain http: before you put this in front of a
network, add a TLS proxy and give `APP_BASE_URL` its https address — that turns
on Secure cookies and HSTS, and `APP_ENV=production` won't start without it.
Compose also publishes the port on `127.0.0.1` only (`WEB_PORT` changes the
number, not the address).

## Makefile

Run `make help` any time to see every target. They're grouped the same way this
project's scripts are: setup (`install`, `install/env`, `install/tools`,
`install/tools/global`), quality control (`audit`, `audit/it`, `test`,
`test/cover`), development (`build`, `up`, `down`, `run`, `stop`, `full/up`,
`full/down`, `tidy`) and operations (`push`, which runs the gate and refuses to
push a dirty tree). Each target just calls the matching `scripts/*.sh` file, so there's one
place to go read (or change) what it actually does.

By default, `make install` (or `make install/tools` on its own) builds
`golangci-lint`, `staticcheck`, `govulncheck`, `modernize` and `mockery` into this
repo's own `bin/tools` folder, which `scripts/check.sh` already adds to `PATH` for
the gate — nothing outside the repo gets touched. If you'd rather have them
available everywhere on your machine, run `make install/tools/global` instead: it
overwrites the tools in your global Go bin (`go env GOBIN`, or `GOPATH/bin` if
that's unset), so every project you work on picks up the same versions. That one's
deliberately left out of `make install`, since it can replace binaries other
projects on your machine expect at a different version — so only run it when you
actually mean to.

## Chat with a real model

By default, Compose starts a scripted agent that just echoes your message back.
To chat with a real model instead, run a real ADK agent and point the Web UI at
it. `scripts/real-agent.sh` builds the `ui-agent` sample from `adk-training-go` (a
sibling checkout) and runs it on Gemini — every message you send goes to Google
with your key and counts against your quota. Gemini is the only model this script
supports.

Everything for this lives in `.env`, under the "Real agent" section (plus
`AGENT_BASE_URL`) — you never need to type a variable before a command.
`.env.example` explains each key.

1. **Settings.** In `.env`: `GOOGLE_AI_STUDIO_API_KEY=...` (get one from Google AI
   Studio — the script hands it to the agent and never prints it; without it, the
   agent falls back to its own `.env` and your environment), `GEMINI_MODEL`
   (defaults to `gemini-3.5-flash`; change it to any model name you want — nothing
   here enforces a particular one), `AGENT_BASE_URL=http://localhost:9094/api`
   and `AGENT_REQUEST_TIMEOUT=600s`.
2. **Database.**

   ```sh
   docker compose up -d --wait postgres
   ```

3. **Agent.**

   ```sh
   scripts/real-agent.sh
   ```

   It prints `The agent is ready ... serving "ui_agent" on Gemini`. The sample
   pins ADK 2.4, whose server listens on every network interface, so
   `REAL_AGENT_ADK` (set to the version in `go.mod`, via `.env.example`) builds it
   against 2.5 instead, which only listens on this computer, using a copy of the
   sample's `go.mod` — its own files are left untouched. If an agent does end up
   listening on the network, the script stops it for you.
4. **Web UI.**

   ```sh
   scripts/run.sh
   ```

   Then open <http://localhost:8300> and sign in with `ADMIN_EMAIL` and
   `ADMIN_PASSWORD` from `.env`. The first word can take 10 to 20 seconds while
   the model thinks.
5. **Stop.**

   ```sh
   scripts/stop.sh
   scripts/real-agent.sh stop
   ```

Want to switch back to the scripted agent? Set `AGENT_BASE_URL` to
`http://localhost:9093/api` and the timeout to `120s` (that's the agent
`docker compose up` starts, on port 9093).

## Configuration

**Every setting lives in `.env`** — for the programs, the scripts, Docker Compose
and the tests — and **`.env.example` documents every one of them**, with its
default and what it does. `scripts/init-env.sh` copies the example into `.env`
with freshly generated passwords, so you never need to type a variable before a
command. Here's who actually reads it:

- The programs read `.env` from whichever directory they're run in. A variable
  already set in your environment wins over the file, and a bad value stops the
  program at startup, listing every problem it found without showing any secrets.
  `agent-web-ui [--env FILE] [healthcheck [URL]]` reads its settings from FILE (or
  `.env` if you don't say otherwise), with your environment still taking
  priority. If you name a file that doesn't exist, pass an unknown flag, add a
  flag after `healthcheck`, or give a second address, it exits with status 2 and
  a usage line. `scripts/run.sh --env FILE` passes the file straight through, so
  the server never picks up keys from a `.env` sitting next to it. Every
  `agent-eval` command accepts `--env FILE` too, and refuses the same way if the
  file is missing.
- The scripts read `.env` (or whatever file `--env` points to, which has to
  exist) and fill in anything missing from `.env.example`. They don't look at
  your environment at all (aside from two hooks the tests use). `run.sh` and
  `eval.sh` hand the whole file to the program they start, which then overrides
  anything already set; `real-agent.sh` only passes the agent — someone else's
  program — its model settings.
- Docker Compose reads `.env` on its own and forwards every Web UI setting into
  its container; it decides `APP_HTTP_ADDR`, `DB_URL`, `DB_MIGRATE_ON_START` and
  `AGENT_BASE_URL` itself.
- An empty value counts as unset.

A test fails if any script, Go file or the Compose file reads a setting that
isn't documented in `.env.example`, so that file always stays the complete list.
Its sections are: App, Database, Agent, Chat, Rate limiting, Sessions, Docker
Compose, Scripted test agent, Real agent, Evaluator, Scripts and tools (the go
command, bin and log folders, coverage floors), Pull requests (title prefixes,
size limits), and Tests (the opt-in switches). Here are the keys you'll touch
first:

| Key | Default | What it does |
|-----|---------|--------------|
| `DB_URL` | required | PostgreSQL connection URL (a secret). Percent-encode `@`, `/`, `?` and `#` in its user and password (`%40`, `%2F`, `%3F`, `%23`): a URL setting with an `@` after its host is refused at start, and so is a `DB_URL` with more than one `@` before it. A `DB_URL` the driver cannot read stops the start with the driver's reason, as in `parsing DB_URL: invalid port`; the reason may name the piece it stumbled on, and the connection string as a whole is not quoted |
| `AGENT_BASE_URL`, `AGENT_APP_NAME` | required | The ADK REST agent to talk to |
| `AGENT_REQUEST_TIMEOUT` | `120s` | The longest a reply may take; use `600s` for GAIA |
| `APP_HTTP_ADDR` | `:8300` | Where to listen: a host, which may be left out, and a port from 1 to 65535 (`.env.example`: `127.0.0.1:8300`) |
| `APP_BASE_URL` | `http://localhost:8300` | The public address; https turns on Secure cookies and HSTS |
| `APP_ENV` | `development` | `production` refuses to start without an https `APP_BASE_URL` |
| `ADMIN_EMAIL`, `ADMIN_PASSWORD` | none | Creates the first user when there is none (password: 12+ characters) |
| `CHAT_MAX_MESSAGE_CHARS`, `SESSION_TTL` | `8000`, `24h` | Longest message; how long a sign-in lasts |
| `RATE_LIMIT_RPS`, `RATE_LIMIT_BURST` | `2`, `10` | Per user on sends, per client on sign-in; both greater than zero, the rate a finite number |
| `APP_LOG_LEVEL`, `APP_LOG_FORMAT` | `info`, `json` | Logging |
| `GEMINI_MODEL`, `GOOGLE_AI_STUDIO_API_KEY` | `gemini-3.5-flash`, none | The Gemini model that `scripts/real-agent.sh` runs, and its key; set `GEMINI_MODEL` to any model name you want |
| `AGENT_LIVE`, `EVAL_MEASURE` | `0`, `0` | Switch on the opt-in live and measurement tests |

The full tables, with the reasoning behind each default, are in
[`docs/spec.md`](docs/spec.md).

## Operating it

- **Logs.** JSON lines go to stdout (`scripts/run.sh` keeps a copy in `logs/`).
  The first line, `starting`, lists every setting under `config` by its field
  name (e.g. `config.DB.MaxConns=10`; nested objects as JSON) — `DB_URL` and
  `ADMIN_PASSWORD` show as `[redacted]`, along with the first user's address;
  URLs appear without credentials or query strings (one that can't be shown
  safely just refuses to start); `config.db` shows the host and database name;
  and `config.admin_bootstrap` says whether a first user is configured. Each
  request gets one line with `request_id`, `method`, `path` (no query string),
  `status`, `bytes`, `duration` (nanoseconds) and `remote` — nothing else: no
  header, cookie or form value, except a well-formed `X-Request-ID`, which
  becomes the request id and gets echoed back to the caller. A failed reply logs
  `reply failed` with its `message_id`; one that can't be rendered logs
  `rendering a reply failed` at error level, also with `message_id`;
  `stream ended: client went away` (debug level) just means a failed write.
  Message text and passwords are never logged.
- **Health.** `GET /healthz` confirms the process is up. `GET /readyz` checks the
  database and the agent, and returns 503 naming whatever failed (the full reason
  is in the log). The container's health check runs `agent-web-ui healthcheck`.
- **Stopping.** SIGTERM (via `scripts/stop.sh` or `docker stop`) lets replies in
  progress finish writing, saves each as stopped, and exits within
  `APP_SHUTDOWN_GRACE`. `scripts/stop.sh` waits that long plus 2 seconds before
  sending SIGKILL. It reads the setting the same way the server does — any Go
  duration like `1h30m`, `1.5s` or `500ms`, rounding any partial second up — and
  falls back to the server's own ten-second default if the setting is missing or
  invalid.
- **The page.** If a page request gets no answer within 30 seconds, it gives up
  and shows "The server took too long to answer. Try again." A reply's event
  stream has no such limit. Code blocks get a `data-lang` label in lowercase — a
  fence written `Go` becomes `go` — and an indented code block gets the same
  styling as a fenced one, labeled `text`, with a Copy button.
- **Back up and restore** (Compose database). The dump contains conversations
  and password hashes, so keep it private.

  ```sh
  docker compose exec -T postgres sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB"' > backup.sql   # back up
  docker compose exec -T postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"' < backup.sql      # restore, into an empty database
  ```

- **Upgrade and roll back** (single instance). The schema is applied at startup
  (`DB_MIGRATE_ON_START`), so take a backup before shipping a release with a
  migration: an older program talking to a newer schema stops with
  `no migration found for version N`. Setting `DB_MIGRATE_ON_START=false` lets it
  start anyway, which is only safe if the newer schema is backward compatible
  (add columns first, remove them in a later release).

  ```sh
  docker tag agent-web-ui:latest agent-web-ui:previous          # tag the running image first
  docker compose --profile full up -d --build web               # upgrade

  docker tag agent-web-ui:previous agent-web-ui:latest           # to roll back
  docker compose --profile full up -d --no-build --force-recreate web   # takes about six seconds
  ```

- **Limits to know.** Only one instance at a time — the Stop registry and rate
  limits live in memory. Long conversations get loaded in full. There's no TLS,
  no metrics endpoint, and no password change. Deleting a chat doesn't delete the
  agent's own copy of that session, and restarting the agent makes it forget the
  conversation, even though the history stays here.

## Evaluating an agent

`agent-eval` opens a headless Chrome through
[Chrome DevTools MCP](https://github.com/ChromeDevTools/chrome-devtools-mcp)
(pinned to a specific version, running with usage statistics off and a throwaway
profile), signs in to the Web UI, and for each problem: opens a new conversation,
types the GAIA prompt, waits for the reply (pressing Stop if it's taking too
long), reads the reply as stored, pulls out the `FINAL ANSWER:`, scores it with a
Go port of GAIA's official `question_scorer` (checked against 366 cases computed
by the official Python file), and judges how the UI behaved the whole time.

```sh
scripts/eval.sh selftest              # check the evaluator itself (no cost, about two minutes)
scripts/eval.sh datasets fetch        # download GAIA (needs HF_TOKEN in .env)
scripts/eval.sh datasets list         # what was downloaded
scripts/eval.sh run --level 1 --limit 5
scripts/eval.sh report <run-id>       # rebuild a run's summary
scripts/eval.sh probe                 # the UI probes alone
```

`scripts/eval.sh` builds `bin/agent-eval`, loads `.env`, and keeps a log in
`logs/`. You'll find every command and flag by running `scripts/eval.sh help`.

### Check the evaluator first

`selftest` runs 13 made-up problems against a scripted agent whose answers are
already known: eight right (a number written `$1,000`, a string in a different
case, a `;`-separated list...), one wrong, one missing a `FINAL ANSWER:`, one
error, one reply that's too slow, and one problem with a file attached. The
results have to match
`internal/eval/infrastructure/synthetic/data/selftest_expected.json` exactly,
every single run, and every required UI check has to pass. For this you need the
Web UI wired to that same agent:

```sh
docker compose --profile selftest up -d --wait   # use this instead of --profile full
scripts/eval.sh selftest
```

If it fails, that means the evaluator (or the UI) has a bug — not the agent.
Don't trust a GAIA score until this passes.

### What is measured

**For the agent** (per GAIA level, with 95% Wilson intervals): accuracy on the
problems it was asked plus on all eligible ones, coverage (problems with
attachments get skipped, which lowers it), status counts, how well it follows the
answer format, latency, tokens and tool calls when the agent reports them, and —
with `--repeats R` — how much the results vary across repeats and how many
problems flip between right and wrong.

**For the UI**, on every attempt (`U-`) and once per run as probes (`P-`). If a
*must* check fails, the run ends with exit status 2:

| Check | Must/Should | What it asks |
|-------|-------------|--------------|
| U-01 Console clean | must | no console errors or uncaught exceptions |
| U-02 Network clean | must | no failed request, none answered with 400 or more |
| U-03 Terminal state | must | the reply reaches a final status (a slow agent that Stop then ends is not a UI fault) |
| U-04 Incremental streaming | must | a long answer arrives in pieces |
| U-05 UI overhead | must | what the UI adds to the agent's own time to first token stays within 100 ms (95th percentile; the median when there are under 20 attempts; not judged when no attempt measured it) |
| U-06 Render fidelity | must | the final answer is in the text the page shows |
| U-07 Scroll follow | must | the view stays at the end of a growing reply |
| U-08 Render safety | must | no script, handler attribute or external image in the answer |
| P-01 Stop | must | takes effect within 1 s, keeps the text; a reply still being written with no Stop button fails |
| P-02 Regenerate | must | replaces only the last reply |
| P-03 Lighthouse | must | accessibility and best practices ≥ 90, desktop and phone |
| P-04 Mobile | should | nothing scrolls sideways at 375 px; conversations reachable |
| P-05 Rename / delete | should | both work and persist after a reload |
| P-06 Theme | should | the switch works and persists |
| P-07 Reload mid-stream | should | one reply, with its text, which ends |
| P-08 Keyboard only | should | send, stop and open a conversation without a mouse |
| P-09 Layout shift | should | CLS ≤ 0.1 while a long reply arrives |
| P-10 Agent down | should | a failed reply says so and Retry works (self-test only) |

The thresholds above are settings (`EVAL_*` in `.env.example`) — please don't
tune them just to make a run pass.

Every probe cleans up the conversations it started, whether it succeeds or
fails, unless you set `EVAL_KEEP_CHATS=true`.

### Running against GAIA

1. Add `HF_TOKEN` to `.env` — a Hugging Face token from an account that's
   accepted GAIA's terms.
2. Download GAIA, then check what came down:

   ```sh
   scripts/eval.sh datasets fetch
   scripts/eval.sh datasets list
   ```

3. Start the Web UI wired to whichever agent you want to evaluate
   (`AGENT_BASE_URL`, and `AGENT_REQUEST_TIMEOUT=600s` for tool-using agents).
   For a first try, `scripts/real-agent.sh` runs the `ui-agent` sample from
   `adk-training-go` at `http://localhost:9094/api`, application `ui_agent`, on
   Gemini (its key is `GOOGLE_AI_STUDIO_API_KEY` in `.env` — see *Chat with a
   real model* above). `scripts/real-agent.sh stop` shuts it down. The model
   takes a little time to think before answering (about 16 to 20 seconds before
   the first word on GAIA problems); a typical problem took 5 to 45 seconds on
   Gemini in testing. The ADK launcher cuts off a reply after it's streamed for
   two minutes, which a long answer can exceed — when that happens the page
   shows the reply cut short — so the script starts the agent with a ten-minute
   limit instead (`REAL_AGENT_SSE_TIMEOUT`). A sample built with an ADK older
   than 2.5 listens on every network interface, meaning anyone who can reach
   your machine could use your model and key — the script checks where it's
   listening and stops it unless you either build the sample against a newer
   ADK (`REAL_AGENT_ADK` in `.env`, already the default) or explicitly set
   `REAL_AGENT_ALLOW_LAN=1`.
4. Start with a quick try, then drop `--limit` for the real thing:

   ```sh
   scripts/eval.sh run --level 1 --limit 5
   ```

   `--repeats 3` tells you how much the answers vary. Ctrl-C stops things
   cleanly, and `--resume <run-id>` picks up where you
   left off, skipping anything already finished. Only run one `agent-eval` per
   run id at a time — nothing stops you from accidentally resuming the same run
   twice, and both would end up appending to `results.jsonl` (each row stays
   intact, but the file would then hold rows from both runs). If Ctrl-C cuts
   short the probes of a resumed run, whatever outcome was already in
   `probes.json` for the unfinished probe is kept.

A run writes everything to `results/<run-id>/`: `results.jsonl` (one row per
attempt, written as it finishes), `run.json` (versions, settings and
thresholds — never a secret), `probes.json`, `summary.json`, `summary.md`, and
`evidence/` (screenshots of failures). A run started with `--human-typing`
records `human_typing=true` among its settings — you'll see it in `run.json`, in
the Settings table of `summary.md`, and in a warning if a resume changes it.

**GAIA's terms forbid sharing its questions and answers**, so the evaluator
keeps `datasets/` and `results/` out of git and refuses to start if either is
tracked; results also hold no question, answer or reply text unless you pass
`--include-text`. Keep those files private.

### Exit status

| | |
|-|-|
| 0 | ok |
| 1 | wrong usage or configuration (including wrong credentials) |
| 2 | a required UI check or probe failed; also a settings file that `--env` names and that is not there |
| 3 | `--min-accuracy` not reached, or the self-test found other results than expected |
| 4 | something outside the evaluator broke (no browser, no Web UI, disk) |
| 130 | interrupted |

### When something is wrong

- *"the Web UI is not answering"*: start it (see above) and double-check
  `EVAL_BASE_URL`. The message tells you where that address points, without
  exposing any credentials it might carry.
- *self-test: "none of the scripted outcomes turned up"*: the Web UI is talking
  to the regular test agent instead. Use `--profile selftest`, and make sure
  nothing else (like a `scripts/run.sh` instance) is already listening on port
  8300.
- *sign-in refused*: set `EVAL_USER_EMAIL` and `EVAL_USER_PASSWORD`, or
  `ADMIN_EMAIL` and `ADMIN_PASSWORD` for the first user.
- *"refuses to start ... not ignored"*: add `datasets/` and `results/` to
  `.gitignore`.

## Tests

```sh
scripts/check.sh                  # the quality gate: format, vet, linters, vulnerabilities, race tests, coverage, the tests under it/ that need no Docker
scripts/check.sh --it             # the same, with all the integration tests
scripts/pr-check.sh --title "fix: stop dropping the partial reply"   # before a pull request: its title and size
go clean -testcache && go test -race ./internal/... ./cmd/...   # unit tests, no Docker
go test ./it/...                  # integration: needs Docker (add -short to skip browsers and image builds)
go test ./it/live/               # with AGENT_LIVE=1 in .env: the real agent it names; never part of the default run
```

Or just run `make audit`, `make audit/it` and `make test` — see
[Makefile](#makefile).

The gate expects 85% coverage on the use cases (`internal/core`,
`internal/eval/core`) and 70% everywhere else under `internal/` and `cmd/`,
counted from unit tests alone and excluding generated mocks (`COVER_CORE_MIN`,
`COVER_ALL_MIN`, `COVER_PKG_MIN` and `COVER_EXEMPT` in `.env` set these targets).
It prints both shares. The linters come from `bin/tools`, since globally
installed ones may predate Go 1.27 — `scripts/build-tools.sh` builds matching
ones for you.

After checking coverage, `scripts/check.sh` runs the tests under `it/` that
don't need Docker: `./it/scripts/` and the `TestRoutes_` tests in
`./it/security/`. It prints a line per package and names any skipped tests; if
something fails, it prints all of that output. `--it` runs everything under
`./it/...` instead; `--quick` skips just the race detector and the vulnerability
scan. Coverage floors are compared against the share as counted, and the printed
share is truncated (not rounded) to one decimal — so 84.96% prints as 84.9% and
counts as under 85 — with a decimal point regardless of locale. If the
integration tests fail, the gate prints the lines naming them and keeps the full
output in a temporary file, which its last line points to; if they pass, that
file gets removed.

On GitHub, the same checks run on every pull request (`.github/workflows/ci.yml`:
the gate, a secret scan, and the title/size check), plus a build, vet and test of
the Windows-specific process handling on a real `windows-latest` runner — the
only Windows machine this project has access to.

The integration suites spin up PostgreSQL with Testcontainers and run the real
application against the scripted agent. The chat and security suites start a
fresh stack per case (chat) or per test (security), and fail if any of the
application's goroutines are still running afterward.

| Suite | What it proves |
|-------|----------------|
| `it/chat` | A reply arrives in pieces and is saved as shown; Stop, closing the page and shutdown cancel the agent's run and keep the text; Regenerate touches only the last reply; a dead agent gives an error with Retry, and so does an answer the agent's server cuts off, with the text it got to; the flush interval setting paces the updates; replays never call the agent; reasoning never reaches the page; the Web UI adds under 100 ms (95th percentile) |
| `it/security` | Security headers on every route (the table follows every route pattern the web package names, in any file); anonymous visitors, other origins and other users get nowhere; hostile replies and user text stay inert; passwords and sessions are stored safely; cookies; size and rate limits; https; the server's log never holds what people write or the passwords they type |
| `it/container` | The images build from their Dockerfiles, run as an ordinary user, answer their health checks, carry a chat between them, and stop at once on `docker stop` |
| `it/evaldriver`, `it/evalselftest` | The evaluator against the real Web UI in headless Chrome: the driver, the observer, a reader who scrolls up, sending brings the view back, only browser-like requests, the self-test three times over, the probes, Ctrl-C and `--resume`, and browsers killed in the middle of a run |
| `it/app` | The assembled application: migrations, first user, restart recovery, and `/readyz` following the agent down and up |
| `it/scripts` | The shell scripts, run for real, with no Docker: the pid-file helpers, `.env` loading and its file mode, how settings are read (and what a broken or missing file does), that `run.sh` passes `--env` on, durations and how long `stop.sh` waits, the real-agent script's network and timeout handling, the pull-request check, the gate itself against stand-in tools, the settings Compose passes on, what `.gitignore` and `.dockerignore` keep out, and that the CI workflow runs the scripts |
| `it/live` | A real agent answers and can be stopped (`AGENT_LIVE=1` in `.env`) |

## Layout

```
cmd/web              the Web UI binary          cmd/eval         agent-eval
cmd/test-agent       the scripted ADK agent
internal/core        chat, conversation, auth, domain (ports in core/port)
internal/infrastructure   postgres, adkrest, web (HTTP, templates), auth, config
internal/contract    the hooks the evaluator relies on (data-testid, names, version)
internal/platform    what both programs share and neither owns (reading .env, secrets, a view of a struct for a log)
internal/testagent   the scripted model and its fixture mode
internal/eval/core   domain, ports, score (GAIA scorer), report, uicheck, probe, run
internal/eval/infrastructure   config, gaia (parquet, fetch), store, devtools (MCP), synthetic
assets               templates, static files (HTMX, Bootstrap vendored)
it                   integration tests
scripts              init-env, run, stop, eval, check, real-agent, build-tools, pr-check
sketches             small, standard-library-only reference implementations (see sketches/README.md)
.github              CI workflow, pull request form
```

Which package can import which is tested too (`internal/architecture`): a Web UI
use case can import its own domain and ports, and no other use case; an adapter
can import the domain and the ports, but no use case; neither side of core
imports `net/http`, `database/sql`, `os` or `io/fs`; and the two programs don't
know anything about each other. The same package also enforces two rules about
secrets: a `Secret` has to live in an exported struct field, and no string in a
production file can hold the `%p` verb.

The UI and the evaluator share one contract, `internal/contract`: stable
`data-testid` hooks, accessible names, and a version stamped in
`<body data-ui-contract>`. A test renders every page state and fails if a
template change would break the evaluator, and the evaluator itself refuses to
work with a page speaking a different version.

## Documentation

[`docs/spec.md`](docs/spec.md) is the specification as actually built —
requirements, the streaming flow, every route and its answers, the data model,
all configuration, the UI contract, the evaluator's checks and files, the tests,
and the evidence behind each success criterion. [`CHANGELOG.md`](CHANGELOG.md)
tracks what each release holds, and
[`docs/design-patterns.md`](docs/design-patterns.md) covers which Gang of Four
patterns the code uses, which ones it skips on purpose, and why.

## License

[MIT](LICENSE) — use, modify and sell it freely.
