#!/bin/sh
# Verify the artifact users install, rather than only the source tree.
set -eu

archive=${1:?usage: verify_release_artifact.sh PATH_TO_ARCHIVE}
case "$archive" in
  *.tar.gz) ;;
  *) echo "unsupported release artifact: $archive" >&2; exit 2 ;;
esac

artifact_tmp=$(mktemp -d)
supervisor_pid=""
echo_pid=""
binary=""
cleanup() {
  if [ -n "$supervisor_pid" ] && kill -0 "$supervisor_pid" 2>/dev/null; then
    "$binary" supervisor stop >/dev/null 2>&1 || kill "$supervisor_pid" 2>/dev/null || true
    wait "$supervisor_pid" 2>/dev/null || true
  fi
  if [ -n "$echo_pid" ] && kill -0 "$echo_pid" 2>/dev/null; then
    kill "$echo_pid" 2>/dev/null || true
    wait "$echo_pid" 2>/dev/null || true
  fi
  rm -rf "$artifact_tmp"
}
trap cleanup EXIT INT TERM

start_supervisor() {
  run_binary=$1
  run_log=$2
  "$run_binary" supervisor run >"$run_log" 2>&1 &
  supervisor_pid=$!
  ready=0
  i=0
  while [ "$i" -lt 100 ]; do
    status_output=$("$run_binary" supervisor status 2>&1 || true)
    if [ "$status_output" = "Supervisor: running" ]; then
      ready=1
      break
    fi
    i=$((i + 1))
    sleep 0.1
  done
  if [ "$ready" -ne 1 ]; then
    echo "packaged supervisor did not become ready" >&2
    cat "$run_log" >&2 || true
    exit 1
  fi
}

members="$(tar -tzf "$archive")"
if [ "$(printf '%s\n' "$members" | awk 'NF { count++ } END { print count + 0 }')" -ne 1 ] ||
   [ "$members" != "portico" ]; then
  echo "release archive must contain only portico at its root" >&2
  printf '%s\n' "$members" >&2
  exit 1
fi
if ! tar -tvzf "$archive" | awk '
  BEGIN { count = 0; valid = 1 }
  { count++; if ($1 !~ /^-/ || $NF != "portico") valid = 0 }
  END { exit !(valid && count == 1) }
'; then
  echo "release archive contains a non-regular or unexpected member" >&2
  exit 1
fi
tar -xzf "$archive" --no-same-owner --no-same-permissions -C "$artifact_tmp"

binary="$artifact_tmp/portico"
if [ ! -x "$binary" ]; then
  echo "release archive does not contain an executable portico binary" >&2
  exit 1
fi

version_output=$("$binary" version)
case "$version_output" in
  *"version:"*|*"Version:"*|*Portico*|*portico*) ;;
  *) echo "artifact version command returned unexpected output: $version_output" >&2; exit 1 ;;
esac

# Exercise the installed binary in an isolated account. This catches binaries
# that only work from the source checkout, wrong XDG path assumptions, and
# supervisor startup regressions that a version-only check cannot see.
isolated_home="$artifact_tmp/home"
isolated_runtime="$artifact_tmp/runtime"
isolated_data="$artifact_tmp/data"
isolated_config="$artifact_tmp/config"
isolated_state="$artifact_tmp/state"
mkdir -p "$isolated_home" "$isolated_runtime" "$isolated_data" "$isolated_config" "$isolated_state"
export HOME="$isolated_home"
export XDG_RUNTIME_DIR="$isolated_runtime"
export XDG_DATA_HOME="$isolated_data"
export XDG_CONFIG_HOME="$isolated_config"
export XDG_STATE_HOME="$isolated_state"

start_supervisor "$binary" "$artifact_tmp/supervisor.log"

"$binary" list >/dev/null

# A clean artifact has no provider credentials, so doctor is expected to report
# blockers here. The release gate is that the packaged binary emits the stable,
# machine-readable diagnostic contract rather than pretending a fresh install
# is ready or failing before it can explain why.
doctor_json=""
if doctor_json=$("$binary" doctor --json 2>"$artifact_tmp/doctor.err"); then
  :
fi
case "$doctor_json" in
  *'"version":1'*'"checks"'*'"blockers"'*) ;;
  *)
    echo "packaged doctor did not emit its versioned JSON result" >&2
    cat "$artifact_tmp/doctor.err" >&2 || true
    exit 1
    ;;
esac

# Exercise the real built-in stable connection lifecycle against a local HTTP
# fixture. This drives the installed CLI and IPC surface rather than a package
# internal provider fake: create, open, traffic, inspect, close, restart,
# reopen and delete must all work from the artifact alone.
ports=$(python3 - <<'PY'
import socket
for _ in range(2):
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    print(sock.getsockname()[1])
    sock.close()
PY
)
backend_port=$(printf '%s\n' "$ports" | awk 'NR == 1 { print $1 }')
local_port=$(printf '%s\n' "$ports" | awk 'NR == 2 { print $1 }')
python3 - "$backend_port" >"$artifact_tmp/http-fixture.log" 2>&1 <<'PY' &
import http.server
import sys

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"portico-artifact-ok\n"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass

