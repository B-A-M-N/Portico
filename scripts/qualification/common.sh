#!/usr/bin/env bash
# Qualification harness shared by the provider proof scripts.
#
# The contract every script inherits (docs/QUALIFICATION.md):
#   1. A canary origin with a fixed unusual body is started first, and its
#      content is proven directly before Portico touches anything.
#   2. The Portico connection is created/opened through the normal user path
#      (CLI for scripts; the TUI is exercised by the PTY suite).
#   3. Every claim about an external effect is verified with an independent
#      client (curl), never by trusting Portico's own status output.
#   4. Cleanup is proven: after close/delete the external effect must be gone.
#
# Artifacts land in artifacts/qualification/<timestamp>/ and include a
# manifest with versions and hashes. Tokens are never recorded.

set -euo pipefail

QUAL_ROOT="${PORTICO_QUAL_ROOT:-artifacts/qualification}"
QUAL_DIR="${QUAL_ROOT}/$(date +%Y%m%d-%H%M%S)"
PORTICO_BIN="${PORTICO_BIN:-./portico}"
CANARY_PORT="${CANARY_PORT:-0}"
CANARY_PID=""
CANARY_URL=""
CANARY_BODY=""

log()  { printf '\033[1;34m[qual]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[qual:FAIL]\033[0m %s\n' "$*" >&2; exit 1; }

require_env() {
    local name="$1"
    if [ -z "${!name:-}" ]; then
        fail "$name must be set for this qualification (never put the value on the command line)"
    fi
}

require_binaries() {
    for bin in "$@"; do
        command -v "$bin" >/dev/null 2>&1 || fail "qualification requires '$bin' on PATH"
    done
}

start_artifacts() {
    mkdir -p "$QUAL_DIR"
    : > "${QUAL_DIR}/commands.log"
    log "artifacts: ${QUAL_DIR}"
}

record() {
    # record <label> <command...> — run, capture stdout/stderr/exit to artifacts.
    local label="$1"; shift
    local out
    if out=$("$@" 2>&1); then
        {
            echo "### $label (exit 0)"
            echo "\$ $*"
            echo "$out"
        } >> "${QUAL_DIR}/commands.log"
        printf '%s' "$out"
    else
        local rc=$?
        {
            echo "### $label (exit $rc)"
            echo "\$ $*"
            echo "$out"
        } >> "${QUAL_DIR}/commands.log"
        fail "$label failed (exit $rc); see ${QUAL_DIR}/commands.log"
    fi
}

record_expect_fail() {
    # record_expect_fail <label> <needle> <command...> — the command must fail
    # and its output must contain the needle.
    local label="$1" needle="$2"; shift 2
    local out rc=0
    out=$("$@" 2>&1) || rc=$?
    if [ "$rc" -eq 0 ]; then
        fail "$label unexpectedly succeeded"
    fi
    case "$out" in
        *"$needle"*) : ;;
        *) fail "$label failed but did not mention \"$needle\"; got: $out" ;;
    esac
    { echo "### $label (expected failure, exit $rc)"; echo "\$ $*"; echo "$out"; } >> "${QUAL_DIR}/commands.log"
}

start_canary() {
    CANARY_BODY="PORTICO-PROOF-$(date +%Y%m%d)-$RANDOM$RANDOM"
    cat > "${QUAL_DIR}/canary.py" <<EOF
import http.server, socketserver
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"$CANARY_BODY")
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("127.0.0.1", CANARY_PORT), H).serve_forever()
EOF
    # Port 0 lets the kernel pick; the real port is parsed from the bound socket.
    CANARY_PID=""
    python3 - <<'EOF' > "${QUAL_DIR}/canary_port"
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
EOF
    CANARY_PORT="$(cat "${QUAL_DIR}/canary_port")"
    sed -i "s/CANARY_PORT/$CANARY_PORT/" "${QUAL_DIR}/canary.py"
    setsid python3 "${QUAL_DIR}/canary.py" > "${QUAL_DIR}/canary.log" 2>&1 &
    CANARY_PID=$!
    CANARY_URL="http://127.0.0.1:${CANARY_PORT}/"
    sleep 1
    prove_canary_direct
}

