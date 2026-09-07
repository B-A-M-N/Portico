#!/usr/bin/env bash
# Direct contract tests for release-tag validation and prior-release selection.
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
test_tmp=$(mktemp -d "${TMPDIR:-/tmp}/portico-release-tag-test.XXXXXX")
trap 'rm -rf "$test_tmp"' EXIT

valid_tags=(
  v1.2.3
  v1.2.3-beta.1
  v1.2.3+build.5
  v1.2.3-beta.1+build.5
)
invalid_tags=(
  v1.2.3bad
  v1.2.3-
  v1.2.3+
  v1.2.3-beta..1
  v1.2.3/unsafe
  v1.2.3.4
)

for tag in "${valid_tags[@]}"; do
  sh "$repo_root/scripts/validate_release_tag.sh" "$tag"
done
for tag in "${invalid_tags[@]}"; do
  if sh "$repo_root/scripts/validate_release_tag.sh" "$tag" >/dev/null 2>&1; then
    echo "release tag validator accepted malformed tag: $tag" >&2
    exit 1
  fi
done

git_root="$test_tmp/git"
mkdir -p "$git_root"
git -C "$git_root" init -q
git -C "$git_root" config user.email test@example.invalid
git -C "$git_root" config user.name "Portico release test"
git -C "$git_root" commit --allow-empty -qm initial
fallback=$(git -C "$git_root" rev-parse HEAD)

# A malformed tag that sorts highest must not enter selection. A future valid
# tag must also not be selected as the predecessor of the current release.
git -C "$git_root" tag v9.9.9bad
git -C "$git_root" tag v0.1.0
git -C "$git_root" tag v0.2.0-beta.1
git -C "$git_root" tag v0.2.0
git -C "$git_root" tag v0.3.0

selected=$(
  cd "$git_root"
  PORTICO_PRIOR_FALLBACK="$fallback" sh "$repo_root/scripts/select_prior_release.sh" v0.2.0 2>/dev/null
)
if [ "$selected" != "v0.2.0-beta.1" ]; then
  echo "wrong predecessor for v0.2.0: $selected" >&2
  exit 1
fi

selected=$(
  cd "$git_root"
  PORTICO_PRIOR_FALLBACK="$fallback" sh "$repo_root/scripts/select_prior_release.sh" "" 2>/dev/null
)
if [ "$selected" != "v0.3.0" ]; then
  echo "wrong latest release selection: $selected" >&2
  exit 1
fi

if (
  cd "$git_root"
  PORTICO_PRIOR_FALLBACK="$fallback" sh "$repo_root/scripts/select_prior_release.sh" v0.2.0bad >/dev/null 2>&1
); then
  echo "prior-release resolver accepted an invalid current tag" >&2
  exit 1
fi

empty_root="$test_tmp/empty-git"
mkdir -p "$empty_root"
git -C "$empty_root" init -q
git -C "$empty_root" config user.email test@example.invalid
git -C "$empty_root" config user.name "Portico release test"
git -C "$empty_root" commit --allow-empty -qm initial
empty_fallback=$(git -C "$empty_root" rev-parse HEAD)
selected=$(
  cd "$empty_root"
  PORTICO_PRIOR_FALLBACK="$empty_fallback" sh "$repo_root/scripts/select_prior_release.sh" "" 2>/dev/null
)
if [ "$selected" != "$empty_fallback" ]; then
  echo "fallback selection returned $selected, want $empty_fallback" >&2
  exit 1
fi

# When no current tag is supplied and HEAD is exactly tagged, the resolver must
# infer the current tag and exclude it: a tagged candidate can never be its own
# "previous release". Put HEAD on a fresh tagged release commit (the state a
# tag build checks out) and confirm the inferred tag is skipped in favor of the
# real predecessor.
git -C "$git_root" commit --allow-empty -qm "release v0.4.0"
git -C "$git_root" tag v0.4.0
git -C "$git_root" checkout -q v0.4.0
selected=$(
  cd "$git_root"
  PORTICO_PRIOR_FALLBACK="$fallback" sh "$repo_root/scripts/select_prior_release.sh" "" 2>/dev/null
)
if [ "$selected" != "v0.3.0" ]; then
  echo "tagged HEAD not excluded without explicit current tag: selected $selected, want v0.3.0" >&2
  exit 1
fi
# The inference must not leak into the no-arg contract on an untagged HEAD
# ("latest release overall"): the newer tag must resolve normally again.
git -C "$git_root" commit --allow-empty -qm "post v0.4.0 work"
selected=$(
  cd "$git_root"
  PORTICO_PRIOR_FALLBACK="$fallback" sh "$repo_root/scripts/select_prior_release.sh" "" 2>/dev/null
)
if [ "$selected" != "v0.4.0" ]; then
  echo "inference leaked into untagged-HEAD selection: selected $selected, want v0.4.0" >&2
  exit 1
fi

echo "release tag tests passed"
