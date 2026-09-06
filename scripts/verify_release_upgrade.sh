#!/usr/bin/env bash
# Verify that a candidate install preserves state created by a prior binary.
set -euo pipefail

prior_binary_input="${1:?usage: verify_release_upgrade.sh PRIOR_BINARY CANDIDATE_ARCHIVE}"
candidate_archive_input="${2:?usage: verify_release_upgrade.sh PRIOR_BINARY CANDIDATE_ARCHIVE}"
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
prior_binary=$(CDPATH= cd -- "$(dirname -- "$prior_binary_input")" && pwd)/$(basename -- "$prior_binary_input")
candidate_archive=$(CDPATH= cd -- "$(dirname -- "$candidate_archive_input")" && pwd)/$(basename -- "$candidate_archive_input")

if [ ! -x "$prior_binary" ]; then
    echo "prior binary is not executable: $prior_binary" >&2
    exit 1
fi
case "$candidate_archive" in
    *.tar.gz) ;;
    *) echo "candidate archive must end in .tar.gz: $candidate_archive" >&2; exit 2 ;;
esac
if [ ! -f "$candidate_archive" ]; then
    echo "candidate archive does not exist: $candidate_archive" >&2
    exit 1
fi

upgrade_tmp=$(mktemp -d "${TMPDIR:-/tmp}/portico-upgrade-test.XXXXXX")
isolated_home="$upgrade_tmp/home"
isolated_runtime="$upgrade_tmp/runtime"
isolated_data="$upgrade_tmp/data"
isolated_config="$upgrade_tmp/config"
isolated_state="$upgrade_tmp/state"
install_dir="$upgrade_tmp/bin"
prior_pid=""
candidate_pid=""

cleanup() {
    if [ -n "$candidate_pid" ] && kill -0 "$candidate_pid" 2>/dev/null; then
        run_portico "$install_dir/portico" supervisor stop >/dev/null 2>&1 || true
        kill "$candidate_pid" 2>/dev/null || true
        wait "$candidate_pid" 2>/dev/null || true
    fi
    if [ -n "$prior_pid" ] && kill -0 "$prior_pid" 2>/dev/null; then
        run_portico "$prior_binary" supervisor stop >/dev/null 2>&1 || true
        kill "$prior_pid" 2>/dev/null || true
        wait "$prior_pid" 2>/dev/null || true
    fi
    rm -rf "$upgrade_tmp"
}
trap cleanup EXIT INT TERM

mkdir -p "$isolated_home" "$isolated_runtime" "$isolated_data" \
    "$isolated_config" "$isolated_state" "$install_dir"

go_cache="${GOCACHE:-$(go env GOCACHE)}"
go_mod_cache="${GOMODCACHE:-$(go env GOMODCACHE)}"

run_portico() {
    env HOME="$isolated_home" \
        XDG_RUNTIME_DIR="$isolated_runtime" \
        XDG_DATA_HOME="$isolated_data" \
        XDG_CONFIG_HOME="$isolated_config" \
        XDG_STATE_HOME="$isolated_state" \
        PORTICO_DEV=true "$@"
}

wait_for_supervisor() {
    binary="$1"
    log_path="$2"
    attempt=0
    while [ "$attempt" -lt 100 ]; do
        if run_portico "$binary" supervisor status 2>/dev/null | grep -q 'Supervisor: running'; then
            return 0
        fi
        # Process liveness is checked directly: the log file exists the moment
        # output is redirected into it, so its existence says nothing about
        # whether the supervisor survived. A dead supervisor must fail this
        # wait promptly, not after the full timeout.
        if ! kill -0 "$active_pid" 2>/dev/null; then
            break
        fi
        attempt=$((attempt + 1))
        sleep 0.1
    done
    echo "supervisor did not become ready: $binary" >&2
    cat "$log_path" >&2 2>/dev/null || true
    return 1
}

