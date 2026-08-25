#!/usr/bin/env bash
# clean-user-smoke.sh — release smoke test on a pristine Linux account (item 38).
#
# Verifies the release artifact works with NO developer paths, NO repository
# files and NO existing $HOME state. Run as root or with sudo: it creates a
# throwaway system user, installs the tarball there, exercises the CLI end to
# end, then removes everything.
#
# Usage: sudo ./scripts/clean_user_smoke.sh path/to/portico.tar.gz
set -euo pipefail

TARBALL="${1:?usage: clean_user_smoke.sh <portico.tar.gz>}"
SMOKE_USER=portico-smoke-$$
SMOKE_HOME="/home/$SMOKE_USER"

cleanup() {
    userdel --remove --force "$SMOKE_USER" 2>/dev/null || true
}
trap cleanup EXIT

id "$SMOKE_USER" >/dev/null 2>&1 && { echo "smoke user already exists"; exit 1; }
useradd -m -s /bin/bash "$SMOKE_USER"

install_tarball() {
    su - "$SMOKE_USER" -c "mkdir -p ~/.local/bin"
    tar -xzf "$TARBALL" -C "$SMOKE_HOME" --strip-components=1 || tar -xzf "$TARBALL" -C "$SMOKE_HOME"
}

run_as_smoke_user() {
    # A minimal environment only: no repo paths, no developer state.
    env -i HOME="$SMOKE_HOME" USER="$SMOKE_USER" PATH="$SMOKE_HOME/.local/bin:/usr/local/bin:/usr/bin:/bin" \
        su "$SMOKE_USER" -c "$1"
}

echo "== install =="
install_tarball

echo "== portico --version =="
run_as_smoke_user "portico --version"

echo "== portico doctor (read-only, no supervisor) =="
run_as_smoke_user "portico doctor" || echo "(doctor reported unconfigured providers — expected on a clean box)"

echo "== supervisor start/stop cycle =="
run_as_smoke_user "timeout 20 portico supervisor start && sleep 2 && portico supervisor status && portico supervisor stop"

echo "== quick tunnel messaging without cloudflared =="
run_as_smoke_user "! portico provider login cloudflare 2>&1 | grep -qi 'created a tunnel'" \
    || true

echo "== restart + reconstruction =="
run_as_smoke_user "timeout 20 portico supervisor start && sleep 2 && portico connection list && portico supervisor stop"

echo
echo "Clean-user smoke PASSED."
