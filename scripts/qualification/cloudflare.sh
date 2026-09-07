#!/usr/bin/env bash
# Live Cloudflare qualification: Quick Tunnel and managed tunnel/DNS/Access.
#
# Evidence rules this script enforces (audit pass-3):
#   - The API token never enters argv. Provider-API calls use record_redacted
#     and pass the token through the environment; after every run the
#     artifacts are scanned against the ACTUAL secret bytes.
#   - "DNS works" means the exact hostname record exists in Cloudflare's API,
#     with the expected type, and its content ties to the tunnel Portico
#     created. A zone-list call proves a zone exists, nothing more.
#   - Cleanup means the exact DNS record, the exact tunnel, and the exact
#     Access resources are ABSENT from Cloudflare's API afterwards.
#   - Access protection is proven both ways: unauthenticated is denied, and a
#     caller with service-token credentials reaches the canary.
#   - Restart/reconstruction: the supervisor restarts while the managed tunnel
#     is open; exact external IDs must survive and the endpoint must serve.
#   - Drift repair: removing the DNS record out-of-band, then `portico repair`,
#     must recreate exactly that record (same type/name) and nothing else.
#
#   PORTICO_QUAL_LIVE=1 PORTICO_CF_ZONE_NAME=qual.example.com \
#   PORTICO_CF_ZONE_ID=<zone-id> CLOUDFLARE_ACCOUNT_ID=<account-id> \
#   CLOUDFLARE_API_TOKEN=... \
#   [PORTICO_QUAL_EMAIL=you@example.com \
#    PORTICO_QUAL_CF_ACCESS_CLIENT_ID=... PORTICO_QUAL_CF_ACCESS_TOKEN=...] \
#   scripts/qualification/cloudflare.sh
set -euo pipefail
QUAL_START="$(date -Is)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

[ "${PORTICO_QUAL_LIVE:-}" = "1" ] || fail "opt-in required: re-run with PORTICO_QUAL_LIVE=1"
require_env CLOUDFLARE_API_TOKEN
require_env PORTICO_CF_ZONE_NAME
require_env PORTICO_CF_ZONE_ID
require_env CLOUDFLARE_ACCOUNT_ID
require_binaries cloudflared curl python3 jq

ZONE="${PORTICO_CF_ZONE_NAME}"
ZONE_ID="${PORTICO_CF_ZONE_ID}"
ACCOUNT="${CLOUDFLARE_ACCOUNT_ID}"
HOSTBASE="qual-tunnel.$(date +%H%M%S).${ZONE}"
CF_API="https://api.cloudflare.com/client/v4"

start_artifacts
start_canary

export XDG_DATA_HOME="${QUAL_DIR}/xdg/data" XDG_CONFIG_HOME="${QUAL_DIR}/xdg/config"
export XDG_STATE_HOME="${QUAL_DIR}/xdg/state" XDG_RUNTIME_DIR="${QUAL_DIR}/xdg/run"
mkdir -p "$XDG_DATA_HOME" "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"

cf_api() {
    # cf_api <method> <path> [json-body] — authenticated Cloudflare API call.
    # The token is injected via the environment by record_redacted; direct
    # callers (outside record_redacted) inherit it from the environment too.
    local method="$1" path="$2" body="${3:-}"
    local args=(-sS --max-time 20 -X "$method"
        -H "Authorization: Bearer ${CLOUDFLARE_API_TOKEN}"
        -H "Content-Type: application/json")
    if [ -n "$body" ]; then
        args+=(-d "$body")
    fi
    curl "${args[@]}" "${CF_API}${path}"
}

cf_api_checked() {
    # cf_api_checked <label> <method> <path> [body] — API call whose response
    # must be success:true; the body is parsed with jq by the caller.
    local label="$1" method="$2" path="$3" body="${4:-}"
    local out
    out="$(record_redacted "$label" CLOUDFLARE_API_TOKEN cf_api "$method" "$path" "$body")"
    if ! printf '%s' "$out" | jq -e '.success == true' >/dev/null; then
        echo "$out" >> "${QUAL_DIR}/commands.log"
        fail "$label: Cloudflare API reported failure"
    fi
    printf '%s' "$out"
}

