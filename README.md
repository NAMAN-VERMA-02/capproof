# capproof

Does your LLM spend cap hold when things go wrong?

capproof sends traffic through a gateway while breaking things on purpose, then
checks the result against a fake provider that keeps its own bill. It trusts nothing
the gateway says about itself.

## Try it in one minute

You need Go. No API key and no real provider are involved.

```bash
go build -o capproof ./cmd/capproof
export CAPPROOF_ORACLE_TOKEN=local-test
./capproof mock &
./capproof control -mode naive -cap-usd 0.0045 -addr 127.0.0.1:9401 &
./capproof control -mode strict -cap-usd 0.0045 -addr 127.0.0.1:9402 &
./capproof run -scenario burst -cap-usd 0.0045 -target http://127.0.0.1:9401   # BREACHED
./capproof run -scenario burst -cap-usd 0.0045 -target http://127.0.0.1:9402   # HELD
```

The naive gateway counts spend after each response, which many real designs do. The
strict one holds the worst-case cost before forwarding. If the suite could not tell
them apart, its other results would mean nothing, so CI checks that it can.

## Verdicts

| Verdict | Meaning | Exit code |
|---|---|---|
| `HELD` | Calls reached the provider, the gateway refused the rest, and the bill stayed within the cap | 0 |
| `BREACHED` | The provider billed more than the cap | 1 |
| `INCONCLUSIVE` | The run can't show either, for example auth failures or crashes | 2 |
| could not run | The setup is wrong, or the test could not have failed | 3 |

`INCONCLUSIVE` is there so a broken setup never reads as a cap that held.

## Scenarios and faults

| Scenario | What it does |
|---|---|
| `burst` | Releases all calls at once |
| `sequential` | Sends one call at a time |
| `disconnect` | Drops each stream after the first byte, while the provider keeps billing |

Add `-wave2 5` to send more calls after a wait, which catches late accounting. Add
`-mock '{"usage":"omit"}'` or `'{"usage":"malformed"}'` to break the usage the
provider reports, or `'{"ignore_max_tokens":true}'` to make it overrun the limit.

## Test a real gateway

`adapters/` has setups for LiteLLM and Bifrost. Each runs on an internal Docker
network with fake keys and no internet route:

```bash
cd adapters/litellm
docker compose up -d litellm mock postgres
docker compose --profile run run --rm --no-deps runner
docker compose --profile run down -v
```

Reports land in `results/`.

## What these results can't tell you

- **The provider is a mock.** Whether real providers keep billing after a disconnect
  is not confirmed, so the `disconnect` scenario tests the case where they do.
- **A cap can only be as sound as its inputs.** "Never over the cap" needs correct
  prices, a valid bound on input size, and a provider that honours its output limit.
- **It tests enforcement only.** It doesn't measure latency, routing or anything else
  a gateway does, and an overshoot may be a deliberate design choice.

## Status

Early. Scoreboard results stay unpublished until the maintainers involved have had
a chance to respond. See [SECURITY.md](SECURITY.md) for the threat model.

The burst approach builds on
[per-run-budget-benchmark](https://github.com/domondi1/per-run-budget-benchmark)
(Apache-2.0), including its gateway configurations.
