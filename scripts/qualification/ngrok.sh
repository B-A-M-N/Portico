#!/usr/bin/env bash
# Live ngrok qualification (experimental provider; explicit opt-in on both
# switches). Proves a real assigned endpoint carries the canary body, that
# close removes the tunnel, that restart observation correlates the correct
# tunnel, and — the experimental boundary — that Portico does NOT claim
# protection for it.
#
#   PORTICO_QUAL_LIVE=1 PORTICO_NGROK_LIVE=1 NGROK_AUTHTOKEN=... \
#   scripts/qualification/ngrok.sh
set -euo pipefail
QUAL_START="$(date -Is)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

[ "${PORTICO_QUAL_LIVE:-}" = "1" ] || fail "opt-in required: re-run with PORTICO_QUAL_LIVE=1"
[ "${PORTICO_NGROK_LIVE:-}" = "1" ] || fail "ngrok is experimental: also set PORTICO_NGROK_LIVE=1"
require_env NGROK_AUTHTOKEN
require_binaries ngrok curl

start_artifacts
start_canary

export XDG_DATA_HOME="${QUAL_DIR}/xdg/data" XDG_CONFIG_HOME="${QUAL_DIR}/xdg/config"
export XDG_STATE_HOME="${QUAL_DIR}/xdg/state" XDG_RUNTIME_DIR="${QUAL_DIR}/xdg/run"
export PORTICO_ENABLE_EXPERIMENTAL_NGROK=1
mkdir -p "$XDG_DATA_HOME" "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"

record "supervisor start" "$PORTICO_BIN" supervisor start

CONN_NAME="qual-ngrok-$(date +%s)"
record "create ngrok tunnel" "$PORTICO_BIN" create "$CONN_NAME" \
    --provider ngrok --source "$CANARY_URL" --source-type existing_service \
    --health-enabled=false
NG_ID="$("$PORTICO_BIN" list --json | python3 -c "import json,sys; rows=json.load(sys.stdin); print([r['id'] for r in rows if r['name']=='$CONN_NAME'][0])")"

record "open ngrok tunnel" "$PORTICO_BIN" open --yes "$NG_ID"
NG_URL="$("$PORTICO_BIN" inspect --json "$NG_ID" | python3 -c "import json,sys; print(json.load(sys.stdin)['public_address'])")"
case "$NG_URL" in
    https://*.ngrok*|https://*.ngrok.io|https://*.ngrok.app|https://*.ngrok.dev) : ;;
    *) fail "assigned endpoint does not look like an ngrok URL: $NG_URL" ;;
esac
prove_endpoint_body "ngrok public URL" "$NG_URL"

# The experimental boundary, proven from the provider capability descriptor:
# Portico must declare NO built-in protection for ngrok rather than implying
# the URL is protected.
record "capability declares no protection" "$PORTICO_BIN" provider list --json
if "$PORTICO_BIN" provider list --json | python3 -c "
import json, sys
provs = json.load(sys.stdin)
ng = [p for p in provs if p['id'] == 'ngrok'][0]
modes = (ng.get('capabilities') or {}).get('protection_modes') or []
sys.exit(0 if modes else 1)
"; then
    fail "ngrok capability advertises protection it does not apply"
fi
log "PASS: capability declares no protection (honest experimental boundary)"

# Restart observation: after a supervisor restart, observation must correlate
# the correct existing tunnel instead of creating a second one.
record "supervisor stop" "$PORTICO_BIN" supervisor stop
record "supervisor start" "$PORTICO_BIN" supervisor start
sleep 3
prove_endpoint_body "ngrok endpoint survives supervisor restart" "$NG_URL"
URL_AFTER="$("$PORTICO_BIN" inspect --json "$NG_ID" | python3 -c "import json,sys; print(json.load(sys.stdin)['public_address'])")"
[ "$URL_AFTER" = "$NG_URL" ] || fail "observation reassigned a different endpoint after restart: $URL_AFTER (was $NG_URL)"
log "PASS: restart observation correlated the same tunnel"

record "close ngrok tunnel" "$PORTICO_BIN" close --yes "$NG_ID"
prove_endpoint_gone "closed ngrok tunnel" "$NG_URL"
record "delete ngrok connection" "$PORTICO_BIN" delete --yes "$NG_ID"

write_manifest "provider:           ngrok (experimental)
endpoint:           proven
restart-correlated: yes
protection claim:   none (correct)"
log "ngrok qualification complete: ${QUAL_DIR}"
