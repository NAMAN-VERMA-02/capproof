. /adapters/lib.sh
GW=http://litellm:4000
MASTER="Authorization: Bearer sk-capproof-fake-master"
wait_for "$ORACLE/healthz"
wait_for "$GW/health/liveliness"

# A fresh key per scenario, so one run's spend cannot affect the next.
new_key() {
  key=$(curl -fsS "$GW/key/generate" -H "$MASTER" -H 'Content-Type: application/json' \
    -d "{\"max_budget\":$CAP,\"models\":[\"gpt-4o-mini\"]}" | jq -er .key)
  export CAPPROOF_API_KEY="$key"
}

T="-target $GW"
new_key; scenario key-burst           $T -scenario burst -requests 30 -wave2 5
new_key; scenario key-burst-stream    $T -scenario burst -requests 30 -wave2 5 -stream
new_key; scenario key-sequential      $T -scenario sequential -requests 15
new_key; scenario key-disconnect      $T -scenario disconnect -requests 30
new_key; scenario key-usage-omitted   $T -scenario sequential -requests 20 -mock '{"usage":"omit"}'
new_key; scenario key-usage-malformed $T -scenario sequential -requests 20 -mock '{"usage":"malformed"}'

# Per-run budget: an agent with max_budget_per_session and a key bound to it.
agent=$(curl -fsS "$GW/v1/agents" -H "$MASTER" -H 'Content-Type: application/json' -d "{
  \"agent_name\":\"capproof-$(date +%s)\",
  \"agent_card_params\":{\"protocolVersion\":\"1.0\",\"name\":\"capproof\",\"description\":\"capproof\",\"url\":\"http://127.0.0.1:9/\",\"version\":\"1.0.0\",\"defaultInputModes\":[\"text\"],\"defaultOutputModes\":[\"text\"],\"capabilities\":{},\"skills\":[]},
  \"litellm_params\":{\"max_budget_per_session\":$CAP}}" | jq -er .agent_id)
key=$(curl -fsS "$GW/key/generate" -H "$MASTER" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg agent "$agent" '{agent_id: $agent, models: ["gpt-4o-mini"]}')" | jq -er .key)
bound=$(curl -fsS "$GW/key/info" -H "Authorization: Bearer $key" | jq -er .info.agent_id)
[ "$bound" = "$agent" ] || { echo "key is not bound to the agent" >&2; exit 3; }
export CAPPROOF_API_KEY="$key"
S='x-litellm-trace-id: {run}'
scenario session-sequential   $T -scenario sequential -requests 15 -H "$S"
scenario session-burst        $T -scenario burst -requests 30 -wave2 5 -H "$S"
scenario session-burst-stream $T -scenario burst -requests 30 -wave2 5 -stream -H "$S"
