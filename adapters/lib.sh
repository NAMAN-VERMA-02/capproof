# Shared helpers, sourced inside the runner container. Every setup step fails
# closed: a failed setup must stop the run, never measure an unbudgeted key.
set -eu

CAP=0.0045 # just under ten calls at the default call cost
ORACLE=http://mock:9400
RESULTS=/results/${GATEWAY}.jsonl
: > "$RESULTS"

wait_for() {
  i=0
  until curl -fsS -o /dev/null "$1"; do
    i=$((i + 1))
    [ "$i" -lt 150 ] || { echo "timed out waiting for $1" >&2; exit 3; }
    sleep 2
  done
}

# scenario NAME [capproof run flags]. A breach is a result, so only a run that
# could not happen at all (exit 3) stops the script.
scenario() {
  name=$1; shift
  echo "=== $GATEWAY / $name"
  code=0
  capproof run -oracle "$ORACLE" -cap-usd "$CAP" -json /tmp/report.json "$@" || code=$?
  [ "$code" -ne 3 ] || exit 3
  jq -c --arg gateway "$GATEWAY" --arg name "$name" '{gateway: $gateway, name: $name} + .' /tmp/report.json >> "$RESULTS"
  echo
}
