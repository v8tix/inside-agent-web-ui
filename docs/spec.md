# Spec: agent-web-ui + agent-eval

| Field | Value |
|-------|-------|
| Status | **Final, as built.** Synced with the code at the end of Phase 7 (Ship) |
| Date | 2026-10-03 |
| Replaces | The Phase 1 draft (`temp/skills-output/phase-1-spec.md`, not tracked) |
| Scope | Two programs in one Go module: the Web UI (`agent-web-ui`) and its evaluator (`agent-eval`), plus a scripted test agent |
| Source of truth | The code and its tests. If this file and the code disagree, the code wins: fix this file |

Requirement IDs (`A`, `SC`, `EC`, `U`, `P`, `OQ`) are the ones the draft used, so older notes still resolve.

---

## 0. What changed from the draft

The build kept the draft's shape. These are the places where it differs, and why. Everything below this section describes the system as built.

| # | Draft said | As built | Why | See |
|---|-----------|----------|-----|-----|
| 1 | Stop closes the SSE connection | `POST /c/{id}/m/{mid}/stop` cancels the run, waits for the save, answers with the stored reply. Closing the stream still stops a run | A closed connection gives no moment to read the final state, and a Stop before the stream opened left the reply `pending` | 4.2 |
| 2 | Finished reply replays as `delta` + `done` | One `delta` with the stored article, then `done`, with zero agent calls | Same result, one render | 4.2 |
| 3 | Errors are a final `delta` then `done` | Same, and the stream **always answers HTTP 200**: foreign or missing replies say "This reply isn't available." inside the stream | `htmx-ext-sse` retries any HTTP error forever. This also settles SC-10 for streams | 4.2, 4.4 |
| 4 | Composition root in `api/web/app.go` | `internal/app` (`Build`, `Serve`) | The HTTP adapter must not import the Postgres and ADK adapters | 4.1 |
| 5 | UI contract is a JSON file | Go package `internal/contract` (hooks, states, names, attributes, version) | Tests read the table: the page test, the driver test and a scan of the hooks the evaluator names tie the templates and the evaluator to it. No production file imports it: the templates carry the version as a literal, and the evaluator keeps its own constant (`SupportedContract`) and scripts | 4.7 |
| 6 | Regenerate takes the conversation | Regenerate takes the message id; the store checks "last message" inside the transaction | Atomic: not latest → 409, not in this conversation → 404 | 4.4 |
| 7 | `x/time/rate` limiter | In-house token buckets (per user on sends, per client on sign-in, IPv6 keyed by /64) | Needed /64 keying and sweeping; no new dependency | 4.8 |
| 8 | `HTTP_MAX_BODY_BYTES=65536` | `131072`, and config checks it is at least `12 × CHAT_MAX_MESSAGE_CHARS + 4096` | A browser form-encodes one character in up to 12 bytes | 4.6 |
| 9 | `APP_HTTP_ADDR=:8300` | Code default `:8300`; `.env.example` and `init-env.sh` ship `127.0.0.1:8300`; production requires an https `APP_BASE_URL` | Loopback unless you choose otherwise | 4.6 |
| 10 | No request deadlines after headers | `Deadlines` middleware: `HTTP_BODY_TIMEOUT` 30 s, `HTTP_RESPONSE_TIMEOUT` 2 m; streams keep a per-write 15 s deadline | One slow client could pin a connection | 4.8 |
| 11 | Pages may be htmx fragments | Pages are always full documents; fragments exist only for rename, delete dialog, sidebar row and message bubbles | One way to answer a page | 4.4 |
| 12 | Static files plain | Gzipped once at startup, ETag, `no-store` on every private page, `immutable` for vendored files | First load was 405 KB against a 150 KB budget only the test met | 4.8 |
| 13 | Evaluator test page is a static fake | Integration tests start the real Web UI (Testcontainers Postgres, scripted agent) | A fake page drifts | 9 |
| 14 | `TESTAGENT_FIXTURE`, data in `internal/eval/testdata/` | `TEST_AGENT_FIXTURE`, data in `internal/eval/infrastructure/synthetic/data/` | Same prefix as the other `TEST_AGENT_*` keys; `go:embed` needs it under the package | 5.8 |
| 15 | U-03/U-05/U-07 simple pass rules | U-03 is `na` when a timed-out reply was stopped; U-05 is judged on p95 from 20 attempts, else the median; U-07 is `na` when nothing could scroll | A check that cannot fail proves nothing; p95 of 11 samples is the maximum | 5.6 |
| 16 | Screenshots on any failure | Opt-in (`--include-text`); always in the self-test | They show question and reply text, which GAIA's terms keep private | 5.9 |
| 17 | 5 exit codes, 6 commands | Added exit 130 (interrupted), exit 3 also for a self-test mismatch, exit 2 also for a settings file that `--env` names and that is not there, `run --no-probes`, `probe --scripted` | Shell convention; P-10 needs an agent that fails on demand | 5.3 |
| 18 | P-09 "CWV", P-10 "selftest only" | P-09 is layout shift (CLS ≤ 0.1); P-10 needs the scripted agent (`--scripted`, or the self-test) | Names say what is measured | 5.6 |
| 19 | Dependencies of the draft | ADK v2.5.0 (binds 127.0.0.1; the test-agent image passes `--host=0.0.0.0`), MCP Go SDK v1.8.0, genai v1.72.0, mockery v2.53.7 | Updated in Phase 5 | 3 |
| 20 | n/a | Added: `GET /c/{id}/item\|rename\|delete`, `healthcheck` subcommands, `/readyz` cache, reply cap 1 MiB, `endLost` repair, startup recovery, Compose profile `selftest`, `scripts/check.sh`, `internal/architecture` | Found while building and reviewing | 4, 6, 9 |
| 21 | Settings from `.env`, plus variables for scripts and tests | One home: `.env`, written down in `.env.example`, read by the programs, the scripts (`setting`), Compose and the tests; a test keeps it complete | The scripts took `GO`, `COVER_*` and others from the environment, where nobody could find them | 4.6 |

---

## 1. Assumptions

| # | Assumption | Status |
|---|-----------|--------|
| A1 | The Web UI is a client of an agent speaking the **ADK REST API** (`/api/run_sse`). Other protocols stay behind the `AgentGateway` port | Held |
| A2 | The browser never talks to the agent; the Go server proxies it | Held |
| A3 | Conversation history lives in Postgres; ADK sessions mirror it | Held, with two known gaps: an agent restart loses its session memory (backlog A12), and deleting a chat does not delete the agent's session (backlog A3) |
| A4 | Email + password sign-in; first user from `.env`; no self-sign-up | Held. There is also no password-change path |
| A5 | Bootstrap 5.3.8, vendored, no Node build step | Held |
| A6 | Router is stdlib `net/http` ServeMux with method patterns | Held |
| A7 | **Single Web UI instance** | Held, and relied on: the run registry (Stop), the rate limits and the startup recovery live in memory |
| A8 | Ports: web `8300`, agent `9093`, Postgres `5440` (the sample real agent uses `9094`) | Held, configurable |
| A9 | Phase documents live in `temp/` (ignored); the final spec lives in `docs/` | Done: this file |
| A10 | `agent-eval` lives in this module; the Web UI image builds only `./cmd/web` | Held |
| A11 | The evaluator is black-box and agent-agnostic: it never calls the agent or the Web UI API. The one exception is an in-page `fetch` of the documented raw endpoint | Held (EC-4) |
| A12 | Scoring uses the GAIA **`validation`** split. `test` has no public answers, so it can be asked but not graded (the evaluator warns) | Held |
| A13 | Prompts are plain text: GAIA's instructions + the question, ending with a `FINAL ANSWER:` marker | Held |
| A14 | **Attachments are out.** Problems with a file are skipped and lower coverage. Level 1 validation: 53 problems, 11 with a file | Held (verified on the real file) |
| A15 | Chrome stable (seen: 154), `chrome-devtools-mcp` pinned to 1.10.1 (Node 20.19 or later in 20, 22.12 or later in 22, or 23 and later; seen: 26.10), headless. The browser tests fail at once, naming the Node they found, when it is older | Held |
| A16 | MCP client is the official Go SDK (v1.8.0); Parquet is read with `parquet-go` (v0.32.0) | Held |
| A17 | Real GAIA runs need a tool-capable agent behind `AGENT_BASE_URL` | Held; see OQ-7 |

---

## 2. Objective

**Web UI.** A fast, familiar chat interface to an AI agent: type a message, watch the answer stream in, keep a history. Users are engineers and internal users of V8TIX agents.

**Evaluator.** Measure two things end to end, through the real UI in a real browser, with GAIA problems typed the way a person would:

1. **Agent quality.** GAIA accuracy with the official scoring rules, by level, plus coverage, latency, answer-format compliance and variance across repeats.
2. **UI quality.** Console and network errors, streaming cadence, rendered-answer fidelity, scroll behavior, accessibility and layout audits, and interaction probes.

### In scope

*Web UI:* conversations (create, list newest first, open, rename, delete; title = first 60 characters of the first message); chat (Enter sends, Shift+Enter adds a line; streamed reply; sanitized Markdown; highlighted code with copy; copy whole reply); Stop, Regenerate, Retry; autoscroll only near the bottom, with "Jump to latest"; responsive layout (offcanvas sidebar on phones), light/dark, keyboard accessible; sign-in/out with per-user isolation; the evaluator contract (4.7), raw-reply endpoint and message metadata; `.env` configuration, run/stop scripts, Compose infrastructure, health probes, structured logs.

*Evaluator:* `datasets fetch|list`, `run`, `selftest`, `probe`, `report`; GAIA loader (Hugging Face download or a local directory); GAIA scorer port; UI driver over Chrome DevTools MCP; eight passive checks per attempt and ten probes per run; JSONL + JSON + Markdown results with Wilson intervals; resumable runs.

### Out of scope (ports leave room)

Attachments and images in the UI (and so attachment-bearing GAIA problems), voice, tool-call or confirmation UI, multi-agent picker, conversation search, sharing, LLM-written titles, OIDC/SSO, Kubernetes, multi-instance streaming, TLS termination, sign-up and password change. Evaluator: LLM-as-judge, GAIA test-split leaderboard export, parallel pages in one browser, load testing, non-Chrome browsers.

---

## 3. Tech stack

Versions are the ones in `go.mod`, the Dockerfiles, `scripts/build-tools.sh` and the vendored assets at the date above.