echo "== prior binary seeds state =="
run_portico "$prior_binary" supervisor run >"$upgrade_tmp/prior-supervisor.log" 2>&1 &
prior_pid=$!
active_pid="$prior_pid"
wait_for_supervisor "$prior_binary" "$upgrade_tmp/prior-supervisor.log"
run_portico "$prior_binary" create upgrade-fixture \
    --source http://127.0.0.1:8080 --provider mock >/dev/null
prior_list=$(run_portico "$prior_binary" list)
fixture_id=$(printf '%s\n' "$prior_list" | awk '$2 == "upgrade-fixture" { print $1; exit }')
if [ -z "$fixture_id" ]; then
    echo "prior binary did not persist the upgrade baseline" >&2
    printf '%s\n' "$prior_list" >&2
    exit 1
fi
run_portico "$prior_binary" supervisor stop >/dev/null
wait "$prior_pid" 2>/dev/null || true
prior_pid=""

echo "== enrich prior state and migrate it to the candidate schema =="
# The prior binary owns the original database schema and profile. Once it has
# shut down, the current Store migrates that exact database and adds the
# durable credential, managed resource, runtime, diagnostics, event history,
# and interrupted operation used by the candidate check.
HOME="$isolated_home" \
XDG_RUNTIME_DIR="$isolated_runtime" \
XDG_DATA_HOME="$isolated_data" \
XDG_CONFIG_HOME="$isolated_config" \
XDG_STATE_HOME="$isolated_state" \
PORTICO_UPGRADE_FIXTURE_ID="$fixture_id" \
GOCACHE="$go_cache" \
GOMODCACHE="$go_mod_cache" \
GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" \
    go run "$script_dir/seed_upgrade_fixture.go"

echo "== candidate installer upgrades the same state =="
archive_name=$(basename -- "$candidate_archive")
(cd "$(dirname -- "$candidate_archive")" && sha256sum "$archive_name") \
    >"$upgrade_tmp/checksums.txt"
PORTICO_RELEASE_ARCHIVE="$candidate_archive" \
PORTICO_RELEASE_CHECKSUMS="$upgrade_tmp/checksums.txt" \
PORTICO_RELEASE_ALLOW_UNSIGNED_LOCAL=1 \
PORTICO_RELEASE_TAG="${PORTICO_RELEASE_TAG:-v0.0.0-local.1}" \
INSTALL_DIR="$install_dir" \
    sh "$script_dir/../install.sh" >/dev/null
test -x "$install_dir/portico"

run_portico "$install_dir/portico" supervisor run >"$upgrade_tmp/candidate-supervisor.log" 2>&1 &
candidate_pid=$!
active_pid="$candidate_pid"
wait_for_supervisor "$install_dir/portico" "$upgrade_tmp/candidate-supervisor.log"
candidate_list=$(run_portico "$install_dir/portico" list)
if ! printf '%s\n' "$candidate_list" | grep -Fq "$fixture_id upgrade-fixture"; then
    echo "candidate binary did not preserve the prior connection" >&2
    printf '%s\n' "$candidate_list" >&2
    exit 1
fi
test -s "$isolated_data/portico/portico.db"
run_portico "$install_dir/portico" supervisor stop >/dev/null
wait "$candidate_pid" 2>/dev/null || true
candidate_pid=""

# Verify the encrypted credential, managed resource, runtime projection,
# diagnostics, journal, and startup-recovered operation directly from the
# candidate's durable store after it has shut down cleanly.
HOME="$isolated_home" \
XDG_RUNTIME_DIR="$isolated_runtime" \
XDG_DATA_HOME="$isolated_data" \
XDG_CONFIG_HOME="$isolated_config" \
XDG_STATE_HOME="$isolated_state" \
PORTICO_UPGRADE_FIXTURE_ID="$fixture_id" \
GOCACHE="$go_cache" \
GOMODCACHE="$go_mod_cache" \
GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" \
    go run "$script_dir/seed_upgrade_fixture.go" verify

echo "release upgrade verified: prior=$prior_binary candidate=$candidate_archive"
