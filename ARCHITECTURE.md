# Architecture

capproof asks one question: when things go wrong, does a gateway's LLM spend cap hold? It answers by comparing what the gateway *says* it spent with what a fake provider *actually billed*.

```
 runner ──calls──▶ gateway under test ──▶ mock provider (billing oracle)
   │                  (naive | strict | capd | LiteLLM | Bifrost ...)    │
   └───────────── reads the bill, with a token only the runner holds ◀───┘
```

The verdict (`HELD`, `BREACHED`, `INCONCLUSIVE`, could not run) uses only the oracle's bill. Nothing the gateway reports about itself is trusted.

## Packages
| Package | Job |
|---|---|
| `internal/mock` | Fake provider speaking OpenAI Chat Completions and Anthropic Messages. It records what a real provider would have charged, independently of any gateway. Faults are injected here (missing or malformed usage, ignored `max_tokens`). |
| `internal/runner` | Sends a scenario (`burst`, `sequential`, `disconnect`, optional second wave) and judges the result from the oracle's bill. Also `bench`, which times calls on a fixed schedule from when each was *due*, so a slow target cannot hide its own queueing. |
| `internal/control` | Two tiny gateways that calibrate the suite. `naive` counts spend after each response and must be BREACHED; `strict` holds the worst case first and must be HELD. If the suite could not tell them apart, no other result would mean anything. |
| `internal/capd` | The reference gateway (below). |
| `internal/faultproxy` | A TCP proxy that breaks responses on purpose: resets, stalls, early closes, tiny fragments. Used by the chaos tests. |
| `internal/meter` | Reads the parts of a request and response that decide cost, defensively. |
| `internal/money` | Exact integer dollar amounts (pico-dollars), because comparing spend with a cap in floating point can move the verdict. |
| `internal/lab` | `capproof lab`: measures a real provider with your own key, built to spend as little as possible. |

## capd
```
client ─▶ authenticate virtual key ─▶ price the worst case ─▶ Reserve (durable) ─▶ forward ─▶ settle
                                                                  │                              │
                                                       refused: 429 / ledger down: 503   Capture true cost,
                                                                                         or full hold if unknown
```
- **Reserve before forwarding.** The worst-case cost of a call (bounded input plus `max_tokens` output) is written to a SQLite ledger and fsynced (`synchronous=FULL` by default) before anything is sent upstream.
- **Settle after.** The true cost replaces the hold. If the cost is unknown (missing usage, dropped stream, crash), the full hold is charged.
- **Fail closed.** If the ledger is unavailable, no call is sent (503).
- **Crash recovery.** On startup, holds left open by a crash are charged at their full amount.
- **Shutdown.** SIGINT/SIGTERM stops accepting connections and waits up to 60 s for in-flight calls.
- **Logging.** Structured (`log/slog`): reservation failures, settlement failures, overage, upstream errors and suspected agent loops.

## Trade-off
Holding the worst case refuses calls that would have fitted, and durable admission costs latency. Both are measured in [published/README.md](published/README.md), including where capd misses its own 10 ms goal.
