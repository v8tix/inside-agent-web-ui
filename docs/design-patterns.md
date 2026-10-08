# Design patterns in this code

We read *Design Patterns* (Gamma, Helm, Johnson, Vlissides) against this code: which of
its 23 patterns the code already uses, where one would remove real complexity, and
where one would only add indirection. The book asks for that care itself (§1.8):

> Design patterns should not be applied indiscriminately. Often they achieve
> flexibility and variability by introducing additional levels of indirection, and
> that can complicate a design and/or cost you some performance. A design pattern
> should only be applied when the flexibility it affords is actually needed.

## The rule we used

- A pattern goes in only when the code gets shorter or flatter (its biggest function and
  its deepest nesting get smaller; totals are shown next to the peaks, below) and the
  existing tests stay as they are (new tests are welcome).
- Table 1.2 of the book asks "what varies?". The answer has to exist in this code today.
- Go already does part of the work. Closures stand in for Command and Strategy objects,
  `iter.Seq2` for Iterator classes, implicit interfaces make Adapter cheap, and with no
  inheritance "favor object composition over class inheritance" is the only option. The
  book says as much about language (§1.1): it shapes which patterns you need.

## The 23 patterns here

**Used** = already in the code ("used, lightly": in a small form, with a bigger one
looked at and left alone). **Applied now** = changed in this branch. **Left alone** =
looked at, and the pattern would not pay. **Avoided** = the pattern's problem exists and
is solved another way on purpose. **Not needed** = no problem for it to solve.

