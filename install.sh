#!/bin/sh
set -e

REPO="B-A-M-N/Portico"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
BINARY="portico"

# Detect OS.
OS="$(uname -s)"
case "$OS" in
  Linux*)  GOOS="linux" ;;
  Darwin*) GOOS="darwin" ;;
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

# Fetch a URL to a file.
fetch() {
  # fetch <url> <output-file>
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$1" -O "$2"
  else
    echo "Error: curl or wget is required" >&2
    exit 1
  fi
}

# Get the latest release tag.
if command -v curl >/dev/null 2>&1; then
  TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//')"
elif command -v wget >/dev/null 2>&1; then
  TAG="$(wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//')"
else
  echo "Error: curl or wget is required" >&2
  exit 1
fi

if [ -z "$TAG" ]; then
  echo "Error: could not determine latest release" >&2
  exit 1
fi

echo "Latest release: ${TAG}"

ARCHIVE="${BINARY}-${TAG}-${GOOS}-${GOARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${TAG}/${ARCHIVE}"
CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${TAG}/checksums.txt"

# Download.
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading ${URL}..."
fetch "$URL" "${TMPDIR}/${ARCHIVE}"

# Download checksums and verify.
echo "Downloading checksums.txt..."
if ! fetch "$CHECKSUMS_URL" "${TMPDIR}/checksums.txt"; then
  echo "Error: checksums.txt asset not found for release ${TAG}." >&2
  echo "Refusing to install without checksum verification." >&2
  exit 1
fi

EXPECTED="$(grep " ${ARCHIVE}\$" "${TMPDIR}/checksums.txt" | head -1 | awk '{print $1}')"
if [ -z "$EXPECTED" ]; then
  # Some checksum generators use a "*" binary-mode marker.
  EXPECTED="$(grep "\*${ARCHIVE}\$" "${TMPDIR}/checksums.txt" | head -1 | awk '{print $1}')"
fi
if [ -z "$EXPECTED" ]; then
  echo "Error: no checksum entry for ${ARCHIVE} in checksums.txt" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TMPDIR}/${ARCHIVE}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "${TMPDIR}/${ARCHIVE}" | awk '{print $1}')"
else
  echo "Error: sha256sum or shasum is required for checksum verification" >&2
  exit 1
fi

if [ "$EXPECTED" != "$ACTUAL" ]; then
  echo "Error: checksum mismatch for ${ARCHIVE}" >&2
  echo "  expected: ${EXPECTED}" >&2
  echo "  actual:   ${ACTUAL}" >&2
  exit 1
fi
echo "Checksum verified."

tar -xzf "${TMPDIR}/${ARCHIVE}" -C "$TMPDIR"

# Install.
mkdir -p "$INSTALL_DIR"
mv "${TMPDIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
chmod +x "${INSTALL_DIR}/${BINARY}"

echo "Installed ${BINARY} ${TAG} to ${INSTALL_DIR}/${BINARY}"

# Check if INSTALL_DIR is in PATH.
case ":$PATH:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    echo ""
    echo "Add ${INSTALL_DIR} to your PATH:"
    echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
    ;;
esac
