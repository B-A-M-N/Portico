#!/bin/sh
# Portico installer.
#
# The contract this script implements is pinned by scripts/install_test.sh,
# which runs as part of `make release-check`. The installer supports two modes:
#
# 1. Local mirror mode (contract tests, air-gapped installs):
#      PORTICO_RELEASE_ARCHIVE      path to a portico-*.tar.gz
#      PORTICO_RELEASE_CHECKSUMS    path to the matching checksums.txt
#      PORTICO_RELEASE_TAG          release tag (validated, required in mirror mode)
#      PORTICO_RELEASE_ALLOW_UNSIGNED_LOCAL=1
#                                   explicitly accept a local release with no
#                                   publisher signature. Production installs
#                                   have no silent fallback: without this
#                                   variable an unsigned local release is
#                                   refused.
#
# 2. Release mode (default): resolves the tag (PORTICO_VERSION or latest),
#    downloads the archive, checksums, and Sigstore signature bundle from
#    PORTICO_RELEASE_BASE_URL (default: the GitHub release), verifies the
#    publisher identity and the checksum, and only then extracts and installs.
#
# Either way the previous executable is replaced atomically and left untouched
# when any verification fails: a failed install must never damage the working
# installation it was meant to upgrade.
set -eu

REPO="B-A-M-N/Portico"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
BINARY="portico"
# The publisher identity the Sigstore bundle must carry. A signature from any
# other workflow, repository, or ref is not Portico's release process.
EXPECTED_ISSUER="https://token.actions.githubusercontent.com"
EXPECTED_SAN="https://github.com/${REPO}/.github/workflows/release.yml@refs/tags"

# Detect OS.
OS="$(uname -s)"
case "$OS" in
  Linux*)  GOOS="linux" ;;
  *)       echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

# Detect architecture.
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  GOARCH="amd64" ;;
  arm64|aarch64)  GOARCH="arm64" ;;
  *)              echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

echo "Detected platform: ${GOOS}/${GOARCH}"

fetch() {
  # fetch <url> <output-file>
  # file:// URLs (contract tests, offline mirrors) go through cp; curl cannot
  # be assumed to support them and wget's file support differs by build.
  case "$1" in
    file://*)
      src="${1#file://}"
      cp "$src" "$2"
      return
      ;;
  esac
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$1" -O "$2"
  else
    echo "Error: curl or wget is required" >&2
    exit 1
  fi
}

# sha256_file prints the hex digest of $1 using whatever tool exists.
sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    echo "Error: sha256sum or shasum is required for checksum verification" >&2
    exit 1
  fi
}

# validate_release_tag accepts a strict semver tag (vMAJOR.MINOR.PATCH with
# optional prerelease/build segments) and refuses anything else before any
# fetch happens. Mirrors scripts/validate_release_tag.sh.
validate_release_tag() {
  case "$1" in
    v[0-9]*.[0-9]*.[0-9]*|v[0-9]*.[0-9]*.[0-9]*-*|v[0-9]*.[0-9]*.[0-9]*+*) ;;
    *) return 1 ;;
  esac
  echo "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
}

# verify_archive_members refuses an archive whose members are not exactly one
# regular file named portico. Extraction is never the first place that
# discovers a traversal, absolute path, duplicate, link, or device member.
verify_archive_members() {
  # verify_archive_members <archive>
  tar -tzf "$1" | awk '
    BEGIN { count = 0; valid = 1 }
    {
      count++
      if ($0 != "portico") valid = 0
    }
    END { exit !(valid && count == 1) }
  '
}

# stage_or_install extracts the verified archive into a staging dir and moves
# the binary into place. The move is the last operation, after every
# verification has passed.
stage_or_install() {
  # stage_or_install <archive> <stage-dir>
  tar -xzf "$1" --no-same-owner --no-same-permissions -C "$2"
  if [ ! -x "$2/$BINARY" ]; then
    echo "Error: archive does not contain an executable $BINARY" >&2
    exit 1
  fi
  mkdir -p "$INSTALL_DIR"
  staged="$INSTALL_DIR/.portico.new.$$"
  cp "$2/$BINARY" "$staged"
  chmod 0755 "$staged"
  mv -f "$staged" "$INSTALL_DIR/$BINARY"
}

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

