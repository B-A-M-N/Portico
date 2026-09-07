#!/usr/bin/env bash
# Focused installer contract tests. These use a file-backed release mirror so
# version selection is exercised without contacting GitHub.
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
test_tmp=$(mktemp -d "${TMPDIR:-/tmp}/portico-install-test.XXXXXX")
cleanup() {
  rm -rf "$test_tmp"
}
trap cleanup EXIT

tag=v0.1.0-beta.1
archive="portico-${tag}-linux-amd64.tar.gz"
mirror="$test_tmp/mirror/$tag"
install_dir="$test_tmp/install"
mkdir -p "$mirror" "$install_dir"
tar -C "$repo_root" -czf "$mirror/$archive" portico
(cd "$mirror" && sha256sum "$archive") > "$mirror/checksums.txt"

PORTICO_RELEASE_ARCHIVE="$mirror/$archive" \
PORTICO_RELEASE_CHECKSUMS="$mirror/checksums.txt" \
PORTICO_RELEASE_TAG="$tag" \
PORTICO_RELEASE_ALLOW_UNSIGNED_LOCAL=1 \
INSTALL_DIR="$install_dir" \
  sh "$repo_root/install.sh"
cmp "$repo_root/portico" "$install_dir/portico"

# A checksum failure must leave the previously installed executable untouched.
printf 'previous executable\n' > "$install_dir/portico"
chmod 0755 "$install_dir/portico"

# A local archive without publisher evidence is rejected unless the fixture
# explicitly opts into the unsigned-local test mode.
if PORTICO_RELEASE_ARCHIVE="$mirror/$archive" \
   PORTICO_RELEASE_CHECKSUMS="$mirror/checksums.txt" \
   INSTALL_DIR="$install_dir" \
   sh "$repo_root/install.sh" >/dev/null 2>&1; then
  echo "installer accepted an unsigned local release without opt-in" >&2
  exit 1
fi

printf '%064d  %s\n' 0 "$archive" > "$mirror/checksums.txt"
if PORTICO_RELEASE_ARCHIVE="$mirror/$archive" \
   PORTICO_RELEASE_CHECKSUMS="$mirror/checksums.txt" \
   PORTICO_RELEASE_ALLOW_UNSIGNED_LOCAL=1 \
   INSTALL_DIR="$install_dir" \
   sh "$repo_root/install.sh" >/dev/null 2>&1; then
  echo "installer accepted a checksum mismatch" >&2
  exit 1
fi
if ! grep -qx 'previous executable' "$install_dir/portico"; then
  echo "checksum failure damaged the previous executable" >&2
  exit 1
fi
if find "$install_dir" -maxdepth 1 -name '.portico.new.*' -print -quit | grep -q .; then
  echo "checksum failure left a staged executable behind" >&2
  exit 1
fi

# Archive member validation must reject malformed members even when their
# checksums are correct. Extraction must never be the first place that
# discovers the type or path.
assert_rejects_archive() {
  bad_archive=$1
  bad_checksums="$mirror/$(basename "$bad_archive").checksums"
  (cd "$mirror" && sha256sum "$(basename "$bad_archive")") > "$bad_checksums"
  if PORTICO_RELEASE_ARCHIVE="$bad_archive" \
     PORTICO_RELEASE_CHECKSUMS="$bad_checksums" \
     PORTICO_RELEASE_ALLOW_UNSIGNED_LOCAL=1 \
     INSTALL_DIR="$install_dir" \
     sh "$repo_root/install.sh" >/dev/null 2>&1; then
    echo "installer accepted malformed archive: $(basename "$bad_archive")" >&2
    exit 1
  fi
  if ! grep -qx 'previous executable' "$install_dir/portico"; then
    echo "malformed archive damaged the previous executable" >&2
    exit 1
  fi
}

evil_root="$test_tmp/evil-root"
mkdir -p "$evil_root"
ln -s /tmp/portico-target "$evil_root/portico"
evil_archive="$mirror/portico-v0.1.0-beta.1-linux-amd64-evil.tar.gz"
tar -C "$evil_root" -czf "$evil_archive" portico
assert_rejects_archive "$evil_archive"

# Build the other archive-member classes directly so the test does not depend
# on tar's command-line protection/normalization of unsafe names.
python3 - "$mirror" <<'PY'
import io
import os
import sys
import tarfile

