#!/usr/bin/env bash
# Clean-user smoke: the packaged artifact, a pristine environment, the real
# installer, and a full local-forward lifecycle — the release candidate's
# first-minute experience.
#
# Everything this script proves is user-visible behavior, not internals:
#   1. install.sh succeeds into an empty HOME and puts a working binary on PATH
#   2. portico --version works with zero state
#   3. doctor is read-only and honest on a clean box
#   4. the supervisor bootstraps automatically on first use
#   5. a local forward moves real bytes through a created-and-opened connection
#   6. desired state survives a supervisor restart
#   7. support export works against live state
#   8. delete removes the connection and the listener stops
#
# Usage: scripts/clean_user_smoke.sh path/to/portico.tar.gz
set -euo pipefail

TARBALL="${1:?usage: clean_user_smoke.sh <portico.tar.gz>}"
[ -f "$TARBALL" ] || { echo "no such tarball: $TARBALL" >&2; exit 1; }

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TARBALL_ABS="$(cd "$(dirname "$TARBALL")" && pwd)/$(basename "$TARBALL")"
SMOKE_CHECKSUMS="$(dirname "$TARBALL_ABS")/portico-clean-user-checksums.$$"
(cd "$(dirname "$TARBALL_ABS")" && sha256sum "$(basename "$TARBALL_ABS")") > "$SMOKE_CHECKSUMS"

SMOKE_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/portico-clean-user.XXXXXX")
SMOKE_HOME="$SMOKE_ROOT/home"
SMOKE_BIN="$SMOKE_HOME/.local/bin"
# The runtime directory must be per-run. The default /tmp/portico-$UID is
# shared by every process of this user, so an unisolated smoke run would talk
# to whatever supervisor a previous run left behind — reporting another
# installation's state as this one's.
SMOKE_RUNTIME="$SMOKE_ROOT/runtime"
SMOKE_SUPPORT="$SMOKE_ROOT/support.json"
SMOKE_LOG="$SMOKE_ROOT/doctor.log"
mkdir -p "$SMOKE_HOME" "$SMOKE_BIN" "$SMOKE_RUNTIME"
chmod 0700 "$SMOKE_RUNTIME"

ORIGIN_PID=""
supervisor_pid=""

cleanup() {
  # Best-effort supervisor stop through the installed binary and the smoke
  # environment: a failed run must not leave a daemon whose HOME is about to
  # be deleted — that daemon would answer the next run's doctor with state
  # from a ghost installation.
  if [ -x "$SMOKE_BIN/portico" ]; then
    env -i HOME="$SMOKE_HOME" PATH="$SMOKE_BIN:/usr/bin:/bin" TERM=dumb \
      XDG_RUNTIME_DIR="$SMOKE_RUNTIME" \
      "$SMOKE_BIN/portico" supervisor stop >/dev/null 2>&1 || true
  fi
  [ -n "$ORIGIN_PID" ] && kill "$ORIGIN_PID" 2>/dev/null || true
  rm -rf "$SMOKE_ROOT"
  rm -f "$SMOKE_CHECKSUMS"
}
trap cleanup EXIT

# The smoke user's complete environment: a pristine HOME, an isolated runtime
# dir, the installed binary's directory, and nothing else — no repo paths, no
# developer state, no developer env vars.
run_as_user() {
  env -i HOME="$SMOKE_HOME" USER="portico-smoke" \
      PATH="$SMOKE_BIN:/usr/local/bin:/usr/bin:/bin" \
      XDG_RUNTIME_DIR="$SMOKE_RUNTIME" \
      TERM=dumb "$@"
}

echo "== install via install.sh (the real path) =="
# The installer is the product: the smoke test must not re-implement
# installation semantics that can drift from it. Env is inlined because
# run_as_user starts from an empty environment by design.
env -i HOME="$SMOKE_HOME" USER="portico-smoke" \
    PATH="$SMOKE_BIN:/usr/local/bin:/usr/bin:/bin" \
    XDG_RUNTIME_DIR="$SMOKE_RUNTIME" TERM=dumb \
    PORTICO_VERSION=v0.0.0-smoke \
    PORTICO_RELEASE_ARCHIVE="$TARBALL_ABS" \
    PORTICO_RELEASE_CHECKSUMS="$SMOKE_CHECKSUMS" \
    PORTICO_RELEASE_TAG=v0.0.0-smoke \
    PORTICO_RELEASE_ALLOW_UNSIGNED_LOCAL=1 \
    INSTALL_DIR="$SMOKE_BIN" \
    sh "$repo_root/install.sh"
[ -x "$SMOKE_BIN/portico" ] || { echo "install.sh did not produce an executable" >&2; exit 1; }

echo "== portico --version with zero state =="
run_as_user portico --version

echo "== doctor is read-only before anything has started =="
# Doctor never starts a supervisor: it is read-only by contract. On a clean
# box with nothing running it must fail with the honest "not running" outcome
# (exit 8 = supervisor unavailable) rather than silently succeeding.
if run_as_user portico doctor >"$SMOKE_LOG" 2>&1; then
  echo "doctor succeeded with no supervisor; it should report the gap" >&2
  cat "$SMOKE_LOG" >&2
  exit 1
