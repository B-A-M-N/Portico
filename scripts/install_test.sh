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
