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

## capd, the reference gateway

`capd` is a small gateway built to pass this suite. It holds a call's worst-case cost
in a durable ledger before forwarding it, then settles the true cost. If the cost is
unknown or the process crashes, the full hold is charged. If the ledger is unavailable,
no call is sent.

It suits unattended, API-billed work such as CI jobs and overnight agents, on one
machine. It stops runaway spend from anything routed through it. It **can't** stop
someone using a stolen provider key directly, so keep provider-side caps on as well.

```bash
go build -o capd ./cmd/capd
./capd hash-key "pick-a-long-random-key"     # put the output in capd.json
export ANTHROPIC_API_KEY=your-real-key
./capd -config capd.json
```

A minimal `capd.json`:

```json
{
  "anthropic_url": "https://api.anthropic.com",
  "keys": [{
    "name": "ci",
    "sha256": "<output of hash-key>",
    "budget_usd": "50",
    "run_budget_usd": "5",
    "max_run_steps": 200,
    "usd_per_minute": "2"
  }],
  "prices": {
    "your-model-id": {"input": "3", "output": "15", "cache_write": "6", "cache_read": "0.3"}
  }
}
```

Prices are in dollars per million tokens, and you must copy them from the provider's
price list. A model with no price is refused. A wrong price makes the budget wrong.

Point a client at it with `http://127.0.0.1:8787` as the base URL and your virtual key
as the API key. Add `X-Run-Id: <id>` to a request, or use `/r/<id>/v1/...` as the path,
to count it against a per-run budget. The run ID comes from the client, so a run budget
guards against bugs, not against an attacker. The key budget is the hard limit.

Measured results, including what the safety costs, are in
[published/](published/README.md). In short: the cap held in every test, but it costs
about 53 ms at p99 at 1000 calls a second on a laptop, and it refuses some calls that
would have fitted.

**Not tested yet:** real providers, and Claude Code or any other real client.

## Test a real gateway

`adapters/` has setups for LiteLLM, Bifrost and capd. Each runs on an internal Docker
network with fake keys and no internet route:

```bash
cd adapters/litellm
docker compose up -d litellm mock postgres
docker compose --profile run run --rm --no-deps runner
docker compose --profile run down -v
```

Reports land in `results/`.

## Test a real provider

`capproof lab` measures what a real provider bills after a dropped stream, and how far
past a spend cap usage can go. It uses your own key and a small amount of your own money,
and sends nothing without `-confirm`. See [docs/provider-lab.md](docs/provider-lab.md).

## What these results can't tell you

- **The provider is a mock.** Whether real providers keep billing after a disconnect
  is not confirmed, so the `disconnect` scenario tests the case where they do.
- **A cap can only be as sound as its inputs.** "Never over the cap" needs correct
  prices, a valid bound on input size, and a provider that honours its output limit.
- **It tests enforcement only.** It doesn't measure latency, routing or anything else
  a gateway does, and an overshoot may be a deliberate design choice.

## Status

v0.1. Early, and not yet tested against real providers. Results for other gateways stay
unpublished until the maintainers involved have had a chance to respond. See [THREAT_MODEL.md](THREAT_MODEL.md) for the threat model, [ARCHITECTURE.md](ARCHITECTURE.md) for the design and [TESTING.md](TESTING.md) for what each test proves.

The burst approach builds on
[per-run-budget-benchmark](https://github.com/domondi1/per-run-budget-benchmark)
(Apache-2.0), including its gateway configurations.