prove_canary_direct() {
    local got
    got="$(curl -fsS --max-time 5 "$CANARY_URL")"
    [ "$got" = "$CANARY_BODY" ] || fail "canary origin does not serve its body directly: $got"
    { echo "### canary direct proof"; echo "$got"; } >> "${QUAL_DIR}/commands.log"
    log "canary origin proven directly at ${CANARY_URL}"
}

prove_endpoint_body() {
    # The independent effect: the endpoint must return the exact canary body.
    local label="$1" url="$2" tries="${3:-30}"
    local got rc
    for _ in $(seq 1 "$tries"); do
        if got="$(curl -fsS --max-time 10 "$url" 2>/dev/null)"; then
            if [ "$got" = "$CANARY_BODY" ]; then
                { echo "### $label — endpoint serves the canary body"; echo "$url"; echo "$got"; } >> "${QUAL_DIR}/commands.log"
                log "PASS: $label serves the canary body"
                return 0
            fi
        fi
        sleep 1
    done
    {
        echo "### $label — FAILED to serve the canary body"
        echo "url: $url"
        echo "last body: ${got:-<no response>}"
    } >> "${QUAL_DIR}/commands.log"
    fail "$label did not serve the canary body (effect not proven)"
}

prove_endpoint_gone() {
    # After close/delete, the endpoint must stop serving. This is the cleanup
    # half of the lifecycle contract.
    local label="$1" url="$2" tries="${3:-20}"
    for _ in $(seq 1 "$tries"); do
        if ! curl -fsS --max-time 5 "$url" >/dev/null 2>&1; then
            { echo "### $label — endpoint unreachable (cleanup proven)"; echo "$url"; } >> "${QUAL_DIR}/commands.log"
            log "PASS: $label is no longer reachable"
            return 0
        fi
        sleep 1
    done
    { echo "### $label — STILL REACHABLE after cleanup"; echo "$url"; } >> "${QUAL_DIR}/commands.log"
    fail "$label remained reachable after close/delete"
}

prove_port_gone() {
    local label="$1" port="$2" tries="${3:-20}"
    for _ in $(seq 1 "$tries"); do
        if ! curl -fsS --max-time 2 "http://127.0.0.1:${port}/" >/dev/null 2>&1; then
            log "PASS: $label listening socket is gone"
            return 0
        fi
        sleep 1
    done
    fail "$label still accepts connections after cleanup"
}

write_manifest() {
    local extra="${1:-}"
    {
        echo "git_commit:      $(git rev-parse HEAD 2>/dev/null || echo unknown)"
        echo "portico_version: $("$PORTICO_BIN" version 2>/dev/null || echo unknown)"
        echo "binary_sha256:   $(sha256sum "$PORTICO_BIN" | cut -d' ' -f1)"
        echo "go_version:      $(go version 2>/dev/null | awk '{print $3}')"
        echo "kernel:          $(uname -sr)"
        echo "cloudflared:     $(cloudflared --version 2>/dev/null || echo 'not installed')"
        echo "tailscale:       $(tailscale version 2>/dev/null | head -1 || echo 'not installed')"
        echo "ngrok:           $(ngrok version 2>/dev/null || echo 'not installed')"
        echo "canary_port:     ${CANARY_PORT}"
        echo "start:           ${QUAL_START:-unknown}"
        echo "end:             $(date -Is)"
        [ -n "$extra" ] && echo "$extra"
    } > "${QUAL_DIR}/manifest.txt"
    # The manifest records tool versions and non-secret identifiers only.
    if grep -qiE 'token|secret|password|api[_-]?key' "${QUAL_DIR}/manifest.txt"; then
        fail "manifest may contain a secret — refusing to continue"
    fi
}

stop_canary() {
    [ -n "$CANARY_PID" ] && kill "$CANARY_PID" 2>/dev/null
}

on_exit() {
    stop_canary
}
trap on_exit EXIT