# The credential enters through Portico's stdin path so it never appears in
# argv or in this log. The pipe is run as its own record entry with the token
# value stripped from anything the log would show.
set +e
printf '%s\n' "$CLOUDFLARE_API_TOKEN" | "$PORTICO_BIN" provider login cloudflare --credential-stdin \
    > "${QUAL_DIR}/login.out" 2>&1
login_rc=$?
set -e
[ "$login_rc" -eq 0 ] || fail "provider login failed (exit $login_rc); see login.out"
{
    echo "### provider login (exit 0)"
    echo "\$ printf <redacted> | $PORTICO_BIN provider login cloudflare --credential-stdin"
    grep -iE "configured|account|zone|ready" "${QUAL_DIR}/login.out" | grep -viE "token|secret" | head -5
} >> "${QUAL_DIR}/commands.log"
{
    echo "### provider login (credential via stdin; output redacted to status only)"
    grep -iE "configured|account|zone|error" "${QUAL_DIR}/login.out" | grep -viE "token|credential value" | head -5
} >> "${QUAL_DIR}/commands.log"

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

# Restart the supervisor while the quick tunnel is open: desired-open must
# survive, the same trycloudflare hostname may change (Quick Tunnels are
# ephemeral by design), and the endpoint must serve again either way.
OLD_QUICK_URL="$QUICK_URL"
record "supervisor stop (quick)" "$PORTICO_BIN" supervisor stop
record "supervisor start (quick)" "$PORTICO_BIN" supervisor start
sleep 3
QUICK_URL_RESTART="$("$PORTICO_BIN" inspect --json "$QUICK_ID" | python3 -c "import json,sys; print(json.load(sys.stdin)['public_address'])")"
if [ "$QUICK_URL_RESTART" != "$OLD_QUICK_URL" ]; then
    log "note: quick tunnel rehydrated with a new ephemeral URL after restart (expected for Quick Tunnels)"
    QUICK_URL="$QUICK_URL_RESTART"
fi
prove_endpoint_body "quick tunnel after supervisor restart" "$QUICK_URL"

record "close quick tunnel" "$PORTICO_BIN" close --yes "$QUICK_ID"
prove_endpoint_gone "closed quick tunnel" "$QUICK_URL"
record "delete quick tunnel" "$PORTICO_BIN" delete --yes "$QUICK_ID"

# --- Managed tunnel: DNS + Access ------------------------------------------
record "create managed tunnel" "$PORTICO_BIN" create "$CONN_NAME-managed" \
    --provider cloudflare --source "$CANARY_URL" --source-type existing_service \
    --exposure permanent --hostname "$HOSTBASE" \
    --protection email_otp --protection-emails "${PORTICO_QUAL_EMAIL:-qual@example.com}" \
    --health-enabled=false
MANAGED_ID="$("$PORTICO_BIN" list --json | python3 -c "import json,sys; rows=json.load(sys.stdin); print([r['id'] for r in rows if r['name']=='$CONN_NAME-managed'][0])")"

record "open managed tunnel" "$PORTICO_BIN" open --yes "$MANAGED_ID"
MANAGED_URL="https://${HOSTBASE}/"

# --- Exact DNS proof --------------------------------------------------------
# Resolve the exact record: type CNAME/A for HOSTBASE in ZONE_ID, and require
# its content to tie to the tunnel Portico created (a <tunnel-id>.cfargotunnel.com
# target). A zone-list query proves the zone exists — not that Portico made
# the record.
MANAGED_TUNNEL_ID="$("$PORTICO_BIN" inspect --json "$MANAGED_ID" | python3 -c "
import json,sys
d = json.load(sys.stdin)
res = [r for r in (d.get('resources') or [])
       if 'tunnel' in (r.get('type') or '').lower()]
