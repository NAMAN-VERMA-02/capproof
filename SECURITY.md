# Security

Report a vulnerability through GitHub's private reporting on this repository's
Security tab. Please do not open a public issue for one.

## Threat model

capproof runs gateways it does not trust, so the gateway under test is treated as
the attacker. A gateway must not be able to:

| Attack | Defence |
|---|---|
| Reset or read the oracle to hide a breach | Oracle routes need a token only the runner holds |
| Fake successes, or delay real calls until the bill has been read | Successes above provider calls are INCONCLUSIVE, not HELD |
| Bill calls from another run into this one | Every prompt carries the run ID; billed calls without it are INCONCLUSIVE |
| Pass by failing: auth errors, crashes, dead ports | HELD requires calls reaching the provider and budget refusals |
| Forge output or plant escape codes in a terminal | Gateway text is stripped of control and bidi characters |
| Get a secret published in a report | The API key and oracle token are redacted; URLs with credentials are refused |
| Exhaust the runner | Per-call timeouts, bounded samples and headers |
| Reach the internet or the host | Containers run on an internal network with no real keys and privileges dropped |

## Limits

- **A report is self-reported.** Nothing signs it. Trust a result only after
  rerunning it: images are pinned by digest and every run prints its settings.
- **Oracle token in compose files.** It is a fixed test value, kept from the gateway
  by not passing it to that container. Do not reuse it. The mock generates a random
  token when none is set.
- **Control gateways are test fixtures.** They have no authentication and are not
  for production traffic.
- **The mock provider is not hardened for the internet.** It binds to loopback by
  default.
- **Verdicts assume honest settings.** The `-cap-usd` value must match the budget
  configured on the gateway, and prices must match on both sides.
