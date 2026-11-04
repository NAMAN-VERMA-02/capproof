. /adapters/lib.sh
GW=http://capd:8787
wait_for "$ORACLE/healthz"
wait_for "$GW/healthz"

T="-target $GW"
key() { export CAPPROOF_API_KEY="sk-capd-test-$1"; }
key burst;        scenario key-burst           $T -scenario burst -requests 30 -wave2 5
key burst-stream; scenario key-burst-stream    $T -scenario burst -requests 30 -wave2 5 -stream
key sequential;   scenario key-sequential      $T -scenario sequential -requests 15
key disconnect;   scenario key-disconnect      $T -scenario disconnect -requests 30
key omit;         scenario key-usage-omitted   $T -scenario sequential -requests 20 -mock '{"usage":"omit"}'
key malformed;    scenario key-usage-malformed $T -scenario sequential -requests 20 -mock '{"usage":"malformed"}'
key run;          scenario run-burst           $T -scenario burst -requests 30 -wave2 5 -H 'X-Run-Id: {run}'