| Concern | Choice |
|---------|--------|
| Language | Go 1.27 (`go 1.27`; images build with `golang:1.27.1-alpine`). Modern idioms in use: `encoding/json/v2` (case-sensitive members, explicit tags), stdlib `uuid`, `errors.AsType`, `t.Context()`, `wg.Go`, range-over-int, `testing/synctest` |
| HTTP | `net/http` ServeMux + `http.CrossOriginProtection`; no router or CSRF dependency |
| Templates | `html/template` + `embed` |
| Interactivity | htmx 2.0.11 + `htmx-ext-sse` 2.2.4, vendored |
| CSS / JS | Bootstrap 5.3.8 (MIT), vendored and checksummed; about 100 lines of our own JS/CSS |
| Markdown | `goldmark` 1.8.6 with tables, strikethrough and links made from bare addresses, `goldmark-highlighting/v2` and `chroma/v2` 2.27.0 → `bluemonday` 1.0.27. Task lists are off, so an item is shown as a list item that begins with `[ ]` or `[x]` (the sanitizer allows no `input` element) |
| Database | PostgreSQL 17.11, `pgx/v5` 5.11.0, `golang-migrate` 4.20.1 with embedded SQL |
| Auth | Opaque cookie → `sessions` table (SHA-256 of the token); `x/crypto` 0.57.0 argon2id; in-house rate limiter |
| Config, logs | `.env` + environment (`godotenv` 1.5.1), typed and validated at start; `log/slog` |
| Tests | `testing` + `testify` 1.12.1; `mockery` 2.53.7; `testcontainers-go` 0.44.0 |
| Test agent | `google.golang.org/adk/v2` 2.5.0 + `genai` 1.72.0, scripted `model.LLM` |
| Evaluator | MCP Go SDK 1.8.0 (`CommandTransport`) → `chrome-devtools-mcp@1.10.1` → Chrome; `parquet-go` 0.32.0; stdlib `net/http` for Hugging Face; stdlib `math` for Wilson intervals |
| Quality tools | golangci-lint 2.14.0, staticcheck 0.8.1, govulncheck 1.8.0, modernize and mockery on `x/tools` 0.51.0, all built into `bin/tools` by `scripts/build-tools.sh` |

**Dependency policy: stable releases only.** Every module that provides a built package is on its latest tagged, non-pre-release version (checked at Ship with `go list -u -deps -test`). Five modules have no tagged release at all, so they are pinned to a commit: `goldmark-highlighting/v2` (a 2023-07-29 snapshot; the module never tagged a v2), `jackc/pgerrcode`, `jackc/pgservicefile`, `genproto/googleapis/api` and `/rpc`. Pre-release versions (alpha, beta, rc) are not adopted.

