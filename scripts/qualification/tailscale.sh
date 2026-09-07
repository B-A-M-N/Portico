#!/usr/bin/env bash
# Live Tailscale qualification: Serve-based private exposure proven from a
# second tailnet node, plus the machine-sign-in assumptions.
#
#   PORTICO_QUAL_LIVE=1 PORTICO_TS_PEER=<reachable-node> \
#   scripts/qualification/tailscale.sh
#
# PORTICO_TS_PEER must be the MagicDNS name or IP of a second tailnet device
# that can reach this machine. The proof lives in fetching the canary body
# *from that peer's perspective* against the Serve URL Portico actually
# registered — curling Serve from the same machine proves almost nothing, and
# curling the peer's own localhost proves literally nothing.
set -euo pipefail
QUAL_START="$(date -Is)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

[ "${PORTICO_QUAL_LIVE:-}" = "1" ] || fail "opt-in required: re-run with PORTICO_QUAL_LIVE=1"
require_env PORTICO_TS_PEER
require_binaries tailscale curl python3

start_artifacts
start_canary

export XDG_DATA_HOME="${QUAL_DIR}/xdg/data" XDG_CONFIG_HOME="${QUAL_DIR}/xdg/config"
export XDG_STATE_HOME="${QUAL_DIR}/xdg/state" XDG_RUNTIME_DIR="${QUAL_DIR}/xdg/run"
mkdir -p "$XDG_DATA_HOME" "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"

serve_snapshot() {
    # `tailscale serve status --json` is the ground truth for what is served.
    # Parsed copies are stored as files, raw command logs via record.
    tailscale serve status --json > "${QUAL_DIR}/serve-raw-$1.json" 2>&1
    cat "${QUAL_DIR}/serve-raw-$1.json"
}

signed_in_identity() {
    # The identity facts that must not change: backend state and login name.
    # A successful `tailscale status` invocation is not itself proof of
    # sameness; comparing parsed identity fields is.
    tailscale status --json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
self_ = d.get("Self") or {}
print(d.get("BackendState", ""), self_.get("UserID", ""), self_.get("LoginName", ""))
'
}

# Machine membership is detected before anything is created.
record "tailscale status" tailscale status --json
record "provider snapshot shows tailscale ready" "$PORTICO_BIN" provider list --json

IDENTITY_BEFORE="$(signed_in_identity)"
serve_snapshot before
cp "${QUAL_DIR}/serve-raw-before.json" "${QUAL_DIR}/serve-before.json"

CONN_NAME="qual-ts-$(date +%s)"
record "create tailscale expose" "$PORTICO_BIN" create "$CONN_NAME" \
    --provider tailscale --source "$CANARY_URL" --source-type existing_service \
    --private-network-mode expose --health-enabled=false
TS_ID="$("$PORTICO_BIN" list --json | python3 -c "import json,sys; rows=json.load(sys.stdin); print([r['id'] for r in rows if r['name']=='$CONN_NAME'][0])")"

record "open tailscale expose" "$PORTICO_BIN" open --yes "$TS_ID"
serve_snapshot after

# Resolve the Serve URL Portico actually registered from Tailscale's own
# config: the handler whose proxy target is the canary. Peer-side proof is
# only meaningful against this URL.
SERVE_URL="$(python3 - "${QUAL_DIR}/serve-raw-after.json" "${CANARY_PORT}" <<'PYEOF'
import json, sys
d = json.load(open(sys.argv[1]))
port = sys.argv[2]
web = d.get("Web") or {}
for host, entry in web.items():
    for path, handler in (entry.get("Handlers") or {}).items():
        if f":{port}" in str(handler):
            base = host if host.startswith("http") else "https://" + host
            print(base.rstrip("/") + ("/" if path in ("", "/") else path))
            sys.exit(0)
sys.exit(1)
PYEOF
)" || fail "Portico's Serve route did not appear in tailscale serve status"
if [ -z "$SERVE_URL" ]; then
    fail "could not resolve the Serve URL from tailscale serve status"
fi
log "Portico registered Serve route: ${SERVE_URL}"

# The route must be reachable from ANOTHER tailnet device, at the Serve URL —
# not at the peer's own loopback, which would test nothing.
record "prove route from second tailnet node" sh -c \
    "ssh -o BatchMode=yes -o ConnectTimeout=10 \"$PORTICO_TS_PEER\" \
     \"curl -fksS --max-time 10 '${SERVE_URL}'\"" > "${QUAL_DIR}/peer_fetch.out"
