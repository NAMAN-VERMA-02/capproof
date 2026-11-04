"""Writes config.json: one virtual key and budget per scenario, so runs stay independent."""
import json
from pathlib import Path

NAMES = ["burst", "burst-stream", "sequential", "disconnect", "omit", "malformed"]
config = {
    "version": 2,
    "client": {"enforce_auth_on_inference": True},
    "config_store": {"enabled": True, "type": "sqlite", "config": {"path": "/app/state/config.db"}},
    # Local files: the gateway has no internet route, and the price must match the oracle's.
    "framework": {
        "pricing": {
            "pricing_url": "file:///app/prices/pricing.json",
            "pricing_sync_interval": 86400,
            "model_parameters_url": "file:///app/prices/pricing.json",
        }
    },
    "providers": {
        "openai": {
            "keys": [{"name": "k1", "value": "env.OPENAI_API_KEY", "models": ["*"], "weight": 1.0}],
            "network_config": {"base_url": "http://mock:9400", "max_retries": 0, "allow_private_network": True},
            "concurrency_and_buffer_size": {"concurrency": 200, "buffer_size": 400},
        }
    },
    "governance": {
        "virtual_keys": [
            {
                "id": f"vk-{n}",
                "name": f"capproof-{n}",
                "value": f"sk-bf-capproof-{n}",
                "is_active": True,
                "provider_configs": [
                    {"provider": "openai", "allowed_models": ["*"], "key_ids": ["*"], "weight": 1}
                ],
            }
            for n in NAMES
        ],
        "budgets": [
            {"id": f"bud-{n}", "max_limit": 0.0045, "reset_duration": "1d", "virtual_key_id": f"vk-{n}"}
            for n in NAMES
        ],
    },
}
Path(__file__).with_name("config.json").write_text(json.dumps(config, indent=1) + "\n", newline="\n")