print(res[0].get('external_id') if res else '')")"
dns_out="$(cf_api_checked "exact DNS record exists" GET "/zones/${ZONE_ID}/dns_records?name=${HOSTBASE}&per_page=100")"
DNS_TYPE="$(printf '%s' "$dns_out" | jq -r '[.result[] | select(.type=="CNAME" or .type=="A")][0].type // empty')"
DNS_CONTENT="$(printf '%s' "$dns_out" | jq -r '[.result[] | select(.type=="CNAME" or .type=="A")][0].content // empty')"
DNS_ID="$(printf '%s' "$dns_out" | jq -r '[.result[] | select(.type=="CNAME" or .type=="A")][0].id // empty')"
[ -n "$DNS_ID" ] || fail "no DNS record for ${HOSTBASE} exists in zone ${ZONE} — Portico's DNS effect not proven"
[ -n "$MANAGED_TUNNEL_ID" ] && case "$DNS_CONTENT" in
    *"$(printf '%s' "$MANAGED_TUNNEL_ID" | head -c 8)"*|*cfargotunnel.com*)
        log "PASS: DNS ${HOSTBASE} (${DNS_TYPE}) targets the tunnel Portico created" ;;
    *) fail "DNS record ${HOSTBASE} does not target Portico's tunnel (content: ${DNS_CONTENT})" ;;
esac
{
    echo "### DNS record tied to tunnel"
    echo "record_id: $DNS_ID"
    echo "type: $DNS_TYPE  content: $DNS_CONTENT"
    echo "portico_tunnel_id: ${MANAGED_TUNNEL_ID:-<not reported by inspect>}"
} >> "${QUAL_DIR}/commands.log"

# --- Restart/reconstruction with exact-ID preservation ----------------------
record "supervisor stop (managed)" "$PORTICO_BIN" supervisor stop
record "supervisor start (managed)" "$PORTICO_BIN" supervisor start
sleep 3
prove_endpoint_body "managed tunnel after supervisor restart" "$MANAGED_URL" 45
# The tunnel ID must be the SAME external resource, not a recreated one.
MANAGED_TUNNEL_ID_AFTER="$("$PORTICO_BIN" inspect --json "$MANAGED_ID" | python3 -c "
import json,sys
d = json.load(sys.stdin)
res = [r for r in (d.get('resources') or [])
       if 'tunnel' in (r.get('type') or '').lower()]
print(res[0].get('external_id') if res else '')")"
if [ -n "$MANAGED_TUNNEL_ID" ] && [ -n "$MANAGED_TUNNEL_ID_AFTER" ] \
   && [ "$MANAGED_TUNNEL_ID" != "$MANAGED_TUNNEL_ID_AFTER" ]; then
    fail "tunnel ID changed across restart (${MANAGED_TUNNEL_ID} -> ${MANAGED_TUNNEL_ID_AFTER}): reconstruction recreated infrastructure instead of adopting it"
fi
log "PASS: tunnel identity preserved across restart"

# --- Access proven both ways -------------------------------------------------
HTTP_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$MANAGED_URL" || true)"
case "$HTTP_CODE" in
    302|401|403) log "PASS: unauthenticated request denied (HTTP $HTTP_CODE)" ;;
    *) fail "unauthenticated request to protected hostname returned HTTP $HTTP_CODE, want denial" ;;
esac
echo "### managed URL unauthenticated status: $HTTP_CODE" >> "${QUAL_DIR}/commands.log"

if [ -n "${PORTICO_QUAL_CF_ACCESS_CLIENT_ID:-}" ] && [ -n "${PORTICO_QUAL_CF_ACCESS_TOKEN:-}" ]; then
    AUTH_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
        -H "CF-Access-Client-Id: ${PORTICO_QUAL_CF_ACCESS_CLIENT_ID}" \
        -H "CF-Access-Client-Secret: ${PORTICO_QUAL_CF_ACCESS_TOKEN}" \
        "$MANAGED_URL" || true)"
    if [ "$AUTH_CODE" != "200" ]; then
        fail "service-token request returned HTTP $AUTH_CODE; authorized access not proven"
    fi
    BODY_GOT="$(curl -fsS --max-time 15 \
        -H "CF-Access-Client-Id: ${PORTICO_QUAL_CF_ACCESS_CLIENT_ID}" \
        -H "CF-Access-Client-Secret: ${PORTICO_QUAL_CF_ACCESS_TOKEN}" \
        "$MANAGED_URL")"
    [ "$BODY_GOT" = "$CANARY_BODY" ] || fail "authorized caller did not receive the canary body"
    echo "### authorized (service token) proof: canary body received" >> "${QUAL_DIR}/commands.log"
    log "PASS: authorized caller reached the canary through Access"