if [ "$(cat "${QUAL_DIR}/peer_fetch.out")" != "$CANARY_BODY" ]; then
    fail "peer fetched the wrong body via ${SERVE_URL}: $(cat "${QUAL_DIR}/peer_fetch.out")"
fi
log "PASS: second tailnet node fetched the canary body via the Serve URL"

# Private-route proof, part 1: Tailscale's own config must not declare the
# route as Funnel. Funnel is the public-internet exposure; Serve is tailnet
# only. A route served with funnel on is not a private route.
if python3 - "${QUAL_DIR}/serve-raw-after.json" <<'PYEOF'
import json, sys
d = json.load(open(sys.argv[1]))
funnel = d.get("Funnel") or {}
for host, entry in funnel.items():
    if entry.get("Handlers"):
        sys.exit(0)
sys.exit(1)
PYEOF
then
    fail "the route is declared as Funnel (public); a private expose must not be"
fi
log "PASS: route is Serve (tailnet-only), not Funnel (public)"

# Private-route proof, part 2: the local TCP listener bound for the Serve
# proxy must be on the loopback or tailnet interface only, not 0.0.0.0.
# (A 0.0.0.0 bind with a public IP would be a public exposure regardless of
# the Serve declaration.)
if [ -n "${PUBLIC_IFACE_CHECK:-1}" ]; then
    LISTEN_HOSTS="$(ss -tlnp 2>/dev/null | awk -v p="$CANARY_PORT" '$4 ~ ":"p"$" {print $4}' || true)"
    if [ -n "$LISTEN_HOSTS" ]; then
        case "$LISTEN_HOSTS" in
            *0.0.0.0*|*"::"*) fail "the canary listens on a wildcard address: $LISTEN_HOSTS" ;;
        esac
    fi
    log "PASS: canary listener is not wildcard-bound"
fi

record "close tailscale expose" "$PORTICO_BIN" close --yes "$TS_ID"
serve_snapshot closed

# Post-close: Portico's route must be gone from Tailscale's config, and every
# pre-existing route must be byte-identical. Unrelated user Serve routes being
# silently withdrawn is a destructive side effect, not cleanup.
if python3 - "${QUAL_DIR}/serve-raw-closed.json" "${CANARY_PORT}" <<'PYEOF'
import json, sys
d = json.load(open(sys.argv[1]))
port = sys.argv[2]
for host, entry in (d.get("Web") or {}).items():
    for handler in (entry.get("Handlers") or {}).values():
        if f":{port}" in str(handler):
            sys.exit(0)
sys.exit(1)
PYEOF
then
    fail "Portico's Serve route is still present in tailscale serve status after close"
fi
log "PASS: Portico's Serve route is withdrawn after close"

if ! python3 - "${QUAL_DIR}/serve-before.json" "${QUAL_DIR}/serve-raw-after.json" <<'PYEOF'
import json, sys
def routes(path):
    d = json.load(open(path))
    out = {}
    for host, entry in (d.get("Web") or {}).items():
        for path_, handler in (entry.get("Handlers") or {}).items():
            out[(host, path_)] = handler
    return out
before, after = routes(sys.argv[1]), routes(sys.argv[2])
sys.exit(0 if before == after else 1)
PYEOF
then
    fail "pre-existing Serve routes changed across the run; Portico altered routes it does not own"
fi
log "PASS: pre-existing Serve configuration preserved exactly"

# The machine must still be signed in as the same identity.
IDENTITY_AFTER="$(signed_in_identity)"
if [ "$IDENTITY_BEFORE" != "$IDENTITY_AFTER" ]; then
    fail "machine identity changed across the run: before=[$IDENTITY_BEFORE] after=[$IDENTITY_AFTER]"
fi
log "PASS: machine remains signed in to Tailscale with the same identity"

record "tailscale status after close" tailscale status --json
record "delete tailscale connection" "$PORTICO_BIN" delete --yes "$TS_ID"

write_manifest "provider:           tailscale
peer:               ${PORTICO_TS_PEER}
serve-url:          ${SERVE_URL}
peer-fetch proven:  yes (at Serve URL, not peer loopback)
funnel-absent:      yes
config-preserved:   yes
identity-unchanged: yes"
log "Tailscale qualification complete: ${QUAL_DIR}"
