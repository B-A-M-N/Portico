#!/usr/bin/env bash
# Deterministic local-forward qualification: the full lifecycle with physical
# proof at every mutation. Requires no provider account, so it runs in any
# gate that opts in.
#
#   PORTICO_QUAL_LIVE=1 scripts/qualification/local_forward.sh
set -euo pipefail
QUAL_START="$(date -Is)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

[ "${PORTICO_QUAL_LIVE:-}" = "1" ] || fail "opt-in required: re-run with PORTICO_QUAL_LIVE=1"

start_artifacts
start_canary

# A fresh supervisor for the run; isolated XDG dirs keep the run hermetic.
export XDG_DATA_HOME="${QUAL_DIR}/xdg/data" XDG_CONFIG_HOME="${QUAL_DIR}/xdg/config"
export XDG_STATE_HOME="${QUAL_DIR}/xdg/state" XDG_RUNTIME_DIR="${QUAL_DIR}/xdg/run"
mkdir -p "$XDG_DATA_HOME" "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"

record "supervisor start" "$PORTICO_BIN" supervisor start

FORWARD_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"

# 1. Create closed through the CLI user path.
record "create forward closed" "$PORTICO_BIN" forward create qual-fwd \
    --local-port "$FORWARD_PORT" --remote-port "$CANARY_PORT"
CONN_ID="$("$PORTICO_BIN" list --json | python3 -c "import json,sys; rows=json.load(sys.stdin); print([r['id'] for r in rows if r['name']=='qual-fwd'][0])")"

# 2. Open, then prove the physical effect independently.
record "open" "$PORTICO_BIN" open --yes "$CONN_ID"
prove_endpoint_body "open forward" "$CANARY_URL" # placeholder for origin-only proof
FORWARD_URL="http://127.0.0.1:${FORWARD_PORT}/"
prove_endpoint_body "forward through portico" "$FORWARD_URL"

# 3. Portico must agree — but the byte proof above is the authority.
record "inspect agrees open" "$PORTICO_BIN" inspect "$CONN_ID"

# 4. Close, and prove the transport actually stops.
record "close" "$PORTICO_BIN" close --yes "$CONN_ID"
prove_port_gone "closed forward" "$FORWARD_PORT"

# 5. Reopen proves the desired state still knows how to rebuild.
record "reopen" "$PORTICO_BIN" open --yes "$CONN_ID"
prove_endpoint_body "reopened forward" "$FORWARD_URL"

# 6. Delete removes the connection; the canary origin itself is ours, not
#    Portico's, so it must survive — unowned resources are not removed.
record "delete" "$PORTICO_BIN" delete --yes "$CONN_ID"
prove_port_gone "deleted forward" "$FORWARD_PORT"
prove_canary_direct
log "unowned origin correctly survived delete"

write_manifest
log "local-forward qualification complete: ${QUAL_DIR}"
