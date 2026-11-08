# Testing

```bash
go test ./...                                          # everything, about a minute
go test -race -count=1 ./...                           # what CI runs on Linux
go test -run Chaos ./internal/capd                     # random faults, CI's quick pass
CAPPROOF_CHAOS_SEEDS=10000 go test -timeout 0 -run Chaos ./internal/capd
go test -run SurvivesKill ./cmd/capd                   # kills the capd binary mid-stream
go test -run '^$' -bench Admission ./internal/capd     # durable admission cost
```

## What proves what
| Area | Files | Proves |
|---|---|---|
| The suite can tell good from bad | `internal/control/control_test.go` | `TestSuiteTellsControlsApart`: 8 scenarios where the naive gateway must be BREACHED and the strict gateway HELD. If this fails, no other verdict is trustworthy. |
| capd keeps the cap | `internal/capd/capd_test.go`, `chaos_test.go` | the cap holds under every injected fault; concurrent reservations never overbook; the ledger fails closed; bad configs are rejected; seeded random faults (resets, stalls, early closes, fragments, cancelled clients, missing or malformed usage) never break the invariants |
| capd survives a crash | `cmd/capd/crash_test.go` | after the binary is killed mid-stream, the ledger is never below the bill |
| The runner is honest | `internal/runner/runner_test.go` | verdicts come only from the oracle |
| The gateway under test is hostile | `internal/*/security_test.go` | limit smuggling, undercounting, hop-by-hop headers, hostile gateway responses, oracle token leaks, credentials in URLs, control characters in output |
| Money is exact | `internal/money`, `internal/meter` | integer arithmetic, defensive request parsing |
| The mock is a faithful oracle | `internal/mock` | OpenAI and Anthropic formats, streaming, fault modes |
| The real-provider lab spends little | `internal/lab` | refuses to start over its ceiling, one call at a time, no retries |
| Latency measurement is sane | `internal/runner/bench_test.go` | p50 ≤ p95 ≤ p99 ≤ max; timing runs from when a call was due |

CI (`.github/workflows/ci.yml`): gofmt, `go vet`, `go test -race` on Linux, plain `go test` on Windows, and govulncheck. Actions are pinned by SHA.

## Gaps
- No fuzz tests (the request parser in `internal/meter` is the obvious target).
- No linter beyond `go vet` and gofmt.
- Power-failure durability is not tested; process kill is.
- Nothing has run against a real provider or a real client yet.
- `internal/capd/bench_test.go` measures admission only, not the whole request path; use `capproof bench` for that.