| Pattern | Verdict | Where, or why not |
|---|---|---|
| Abstract Factory | Not needed | One product per factory; there are no product families. |
| Builder | **Applied now**, small form | `frameBuilder` in `adkrest/parser.go` builds an SSE frame line by line. It has one product and no director, so strictly it is an extracted class; the name is the vocabulary. Option funcs (`testagent.NewModel`, `auth.NewArgon2`) cover the rest. |
| Factory Method | Used | `DriverFactory.NewDriver` (one browser per worker); `cli.buildRunner` and `cli.buildProber`, which tests replace. |
| Prototype | Not needed | Nothing is created by cloning a registered instance; `slices.Clone` and `maps.Clone` copy values. |
| Singleton | Avoided | The composition roots (`internal/app`, `cmd/eval`) build each service once and pass it down. No package-level service instances; `sync.Once` only makes a `Close` idempotent. |
| Adapter | Used | Every adapter package: `adkrest.Client`, the `postgres` stores, `devtools.Driver`, `gaia.Parquet`, `auth.Argon2`, and `testagent.Model` (adapts a script to ADK's `model.LLM`). |
| Bridge | Used | The ports are designed first, with implementors behind them. The book: "The Adapter pattern makes things work after they're designed; Bridge makes them work before they are." |
| Composite | Not needed | The only tree is the Markdown AST, which goldmark owns. |
| Decorator | Used | `web.Middleware` and `web.Chain` (`middleware.go`); `statusWriter` wraps `http.ResponseWriter`. |
| Facade | Used | `service.Chat`, `Conversation` and `Auth` hide stores, agent and run registry from the handlers; `service.Runner` does the same for the evaluator. |
| Flyweight | Not needed | The book wants all five conditions; the first already fails, there is no huge population of fine-grained objects. |
| Proxy | Left alone | See "Considered" below. |
| Chain of Responsibility | Used | The same chain, plus `authenticate` and the `limited` rate limits on the routes: each can stop a request or pass it on. |
| Command | Used, lightly | `cli` handler fields (tests swap them); `probe.Suite` runs probe funcs registered by id; `run` queues `slot`s for its workers. A dispatch table was left alone. |
| Interpreter | Not needed | The scripted model's `#error`, `#slow` and friends are six substrings, not a grammar. |
| Iterator | Used, **applied now** | `ProblemSource.Problems`, `AgentGateway.Run`, `Chat.Stream`, `parseStream`, `Select` are all `iter.Seq2`. The GAIA parquet reader (`fileRecords`, `allRecords`, `groupRows`) now is too. `feed` in `stream_handler.go` turns an iterator into a channel so `pump` can `select` between it and timers (the book's internal versus external iterators, in Iterator's Implementation notes). |
| Mediator | Not needed | No set of peers talks to each other. `chat.Service` calls ports; that is a Facade. |
| Memento | Not needed | No undo. `--resume` reads the rows already in `results.jsonl`. |
| Observer | Not needed | A reply has one consumer, the browser over SSE. Nothing registers or notifies dependents. |
| State | Left alone | See "Considered" below. |
| Strategy | Used | `ProblemSource` has two interchangeable sources (`Parquet`, `Synthetic`); the scripted model picks a script per keyword, or from a fixture. |
| Template Method | **Applied now**, as a function | `judgeSteps` in `probe/judge.go`: the skeleton (first step that failed wins) takes the varying part as data, not as a subclass hook. It varies only data, so strictly it is an extracted function; it is the book's idea in the form Go has. |
| Visitor | Not needed | goldmark's `ast.Walk` is the visitor (`render/transform.go`); our own closed sets use `switch`. |

## What changed in this branch

Cognitive complexity comes from gocognit through golangci-lint (the threshold that flags a
function is 18; `dupl` 60, `nestif` 5 and `funlen` 70 lines or 45 statements find the rest).

| File | Nearest pattern | Before | Now |
|---|---|---|---|
| `internal/eval/infrastructure/gaia/parquet.go` | Iterator | The reader called a function for each row and stopped it with a sentinel error and a flag, returned a row count, and closed by hand. Biggest function 36 (`Problems`; `Inventory` 28, `readFile` 22); file total 106; 315 lines. | `fileRecords` yields rows and the callers are plain `for` loops: `Problems` 14, `Inventory` 9, biggest function 23; file total 113; 359 lines. |
| `internal/infrastructure/agent/adkrest/parser.go` | Builder, small | `readFrames` mixed reading lines with the SSE field rules: 31; file total 80; 231 lines. | `frameBuilder` holds the field rules, `readFrames` only reads: 16; file total 69; 246 lines. |
| `internal/eval/core/probe/judge.go` | Template Method, as a function | P-05 and P-10 each carried the same "first step that failed" loop; file total 23. | `judgeSteps` holds it once; file total 20, same length. |
| `internal/eval/core/report/aggregate.go` | Refactor to generalize | `undecided` and `mustFailed` differed by one word; file total 89; 363 lines. | `mustWithVerdict(scorecard, result)`; file total 81; 347 lines. |

Read the totals next to the peaks. The parquet reader is 44 lines longer and 7 points
higher in total: it has more, smaller functions (`fileRecords` 23, `allRecords` 18,
`groupRows` 16, `(*DatasetInfo).add` 10). It is "flatter", not "shorter", and we kept it for
what it takes away: the callback, the sentinel error, the flag, the returned row count
and the hand-written `Close` calls (a `defer` closes the file on every exit), and the
callers drop from 36 to 14 and from 28 to 9. `allRecords` is the one place that walks
configs and files; folded into `Problems` it makes that function 32. The other three
changes are plain gains.

How we know behavior held: every test that existed still passes with its checks
unchanged (one got an extra assertion, two tables extra rows), and the 41 test cases
added (counting each test and subtest) pass on the old code as well as the new. The
quality gate passes (`scripts/check.sh`). An independent review (an AI reviewer, whose
findings were checked by hand) found gaps that the tests below close.
The cases that were missing got tests: a row group bigger than one read batch; a file that
cannot be opened or whose data is damaged; no file left open on any exit; a cancelled
`Inventory`; `readFrames` on its own, with a frame that ends the stream, a bare field,
spaces after the colon, a cap that starts again with each frame and a cap on lines of any
kind; which of two steps a probe reports when both went wrong, for every pair of
neighbours; and the measures a probe carries.

Every change was also mutation-checked: 55 one-line mutants of the new code, which the tests
have to fail. 52 do, among them the parquet row-count check (`rows != file.NumRows()`): a
test rewrites the footer of a file so that it promises one row more than the file holds.
Three survive, each for a reason:

- `allRecords` carrying on to the next file after an error would differ only for a caller
  that ignores errors, and none does;
- the row reader's `Close` is not observable from outside (it holds no descriptor);
- the `:` check in `frameBuilder.add` is redundant with the field switch, since a
  comment's field name is empty and matches nothing; it stays because it mirrors the SSE
  rules.

One line outside these four files is known to survive too. In `internal/core/chat/stop.go`,
`stopUnstarted` returns `ErrAlreadyStreaming` when another writer holds the reply, and no
test fails without that return: no call lies between Stop's own look at the run registry
and its registration there, so a test has no moment to put a writer in between.

## Considered and left alone

- **Command table for `cmd/eval/cli.go` `dispatch`.** Five places repeat "read the
  flags, read the settings, call the handler". A generic helper would save about twelve
  lines and still need `run`'s extra step for its warnings, and it adds an interface
  and a method. The handler fields already are the commands.
- **State for `chat.Stream`.** One operation branches on three situations of a reply's
  status. The book wants "large, multipart conditional statements" in several
  operations (State, Applicability), and says State "increases the number of classes".
- **Proxy or Decorator to restart the browser in `run/batch.go` `worker`.** What is
  retried is a whole attempt, not a call, so a transparent proxy cannot do it. A
  decorator needs a one-method interface, a wrapper type and a restart callback to
  save about ten lines.
- **Template Method for the "sign in, run, tidy" start of each probe in
  `devtools/probes.go`.** Wrapping each 40 to 60 line probe in a closure adds a level
  of nesting to all of them to save three lines each.
- **`judgeSteps` for `JudgeTheme` (P-06).** It has the same two-step shape and stays a
  `switch`: two conditions, each with its own message, read as well as a list would. The
  helper is for lists of steps in a fixed order.
- **A shared helper for `positive` and `nonNegative` in `eval_config.go`.** Two
  eight-line functions that differ in the comparison and the message; a helper needs a
  predicate and a message, which is no simpler.
- **`score.pyFloat`.** A faithful port of Python's `float()` parsing; scoring code is
  not touched for style.

## The book's eight causes of redesign, and this code

| Cause (§1.6) | How this code stands |
|---|---|
| Creating an object by specifying a class | Use cases take ports; only the composition roots name concrete types. |
| Dependence on specific operations | `cli` handler fields and the probe registry. |
| Dependence on hardware or software platform | Chrome DevTools MCP is behind `UIDriver`, PostgreSQL behind the stores. |
| Dependence on object representation | Adapters map wire and row types to `domain` types (`wireEvent`, store rows). |
| Algorithmic dependencies | The scorer, the Markdown pipeline and the stream `pacer` each live in one place. |
| Tight coupling | `internal/architecture` fails the build on a forbidden import. |
| Extending functionality by subclassing | Go has none: middleware and embedding instead. |
| Inability to alter classes conveniently | ADK, parquet-go, goldmark and the MCP client sit behind adapters. |

## Before adding a pattern

1. Name what varies, today. If the answer is "maybe later", wait for the second user.
2. Read the pattern's Consequences, not only its Intent.
3. In Go, try a function, an `iter.Seq2` or a small struct first.
4. Keep the tests that describe the behavior unchanged; add the ones that were missing
   before you refactor.