CSS framework choice, kept for the record: Bootstrap (MIT, no build step, offcanvas, modal and dark mode built in) over Tailwind v4 + daisyUI (needs a Node toolchain), Pico (too few components) and Flowbite/Preline (Tailwind's cost). `chromedp` is a good future swap for the browser driver, because the driver is a port.

---

## 4. Part A: Web UI

### 4.1 Hexagonal map

```
cmd/web ─► internal/app (composition root: builds adapters, joins them to the use cases)

Driving adapter                 Core                                  Driven adapters
internal/infrastructure/        internal/core                         internal/infrastructure/
  api/web                                                               postgres   (stores, migrations)
HTTP handlers ─► port/service ─► use cases ─► port/resource ─►          agent/adkrest (AgentGateway)
templates, SSE    (inbound)      auth, chat,    (outbound)               auth (argon2 PasswordHasher)
middleware                       conversation
```

- A use case imports only `domain` and the ports: no other use case, and none of `net/http`, `database/sql`, `os` and `io/fs`. An adapter imports `domain` and the ports, and no use case; `internal/app` joins the two. Adapters do not import each other. They share one package, the settings: `adkrest`, `postgres`, `api/web` and `logging` import `internal/infrastructure/config`, and the rules in `internal/architecture` (`rules`) allow any import inside `internal/infrastructure`.
- `internal/platform` holds what both programs share. `envcfg` reads `.env` and has `Secret`. `redact` is a view of a struct for a log, with named fields and values that hide themselves hidden (`Struct`), and where a URL points (`URL`); it uses the standard library only. `tmplcache` reads a folder of `.gotmpl` files once and hands out the filled-in text by path, so a broken template fails the start and not a request; `internal/infrastructure/postgres` uses it for its SQL. `internal/contract` is the UI contract: only tests import it (4.7), and the page and the evaluator each carry the version as a literal.
- The evaluator has the same shape under `internal/eval` and imports nothing from the Web UI. Its core keeps out the same four packages of the standard library. Its use cases may import each other, and its adapters use three of them: the verdict functions of `probe`, a constant of `report` and a type of `run`.
- These rules are a test: `internal/architecture` fails with the file and the import when one is broken (`TestDependencies`). A second test gives the rules imports made for them, what they promise to refuse and what the programs are built from (`TestTheRulesCatchWhatTheyForbid`), and a third asks for production code under every directory a rule names. The package keeps two more rules, about secrets: a `Secret` sits in an exported struct field, and no string of a production file holds the verb `%p` (4.8).

### 4.2 Streaming flow

```
Browser                          Go web                                     ADK agent
  │ POST /c/{id}/messages         │ save user msg + assistant (pending)       │
  │──────────────────────────────►│                                           │
  │◄── HTML: two bubbles; the reply's wrapper has sse-connect                 │
  │ GET /c/{id}/m/{mid}/stream    │ claim (pending → streaming)               │
  │──────────────────────────────►│ POST /api/run_sse {streaming:true} ──────►│
  │◄── event: delta (whole article)◄── partial events ────────────────────────│
  │◄── event: delta, event: done  │ save text + meta, status = complete       │
```

1. **Updates.** Each `delta` carries the whole assistant article (sanitized HTML) as it stands. The first goes at once; after that, at most one per `max(CHAT_STREAM_FLUSH_INTERVAL, 4 × the time the last update took to make and send)`, always carrying the newest text, with a trailing flush so the last text never waits. That time is the render, the write and the flush, so a client that is slow to take an update gets them further apart. The end is a `delta` with the stored state, then `done`, which makes the browser close the stream.
2. **Starting.** `POST /c` creates the conversation and its first message and redirects to `/c/{id}`; the page opens the reply's stream as it loads, so there is one path to a streaming reply.
3. **Stop.** `POST /c/{id}/m/{mid}/stop` cancels the run this process owns, waits for the save, and answers with the reply as stored; swapping it in closes the stream. A reply that never started is claimed and finished as `stopped`. While it does so Stop is that reply's writer in the run registry: a stream that connects in between gets `retry`, a second Stop waits for the save, and both show the reply `stopped`. A reply that is already final, or that became final at the same moment, is answered as stored. A message that is not a reply is a 404, like an unknown or foreign one. Closing the stream (tab close) also stops the run and keeps the text as `stopped`.
4. **Replay.** Connecting to a final reply sends its stored article and `done`; the agent is not called.
5. **Lost runs.** A reply that says `streaming` but has no run in this process (a crash, a lost save) is ended as `failed` with "This reply was interrupted before it finished." (`endLost`). A reply another connection is writing gets `retry: 1500` and the stream ends; the browser reconnects.
6. **Failures** appear in the stream as the failed article with a user-safe sentence (`domain.Failure*`: agent unreachable, took too long, ran into a problem, answer cut off, grew too long, unknown, interrupted) and then `done`. Technical detail goes to the log, never the page. The stream always answers **HTTP 200**.
7. **Limits.** Reply cap 1 MiB (`DefaultMaxReplyBytes`; over it the run is stopped and the reply fails as "grew too long"); the agent run is bounded by `AGENT_REQUEST_TIMEOUT`; messages with NUL or invalid UTF-8 are refused with 422.
8. **Transport.** `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`, a first `: ok` comment, a `: keep-alive` comment every 15 s, a 15 s deadline on each write, and no global `WriteTimeout`.
9. **Segments.** A reply can have several segments (text, tool call, more text). A segment's final text replaces the deltas received for it; segments are joined with a blank line. Token usage and tool names are captured for the message metadata.
10. **Shutdown.** SIGTERM cancels each run and saves it as `stopped`; browsers get the final state and `done`; streams that start during shutdown get `retry`. The process exits within `APP_SHUTDOWN_GRACE`.
11. **Startup.** Open the listener → migrate (if `DB_MIGRATE_ON_START`) → mark every reply still `streaming` as failed (interrupted) → create the first user → serve. The listener comes first so a second process that cannot get the port fails before it can run the recovery against the first one's live replies.

### 4.3 ADK wire contract (ADK REST, `adk/v2` 2.5.0)

| Step | Call |
|------|------|
| Session | `POST {AGENT_BASE_URL}/apps/{app}/users/{userId}/sessions/{sessionId}`, body `{}`; created lazily, "exists" is fine. Session id = conversation id |
| Run | `POST {AGENT_BASE_URL}/run_sse`, body `{"appName","userId","sessionId","newMessage":{"role":"user","parts":[{"text":…}]},"streaming":true}`. camelCase; unknown fields return 400 |
| Frames | `data: <event-json>\n\n`; errors as `event: error` |
| Parse rules | Skip `thought:true` parts. `partial:true` chunks are deltas; the aggregated non-partial event is the final text, never appended to the deltas. Capture `usageMetadata` and `functionCall` names |
| Readiness | `AgentGateway.Ping` calls `GET {AGENT_BASE_URL}/list-apps` and checks that `AGENT_APP_NAME` is among them |
| Known gaps | An agent restart loses its in-memory session; history survives here, agent context does not. The ADK launcher ends a reply that has streamed for two minutes (`--sse-write-timeout`); the page then shows the reply cut off. `scripts/real-agent.sh` raises that to ten minutes |

### 4.4 HTTP surface

| Method | Path | Who | Answers |
|--------|------|-----|---------|
| GET | `/healthz` | public | `200 {"status":"ok"}`; never touches a dependency |
| GET | `/readyz` | public | `200`/`503` `{"status","checks":[{"name","status"}]}` for `database` and `agent`; each check has 2 s; the answer is cached 1 s; reasons go to the log, not the body |
| GET | `/static/{path...}`, `/favicon.ico` | public | Embedded files from memory, gzip when accepted, ETag; vendored files `immutable`, our own `no-cache` |
| GET | `/login` | public | Sign-in page (`303` to `/` when already signed in) |
| POST | `/login` | public, limited per client | `303` to `next` (a path on this site) or `/`; `422` "Wrong email or password." for every failed attempt; `503` + `Retry-After: 5` when the sign-in could not get a turn to hash its password, the same for every email; `429` + `Retry-After` |
| POST | `/logout` | session | Clears the cookie and goes to `/login`. When ending the session fails, the cookie is cleared all the same and the session stays valid until it expires; the failure is logged as an error. POST only |
| GET | `/` | session | New-chat page |
| POST | `/c` | session, limited per user | Create conversation + first message → redirect to `/c/{id}`; `422` on invalid text; `429` |
| GET | `/c/{id}` | session | Full page. `404` when missing or someone else's |
| PATCH | `/c/{id}` | session | Rename → the sidebar row (plus a `<title>` when it is the open chat); `422` redraws the form with the problem |
| DELETE | `/c/{id}` | session | Empty `200` (the row disappears); `HX-Redirect: /` when the page was showing it. Cascades to messages |
| GET | `/c/{id}/item`, `/rename`, `/delete` | session | Partials: sidebar row (cancel rename), rename form, delete confirmation (server HTML in `#modal-root`, never `window.confirm`) |
| POST | `/c/{id}/messages` | session, limited per user | htmx: the two new bubbles; plain form: redirect to the page. `409` while a reply is in flight; `422`; `429` |
| GET | `/c/{id}/m/{mid}/stream` | session | SSE (4.2). Always `200` |
| GET | `/c/{id}/m/{mid}/raw` | session | The stored Markdown: `text/plain; charset=utf-8`, `no-store`, `nosniff`. `409` until the reply is final; `404` otherwise |
| POST | `/c/{id}/m/{mid}/stop` | session | The reply as stored (HTML), also when it is already final. `404` for a message that is not a reply, an unknown one or another user's |
| POST | `/c/{id}/m/{mid}/regenerate` | session, limited per user | The new reply's wrapper, replacing the last reply. Also used by Retry. `409` "That reply can't be regenerated right now." when it is not the latest or still writing |

Rules that apply to every route:

- **Default deny.** A route is private unless `isPublic` lists it (`/healthz`, `/readyz`, `/login`, `/favicon.ico`, `/static/*`), so unknown paths send anonymous visitors to sign-in rather than 404.
- **Anonymous requests:** pages → `303 /login?next=…`; htmx → `HX-Redirect: /login`; event streams → an in-stream "Your session has ended" message; scripts (`Sec-Fetch-Mode` other than `navigate`) → `401` with `WWW-Authenticate`.
- **Someone else's id is a missing id** (`404`, same body), including `raw`, `stop`, `regenerate`, rename and delete. The stream is the one exception in shape, not in content: `200` with "This reply isn't available.", identical for foreign and missing ids.
- **Middleware, outermost first:** request id → access log → panic recovery → security headers → deadlines → cross-origin check → body limit → (route table) authentication. The deadlines sit inside the security headers and outside the two layers that refuse a request, the cross-origin check and the body limit; the cross-origin check comes first, so a request from another site is refused as such whatever it carries. The order of recovery, security headers and deadlines among themselves changes nothing that can be seen.
- **Errors** to the reader are plain sentences. A cross-origin write gets `403`, a too-large body `413`, a bad form `400`.

### 4.5 Data model

Migration `0001_init` (`internal/infrastructure/postgres/migrations`, embedded; `0001_init.down.sql` drops everything and exists for tests and local resets only). The SQL of the four stores is embedded `.gotmpl` files (`internal/infrastructure/postgres/sql`, one statement to a file, a column list shared by `{{template}}`), loaded once at startup into `postgres.Statements` by `internal/platform/tmplcache`.

| Table | Columns and rules |
|-------|-------------------|
| `users` | `id uuid` PK, `email` unique and stored lowercase (CHECK `email <> '' AND email = lower(email)`), `password_hash` (argon2id PHC string), `created_at` |
| `sessions` | `id text` PK = **SHA-256 hex of the cookie token** (a leaked table cannot be replayed), `user_id` → users (cascade), `expires_at`, `created_at`; indexes on `user_id` and `expires_at` |
| `conversations` | `id`, `user_id` → users (cascade), `title` (1–120 characters), `created_at`, `updated_at`; index `(user_id, updated_at DESC)` for the sidebar |
| `messages` | `id`, `conversation_id` → conversations (cascade), `seq > 0` with `UNIQUE (conversation_id, seq)`, `role` (`user`\|`assistant`), `content` (Markdown exactly as typed or produced), `status` (`pending`\|`streaming`\|`complete`\|`stopped`\|`failed`), `error`, `meta jsonb`, `created_at`, `updated_at` |

`meta` holds `ttft_ms`, `duration_ms`, `usage{prompt,completion}` and `tool_calls[]`. Times are measured on the server and rounded **up** to whole milliseconds, so 0 only ever means "not measured"; unknown values are left out of the JSON and the page. `ttft_ms` is the time from sending the upstream request to the first non-empty text.

Status life: `pending → streaming → complete | stopped | failed`. Final messages never change; Regenerate and Retry replace the last reply with a new `pending` one.

### 4.6 Configuration

**Every setting lives in `.env`, and `.env.example` writes each one down** with its default: the Web UI, the evaluator, the test agent, Docker Compose, the scripts and the opt-in tests alike. Nothing needs a variable typed before a command.

- **Programs** read `.env` from their working directory, or the file `--env` names; a variable already in the environment wins over the file. An empty value counts as unset. All problems are reported at once and no secret is echoed; a syntax error in the file prints a fixed message, never the file. `agent-web-ui [--env FILE] [healthcheck [URL]]` reads its settings from FILE, `.env` unless told otherwise, with the environment over it. A named file that does not exist, an unknown flag or word, a flag after `healthcheck` or a second address ends it with status 2 and a usage line. `scripts/run.sh --env FILE` passes the file on, so no key comes from the `.env` beside the server. Every command of `agent-eval` takes `--env FILE` too, and refuses a named file that does not exist with the same status (5.3). The file that is not named, `.env`, may be missing: the settings are then the environment's.
- **Scripts** read `.env` (or the file `--env` names) with `setting` (`scripts/lib/process.sh`) and take what it lacks from `.env.example`; an empty value counts as unset. They do not look at the environment, apart from two hooks for tests (`ENV_EXAMPLE`, `LSOF`). Each settings file is read by a bash started for it (it has the environment, not the script's own variables, and no `BASH_ENV`), as strictly as the scripts run (`set -eu`) under bash 5 as under 3.2, and only the one value comes out, so asking for a setting does not put the file's passwords and tokens in the script's environment. A file that cannot be read (a syntax error, an unset `$VARIABLE`, an unquoted space), or that `--env` names and does not exist (status 2), is an error. `run.sh` and `eval.sh` then load the whole file for the program they start, and the file wins over variables already set; `real-agent.sh` gives the agent only its model settings.
- **Compose** reads `.env` itself and passes every setting of the Web UI on to its container, also `HTTP_MAX_BODY_BYTES`, `HTTP_BODY_TIMEOUT`, `HTTP_RESPONSE_TIMEOUT`, `DB_MAX_CONNS`, `CHAT_STREAM_FLUSH_INTERVAL` and `CHAT_LIST_LIMIT`; it decides `APP_HTTP_ADDR`, `DB_URL`, `DB_MIGRATE_ON_START` and `AGENT_BASE_URL` itself.
- **Tests** that take a setting (`AGENT_LIVE`, `EVAL_MEASURE`, the go command) read it from `.env`; the golden file is rewritten with `-update`, not a variable.

Three tests keep the file the complete list: each loader's test fails on a key it reads that the example lacks (Web UI, evaluator, test agent); `it/scripts/settings_test.go` fails on a script that asks for an undocumented setting or reads a variable with a default of its own, on a Compose variable the example lacks, on a key of the Web UI's loader that Compose does not take from `.env` for both web services, on a Go read of an undocumented variable, and when `REAL_AGENT_ADK` in the example drifts from the ADK in `go.mod`.

| Key | Default | Notes |
|-----|---------|-------|
| `APP_ENV` | `development` | `development`, `test`, `production`. Production needs an https `APP_BASE_URL` |
| `APP_HTTP_ADDR` | `:8300` in code; `127.0.0.1:8300` in `.env.example` | A host, which may be left out, and a port from 1 to 65535; anything else is reported at start with the other problems of the settings. Loopback keeps the page to this computer. The container sets `:8300` and Compose publishes on the host's `127.0.0.1` |
| `APP_BASE_URL` | `http://localhost:8300` | The trusted origin for the cross-origin check. https turns on `Secure` cookies and HSTS |
| `APP_LOG_LEVEL`, `APP_LOG_FORMAT` | `info`, `json` | `debug\|info\|warn\|error`; `json\|text` |
| `APP_SHUTDOWN_GRACE` | `10s` | How long a shutdown may take (4.2); `scripts/stop.sh` waits that long and 2 s more (4.9) |
| `HTTP_MAX_BODY_BYTES` | `131072` | Must be ≥ `12 × CHAT_MAX_MESSAGE_CHARS + 4096` |
| `HTTP_BODY_TIMEOUT`, `HTTP_RESPONSE_TIMEOUT` | `30s`, `2m` | Event streams are not held to these. Both must be greater than zero: a limit is off only for code that builds the options itself |
| `DB_URL` | none, **required** | A secret. The start line shows its host and its database (`config.db`), and the driver's error for a database it cannot reach names the user, the host and the database. In the user and the password, `@`, `/`, `?` and `#` are percent-encoded (`%40`, `%2F`, `%3F`, `%23`). Written as they are, they end the user and the password, or the host, at another place than was meant, for the loader, for the driver or for both, and an error then names a piece of the password as a host or a database. So the loader refuses, at start and with the other problems of the settings, every URL setting that has an `@` after its host, and a `DB_URL` that has more than one `@` before it; the text is fixed and holds nothing of the value. A `DB_URL` the driver cannot read stops the start with the driver's reason, as in `parsing DB_URL: invalid port`; the reason may name the piece it stumbled on, and the connection string as a whole is not quoted |
| `DB_MAX_CONNS`, `DB_MIGRATE_ON_START` | `10`, `true` | |
| `AGENT_BASE_URL`, `AGENT_APP_NAME` | none, **required** (`http://localhost:9093/api`, `ui_agent` in the example) | |
| `AGENT_REQUEST_TIMEOUT` | `120s` | Use `600s` for GAIA runs; tool-using agents are slow |
| `CHAT_MAX_MESSAGE_CHARS` | `8000` | |
| `CHAT_STREAM_FLUSH_INTERVAL` | `50ms` | The floor of the pacer (4.2) |
| `CHAT_LIST_LIMIT` | `100` | Conversations in the sidebar |
| `RATE_LIMIT_RPS`, `RATE_LIMIT_BURST` | `2`, `10` | Per user on sends; per client on sign-in. Both must be greater than zero, and the rate a finite number (`NaN` and `Inf` are refused): a limit is off only for code that builds the options itself |
| `SESSION_TTL` | `24h` | |
| `ADMIN_EMAIL`, `ADMIN_PASSWORD` | none | Creates the first user when none exists. Both or neither; password at least 12 characters |

Settings for the scripts, the test agent and the tests, all in `.env.example`: `TEST_AGENT_WORD_DELAY` (10ms), `TEST_AGENT_SLOW_DELAY` (100ms), `TEST_AGENT_SLOW_CHUNKS` (50), `TEST_AGENT_FIXTURE`; the real agent (`scripts/real-agent.sh`, which runs it on Gemini and on no other model): `GEMINI_MODEL`, `GOOGLE_AI_STUDIO_API_KEY`, `ADK_TRAINING_DIR`, `REAL_AGENT` (`ui-agent`), `REAL_AGENT_APP` (`ui_agent`), `REAL_AGENT_PORT` (9094), `REAL_AGENT_SSE_TIMEOUT` (10m), `REAL_AGENT_ADK` (the ADK of `go.mod`; `sample` for the version the sample pins), `REAL_AGENT_ALLOW_LAN` (0); scripts and tools: `GO`, `BIN_DIR` (bin), `LOG_DIR` (logs), `TOOLS_DIR` (bin/tools), `COVER_CORE_MIN` (85), `COVER_ALL_MIN` (70), `COVER_PKG_MIN` (50), `COVER_EXEMPT`; tests: `AGENT_LIVE` (0), `EVAL_MEASURE` (0).

Also read by Compose only: `POSTGRES_USER`, `POSTGRES_PASSWORD` (must match `DB_URL`; Compose writes it into the `DB_URL` of its web server as it is, so keep it to what a URL takes as it is: letters, digits, `-`, `.` and `_`), `POSTGRES_DB`, `POSTGRES_PORT` (5440), `AGENT_PORT` (9093), `WEB_PORT` (8300). The test agent reads `TEST_AGENT_WORD_DELAY` (10 ms), `TEST_AGENT_SLOW_DELAY` (100 ms), `TEST_AGENT_SLOW_CHUNKS` (50) and `TEST_AGENT_FIXTURE` (5.8). Evaluator keys are in 5.4.

### 4.7 UI test contract (version 1)

`internal/contract` is the single definition (Go code). A test renders every page state (sign-in, empty, finished, failed, all statuses, rename form, delete dialog) and fails when a hook is missing, misnamed or lacks an attribute. `<body data-ui-contract="1">` is on every page, and the evaluator refuses a page that speaks another version.

| Group | `data-testid` hooks | Accessible name / attributes |
|-------|--------------------|------------------------------|
| Sign-in | `login-email`, `login-password`, `login-submit` | "Email", "Password", "Sign in" |
| Every page | `theme-toggle`, `live-status`, `logout` | "Theme…"; `role` + `aria-live`; "Sign out" |
| Composer | `composer-input`, `send`, `stop`, `composer-error` | textarea "Message"; "Send message" (disabled while a reply streams); "Stop generating" (only while streaming); `role` |
| Conversation | `messages`, `jump-latest`, `message`, `message-body`, `error`, `regenerate`, `retry`, `copy-message` | `message` is an `article` with `data-role` (`user`\|`assistant`), `data-id`, `data-status` (the five statuses); "Regenerate response"; "Retry"; "Copy response" with `data-raw-url`; `jump-latest` is hidden until the reader scrolls up |
| Sidebar | `sidebar`, `sidebar-toggle`, `new-chat`, `conversation`, `conversation-link`, `rename`, `delete` | `nav`; "Open conversations"; "New chat"; `li[data-id]`; the title; "Rename conversation {title}"; "Delete conversation {title}" |
| Rename, delete | `rename-input`, `rename-save`, `rename-cancel`, `rename-error`, `delete-dialog`, `delete-confirm`, `delete-cancel` | "Conversation title", "Save", "Cancel"; dialog has `aria-labelledby`; "Delete", "Cancel" |
| Telemetry | `data-ttft-ms`, `data-duration-ms`, `data-prompt-tokens`, `data-completion-tokens`, `data-tool-calls` on final assistant messages | Absent, never zero or empty, when unknown |

Change rule: adding a hook, or a state where one appears, is compatible. Removing or renaming a hook, changing an accessible name, a data attribute or its meaning raises `Version`, updates the evaluator in the same change, and says so in the commit message.

The evaluator clicks and types by accessible name (it works from the accessibility tree, where elements have uids) and reads state through `data-testid` selectors in `evaluate_script`.

**Rendered replies.** Every code block sits in a `div.code-block` whose `data-lang` is its language in lower case (a fence written `Go` is `go`), or `text` when it names none or one outside a safe alphabet. A code block made by indenting gets the same container as a fenced one, with the label `text`. The page puts a Copy button in each container once the reply has ended. A link keeps its address only (a title is dropped); every link gets `rel="nofollow noreferrer"`, and a web link also `target="_blank"` and `noopener`.

### 4.8 Security and hardening

| Area | What is in place |
|------|------------------|
| Passwords | argon2id (19 MiB, 2 iterations, 1 lane, encoded in each hash so cost can be raised later). Concurrent hashes are capped at `GOMAXPROCS` so a sign-in flood cannot multiply the memory. A hash waits at most five seconds for its turn and not at all once the visitor has left; a sign-in turned away for being too busy is a `503` with `Retry-After`, the same for every email. Parameters read back from a stored hash are capped. Passwords over 1024 bytes are refused. Unknown emails cost one decoy hash, and every failure shows one message |
| Sessions | 32 random bytes in an `awu_session` cookie: `HttpOnly`, `SameSite=Lax`, `Path=/`, `Secure` over https. Only the SHA-256 digest is stored. A stale cookie is cleared. Sign-out is POST only |
| CSRF | stdlib `CrossOriginProtection` (`Sec-Fetch-Site`, `Origin`); `APP_BASE_URL` is trusted so a proxy that rewrites `Host` still works. Cross-origin writes get `403` |
| Headers (every response) | `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'` (no inline script or style), `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, `X-Frame-Options: DENY`, `Cross-Origin-Opener-Policy` and `-Resource-Policy: same-origin`, `Permissions-Policy` (camera, microphone, geolocation, payment, usb off), `Cache-Control: no-store` unless a route says otherwise, `Strict-Transport-Security` over https only. No CORS headers: nothing is meant to be read cross-origin |
| Model and user text | Markdown → goldmark → bluemonday. No raw HTML, inert `javascript:` links, no `on*` attributes, and Markdown images become plain links, so the page never fetches a remote image. User text is plain text. htmx is configured with `allowEval=false`, `allowScriptTags=false`, no injected style, no settle delay, and a time limit of 30 s: a request of the page that gets no answer by then is given up, and the page says "The server took too long to answer. Try again." A reply's event stream has no such limit |
| Input | Body cap `HTTP_MAX_BODY_BYTES` (declared length refused up front, chunked bodies cut while read); message cap `CHAT_MAX_MESSAGE_CHARS`; titles 1–120 characters; NUL and invalid UTF-8 refused |
| Rate limits | Token buckets in memory: per user on `POST /c`, `/messages`, `/regenerate`; per client on `POST /login` (an IPv4 address, or a whole IPv6 /64). `429` with `Retry-After`; the typed text is kept. Idle buckets are forgotten at most once a minute, when a limited request arrives. Behind a reverse proxy everyone shares the proxy's address: limit sign-ins at the proxy as well |
| Isolation | Every store query is scoped by user id; another user's data is "not found" |
| Deadlines | Header read 10 s, idle 120 s, body 30 s, non-stream response 2 m; streams use a 15 s deadline per write |
| Resilience | Panics are recovered in the middleware, the stream goroutine and the readiness checks; a panic mid-reply ends that reply as failed (interrupted), with the text that had arrived |
| Logging | No message content or secrets in logs (a test types canaries and checks). The first line, `starting`, shows every setting under `config` by the name of its field (`config.DB.MaxConns=10`; nested objects as JSON): `[redacted]` for `DB_URL`, `ADMIN_PASSWORD` and the address of the first user, URLs without credentials or query (a URL that cannot be shown so is refused at start, 4.6), `config.db`, the host and name of the database, and `config.admin_bootstrap`, whether a first user is set. A `Secret` prints `[redacted]` under every verb `fmt` asks it about, and two tests keep it where it can be asked: one fails on a `Secret` in a struct field that is not exported, the other on the verb `%p`, which `fmt` answers itself, in a string of a production file. A line of the access log has seven attributes and no more (4.9). `X-Request-ID` is accepted if well formed, else generated |
| Containers | Distroless `static` image, non-root user, binary-based health checks (`agent-web-ui healthcheck`, `test-agent healthcheck`), ports bound to `127.0.0.1` |
| Not provided | TLS (put a proxy in front and set an https `APP_BASE_URL`), a metrics endpoint, an audit log |

### 4.9 Operations

- **Logs.** `log/slog`, JSON by default, to stdout; the scripts keep them in `logs/`. The first line, `starting`, shows the settings (4.8). Each request logs one line at info with seven attributes and no more (`request_id`, `method`, `path`, `status`, `bytes`, `duration` in nanoseconds, `remote`): the path stands without its query, and no header, cookie or form value is logged, except a well-formed `X-Request-ID`, which becomes the request id. Probes and static files log at debug. Failures log at warn or error with the `message_id`: a reply that cannot be rendered is logged at error level as `rendering a reply failed`, and `stream ended: client went away` (debug) is a failed write only.
- **Health.** `/healthz` (liveness) and `/readyz` (database + agent). `agent-web-ui healthcheck [url]` asks `/readyz` and is what the container health check runs.
- **Scripts.** `scripts/run.sh [--env file] [--rebuild]` builds if needed, refuses a double start, starts in the background (with `--env file` when the file is not `.env`), waits up to 20 s for `/readyz`, logs to `logs/agent-web-ui-<time>.log` (on failure it prints the last lines, stops what it started and exits 1). `scripts/stop.sh` sends SIGTERM, waits `APP_SHUTDOWN_GRACE` plus 2 s, then SIGKILL, and is safe to run twice. It reads the setting as the server does (any Go duration: `1h30m`, `1.5s`, `500ms`), a part of a second counting as a whole one, and takes the server's ten seconds when the setting is missing or no duration. `scripts/init-env.sh` writes a private `.env` (mode 600) with generated database and first-user passwords, never overwrites a non-empty one, and never prints the passwords.
- **Compose.** `docker compose up -d --wait` starts Postgres and the scripted agent; `--profile full` adds the web server; `--profile selftest` starts the web server wired to an agent that plays the evaluator's self-test script. `full` and `selftest` publish the same web port, so use one at a time.
- **Known limits.** Single instance; Stop registry and rate limits are in memory; no pagination of long conversations; no TLS; no metrics.

---

## 5. Part B: Evaluation framework (`agent-eval`)

### 5.1 Architecture

```
agent-eval (cmd/eval: flags and wiring only)
   │
   ▼
Runner (core/run) ──► ProblemSource ──► GAIA parquet (Hugging Face fetch | local dir) | synthetic JSONL
   │       │
   │       ├────────► UIDriver ─► devtools adapter ─stdio MCP─► chrome-devtools-mcp ─CDP─► Chrome ─► agent-web-ui ─► agent
   │       ├────────► UIProbes (functional probes and audits, same adapter)
   │       ├────────► Scorer (core/score: pure functions, GAIA rules)
   │       └────────► ResultStore ─► results.jsonl · run.json · probes.json · summary.json · summary.md · evidence/
```

Ports: services `Runner`, `Reporter`, `Prober`; resources `ProblemSource`, `UIDriver`, `DriverFactory` (a driver that died is replaced by asking for another), `UIProbes`, `ResultStore`. The `UIDriver` speaks in intentions, not clicks: `Login`, `StartChat`, `Submit`, `AwaitReply`, `Stop`, `ReadRaw`, `Collect`, `Screenshot`, `DeleteChat`, `Close`. The core (`domain`, `port`, `score`, `report`, `uicheck`, `probe`, `run`) knows nothing about Chrome, files or HTTP; checks and probe verdicts are pure functions of measurements and thresholds.

### 5.2 One attempt, like a human

1. A fresh chat ("New chat"), so the agent starts with no memory of earlier problems. Viewport fixed at 1280×800 (`emulate`).
2. Compose the prompt (GAIA's instruction text + the question) and put it in the composer. Default: fill. `--human-typing`: key by key with `EVAL_TYPING_DELAY`, a line break typed as Shift+Enter. A run started with `--human-typing` records `human_typing=true` among its settings: it shows in `run.json`, in the Settings table of `summary.md`, and in the warning of a resume that changes it.
3. Press Enter. An in-page observer, installed through DevTools outside the UI's code, records on **one page clock** (`performance.timeOrigin + performance.now()`): Enter, text updates (visible text only), status changes and scroll samples. The duration of a tool call is never used: calls to the browser are slow and uneven while a stream runs.
4. Wait for the assistant message's `data-status` to be final, or for the task timeout, then press Stop through the UI and record `timeout`.
5. Read the reply as stored from `GET /c/{id}/m/{mid}/raw` (an in-page fetch, same-origin path only). **Answer rule (frozen):** the first non-empty line after the **last** `FINAL ANSWER:` (any case), with `**` removed. No marker → `no_final_answer`; a backticked answer or one with trailing explanation is scored as written.
6. Score the answer against the truth: `correct` or `incorrect`. An attempt that already ended as `timeout`, `agent_error` or `ui_error` is not scored.
7. After scoring, run the passive checks over the observations (they are given the answer and whether time ran out); screenshot on failure when screenshots are allowed (5.9); delete the chat (unless `EVAL_KEEP_CHATS=true`). The runner then appends one JSONL row.

A driver that dies mid-attempt, or whose web session ended (the page asks to sign in again), is replaced by a new browser that signs in, and the attempt is tried once more; a second death is recorded as `ui_error` ("the page broke"). The session logs one warning, `the browser session was lost`, with `tool` and `error`, when a tool does not answer in time or the server goes away. The error quotes the last 600 characters of the server's output (`its output ended with: …`).

A console or network listing is read up to 50 pages of 200 entries; a longer one is cut there with a warning (`a listing was cut at the page limit`, with `tool`, `pages`, `pages_listed`), and U-01 and U-02 judge what was read.

### 5.3 CLI

| Command | Purpose |
|---------|---------|
| `agent-eval datasets fetch` | Download `HF_GAIA_DATASET` / `HF_GAIA_LEVELS` parquet files into `HF_DATASETS_DIR` (needs `HF_TOKEN`; skips files already present at the same size) |
| `agent-eval datasets list` | Show local configs, splits, row counts, attachment counts |
| `agent-eval run [--level 1\|2\|3\|all] [--limit N] [--task-id ID …] [--repeats R] [--workers W] [--timeout D] [--human-typing] [--resume RUN_ID] [--include-text] [--no-probes] [--min-accuracy X]` | Real run, then the probes unless `--no-probes` |
| `agent-eval selftest` | The synthetic problems against the scripted agent; compares with the golden file |
| `agent-eval probe [--scripted]` | Probes and audits only; `--scripted` says the agent is the scripted one (adds P-10) |
| `agent-eval report RUN_ID [--format md\|json]` | Rebuild a finished run's summaries from disk |

Every command takes `--env FILE` (default `.env`). A named file that does not exist ends the command before it does anything, with `agent-eval: no settings file "FILE"` and status 2, the status the scripts and `agent-web-ui` end with for the same mistake; `.env` itself may be missing. `scripts/eval.sh` builds `bin/agent-eval`, loads `.env`, and logs to `logs/agent-eval-<time>.log`.

**Exit codes:**

| Code | Meaning |
|------|---------|
| 0 | OK |
| 1 | Usage or configuration error (including refused credentials, or no problem matching the filters) |
| 2 | A required (Must) UI check or probe failed, or the page broke attempts (`ui_error`; resume asks them again). Also the status for a settings file that `--env` names and that is not there |
| 3 | `--min-accuracy` not reached, or the self-test found other results than the golden file |
| 4 | Infrastructure failure: no Web UI, not ready, the browser could not be driven, disk |
| 130 | Interrupted. What finished is saved; `--resume RUN_ID` continues |

### 5.4 Configuration (same `.env`)

Hugging Face and dataset keys reuse the names from `ai-agent-from-scratch-go`; their values were never read or copied.

| Key | Default | Notes |
|-----|---------|-------|
| `HF_TOKEN` | none | Needed by `datasets fetch` only. Never logged or written; sent only to the configured hosts (also across redirects); the MCP child never sees it |
| `HF_DATASETS_SERVER_URL`, `HF_HUB_URL` | `https://datasets-server.huggingface.co`, `https://huggingface.co` | https only (http for this computer), because the token is sent to them. They are compared with a request as they are written (scheme, host, port): `https://host:443` is not `https://host` |
| `HF_GAIA_DATASET`, `HF_GAIA_LEVELS` | `gaia-benchmark/GAIA`, `2023_level1` | Comma list of `2023_level1\|2023_level2\|2023_level3\|2023_all` |
| `HF_DATASETS_DIR` | `./datasets` | Must not be tracked by git. May point at an existing directory, read-only |
| `GAIA_SPLIT`, `GAIA_ATTACHMENTS` | `validation`, `skip` | `validation\|test`; `skip\|include` |
| `RESULTS_DIR` | `./results` | Must not be tracked by git |
| `EVAL_BASE_URL` | `http://localhost:8300` | May carry credentials: the browser is given it as it is, and an error names where it points and nothing else (5.9) |
| `EVAL_USER_EMAIL`, `EVAL_USER_PASSWORD` | none | Fall back to `ADMIN_EMAIL`/`ADMIN_PASSWORD` |
| `EVAL_MCP_COMMAND` | `npx -y chrome-devtools-mcp@1.10.1` | Must be `npx`, then only `-y --yes --no-install --prefer-offline -q --quiet`, then a **pinned** `chrome-devtools-mcp@x.y.z` |
| `EVAL_MCP_ARGS` | `--headless --isolated --no-usage-statistics --no-performance-crux` | All four are mandatory |
| `EVAL_TASK_TIMEOUT` | `10m` | Keep ≤ `AGENT_REQUEST_TIMEOUT` (`run` warns otherwise) |
| `EVAL_WORKERS`, `EVAL_REPEATS` | `1`, `1` | A worker is one MCP process + Chrome |
| `EVAL_TYPING_DELAY` | `25ms` | Only with `--human-typing` |
| `EVAL_KEEP_CHATS` | `false` | `true` leaves the conversations of the attempts and of the probes in the Web UI |
| `EVAL_PROMPT_FILE` | built-in GAIA template | Must use `{{.Question}}` and nothing else. The command reads the file and gives the core its text |
| `EVAL_PROBE_PROMPT` | an essay request | What the probes needing a long reply ask. One line (it is typed) |
| `EVAL_UI_OVERHEAD_P95_MS` | `100` | U-05 |
| `EVAL_STREAM_CHECK_MIN_CHARS`, `EVAL_MIN_STREAM_UPDATES` | `200`, `3` | U-04 |
| `EVAL_SCROLL_FOLLOW_PX` | `80` | U-07 |
| `EVAL_STOP_WITHIN_MS` | `1000` | P-01 |
| `EVAL_LIGHTHOUSE_MIN_SCORE` | `90` | P-03 (1 to 100) |
| `EVAL_CLS_MAX` | `0.1` | P-09 |

Thresholds are recorded in `run.json`, so `report` judges a finished run the way the run did. They are never to be tuned to make a result pass.

### 5.5 Scoring and metrics

**Scorer.** A Go port of GAIA's `question_scorer`, quirks included:
- Truth is a number → strip `$`, `%`, `,` from the answer, parse as float, compare.
- Truth contains `,` or `;` → split both on `,`/`;`; lengths must match; per element, numbers compare as floats and strings compare lowercased without whitespace (punctuation kept).
- Otherwise → remove whitespace and punctuation, lowercase, compare.
- No answer → the string `"None"`.
- A number is read as Python's `float()` reads it: white space around it is skipped, the separators U+001C to U+001F are not, so an answer with one of them beside a number is not that number.

Parity is proven against 366 vectors computed by the official `scorer.py` (`internal/eval/testdata/gen_scorer_vectors.py`; the vectors are committed, the official file is not, and the script is not run in CI). The rule for the four separators is not among the vectors yet: two tests of the port carry it (`TestPyFloat`, `TestQuestion`), and its inputs are in the script's list, so they join the vectors when the file is made again.

**Statuses** (`results.jsonl`): `correct`, `incorrect`, `no_final_answer`, `agent_error` (the page shows an error bubble), `timeout`, `ui_error` (the page broke before an answer; retried on resume), `skipped` (attachment or filter).

**Agent scorecard** (counts are attempts: with `--repeats 3`, a problem adds three)

| Metric | Definition |
|--------|------------|
| Accuracy (attempted) | `correct / asked`, overall and per level |
| Accuracy (eligible) | `correct / every slot in scope`, skipped included |
| Coverage | `attempted / eligible`, next to accuracy so skipped problems cannot hide |
| Wilson 95% interval | On both accuracy figures |
| Format compliance | Share of answered replies with a `FINAL ANSWER:` marker |
| Latency | Enter → final status, p50/p95/mean/max, from the page clock |
| Cost proxies | Tokens and tool calls from message metadata, when reported |
| Repeatability | With `--repeats R > 1`: mean ± SD of accuracy, tasks that flip between right and wrong |

**UI scorecard.** Per-check tallies (pass/fail/na) and a **verdict** per check (fail if any attempt failed it, pass if some passed and none failed, na otherwise; U-05 as in 5.6); TTFT in the UI (Enter → first text); UI overhead (`TTFT(UI) − data-ttft-ms`, what the UI adds on top of the agent's own time) with its basis (`p95` or `p50`; when no attempt measured the overhead there is no basis: `ui_overhead_basis` is left out of `summary.json`, the verdict of U-05 is `na`, and `summary.md` says that U-05 is not judged); update cadence and largest gap (informational); probe outcomes; `must_failed` (any → exit 2), `undecided` (Must checks no attempt could decide) and `ui_errors` (attempts the page broke, by where). A report says "all required checks passed" only when the UI side is `Clean()`: nothing failed, nothing undecided, no page error.

### 5.6 UI checks

Passive checks run on every attempt; probes run once per `run` (unless `--no-probes`) or `probe`. "Must" failures end the run with exit 2.

| ID | Check | Passes when | Notes | Level |
|----|-------|-------------|-------|-------|
| U-01 | Console clean | No `error` messages and no uncaught exceptions | Text is quoted only with `--include-text` | Must |
| U-02 | Network clean | No failed request and none answered ≥ 400 | `na` when no request was recorded | Must |
| U-03 | Terminal state | The message reaches a final `data-status` | `na` when the reply was still going at the timeout and Stop then ended it; fails if it stays stuck after Stop | Must |
| U-04 | Incremental streaming | An answer of ≥ 200 characters shows ≥ 3 distinct updates | `na` for short answers or a late observer | Must |
| U-05 | UI overhead | `TTFT(UI) − data-ttft-ms` ≤ 100 ms | Per attempt a plain comparison. **A run is judged on the 95th percentile with ≥ 20 attempts, else on the median** (the p95 of a few samples is the maximum); the summary names the basis, or says that U-05 is not judged | Must |
| U-06 | Render fidelity | The extracted final answer is in the page's visible text | Compared without spaces, case or Markdown marks. `na` without a final answer | Must |
| U-07 | Scroll follow | While the reply grows, the view stays within 80 px of the bottom | `na` when no sample could scroll (nothing to follow) | Must |
| U-08 | Render safety | No `<script>`, `on*` attribute or external `<img>` in the answer body | `na` with no body to inspect | Must |
| P-01 | Stop | Stop at a streaming reply → `stopped` within 1 s, partial text kept | `na` only when the reply finished before Stop could be pressed, also when it ended as Stop was reached for; a reply still being written with no Stop button fails ("there was no Stop button while the reply was being written"), like a Stop that does nothing | Must |
| P-02 | Regenerate | Replaces only the last reply; one Regenerate button, on the newest | Fails when no new reply appears; `na` when it exists but the agent is slow | Must |
| P-03 | Lighthouse | Accessibility and best practices ≥ 90, desktop and mobile | A missing score is `na`, not a pass | Must |
| P-04 | Mobile | At 375 px: no horizontal scroll, sidebar reachable, composer on screen | | Should |
| P-05 | Rename / delete | Both work and persist after a reload | | Should |
| P-06 | Theme | The toggle works and persists across a reload | | Should |
| P-07 | Reload mid-stream | After a reload: one question, one reply, with its text, which ends | | Should |
| P-08 | Keyboard only | Send, stop and open a conversation without a mouse | `Sent` needs a complete reply (backlog D4) | Should |
| P-09 | Layout shift | CLS ≤ 0.1 on a long reply | Waits for the reply to end; with a slow model set `EVAL_PROBE_PROMPT` shorter | Should |
| P-10 | Agent down | A failed reply says so, offers Retry, and Retry gets the answer | Needs the scripted agent (`--scripted` or the self-test) | Should |

Probe order: P-01, P-02, P-07, P-05, P-10, P-06, P-08, P-09, P-04, P-03 (calm page first, reloading audits last). A dead browser marks the rest `na`. Only attempts feed U-01 to U-08; probes never read the console or the request list, so the deliberate 409/422 of a probe cannot fail U-01/U-02.

Every probe deletes the conversations it started however it ends, with an error or without, unless `EVAL_KEEP_CHATS=true`. A deletion that fails is tried once more on the page loaded again; a conversation that is still left is reported in one warning (`could not delete a probe's conversation`, with the error).

### 5.7 Results

`RESULTS_DIR/<run-id>/` (folders 0700, files 0600):

| File | Holds |
|------|-------|
| `results.jsonl` | One row per attempt, appended as it finishes (a half-written last line is dropped on resume). One `agent-eval` for a run id at a time: nothing stops a second one from resuming the same run, and the file would then hold the rows of both, each still one whole line |
| `run.json` | Command, times, git SHA, UI contract, tool versions (Go, Chrome, Node, MCP), non-secret settings (`human_typing=true` among them when the prompts were typed key by key), thresholds, `include_text`. Secret-looking keys are dropped |
| `probes.json` | What the probes found, with measures |
| `summary.json`, `summary.md` | The two scorecards, for machines and people |
| `evidence/` | Screenshots of failures, when allowed (5.9) |

Row fields: `run_id`, `task_id`, `level`, `attempt`, `status`, `score`, `latency_ms`, `ttft_ui_ms`, `ttft_agent_ms`, `ui_overhead_ms`, `updates`, `max_gap_ms`, `tokens{prompt,completion}`, `tool_calls[]`, `ui_checks{id: pass|fail|na}`, `console_errors`, `failed_requests`, `conversation_id`, `error_class`, `evidence[]`, `notes[]` (the evaluator's own words). `question`, `truth` and `answer` exist only with `--include-text`.

Failures are rows, never aborts. Whole repeats run first (repeat 1 of every problem, then repeat 2), so a stopped run holds complete repeats. Ctrl-C finishes in order and exits 130, after the summary of what finished is written, which gets ten seconds, as the probes' outcomes do; `--resume RUN_ID` skips finished `(task_id, attempt)` pairs and asks `ui_error` rows again; the first start time is kept. When Ctrl-C cuts the probes of a resumed run short, a probe that did not finish keeps the outcome already in `probes.json` and the ones that finished replace theirs; probes that run to their end replace them all. If the earlier outcomes cannot be read, nothing is written over them and the command exits 4.

### 5.8 Self-test

- `internal/eval/infrastructure/synthetic/data/synthetic_gaia.jsonl`: 13 invented problems (not GAIA content) across levels 1–3, each with a stable id in its text.
- `synthetic_replies.json` is the scripted agent's **fixture** (`TEST_AGENT_FIXTURE`, format v1): it maps those ids to canned replies. 8 correct (including `$1,000`, mixed-case strings, `;`-separated lists), 1 wrong, 1 without a marker, 1 agent error, 1 reply slower than `SelftestTimeout` (20 s), 1 problem with a file (skipped). Counters are per (problem id, exact question), so the P-10 probe can fail once and succeed on Retry.
- `selftest_expected.json` is the golden result: statuses (8/1/1/1/1/0/1), eligible 13, attempted 12, correct 8, per level (5·4·3, 5·5·3, 3·3·2 as eligible·attempted·correct), format compliance 9 of 10, 1 tool call. A unit test derives the same file with the real scorer. All Must checks must pass; the result must not vary between runs.
- It is the Phase 4 browser end-to-end test, and the canary for every release: `docker compose --profile selftest up -d --wait && scripts/eval.sh selftest`.

### 5.9 Data and secret handling

- GAIA is gated and its terms forbid resharing. Data and results never enter git: the evaluator **refuses to start** if `HF_DATASETS_DIR` or `RESULTS_DIR` sits inside a git work tree and is not ignored. Results hold no question, truth, answer or reply text unless `--include-text`. Screenshots show both, so they are opt-in too (always taken in the self-test).
- `HF_TOKEN` and the sign-in password never appear in logs, results or evidence (a canary-token test enforces it). The MCP child gets an allow-listed environment; `run.json` drops secret-looking keys.
- `EVAL_BASE_URL` can carry a user and a password. The loader never echoes it, and the five messages that name the Web UI's address (three of the readiness check of `agent-eval`, and the driver's "opening the Web UI" and "declares no UI contract version") show where it points and nothing else; the loader refuses an address that cannot be shown so (4.6). What the browser's own tool says when it fails is passed on as it is.
- Chrome DevTools MCP starts with the pinned version and all four privacy flags (it sends usage statistics by default; `--no-performance-crux` keeps trace URLs from the CrUX API). Its working directory is scratch and its Chrome profile is temporary. The MCP child runs in its own process group and is killed with its Chrome.
- `run.sh` hands every setting in `.env` to the web server, `HF_TOKEN` included. The server never reads it; comment it out once the dataset is downloaded.
- The reference repo `ai-agent-from-scratch-go` tracks a `.env_ch1` file with API keys and the GAIA parquet files. Nothing was copied from it; our `.gitignore` uses `/.env*` with `!/.env.example`, and `*.env` for a copy under another name; `.dockerignore` leaves `.env*` and `*.env` in any directory out of the build context, `.env.example` excepted. Rotating those keys and untracking that data is the owner's call.

---

## 6. Commands

```bash
# Build
go build -o bin/agent-web-ui ./cmd/web
go build -o bin/agent-eval ./cmd/eval

# The quality gate: gofmt, vet, golangci-lint, staticcheck, modernize, govulncheck,
# unit tests with -race, coverage floors (use cases 85%, all 70%, each package 50%),
# then the tests under it/ that need no Docker: ./it/scripts/ and TestRoutes_ of ./it/security/
scripts/build-tools.sh                 # once: tools built with this project's Go into bin/tools
scripts/check.sh [--it] [--quick]      # --it runs all of ./it/... in their place; --quick leaves out only -race and govulncheck

# Tests by hand (clean the cache first; run whole package suites)
go clean -testcache && go test -race ./internal/... ./cmd/...
go test ./it/...                       # Docker; the browser suites also need Node and Chrome
go test ./it/live/                    # with AGENT_LIVE=1 in .env: the real agent it names; never in the default run

# Mocks: mockery directly, only for modified interfaces
go generate ./internal/core/port/service/<file>.go
go generate ./internal/core/port/resource/<file>.go
go generate ./internal/eval/core/port/resource/<file>.go

# Web UI
scripts/init-env.sh                    # private .env with generated passwords
scripts/run.sh [--rebuild] [--env f]   # background, logs/agent-web-ui-<time>.log
scripts/stop.sh

# Infrastructure and agents
docker compose up -d --wait                       # Postgres + scripted agent
docker compose --profile full up -d --wait        # + the web server
docker compose --profile selftest up -d --wait    # web server wired to the self-test agent
docker compose down                               # add -v to delete the database volume
go run ./cmd/test-agent web --port=9093 api       # the scripted agent without Docker
scripts/real-agent.sh [start|stop|status]         # sample real agent on :9094; model, key and ADK are settings in .env

# Evaluator
scripts/eval.sh selftest
scripts/eval.sh datasets fetch|list
scripts/eval.sh run --level 1 --limit 5
scripts/eval.sh report <run-id>
scripts/eval.sh probe [--scripted]
```

A Makefile wraps the commands above as `make` targets (`make help` lists them); `scripts/build-tools.sh` also takes `--global` to install the tools onto this machine instead of into `bin/tools`. Scripts use `set -euo pipefail`, load `.env` safely, keep timestamped logs and wait for health. `go` and the tools must be Go 1.27; the globally installed linters are often built with an older Go, which is why `bin/tools` exists.

After the coverage, `scripts/check.sh` runs the tests under `it/` that need no Docker: `./it/scripts/` and the `TestRoutes_` tests of `./it/security/`. Of these it prints a line for each package and names the tests that were skipped; when they fail, it prints all of their output. `--it` runs all of `./it/...` in their place; `--quick` leaves out only the race detector and the vulnerability scan. A coverage floor is compared with the share as counted; the printed share is cut to one decimal, not rounded (84.96% prints 84.9% and is below 85), with a decimal point in every locale. When the integration tests fail, the gate prints the lines that name them and keeps all of their output in a temporary file, which its last line names; when they pass the file is removed.

---

## 7. Project structure

```
cmd/web/                      Web UI binary ([--env FILE]; also: healthcheck [url])
cmd/test-agent/               scripted ADK agent (also: healthcheck)
cmd/eval/                     agent-eval CLI: flags, exit codes, wiring
internal/app/                 composition root of the Web UI (Build, Serve)
internal/core/                Web UI hexagon: domain, port/{service,resource}(+mocks), auth, chat, conversation
internal/infrastructure/      api/web (+render), agent/adkrest, postgres (+migrations), auth, config, logging
internal/contract/            the UI test contract (hooks, states, names, version)
internal/platform/envcfg/     .env reading and Secret, shared by both programs
internal/platform/redact/     a struct as a slog group with its secrets hidden (Struct), a URL without its credentials (URL); standard library only
internal/platform/tmplcache/  embedded .gotmpl files read once at startup and filled in by path; standard library only
internal/testagent/           scripted model, fixture mode, in-process ADK server
internal/eval/core/           domain, port/{service,resource}(+mocks), score, report, uicheck, probe, run
internal/eval/infrastructure/ config, gaia (parquet, fetch), safety (git guard), store, devtools (MCP driver, probes, page scripts), synthetic
internal/architecture/        the dependency rules and two rules about secrets, as tests
assets/                       embedded templates/ and static/ (vendored htmx, Bootstrap)
it/                           integration suites: app, auth, chat, container, conversation, evaldriver, evalselftest, live, message, postgres, scripts, security
it/internal/                  stack (the Web UI as the program runs it), evalenv, leak (goroutine check), dbtest, pgenv
scripts/                      init-env, run, stop, eval, check, build-tools, real-agent, lib/process.sh
docs/spec.md                  this file
docker-compose.yml, Dockerfile, Dockerfile.testagent, .env.example, README.md, CHANGELOG.md
datasets/ results/ logs/ bin/ gitignored (so are .env* and temp/)
```

---

## 8. Code style

Port + `Dependencies` struct + interface assertion, with Go 1.27 idioms.

```go
// internal/core/port/resource/agent_gateway.go
//go:generate mockery --name AgentGateway --filename agent_gateway.go
type AgentGateway interface {
	// Run streams the reply and stops when ctx is canceled.
	Run(ctx context.Context, req domain.AgentRequest) iter.Seq2[domain.AgentEvent, error]
}

// handler error mapping (internal/infrastructure/api/web)
if ve, ok := errors.AsType[*domain.ValidationError](err); ok {
	a.composerProblem(w, r, id, http.StatusUnprocessableEntity, content, ve.Message)
	return
}
if err != nil {
	a.fail(w, r, err) // 404 for domain.ErrNotFound, otherwise a logged 500
	return
}
```

Conventions: small interfaces at the consumer; `context.Context` first; wrap errors with `%w`; `x_test` packages for use-case tests; table-driven tests; `encoding/json/v2` with explicit tags; stdlib `uuid`; no loop-variable copies; `testing/synctest` for time (use the bubble's own `t.Context()`); mocks generated by `mockery` directly and committed under `mocks/`; gofmt, `go vet`, `modernize` and the three linters clean.

---

## 9. Testing strategy

| Level | Where | What |
|-------|-------|------|
| Unit | `./internal/...`, `./cmd/...` | Use cases with mocks; handlers with `httptest`; Markdown/XSS golden table; ADK frame parser on recorded fixtures; config loaders; the UI contract; the pacer and pump at exact virtual instants; evaluator: scorer vs. official vectors, statistics, check and probe verdicts, runner with a mocked `UIDriver`, GAIA loader on generated parquet, the MCP driver against an in-process fake MCP server, probes against a scripted "scene" page, the command line builder (privacy flags, pinned package), recorded tool outputs, each read by a test (`TestEveryRecordingIsRead`), the git guard, the dependency rules (also on imports made for them) and the two rules about secrets |
| Integration | `./it/...` | Needs Docker, except **scripts** and the `TestRoutes_` tests of **security**, which read the repository or run its scripts and are part of the default gate. Postgres via Testcontainers; **auth**, **conversation**, **message** and **postgres** (the stores and migrations against a real database). The suites that start the stack run the real application through `app.Serve`, the same lines the program runs, against the scripted agent: **chat** (pieces, Stop, close, Regenerate, agent down + Retry, replay, reasoning hidden, flush interval, cut-off replies, shutdown, overhead), **security** (headers per route against every route pattern the web package names, in any file; anonymous access, other origins, other users, hostile content, stored secrets, cookies, limits, https, log content), the readiness test of **app**, the opt-in **live**, and **evaldriver** and **evalselftest** (the evaluator against the real Web UI in headless Chrome: driver, observer, scrolled-up reader, sending brings the view back, browser-like requests only, the self-test three times, probes, Ctrl-C + resume, killed browsers), which need Node and Chrome. The other tests of **app** build the application with `app.Build` and serve its handler with a test server (migrations, first user, restart recovery). **container** runs the two images (built, non-root, health checks, a chat across three containers, bad settings, `docker stop`). **scripts**: pid helpers, `.env` loading and mode, how a settings file is read, the real-agent network gate, `run.sh --env`, durations and the wait of `stop.sh`, the gate against stand-in tools, the Compose settings, `.gitignore` and `.dockerignore`, the CI workflow |
| Live (opt-in) | `./it/live` | A real agent answers and can be stopped. Skipped unless `.env` says `AGENT_LIVE=1` |
| Browser E2E | `scripts/eval.sh selftest` | The Phase 4 end-to-end test and the release canary |
| Goroutines | `it/internal/leak` | Chat and security cases fail if application goroutines remain |
| Mutation checks | By hand, in Phases 3 to 6 | A record of what was done, and no step of the gate: several hundred deliberate breaks (341 of them in Phase 5), one at a time, each expected to fail a test; survivors were fixed with a new test or justified as equivalent behavior. Nothing runs them again |

The numbers of a day are not kept here; the gate prints them. `scripts/check.sh` prints the coverage of the use cases and of everything under `internal/` and `cmd/` against their floors (85 and 70), and fails with the name of every package below 50%, except those named in `COVER_EXEMPT` (`internal/app`, `postgres`, `cmd/web`, `cmd/test-agent`, `chromacss`, which the integration suites cover). `scripts/check.sh` lists the packages of the tests it runs under `it/` and names the tests that were skipped, with `--it` and without it: with `--it` the two opt-in ones (`TestLiveSuite`, `TestMeasureUIOverhead`), and either way any that lacks what it needs on the machine, such as curl, lsof or a locale with a decimal comma. `go test -count=1 -v ./internal/... ./cmd/... | grep -c '^=== RUN'` counts the unit cases, subtests included.

---

## 10. Boundaries

**Always**
- Table-driven tests; clean the test cache; run whole package suites.
- Pass all model output through the Markdown sanitizer; treat it as untrusted.
- Keep every setting in `.env` and write it down in `.env.example`; a test fails on one that is not.
- Propagate `context.Context`; cancel upstream work when the client leaves.
- Keep each core free of infrastructure imports (the architecture test enforces it).
- Regenerate mocks with `mockery` after any interface change.
- Score with GAIA's rules; record tool versions in `run.json`.

**Ask first**
- Any dependency outside section 3; any Node build step in the Web UI.
- Schema changes; changing the UI contract version.
- Editing `ai-agent-from-scratch-go`.
- Adding attachments to the UI; adding Redis, Kubernetes or a CDN.
- Starting a real GAIA run that uses paid model keys.

**Never**
- Commit `.env*` (except `.env.example`), secrets, GAIA data or results.
- Print or log `HF_TOKEN`, passwords or session cookies.
- Let the evaluator call the agent or the Web UI API as a shortcut.
- Tune the scorer, prompts or thresholds to make a result pass.
- Render model or user text as raw HTML; log message content at info.
- Adopt a pre-release (alpha, beta, rc) version of a dependency.
- Disable the cross-origin check or the CSP to make a test pass; skip or delete a failing test without approval.
- Share a conversation across users (cross-user access is 404).

---

## 11. Success criteria and evidence

Status at Ship: **Met** unless noted. "Evidence" is where it is proven.

**Web UI: functional**

| ID | Criterion | Evidence |
|----|-----------|----------|
| SC-1 | A message produces ≥ 2 incremental `delta` events before `done`; persisted text equals the final text; reload shows the same conversation | `it/chat` |
| SC-2 | Stop cancels the upstream run within 1 s (seen by the test agent); the reply keeps its partial text as `stopped` | `it/chat`; P-01 (14 ms in the last self-test) |
| SC-3 | Regenerate replaces only the last assistant message | `it/chat`; P-02 |
| SC-4 | With the agent down, an error bubble with Retry appears within 5 s; after the agent returns, Retry succeeds with the prompt intact | `it/chat`; P-10 |
| SC-5 | Reconnecting to a finished message returns its final HTML and `done` with zero agent calls | `it/chat` |
| SC-6 | Rename, delete, list work; default title = first 60 characters; delete removes messages | store ITs, unit tests, P-05 |
| SC-7 | `thought:true` parts never reach the page; partial plus aggregated events never duplicate text | `it/chat`, parser fixtures |

**Web UI: security**

| ID | Criterion | Evidence |
|----|-----------|----------|
| SC-8 | Hostile Markdown renders inert; the CSP is `script-src 'self'` with no inline scripts | `it/security` |
| SC-9 | A cross-origin state-changing request returns 403 | `it/security` |
| SC-10 | User A requesting user B's conversation, stream, raw text or delete gets 404 — **except the stream, which answers 200 with an in-stream "This reply isn't available." that is identical to a missing id** (see 4.2) | `it/security` |
| SC-11 | argon2id; cookies `HttpOnly`, `SameSite=Lax`, `Secure` over https; oversize input rejected; sends and sign-ins rate limited | `it/security` |

**Web UI: UX, performance, operations**

| ID | Criterion | Evidence |
|----|-----------|----------|
| SC-12 | Scrolled > 80 px up, new tokens don't move the view and "Jump to latest" shows | `TestPage_ReadingPositionIsKept`, U-07 |
| SC-13 | UI overhead ≤ 100 ms p95 locally; first load ≤ 150 KB gzip; zero external hosts | Over HTTP: median 0.2 ms, p95 0.4 ms (`TestUIOverhead`). In Chrome: median 31 ms, p95 74 ms over 10 attempts in the release-candidate self-test (19.2 ms and 26.9 ms in Phase 5; with under 20 attempts the verdict uses the median). First load 84,228 bytes as sent, 10 requests (`TestFirstLoadWeight`); the CSP blocks other hosts |
| SC-14 | Fully keyboard operable; completion announced once; Lighthouse accessibility ≥ 90; works at 375 px | P-08, P-03 (100 desktop and mobile in the last self-test), P-04; `TestPage_CompletionIsAnnouncedOnce`; dark-theme contrast measured. Not tried with a screen reader |
| SC-15 | Non-zero exit with a clear message on missing or invalid required config | `it/container`, config unit tests |
| SC-16 | `scripts/run.sh` starts and waits for `/readyz` (≤ 20 s), logging to `logs/`; `scripts/stop.sh` stops gracefully and is safe to run twice | **Partly automated**: `it/scripts` covers the pid helpers, `.env` loading and mode, the real-agent script, that `run.sh` passes `--env` on, and how long `stop.sh` waits. The start/stop flow was run by hand against the final build at Ship: ready in 1 s; a second `run.sh` refused (exit 1); `stop.sh` exit 0 in under a second; a second `stop.sh` exit 0 |
| SC-17 | `docker compose up -d --wait` gives healthy Postgres and test agent; a documented path reaches a working chat in under 5 minutes | `it/container`; Compose run at Ship |
| SC-18 | Unit and integration suites pass; coverage targets met; vet, golangci-lint, staticcheck, modernize, govulncheck and `-race` clean | `scripts/check.sh` |
| SC-19 | SIGTERM cancels in-flight streams, saves partial text, exits within `APP_SHUTDOWN_GRACE` | `it/chat` shutdown; `it/container` stop (exit 0) |

**Web UI: evaluator support**

| ID | Criterion | Evidence |
|----|-----------|----------|
| SC-20 | Every hook in 4.7 exists with its accessible name; a contract test fails otherwise | contract unit test |
| SC-21 | `GET /c/{id}/m/{mid}/raw` returns the exact stored Markdown, `no-store`, owner only | `it/security` |
| SC-22 | Final assistant messages persist `meta` and expose it as `data-*` attributes | `it/chat` |

**Evaluator**

| ID | Criterion | Evidence |
|----|-----------|----------|
| EC-1 | `agent-eval selftest` passes end to end in headless Chrome against Postgres + web + scripted agent: golden summary, all Must checks, identical across 3 runs, under 5 minutes | `TestSelftestSuite`; at Ship: exit 0 in 97 s against the Compose `selftest` profile |
| EC-2 | The scorer agrees with the official `scorer.py` on ≥ 100 vectors | 366 vectors |
| EC-3 | `datasets fetch` downloads with `HF_TOKEN`, skips present files, errors clearly without a token; row count equals the parquet metadata; a canary proves the token is never logged or written | fetch unit tests; one real fetch (53 problems, 11 with a file) |
| EC-4 | Black-box: questions are typed into the composer; the network log holds only requests the UI's pages made, plus the raw fetch | `TestDriver_OnlyDoesWhatABrowserDoes` |
| EC-5 | Each attempt appends a complete JSONL row; `summary.md` shows per-level accuracy, coverage, intervals, latency, status counts and both scorecards; no text unless `--include-text` | store and report tests; two real runs of 10 level 1 problems |
| EC-6 | Ctrl-C leaves valid JSONL; `--resume` skips finished pairs; a killed MCP or Chrome is restarted and the attempt retried once; failures never abort the batch | `TestSelftest_InterruptedThenResumed`, `..._BrowsersThatDieAreReplaced` |
| EC-7 | `--repeats 3` reports mean ± SD, Wilson interval and flip rate | report unit tests (no browser test) |
| EC-8 | A task timeout presses Stop, records `timeout`, continues in a fresh chat | self-test problem `syn-l3-01` |
| EC-9 | MCP starts with the pinned version and all four privacy flags; the run refuses to start when dataset or results directories are tracked | config and git-guard unit tests |
| EC-10 | Every Must check has a unit test over fixture observations; thresholds are configurable | uicheck and probe tests |
| EC-11 | The evaluator refuses an unknown `data-ui-contract` version | driver unit tests |
| EC-12 | `scripts/eval.sh` loads `.env` and logs to `logs/agent-eval-<ts>.log`; the README documents self-test and real runs | `cmd/eval` tests, README |

---

## 12. Risks

| Risk | Status |
|------|--------|
| `htmx-ext-sse` 2.x does not swap or close as assumed | Resolved. Works with "always 200, report inside the stream"; no custom `EventSource` script was needed |
| Chrome DevTools MCP output is LLM-oriented text and may change | Contained in one adapter; version pinned; element lookup by accessible name; state read by `evaluate_script`; recorded snapshots in unit tests |
| ADK partial and aggregated events duplicate text | Parser rules (4.3) and fixtures |
| Agent restart wipes its in-memory session | Accepted. History survives here; backlog A12 |
| Stream/cancel races, goroutine leaks | `-race`, `synctest`, goroutine-leak checks in the integration suites, recovery in every goroutine |
| Re-rendering full HTML per delta is O(n²) | The pacer spaces updates at ≥ 4× the cost of the last one (render, write and flush); terminal messages are cached in a bounded LRU; reply cap 1 MiB. Tail-only rendering is backlog F4 |
| Long GAIA tasks hit timeouts | `AGENT_REQUEST_TIMEOUT=600s`, `EVAL_TASK_TIMEOUT` ≤ it, keep-alive comments. New finding: the ADK launcher cuts a reply at two minutes; `real-agent.sh` raises it |
| GAIA samples are small; runs are noisy | Wilson intervals, repeats, no accuracy gate (OQ-10) |
| Timing jitter from MCP round trips | One in-page clock; call durations are never used |
| Tool-less agents score low on GAIA | The self-test proves the harness; two real 10-problem runs (local model 30%, Gemini 50%) proved the path, not the agent |
| json/v2 is strict and case-sensitive | Explicit tags; readers reject unknown members |
| Telemetry or data leaks through MCP or logs | Privacy flags, canary tests, allow-listed child environment, git guard, token stripped on redirects |
| **New:** single instance (memory-only Stop registry and rate limits) | Documented; the Stop registry is correct because the process that claimed a reply is the one writing it |
| **New:** goldmark v1 with a code-highlighting extension that has no tagged release | Stays on the stable releases (goldmark 1.8.6, chroma 2.27.0). goldmark v2 is stable now (2.1.6), but its extension, `goldmark-highlighting/v3`, needs `chroma/v3` alpha, so adopting v2 would mean writing the small chroma-v2 glue in this repository (backlog) |
| **New:** a rollback across a future migration | Measured at Ship: an older binary on a newer schema with `DB_MIGRATE_ON_START=true` exits 1 with `no migration found for version N`; with `false` it starts and serves. Rule: migrations are additive (add first, remove in a later release), and a backup is taken before any release that adds one |

---

## 13. Decisions (from the open questions)

| # | Question | Decision |
|---|----------|----------|
| OQ-1 | Sign-in | Email + password |
| OQ-2 | Database | PostgreSQL |
| OQ-3 | Real agent for the Web UI | A sample `ui_agent` from `adk-training-go` (`scripts/real-agent.sh`, port 9094), on Gemini |
| OQ-4 | Test agent | A new scripted ADK agent (`cmd/test-agent`) |
| OQ-5 | Chrome DevTools MCP in the session | Resolved by design: the evaluator is its own MCP client |
| OQ-6 | Packaging | Dockerfile + Compose only |
| OQ-7 | Which agent for real GAIA runs | (c) an `adk-training-go` agent for now; `ai-agent-from-scratch-go` has no network interface and was not edited |
| OQ-8 | Attachments | Skipped; coverage is reported |
| OQ-9 | Parquet library | `parquet-go` |
| OQ-10 | Gate real runs on accuracy | No. Must UI checks gate; `--min-accuracy` is optional |
| OQ-11 | Same `.env` key names as `ai-agent-from-scratch-go` | Yes; values never read or copied |

---

## 14. Known limits and backlog

Not defects; each has a reason in the Phase 5 and Phase 6 documents.

| ID | What | Why it waits |
|----|------|--------------|
| — | goldmark v2 | Stable (2.1.6), but the only highlighting extension for it needs chroma v3 alpha. It would take in-repo glue around chroma v2 and a pass over the sanitizing pipeline; v1 passes every hostile-input test |
| F4 | Tail-only Markdown rendering; gzip of the event stream | Shows at hundreds of concurrent streams; single instance |
| F6, F7, F9, F11 | Conversation pagination, hashed asset URLs, fewer DB round trips, per-update JS work | Sub-10 ms at tested sizes; each is feature-sized |
| A3 | Delete the agent's session when a chat is deleted | New port method; privacy note in the README |
| A12 | Rebuild the agent session from Postgres after an agent restart | Needs a stateful-agent experiment |
| A10 | A second Ctrl-C during the grace is swallowed; `StartConversation` as one transaction | Nits |
| B12 | `/` shortcut (WCAG 2.1.4), focus after hide, `h1`, Safari clipboard | Accessibility polish |
| C1 | A separate, lower limit for sign-ins | /64 keying and the hash slots bound the cost |
| C2 | Committed npm lockfile + `npx --no-install` | Needs a Node-side step and a network |
| C3, C4 | Warn on non-loopback http; per-user cap on concurrent runs | Default is loopback; single-user deployment |
| D3 | U-06 with leading list/quote markers; U-04 on non-streaming agents; run verdict on a rate | Changes what "pass" means; thresholds are not tuned to pass |
| D4 | P-08 `Sent` needs a complete reply; a fast agent fails the keyboard Stop | P-08 is Should |
| D5 | Wilson interval counts repeats as trials | An estimand choice |
| E9, E13 | Tests for the remaining `run.sh`/`stop.sh` branches; test nits | Helpers, `.env` loading and `.gitignore` are covered |
| — | Stop answers 404 for a message that is not a reply (a final reply comes back as stored), Regenerate answers 409 | Unifying changes what a client sees |

---

## 15. References

- Alex Edwards, [How I use htmx with Go](https://www.alexedwards.net/blog/how-i-use-htmx-with-go).
- [chrome-devtools-mcp](https://github.com/ChromeDevTools/chrome-devtools-mcp): tool reference (click/fill by snapshot uid, `evaluate_script`, `wait_for`, console and network lists, Lighthouse, performance traces, `emulate`), flags `--headless --isolated --no-usage-statistics --no-performance-crux`, Node ^20.19 || ^22.12 || ≥ 23.
- [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk): `mcp.NewClient`, `CommandTransport`, `CallTool`.
- GAIA: [dataset card](https://huggingface.co/datasets/gaia-benchmark/GAIA) (gated; no resharing outside gated or private Hugging Face repos), [paper](https://arxiv.org/pdf/2311.12983), [official scorer](https://huggingface.co/spaces/gaia-benchmark/leaderboard/raw/main/scorer.py).
- ADK Go (`google.golang.org/adk/v2`) REST API; `adk-training-go` samples (`cmd/ui-agent`, `docs/module-05`).
- Chat UX: [AI Chat UI Best Practices 2026](https://thefrontkit.com/blogs/ai-chat-ui-best-practices).
- Phase documents (not tracked): `temp/skills-output/phase-1-spec.md` … `phase-7-ship.md`.