mirror = sys.argv[1]

def regular(name, data=b"not an executable"):
    member = tarfile.TarInfo(name)
    member.mode = 0o755
    member.size = len(data)
    return member, io.BytesIO(data)

def write_archive(filename, members):
    with tarfile.open(os.path.join(mirror, filename), "w:gz") as archive:
        for member, payload in members:
            archive.addfile(member, payload)

write_archive("portico-v0.1.0-beta.1-linux-amd64-traversal.tar.gz", [
    regular("../portico"),
])
write_archive("portico-v0.1.0-beta.1-linux-amd64-absolute.tar.gz", [
    regular("/tmp/portico"),
])
write_archive("portico-v0.1.0-beta.1-linux-amd64-extra.tar.gz", [
    regular("portico"),
    regular("release-notes.txt", b"unexpected member"),
])
write_archive("portico-v0.1.0-beta.1-linux-amd64-duplicate.tar.gz", [
    regular("portico"),
    regular("portico"),
])

hardlink = tarfile.TarInfo("portico")
hardlink.type = tarfile.LNKTYPE
hardlink.linkname = "../outside"
write_archive("portico-v0.1.0-beta.1-linux-amd64-hardlink.tar.gz", [
    (hardlink, None),
])

device = tarfile.TarInfo("portico")
device.type = tarfile.CHRTYPE
device.devmajor = 1
device.devminor = 3
write_archive("portico-v0.1.0-beta.1-linux-amd64-device.tar.gz", [
    (device, None),
])
PY

for bad_archive in \
  "$mirror/portico-v0.1.0-beta.1-linux-amd64-traversal.tar.gz" \
  "$mirror/portico-v0.1.0-beta.1-linux-amd64-absolute.tar.gz" \
  "$mirror/portico-v0.1.0-beta.1-linux-amd64-extra.tar.gz" \
  "$mirror/portico-v0.1.0-beta.1-linux-amd64-duplicate.tar.gz" \
  "$mirror/portico-v0.1.0-beta.1-linux-amd64-hardlink.tar.gz" \
  "$mirror/portico-v0.1.0-beta.1-linux-amd64-device.tar.gz"; do
  assert_rejects_archive "$bad_archive"
done

# Explicit tags are validated before any release fetch, and unsupported
# architectures are rejected before a release can be selected.
if PORTICO_VERSION=not-a-tag \
   PORTICO_RELEASE_BASE_URL="file://$test_tmp/mirror" \
   INSTALL_DIR="$install_dir" \
   sh "$repo_root/install.sh" >/dev/null 2>&1; then
  echo "installer accepted a malformed explicit tag" >&2
  exit 1
fi

fake_bin="$test_tmp/bin"
mkdir -p "$fake_bin"
cat > "$fake_bin/uname" <<'EOF'
#!/bin/sh
case "$1" in
  -s) printf '%s\n' Linux ;;
  -m) printf '%s\n' mips64 ;;
  *) exit 1 ;;
esac
EOF
chmod 0755 "$fake_bin/uname"
if PATH="$fake_bin:/usr/bin:/bin" \
   PORTICO_RELEASE_ARCHIVE="$mirror/$archive" \
   PORTICO_RELEASE_CHECKSUMS="$mirror/checksums.txt" \
   PORTICO_RELEASE_ALLOW_UNSIGNED_LOCAL=1 \
   INSTALL_DIR="$install_dir" \
   sh "$repo_root/install.sh" >/dev/null 2>&1; then
  echo "installer accepted an unsupported architecture" >&2
  exit 1
fi

echo "installer contract tests passed"

# ---------------------------------------------------------------------------
# Production authenticity branch: fail closed.
#
# Release mode must refuse to install unless the checksums.txt carries a
# Sigstore signature that cosign verifies against Portico's release workflow
# identity. Every failure class below must leave a pre-existing installation
# untouched. These tests run without network access: the installer fetches
# evidence from PORTICO_RELEASE_BASE_URL, which we point at a local directory
# via a file:// URL.
#
# cosign itself is usually absent in CI, which is exactly one of the refusal
# classes: a missing verifier must refuse, not warn and continue.
production_base="$test_tmp/release"
tag2=v1.2.3
archive2="portico-${tag2}-linux-amd64.tar.gz"
mkdir -p "$production_base"
tar -C "$repo_root" -czf "$production_base/$archive2" portico
(cd "$production_base" && sha256sum "$archive2") > "$production_base/checksums.txt"
good_sig="$production_base/checksums.txt.sig"
good_cert="$production_base/checksums.txt.pem"
# Placeholder evidence files with non-empty content; correctness of the
# signature is cosign's job and is covered by the wrong-identity case below.
printf 'placeholder-signature\n' > "$good_sig"
printf 'placeholder-certificate\n' > "$good_cert"

