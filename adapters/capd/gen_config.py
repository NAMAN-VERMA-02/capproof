"""Writes capd.json: one test key and budget per scenario, so runs stay independent."""
import hashlib
import json
from pathlib import Path

NAMES = ["burst", "burst-stream", "sequential", "disconnect", "omit", "malformed", "run"]
config = {
    "listen": ":8787",
    "db": "/state/capd.db",
    "synchronous": "FULL",
    "openai_url": "http://mock:9400",
    "anthropic_url": "http://mock:9400",
    "keys": [
        {
            "name": n,
            # Fake keys, public on purpose. Each exists only on this test network.
            "sha256": hashlib.sha256(f"sk-capd-test-{n}".encode()).hexdigest(),
            "budget_usd": "1" if n == "run" else "0.0045",
            **({"run_budget_usd": "0.0045"} if n == "run" else {}),
        }
        for n in NAMES
    ],
    "prices": {"gpt-4o-mini": {"input": "0.15", "output": "0.60", "cache_write": "0.15"}},
}
Path(__file__).with_name("capd.json").write_text(json.dumps(config, indent=1) + "\n", newline="\n")
