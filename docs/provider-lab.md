# Testing real providers

Two questions have no independent public answer. Does a provider keep billing after a
client drops a stream? How far past a spend cap can usage go? `capproof lab` measures
both using your own key and a small amount of your own money.

## Keep the risk small

The tool refuses to start if the worst case is above `-max-spend-usd` (default $1, never
above $50). It sends one call at a time, never retries, stops at the first refusal, and
sends your key only to the provider's official API. Calls are limited to OpenAI and
Anthropic. Gemini is left out because its caps can lag by about ten minutes.

Do these first:

1. Create a **new project or workspace** just for this test, with its own new API key.
   Billing is then easy to read and the key is easy to revoke.
2. Set a **low spend limit** on that project, such as $5.
3. Use a **cheap model**, and copy its prices from the provider's price list.
4. Read the provider's terms. Delete the key when you finish.

Set the key in your terminal. It is read from the environment so that it stays out of
your shell history and process list:

```bash
read -rs CAPPROOF_LAB_KEY && export CAPPROOF_LAB_KEY
```

Build the tool. These commands are for Git Bash; on Windows, name the output `capproof.exe` and run it the same way:

```bash
go build -o capproof ./cmd/capproof
```

## Experiment 1: what does a cancelled stream cost?

Check the worst case first. Without `-confirm`, nothing is sent:

```bash
./capproof lab -provider openai -model MODEL -input-usd-per-mtok IN -output-usd-per-mtok OUT -phase full
```

Then run the baseline and write down its time window:

```bash
./capproof lab -provider openai -model MODEL -input-usd-per-mtok IN -output-usd-per-mtok OUT -phase full -confirm -json full.json
```

Wait at least an hour so the two runs land in different usage buckets, then run the
cancelled streams:

```bash
./capproof lab -provider openai -model MODEL -input-usd-per-mtok IN -output-usd-per-mtok OUT -phase cancel -confirm -json cancel.json
```

Wait for the provider's usage dashboard to update, then read the output tokens billed
for each window and work out:

```text
output tokens billed per cancelled call / output tokens billed per finished call
```

| Result | What it means |
|---|---|
| Close to 1 | Cancelling doesn't save money: the provider bills the whole completion |
| Close to 0 | The provider stops billing near the point of the cancel |
| In between | It stops partway, so record the actual number |

Repeat with `-provider anthropic`. One run proves little, so repeat it on another day.
Note the date and model each time, because this behaviour can change.

## Experiment 2: how far past a cap can usage go?

Use a separate new project with a very low limit, such as $1, and enough calls to reach
it. Set `-max-spend-usd` just above that limit, so the tool's own ceiling doesn't stop
the run first:

```bash
./capproof lab -provider openai -model MODEL -input-usd-per-mtok IN -output-usd-per-mtok OUT -phase cap -calls 200 -delay 2s -max-spend-usd 2 -confirm -json cap.json
```

The tool stops at the first refusal and prints how long that took. Then compare the
dashboard's final total with your limit: the difference is the overshoot. Run this one
slowly, because a low call rate limits how far it can overshoot.

## Publishing the result

State the provider, model, date, number of calls, and what you read from the dashboard.
Don't claim more than one run shows. If a provider's limit works well, publish that too:
it's a useful result. Check your JSON files for anything private before sharing them.