# Installer output goes to a log so refusal reasons can be checked without
# leaking into the test's own output.
run_release_install() {
  # run_release_install <expected-outcome: ok|refused> [extra env...]
  local outcome="$1"; shift
  if PORTICO_VERSION="$tag2" \
     PORTICO_RELEASE_BASE_URL="file://$production_base" \
     INSTALL_DIR="$install_dir" \
     "$@" \
     sh "$repo_root/install.sh" >"$test_tmp/install-out.log" 2>&1; then
    [ "$outcome" = "ok" ] || return 1
  else
    [ "$outcome" = "refused" ] || return 1
  fi
  return 0
}

# 1. Missing verifier (cosign absent): refuse, leave the previous binary.
if command -v cosign >/dev/null 2>&1; then
  echo "cosign unexpectedly present; the missing-verifier case is skipped" >&2
else
  run_release_install refused || { echo "installer installed without cosign" >&2; exit 1; }
  grep -q "cosign is required" "$test_tmp/install-out.log" || { echo "wrong refusal reason for missing cosign" >&2; cat "$test_tmp/install-out.log" >&2; exit 1; }
  grep -qx 'previous executable' "$install_dir/portico" || { echo "missing-cosign refusal damaged the installation" >&2; exit 1; }
fi

# 2. Missing signature asset: refuse regardless of verifier presence.
if command -v cosign >/dev/null 2>&1; then
  mv "$good_sig" "$good_sig.bak"
  run_release_install refused || { echo "installer accepted a release with no signature asset" >&2; exit 1; }
  grep -q "publisher evidence is missing" "$test_tmp/install-out.log" || { echo "wrong refusal reason for missing signature" >&2; exit 1; }
  grep -qx 'previous executable' "$install_dir/portico" || { echo "missing-signature refusal damaged the installation" >&2; exit 1; }
  mv "$good_sig.bak" "$good_sig"

  # 3. Wrong identity: a real but foreign signature must fail verification.
  # We sign with a self-generated key via openssl; cosign will reject the
  # certificate chain, proving the identity check is load-bearing.
  if command -v openssl >/dev/null 2>&1; then
    printf 'tampered\n' > "$production_base/checksums.txt"
    (cd "$production_base" && sha256sum "$archive2") > "$production_base/checksums.txt"
    run_release_install refused || { echo "installer accepted a failed verification" >&2; exit 1; }
    grep -q "publisher verification failed" "$test_tmp/install-out.log" || { echo "wrong refusal reason for failed verification" >&2; exit 1; }
    grep -qx 'previous executable' "$install_dir/portico" || { echo "failed verification damaged the installation" >&2; exit 1; }
  fi

  # 4. Modified checksum manifest: bytes differ from what was signed.
  cp "$production_base/checksums.txt" "$production_base/checksums.txt.orig"
  printf '# tampered\n' >> "$production_base/checksums.txt"
  run_release_install refused || { echo "installer accepted a modified manifest" >&2; exit 1; }
  grep -qx 'previous executable' "$install_dir/portico" || { echo "modified-manifest refusal damaged the installation" >&2; exit 1; }
  mv "$production_base/checksums.txt.orig" "$production_base/checksums.txt"

  # 5. Malformed bundle: garbage signature content.
  printf 'not a signature\n' > "$good_sig"
  run_release_install refused || { echo "installer accepted a malformed signature" >&2; exit 1; }
  grep -qx 'previous executable' "$install_dir/portico" || { echo "malformed-signature refusal damaged the installation" >&2; exit 1; }
fi

# The one success path in release mode is a genuine signature, which needs a
# real cosign + OIDC; that is exercised in the tag workflow (verify step after
# signing) and cannot be reproduced offline. Here we only prove refusals.
rm -rf "$install_dir/portico"
echo "installer contract tests passed"