if [ -n "${PORTICO_RELEASE_ARCHIVE:-}" ]; then
  # ---- Local mirror mode -------------------------------------------------
  ARCHIVE_PATH="${PORTICO_RELEASE_ARCHIVE:?PORTICO_RELEASE_ARCHIVE is required in local mode}"
  CHECKSUMS_PATH="${PORTICO_RELEASE_CHECKSUMS:?PORTICO_RELEASE_CHECKSUMS is required in local mode}"
  TAG="${PORTICO_RELEASE_TAG:-}"

  if [ -z "$TAG" ]; then
    echo "Error: PORTICO_RELEASE_TAG is required in local mode" >&2
    exit 1
  fi
  if ! validate_release_tag "$TAG"; then
    echo "Error: malformed release tag: $TAG" >&2
    exit 1
  fi
  [ -f "$ARCHIVE_PATH" ] || { echo "Error: archive not found: $ARCHIVE_PATH" >&2; exit 1; }
  [ -f "$CHECKSUMS_PATH" ] || { echo "Error: checksums not found: $CHECKSUMS_PATH" >&2; exit 1; }

  verify_archive_members "$ARCHIVE_PATH"

  ARCHIVE_NAME="$(basename "$ARCHIVE_PATH")"
  EXPECTED="$(grep " ${ARCHIVE_NAME}\$" "$CHECKSUMS_PATH" | head -1 | awk '{print $1}')"
  [ -z "$EXPECTED" ] && EXPECTED="$(grep "\*${ARCHIVE_NAME}\$" "$CHECKSUMS_PATH" | head -1 | awk '{print $1}')"
  if [ -z "$EXPECTED" ]; then
    echo "Error: no checksum entry for ${ARCHIVE_NAME} in ${CHECKSUMS_PATH}" >&2
    exit 1
  fi
  ACTUAL="$(sha256_file "$ARCHIVE_PATH")"
  if [ "$EXPECTED" != "$ACTUAL" ]; then
    echo "Error: checksum mismatch for ${ARCHIVE_NAME}" >&2
    echo "  expected: ${EXPECTED}" >&2
    echo "  actual:   ${ACTUAL}" >&2
    exit 1
  fi

  # A local archive has no publisher evidence unless the fixture explicitly
  # opts in. Production installs must never take this branch silently.
  if [ "${PORTICO_RELEASE_ALLOW_UNSIGNED_LOCAL:-0}" != "1" ]; then
    echo "Error: local release carries no publisher signature." >&2
    echo "Refusing to install without PORTICO_RELEASE_ALLOW_UNSIGNED_LOCAL=1." >&2
    exit 1
  fi

  stage_dir="$TMPDIR/extract"
  mkdir -p "$stage_dir"
  stage_or_install "$ARCHIVE_PATH" "$stage_dir"

  echo "Installed ${BINARY} ${TAG} to ${INSTALL_DIR}/${BINARY}"