else
  code=$?
  [ "$code" = "8" ] || { echo "doctor exited $code without a supervisor (expected 8):" >&2; cat "$SMOKE_LOG" >&2; exit 1; }
fi
grep -q "not running" "$SMOKE_LOG" || { echo "doctor did not name the missing supervisor:" >&2; cat "$SMOKE_LOG" >&2; exit 1; }

echo "== automatic supervisor bootstrap + status =="
# First ordinary use (any client command) bootstraps the supervisor into the
# smoke HOME. `list` is the cheapest honest trigger: supervisor status
# deliberately does not start one.
run_as_user portico list >/dev/null
run_as_user portico supervisor status | grep -qi running \
  || { echo "supervisor did not bootstrap" >&2; exit 1; }

# With the supervisor up, a clean installation has no blockers: everything
# else doctor reports is attention, not failure.
if ! run_as_user portico doctor >"$SMOKE_LOG" 2>&1; then
  echo "doctor failed on a running clean installation:" >&2
  cat "$SMOKE_LOG" >&2
  exit 1
fi
if grep -qE "[1-9][0-9]* blocker\(s\) prevent" "$SMOKE_LOG"; then
  echo "doctor reported blockers on a running clean installation:" >&2
  cat "$SMOKE_LOG" >&2
  exit 1
fi

echo "== local forward: create, open, move real bytes, verify endpoint =="
origin_port=$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)
# Canary origin bound to loopback only, serving a fixed canary body.
python3 - "$origin_port" >"$SMOKE_ROOT/origin.log" 2>&1 <<'PY' &
import http.server, sys

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"CLEAN-USER-SMOKE-CANARY"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
ORIGIN_PID=$!
sleep 1
# The origin must be directly reachable before Portico is involved at all.
body=$(python3 -c "
import urllib.request
print(urllib.request.urlopen('http://127.0.0.1:$origin_port/', timeout=3).read().decode())
")
[ "$body" = "CLEAN-USER-SMOKE-CANARY" ] || { echo "origin canary mismatch before any Portico involvement: $body" >&2; exit 1; }

FORWARD_PORT=$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)
conn_json=$(run_as_user portico forward create smoke-forward \
  --local-port "$FORWARD_PORT" --remote-host 127.0.0.1 --remote-port "$origin_port" --json)
conn_id=$(printf '%s' "$conn_json" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# Opening is its own step: the preview-then-apply boundary is part of the
# product, and the smoke test drives it the way a user does.
run_as_user portico open "$conn_id" --yes

# Byte transport through the forward is the smoke test's physical assertion.
fetch_canary() {
  python3 -c "
import urllib.request
print(urllib.request.urlopen('http://127.0.0.1:$FORWARD_PORT/', timeout=3).read().decode())
" 2>/dev/null
}
ok=""
for attempt in 1 2 3 4 5 6 7 8; do
  if body=$(fetch_canary) && [ "$body" = "CLEAN-USER-SMOKE-CANARY" ]; then
    ok=1; break
  fi
  sleep 1
done
[ -n "$ok" ] || { echo "forward never carried the canary" >&2; exit 1; }
echo "byte transport verified through 127.0.0.1:$FORWARD_PORT"

echo "== desired state survives a supervisor restart =="
run_as_user portico supervisor stop
run_as_user portico supervisor start
ok=""
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12; do
  if body=$(fetch_canary) && [ "$body" = "CLEAN-USER-SMOKE-CANARY" ]; then
    ok=1; break
  fi
  sleep 1
done
[ -n "$ok" ] || { echo "transport did not recover within 12s of the restart" >&2; exit 1; }
state=$(run_as_user portico inspect "$conn_id" --json | python3 -c "import sys,json; print(json.load(sys.stdin).get('desired_state',''))")
[ "$state" = "open" ] || { echo "connection did not reconcile to open after restart (desired_state=$state)" >&2; exit 1; }
echo "transport recovered after restart; desired_state=open"

echo "== support export against live state =="
run_as_user portico support export --output "$SMOKE_SUPPORT" --force
python3 -c "import json; json.load(open('$SMOKE_SUPPORT'))" \
  || { echo "support export is not valid JSON" >&2; exit 1; }

echo "== delete removes the connection and the listener stops =="
run_as_user portico delete "$conn_id" --yes
ok=""
for attempt in 1 2 3 4 5; do
  if python3 -c "
import socket
s = socket.create_connection(('127.0.0.1', $FORWARD_PORT), timeout=2)
s.close()
" 2>/dev/null; then
    sleep 1
  else
    ok=1; break
  fi
done
[ -n "$ok" ] || { echo "listener still accepting connections after delete" >&2; exit 1; }

run_as_user portico supervisor stop

echo
echo "Clean-user smoke PASSED."
