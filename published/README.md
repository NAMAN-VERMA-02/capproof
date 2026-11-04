# capd's measured results

These are capd's own results, measured on 2026-11-05 at the commit tagged v0.1.0.
Results for other gateways stay unpublished until their maintainers have had a chance
to respond.

**Machine:** Windows 11 laptop, 12th Gen Intel Core i3-1215U, with the mock provider on
the same machine and no network between them. Expect different numbers elsewhere.

## Does the cap hold?

| Test | Result |
|---|---|
| Docker suite, 7 scenarios ([raw](capd.jsonl)) | 7 HELD, 0 BREACHED |
| Random faults: 10,000 seeds, 155,285 calls | 0 invariant violations |
| Killing the binary mid-stream, 4 rounds of 12 streams | Ledger never below the bill |

The random fault test mixes connection resets, stalls, early closes, fragmented
responses, cancelled clients, and missing or malformed usage. It ran with
`synchronous=NORMAL`. Removing either of two safeguards made it fail on the first seed,
so it can detect these bugs. A process kill is tested, but a power failure is not.

The invariants hold only if prices are correct, input fits the bound, the provider
honours its output limit, and no unpriced server-side tools run.

## What the safety costs

**Usable budget.** Holding the worst case refuses calls that would have fitted:

| Scenario | Budget actually spent | Calls reaching the provider (cap allows 9) |
|---|---|---|
| Burst, 30 at once, then 5 more | 60.2% | 6 |
| One at a time | 80.2% | 8 |
| Client disconnects | 40.1% | 4 |
| Usage missing or malformed | 40.1% | 4 |

The test budget is tiny, about ten calls, so one hold is a large share of it. Input is
bounded at one token per byte, which can be proven but is far above real prompts. When
a call's cost is unknown, its full hold is charged.

**Latency.** Added on a fixed send schedule, timed from when each call was due, over
20 seconds at each rate with a non-streaming call:

| Rate | Setup | p50 | p99 | p99.9 | Max |
|---|---|---|---|---|---|
| 200/s | Direct to the mock | 0.9 ms | 4.7 ms | 14.4 ms | 19.9 ms |
| 200/s | capd, `FULL` | 12.4 ms | 21.9 ms | 27.3 ms | 31.4 ms |
| 200/s | capd, `NORMAL` | 2.5 ms | 29.0 ms | 88.6 ms | 98.6 ms |
| 1000/s | Direct to the mock | 0.6 ms | 3.8 ms | 10.2 ms | 12.2 ms |
| 1000/s | capd, `FULL` | 22.2 ms | 56.4 ms | 66.4 ms | 75.4 ms |
| 1000/s | capd, `NORMAL` | 2.5 ms | 234.6 ms | 301.6 ms | 352.6 ms |

**This misses the project's goal** of under 10 ms added at p99 with `FULL` at 1000/s:
the measured figure is about 53 ms. Durable admission costs real time on this laptop.
`NORMAL` has a better median, a much worse tail at 1000/s, and can lose recent commits
in a power failure. All numbers come from one run, so treat them as a rough guide.

## Reproduce

```bash
go test -run Chaos ./internal/capd                    # CI's quick pass
CAPPROOF_CHAOS_SEEDS=10000 go test -timeout 0 -run Chaos ./internal/capd
go test -run SurvivesKill ./cmd/capd
go test -run '^$' -bench Admission ./internal/capd
```

For the Docker suite, run `python adapters/capd/gen_config.py`, then follow the steps in
the main README from `adapters/capd`, starting `capd mock` in place of the gateway.