server = http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler)
server.serve_forever()
PY
echo_pid=$!
fixture_ready=0
i=0
while [ "$i" -lt 50 ]; do
  if curl -fsS --max-time 1 "http://127.0.0.1:$backend_port/" >/dev/null 2>&1; then
    fixture_ready=1
    break
  fi
  i=$((i + 1))
  sleep 0.1
done
if [ "$fixture_ready" -ne 1 ]; then
  echo "local HTTP fixture did not become ready" >&2
  cat "$artifact_tmp/http-fixture.log" >&2 || true
  exit 1
fi

created_json=$("$binary" forward create artifact-forward \
  --local-port "$local_port" --remote-host 127.0.0.1 --remote-port "$backend_port" \
  --protocol tcp --json)
connection_id=$(printf '%s\n' "$created_json" | awk -F'"' '/"id"/ { print $4; exit }')
if [ -z "$connection_id" ]; then
  echo "packaged forward create did not return a connection ID" >&2
  printf '%s\n' "$created_json" >&2
  exit 1
fi

"$binary" open "$connection_id" --yes --json >/dev/null
open_ready=0
i=0
while [ "$i" -lt 100 ]; do
  inspect_json=$("$binary" inspect "$connection_id" --json 2>/dev/null || true)
  if printf '%s\n' "$inspect_json" | grep -Fq '"runtime_state":"open"'; then
    open_ready=1
    break
  fi
  i=$((i + 1))
  sleep 0.1
done
if [ "$open_ready" -ne 1 ]; then
  echo "packaged forward did not become open" >&2
  printf '%s\n' "$inspect_json" >&2
  exit 1
fi
if ! curl -fsS --max-time 5 "http://127.0.0.1:$local_port/" | grep -Fxq 'portico-artifact-ok'; then
  echo "packaged forward did not relay HTTP traffic" >&2
  exit 1
fi
"$binary" inspect "$connection_id" --json >/dev/null
"$binary" close "$connection_id" --yes --json >/dev/null

"$binary" supervisor stop >/dev/null
wait "$supervisor_pid" 2>/dev/null || true
supervisor_pid=""
start_supervisor "$binary" "$artifact_tmp/forward-restart.log"
if ! "$binary" list --json | grep -Fq "$connection_id"; then
  echo "packaged restart lost the durable forward connection" >&2
  exit 1
fi
"$binary" open "$connection_id" --yes --json >/dev/null
open_ready=0
i=0
while [ "$i" -lt 100 ]; do
  inspect_json=$("$binary" inspect "$connection_id" --json 2>/dev/null || true)
  if printf '%s\n' "$inspect_json" | grep -Fq '"runtime_state":"open"'; then
    open_ready=1
    break
  fi
  i=$((i + 1))
  sleep 0.1
done
if [ "$open_ready" -ne 1 ]; then
  echo "packaged forward did not reopen after restart" >&2
  exit 1
fi
if ! curl -fsS --max-time 5 "http://127.0.0.1:$local_port/" | grep -Fxq 'portico-artifact-ok'; then
  echo "reopened packaged forward did not relay HTTP traffic" >&2
  exit 1
fi
"$binary" close "$connection_id" --yes --json >/dev/null
"$binary" delete "$connection_id" --yes --json >/dev/null
if "$binary" list --json | grep -Fq "$connection_id"; then
  echo "packaged delete left the forward in durable state" >&2
  exit 1
fi

if [ -n "$echo_pid" ] && kill -0 "$echo_pid" 2>/dev/null; then
  kill "$echo_pid" 2>/dev/null || true
  wait "$echo_pid" 2>/dev/null || true
fi
echo_pid=""

# Either outcome is valid for a fresh install: zero means this environment
# happens to have every provider dependency, nonzero means doctor found the
# expected readiness blockers. The JSON contract is the required assertion.

database="$isolated_data/portico/portico.db"
if [ ! -f "$database" ]; then
  echo "packaged supervisor did not create its database at $database" >&2
  exit 1
fi
database_mode=$(stat -c '%a' "$database" 2>/dev/null || stat -f '%Lp' "$database")
if [ "$database_mode" != "600" ]; then
  echo "packaged database mode is $database_mode, want 600" >&2
  exit 1
fi

"$binary" supervisor stop >/dev/null
wait "$supervisor_pid" 2>/dev/null || true
supervisor_pid=""

# A second invocation from a copied install must read the same state paths.
reinstalled="$artifact_tmp/reinstalled-portico"
cp "$binary" "$reinstalled"
chmod 755 "$reinstalled"
start_supervisor "$reinstalled" "$artifact_tmp/supervisor-restart.log"
"$reinstalled" list >/dev/null
"$reinstalled" supervisor stop >/dev/null
wait "$supervisor_pid" 2>/dev/null || true
supervisor_pid=""

echo "release artifact verified: $archive"
