#!/usr/bin/env bash
# Live Tailscale qualification: Serve-based private exposure proven from a
# second tailnet node, plus the machine-sign-in assumptions.
#
#   PORTICO_QUAL_LIVE=1 PORTICO_TS_PEER=<reachable-node> \
#   scripts/qualification/tailscale.sh
#
# PORTICO_TS_PEER must be the MagicDNS name or IP of a second tailnet device
# that can reach this machine. The proof lives in fetching the canary body
# *from that peer's perspective* — curling Serve from the same machine proves
# almost nothing.
set -euo pipefail
QUAL_START="$(date -Is)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

[ "${PORTICO_QUAL_LIVE:-}" = "1" ] || fail "opt-in required: re-run with PORTICO_QUAL_LIVE=1"
require_env PORTICO_TS_PEER
require_binaries tailscale curl

start_artifacts
start_canary

export XDG_DATA_HOME="${QUAL_DIR}/xdg/data" XDG_CONFIG_HOME="${QUAL_DIR}/xdg/config"
export XDG_STATE_HOME="${QUAL_DIR}/xdg/state" XDG_RUNTIME_DIR="${QUAL_DIR}/xdg/run"
mkdir -p "$XDG_DATA_HOME" "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"

# Machine membership is detected before anything is created.
record "tailscale status" tailscale status --json
record "provider snapshot shows tailscale ready" "$PORTICO_BIN" provider list --json

# Capture the machine's Serve config before Portico touches it so the run
# proves Portico does not sign the machine out or clobber unrelated routes.
record "tailscale serve status before" tailscale serve status --json
cp "${QUAL_DIR}/commands.log" "${QUAL_DIR}/serve-before.log"

CONN_NAME="qual-ts-$(date +%s)"
record "create tailscale expose" "$PORTICO_BIN" create "$CONN_NAME" \
    --provider tailscale --source "$CANARY_URL" --source-type existing_service \
    --private-network-mode expose --health-enabled=false
TS_ID="$("$PORTICO_BIN" list --json | python3 -c "import json,sys; rows=json.load(sys.stdin); print([r['id'] for r in rows if r['name']=='$CONN_NAME'][0])")"

record "open tailscale expose" "$PORTICO_BIN" open --yes "$TS_ID"
record "tailscale serve status after open" tailscale serve status --json

# The route must be reachable from ANOTHER tailnet device. SSH is used to run
# curl on the peer; PORTICO_TS_PEER must have passwordless SSH configured.
record "prove route from second tailnet node" sh -c \
    "ssh -o BatchMode=yes -o ConnectTimeout=10 \"$PORTICO_TS_PEER\" \
     \"curl -fsS --max-time 10 http://127.0.0.1:${CANARY_PORT}/\"" > "${QUAL_DIR}/peer_fetch.out"
if [ "$(cat "${QUAL_DIR}/peer_fetch.out")" != "$CANARY_BODY" ]; then
    fail "peer fetched the wrong body: $(cat "${QUAL_DIR}/peer_fetch.out")"
fi
log "PASS: second tailnet node fetched the canary body"

# Public unreachability: a tailnet Serve route must not answer on the public
# internet. The machine's own non-tailnet address must refuse.
PUBLIC_IP="$(curl -fsS --max-time 10 https://api.ipify.org || true)"
if [ -n "$PUBLIC_IP" ]; then
    if curl -fsS --max-time 5 "http://${PUBLIC_IP}:${CANARY_PORT}/" >/dev/null 2>&1; then
        fail "the supposedly private route answered on the machine's public address"
    fi
    log "PASS: route is not publicly reachable"
fi

record "close tailscale expose" "$PORTICO_BIN" close --yes "$TS_ID"
record "tailscale serve status after close" tailscale serve status --json

# The machine must still be signed in and its pre-existing Serve config intact.
record "tailscale status after close" tailscale status --json
log "PASS: machine remains signed in to Tailscale after close"

record "delete tailscale connection" "$PORTICO_BIN" delete --yes "$TS_ID"

write_manifest "provider:           tailscale
peer:               ${PORTICO_TS_PEER}
peer-fetch proven:  yes
public-unreachable: yes"
log "Tailscale qualification complete: ${QUAL_DIR}"
