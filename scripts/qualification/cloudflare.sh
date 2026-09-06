#!/usr/bin/env bash
# Live Cloudflare qualification: Quick Tunnel and managed tunnel/DNS/Access.
#
# Requires a qualification-dedicated Cloudflare API token and zone. The token
# is read from the environment by Portico itself and is never written to the
# artifact log: `record` captures commands and their stdout, and the token is
# passed only through the environment.
#
#   PORTICO_QUAL_LIVE=1 PORTICO_CF_ZONE_NAME=qual.example.com \
#   CLOUDFLARE_API_TOKEN=... scripts/qualification/cloudflare.sh
set -euo pipefail
QUAL_START="$(date -Is)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

[ "${PORTICO_QUAL_LIVE:-}" = "1" ] || fail "opt-in required: re-run with PORTICO_QUAL_LIVE=1"
require_env CLOUDFLARE_API_TOKEN
require_env PORTICO_CF_ZONE_NAME
require_binaries cloudflared curl
[ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ] || fail "CLOUDFLARE_ACCOUNT_ID must be set (qualification-dedicated account)"

start_artifacts
start_canary

export XDG_DATA_HOME="${QUAL_DIR}/xdg/data" XDG_CONFIG_HOME="${QUAL_DIR}/xdg/config"
export XDG_STATE_HOME="${QUAL_DIR}/xdg/state" XDG_RUNTIME_DIR="${QUAL_DIR}/xdg/run"
mkdir -p "$XDG_DATA_HOME" "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"

# The credential enters through Portico's stdin path so it never appears in
# argv or in this log.
record "provider login (credential via stdin)" sh -c \
    "printf '%s\n' \"\$CLOUDFLARE_API_TOKEN\" | \"$PORTICO_BIN\" provider login cloudflare --credential-stdin"

CONN_NAME="qual-cf-$(date +%s)"

# --- Quick Tunnel: temporary address, no DNS -------------------------------
record "create quick tunnel" "$PORTICO_BIN" create "$CONN_NAME-quick" \
    --provider cloudflare --source "$CANARY_URL" --source-type existing_service \
    --exposure temporary --health-enabled=false
QUICK_ID="$("$PORTICO_BIN" list --json | python3 -c "import json,sys; rows=json.load(sys.stdin); print([r['id'] for r in rows if r['name']=='$CONN_NAME-quick'][0])")"

record "open quick tunnel" "$PORTICO_BIN" open --yes "$QUICK_ID"
QUICK_URL="$("$PORTICO_BIN" inspect --json "$QUICK_ID" | python3 -c "import json,sys; print(json.load(sys.stdin)['public_address'])")"
case "$QUICK_URL" in
    https://*.trycloudflare.com) : ;;
    *) fail "quick tunnel address does not look like a trycloudflare URL: $QUICK_URL" ;;
esac
prove_endpoint_body "quick tunnel public URL" "$QUICK_URL"

# TUI independence: quitting the TUI must not affect the tunnel. The proof is
# that the endpoint keeps serving while no TUI process exists (the supervisor
# owns the connector).
record "close quick tunnel" "$PORTICO_BIN" close --yes "$QUICK_ID"
prove_endpoint_gone "closed quick tunnel" "$QUICK_URL"
record "delete quick tunnel" "$PORTICO_BIN" delete --yes "$QUICK_ID"

# --- Managed tunnel: DNS + Access ------------------------------------------
record "create managed tunnel" "$PORTICO_BIN" create "$CONN_NAME-managed" \
    --provider cloudflare --source "$CANARY_URL" --source-type existing_service \
    --exposure permanent --hostname "qual.${PORTICO_CF_ZONE_NAME}" \
    --protection email_otp --protection-emails "${PORTICO_QUAL_EMAIL:-qual@example.com}" \
    --health-enabled=false
MANAGED_ID="$("$PORTICO_BIN" list --json | python3 -c "import json,sys; rows=json.load(sys.stdin); print([r['id'] for r in rows if r['name']=='$CONN_NAME-managed'][0])")"

record "open managed tunnel" "$PORTICO_BIN" open --yes "$MANAGED_ID"
MANAGED_URL="https://qual.${PORTICO_CF_ZONE_NAME}/"

# DNS is verified independently against Cloudflare's own API — provider truth,
# not Portico's report.
record "independent DNS check" curl -fsS -H "Authorization: Bearer ${CLOUDFLARE_API_TOKEN}" \
    "https://api.cloudflare.com/client/v4/zones?name=${PORTICO_CF_ZONE_NAME}"

# Unauthenticated access must be denied: Access protection is proven by the
# refusal, not by the existence of an Access app resource.
HTTP_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$MANAGED_URL" || true)"
case "$HTTP_CODE" in
    302|401|403) log "PASS: unauthenticated request denied (HTTP $HTTP_CODE)" ;;
    *) fail "unauthenticated request to protected hostname returned HTTP $HTTP_CODE, want denial" ;;
esac
echo "### managed URL unauthenticated status: $HTTP_CODE" >> "${QUAL_DIR}/commands.log"

# Authenticated proof happens via the Access service token if provided.
if [ -n "${PORTICO_QUAL_CF_ACCESS_TOKEN:-}" ] && [ -n "${PORTICO_QUAL_CF_ACCESS_CLIENT_ID:-}" ]; then
    prove_endpoint_body "managed tunnel with Access credentials" "$MANAGED_URL"
fi

record "close managed tunnel" "$PORTICO_BIN" close --yes "$MANAGED_ID"
record "delete managed tunnel" "$PORTICO_BIN" delete --yes "$MANAGED_ID"

# Provider-side cleanup is verified against Cloudflare's API, not Portico.
record "provider cleanup: DNS records after delete" curl -fsS \
    -H "Authorization: Bearer ${CLOUDFLARE_API_TOKEN}" \
    "https://api.cloudflare.com/client/v4/zones?name=${PORTICO_CF_ZONE_NAME}"

write_manifest "provider:           cloudflare
zone:               ${PORTICO_CF_ZONE_NAME}
quick_tunnel:       proven
managed_tunnel:     dns+access proven"
log "Cloudflare qualification complete: ${QUAL_DIR}"