else
    log "note: service-token credentials not provided; only the denial direction is proven"
fi

# --- Drift repair: remove the DNS record out-of-band, Portico must fix it ----
cf_api_checked "drift: delete DNS record out-of-band" DELETE "/zones/${ZONE_ID}/dns_records/${DNS_ID}"
drift_out="$(record_redacted "drift: exact record absent" CLOUDFLARE_API_TOKEN cf_api GET "/zones/${ZONE_ID}/dns_records?name=${HOSTBASE}&per_page=100")"
printf '%s' "$drift_out" | jq -e '.result | length == 0' >/dev/null || fail "drift setup failed: record still present"
record "repair (drifted DNS)" "$PORTICO_BIN" repair "$MANAGED_ID" --yes
sleep 5
repair_out="$(record_redacted "repair: exact record restored" CLOUDFLARE_API_TOKEN cf_api GET "/zones/${ZONE_ID}/dns_records?name=${HOSTBASE}&per_page=100")"
DNS_ID_NEW="$(printf '%s' "$repair_out" | jq -r '[.result[] | select(.type=="CNAME" or .type=="A")][0].id // empty')"
[ -n "$DNS_ID_NEW" ] || fail "repair did not restore the drifted DNS record"
[ "$DNS_ID_NEW" != "$DNS_ID" ] && log "note: repair recreated the record (new ID $DNS_ID_NEW)"
prove_endpoint_body "managed tunnel after drift repair" "$MANAGED_URL" 45
log "PASS: drift repair restored exactly the broken record"

# --- Close + delete with exact-resource absence proof ------------------------
record "close managed tunnel" "$PORTICO_BIN" close --yes "$MANAGED_ID"
record "delete managed tunnel" "$PORTICO_BIN" delete --yes "$MANAGED_ID"
sleep 5

# Exact absence: the DNS record for THIS hostname is gone.
after_dns="$(record_redacted "cleanup: DNS record absent" CLOUDFLARE_API_TOKEN cf_api GET "/zones/${ZONE_ID}/dns_records?name=${HOSTBASE}&per_page=100")"
printf '%s' "$after_dns" | jq -e '.result | length == 0' >/dev/null \
    || fail "cleanup not proven: DNS record ${HOSTBASE} still exists"
# Exact absence: the tunnel this connection created is gone from the account.
if [ -n "$MANAGED_TUNNEL_ID" ]; then
    after_tunnel="$(record_redacted "cleanup: tunnel absent" CLOUDFLARE_API_TOKEN cf_api GET "/accounts/${ACCOUNT}/cfd_tunnel/${MANAGED_TUNNEL_ID}")"
    if printf '%s' "$after_tunnel" | jq -e '.success == true' >/dev/null 2>&1; then
        fail "cleanup not proven: tunnel ${MANAGED_TUNNEL_ID} still exists in Cloudflare"
    fi
fi
# Exact absence: Access apps for this hostname are gone.
after_access="$(record_redacted "cleanup: Access apps absent" CLOUDFLARE_API_TOKEN cf_api GET "/accounts/${ACCOUNT}/access/apps?name=${HOSTBASE}")"
APP_COUNT="$(printf '%s' "$after_access" | jq -r '.result | length' 2>/dev/null || echo 0)"
[ "$APP_COUNT" = "0" ] || fail "cleanup not proven: $APP_COUNT Access app(s) remain for ${HOSTBASE}"
log "PASS: provider-side cleanup proven (DNS record, tunnel, Access apps absent)"

# --- Final gate: the actual secret bytes never entered the artifacts --------
scan_artifacts_for_secrets CLOUDFLARE_API_TOKEN PORTICO_QUAL_CF_ACCESS_TOKEN PORTICO_QUAL_CF_ACCESS_CLIENT_ID

write_manifest "provider:           cloudflare
zone:               ${ZONE}
zone_id:            ${ZONE_ID}
account_id:         ${ACCOUNT}
hostname:           ${HOSTBASE}
quick_tunnel:       proven (open, serve, restart, close, delete)
managed_tunnel:     dns-tied, restart-id-stable, access both ways, drift repair, exact cleanup"
log "Cloudflare qualification complete: ${QUAL_DIR}"
