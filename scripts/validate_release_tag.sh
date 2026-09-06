#!/bin/sh
set -eu
tag="${1:?usage: validate_release_tag.sh TAG}"
case "$tag" in
  *[!A-Za-z0-9.+-]*)
    echo "Unsupported release tag: $tag" >&2
    exit 1
    ;;
esac
if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'; then
  echo "Unsupported release tag: $tag" >&2
  exit 1
fi
