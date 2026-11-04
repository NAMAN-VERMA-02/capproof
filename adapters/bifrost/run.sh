. /adapters/lib.sh
GW=http://bifrost:8080
wait_for "$ORACLE/healthz"
wait_for "$GW/health"

T="-target $GW -model openai/gpt-4o-mini"
# Each scenario has its own virtual key and budget from config.json.
key() { export CAPPROOF_API_KEY="sk-bf-capproof-$1"; }
key burst;        scenario key-burst           $T -scenario burst -requests 30 -wave2 5
key burst-stream; scenario key-burst-stream    $T -scenario burst -requests 30 -wave2 5 -stream
key sequential;   scenario key-sequential      $T -scenario sequential -requests 15
key disconnect;   scenario key-disconnect      $T -scenario disconnect -requests 30
key omit;         scenario key-usage-omitted   $T -scenario sequential -requests 20 -mock '{"usage":"omit"}'
key malformed;    scenario key-usage-malformed $T -scenario sequential -requests 20 -mock '{"usage":"malformed"}'