else
  # ---- Release mode ------------------------------------------------------
  TAG="${PORTICO_VERSION:-}"
  if [ -z "$TAG" ]; then
    if command -v curl >/dev/null 2>&1; then
      TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//')"
    elif command -v wget >/dev/null 2>&1; then
      TAG="$(wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//')"
    else
      echo "Error: curl or wget is required" >&2
      exit 1
    fi
  fi

  if [ -z "$TAG" ]; then
    echo "Error: could not determine latest release" >&2
    exit 1
  fi
  if ! validate_release_tag "$TAG"; then
    echo "Error: malformed release tag: $TAG" >&2
    exit 1
  fi

  echo "Installing release: ${TAG}"

  BASE_URL="${PORTICO_RELEASE_BASE_URL:-https://github.com/${REPO}/releases/download/${TAG}}"
  ARCHIVE="${BINARY}-${TAG}-${GOOS}-${GOARCH}.tar.gz"
  ARCHIVE_PATH="${TMPDIR}/${ARCHIVE}"

  echo "Downloading ${BASE_URL}/${ARCHIVE}..."
  fetch "${BASE_URL}/${ARCHIVE}" "$ARCHIVE_PATH"

  echo "Downloading checksums.txt..."
  if ! fetch "${BASE_URL}/checksums.txt" "${TMPDIR}/checksums.txt"; then
    echo "Error: checksums.txt asset not found for release ${TAG}." >&2
    echo "Refusing to install without checksum verification." >&2
    exit 1
  fi

  EXPECTED="$(grep " ${ARCHIVE}\$" "${TMPDIR}/checksums.txt" | head -1 | awk '{print $1}')"
  [ -z "$EXPECTED" ] && EXPECTED="$(grep "\*${ARCHIVE}\$" "${TMPDIR}/checksums.txt" | head -1 | awk '{print $1}')"
  if [ -z "$EXPECTED" ]; then
    echo "Error: no checksum entry for ${ARCHIVE} in checksums.txt" >&2
    exit 1
  fi

  ACTUAL="$(sha256_file "$ARCHIVE_PATH")"
  if [ "$EXPECTED" != "$ACTUAL" ]; then
    echo "Error: checksum mismatch for ${ARCHIVE}" >&2
    echo "  expected: ${EXPECTED}" >&2
    echo "  actual:   ${ACTUAL}" >&2
    exit 1
  fi
  echo "Checksum verified."

  # Publisher authentication: fail closed. The checksums file must carry a
  # Sigstore signature produced by Portico's own release workflow over the
  # exact bytes being trusted. A missing verifier, missing evidence, or a
  # failed verification all refuse the install — there is no production path
  # that proceeds unverified, because checksums.txt from the same location as
  # the archive proves nothing beyond accidental corruption.
  SIG_PATH="${TMPDIR}/checksums.txt.sig"
  CERT_PATH="${TMPDIR}/checksums.txt.pem"
  echo "Downloading checksums.txt.sig..."
  if ! fetch "${BASE_URL}/checksums.txt.sig" "$SIG_PATH"; then
    echo "Error: checksums.txt.sig asset not found for release ${TAG}." >&2
    echo "Refusing to install: publisher evidence is missing." >&2
    exit 1
  fi
  echo "Downloading checksums.txt.pem..."
  if ! fetch "${BASE_URL}/checksums.txt.pem" "$CERT_PATH"; then
    echo "Error: checksums.txt.pem asset not found for release ${TAG}." >&2
    echo "Refusing to install: publisher evidence is missing." >&2
    exit 1
  fi
  if ! command -v cosign >/dev/null 2>&1; then
    echo "Error: cosign is required to verify the publisher of release ${TAG}." >&2
    echo "Install cosign (https://docs.sigstore.dev) and retry." >&2
    exit 1
  fi
  if ! cosign verify-blob \
      --certificate-identity-regexp "^${EXPECTED_SAN}.*$" \
      --certificate-oidc-issuer "$EXPECTED_ISSUER" \
      --signature "$SIG_PATH" \
      --certificate "$CERT_PATH" \
      "${TMPDIR}/checksums.txt" >/dev/null 2>&1; then
    echo "Error: publisher verification failed for release ${TAG}." >&2
    echo "The checksums file is not signed by ${REPO}'s release workflow." >&2
    exit 1
  fi
  echo "Publisher verified: ${REPO} release workflow."

  verify_archive_members "$ARCHIVE_PATH"

  stage_dir="$TMPDIR/extract"
  mkdir -p "$stage_dir"
  stage_or_install "$ARCHIVE_PATH" "$stage_dir"

  echo "Installed ${BINARY} ${TAG} to ${INSTALL_DIR}/${BINARY}"
fi

# Check if INSTALL_DIR is in PATH.
case ":$PATH:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    echo ""
    echo "Add ${INSTALL_DIR} to your PATH:"
    echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
    ;;
esac
