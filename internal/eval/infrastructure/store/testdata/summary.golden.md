# agent-eval run 20261002T153012Z-ab12

- **Command:** run
- **Started:** 2026-10-02 15:30:12 UTC · **finished:** 2026-10-02 15:41:03 UTC (10m51s)
- **Build:** Web UI contract 1 · git abc1234
- **Versions:** chrome 143.0.7499.0, chrome-devtools-mcp 1.10.1, go 1.27

## Agent

| Level | Eligible | Attempted | Correct | Accuracy (attempted) | 95% CI | Accuracy (eligible) | 95% CI |
|:--|--:|--:|--:|--:|:--|--:|:--|
| 1 | 4 | 3 | 2 | 66.7% | 20.8–93.9% | 50.0% | 15.0–85.0% |
| 2 | 3 | 3 | 1 | 33.3% | 6.1–79.2% | 33.3% | 6.1–79.2% |
| 3 | 3 | 2 | 0 | 0.0% | 0.0–65.8% | 0.0% | 0.0–56.1% |
| **All** | 10 | 8 | 3 | 37.5% | 13.7–69.4% | 30.0% | 10.8–60.3% |

- **Coverage:** 80.0% (8 of 10 asked; a skipped problem lowers the eligible accuracy, not the attempted one)
- **Statuses:** correct 3 · incorrect 1 · no_final_answer 1 · agent_error 1 · timeout 1 · ui_error 1 · skipped 2
- **Format compliance:** 80.0% of 5 answered replies ended with a `FINAL ANSWER:` marker (95% CI 37.6–96.4%)
- **Latency** (submit to end of reply): p50 2.0 s · p95 5.0 s · mean 2.6 s · max 5.0 s (n=6)
- **Tokens** (reported by 3 attempts): mean 200 prompt + 40 completion
- **Tool calls:** 3 in all, 0.60 per answered reply; 2 replies used tools

## Web UI

| Check | Level | Pass | Fail | N/A | Verdict |
|:--|:--|--:|--:|--:|:--|
| U-01 Console clean | must | 6 | 0 | 2 | pass |
| U-02 Network clean | must | 6 | 0 | 2 | pass |
| U-03 Terminal state | must | 6 | 1 | 1 | fail |
| U-04 Incremental streaming | must | 4 | 0 | 4 | pass |
| U-05 UI overhead | must | 4 | 1 | 3 | pass |
| U-06 Render fidelity | must | 3 | 1 | 4 | fail |
| U-07 Scroll follow | must | 4 | 0 | 4 | pass |
| U-08 Render safety | must | 5 | 0 | 3 | pass |

U-05 is judged on the median of the overhead, because fewer than 20 attempts is too few for a 95th percentile to be more than the slowest one.

- **TTFT (UI)** (Enter to the first text in the reply): p50 350 ms · p95 550 ms (n=5)
- **UI overhead** (TTFT in the UI minus the agent's own): p50 60 ms · p95 120 ms, against a limit of 100 ms at the median (n=5)
- **Update cadence** (informational): 25.0 updates per reply, largest gap p95 200 ms

### Probes

| Probe | Level | Result | Detail |
|:--|:--|:--|:--|
| P-01 Stop | must | pass | stopped in 312 ms |
| P-04 Mobile | should | fail | horizontal scroll \| at 375 px second line |

**Result:** Must checks failed: U-03, U-06. The run exits with status 2.

The page broke 1 attempt: unknown 1.

## Settings

| Setting | Value |
|:--|:--|
| EVAL_REPEATS | 1 |
| EVAL_WORKERS | 2 |
| GAIA_LEVEL | 1 |

