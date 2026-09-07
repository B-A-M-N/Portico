#!/bin/sh
# Print the previous published SemVer tag, or the immutable fallback when the
# repository has no earlier published release. This is the single policy used
# by CI and release qualification.
set -eu

current_tag="${1:-}"
fallback="${PORTICO_PRIOR_FALLBACK:-0082c71}"

is_release_tag() {
  case "$1" in
    *[!A-Za-z0-9.+-]*) return 1 ;;
  esac
  printf '%s\n' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
}

semver_compare() {
  awk -v a="$1" -v b="$2" '
    function normnum(value) {
      sub(/^0+/, "", value)
      return value == "" ? "0" : value
    }
    function cmpnum(left, right, l, r) {
      l = normnum(left)
      r = normnum(right)
      if (length(l) != length(r)) return length(l) < length(r) ? -1 : 1
      if (("x" l) == ("x" r)) return 0
      return ("x" l) < ("x" r) ? -1 : 1
    }
    function cmppre(left, right, la, ra, i, c) {
      la = split(left, pa, ".")
      ra = split(right, pb, ".")
      for (i = 1; i <= la || i <= ra; i++) {
        if (i > la) return -1
        if (i > ra) return 1
        if (pa[i] ~ /^[0-9]+$/ && pb[i] ~ /^[0-9]+$/) {
          c = cmpnum(pa[i], pb[i])
        } else if (pa[i] ~ /^[0-9]+$/) {
          c = -1
        } else if (pb[i] ~ /^[0-9]+$/) {
          c = 1
        } else if (("x" pa[i]) == ("x" pb[i])) {
          c = 0
        } else {
          c = (("x" pa[i]) < ("x" pb[i])) ? -1 : 1
        }
        if (c != 0) return c
      }
      return 0
    }
    BEGIN {
      sub(/^v/, "", a); sub(/^v/, "", b)
      sub(/\+.*/, "", a); sub(/\+.*/, "", b)
      ia = index(a, "-"); ib = index(b, "-")
      if (ia) { ac = substr(a, 1, ia - 1); ap = substr(a, ia + 1) } else { ac = a; ap = "" }
      if (ib) { bc = substr(b, 1, ib - 1); bp = substr(b, ib + 1) } else { bc = b; bp = "" }
      na = split(ac, ca, "."); nb = split(bc, cb, ".")
      for (i = 1; i <= 3; i++) {
        c = cmpnum(ca[i], cb[i])
        if (c != 0) { print c; exit }
      }
      if (ap == "" && bp == "") { print 0; exit }
      if (ap == "") { print 1; exit }
      if (bp == "") { print -1; exit }
      print cmppre(ap, bp)
    }'
}

if [ -n "$current_tag" ] && ! is_release_tag "$current_tag"; then
  echo "Unsupported current release tag: $current_tag" >&2
  exit 1
fi

# Defensive: when called with no argument (as Makefile::upgrade-check did for a
# while), infer the current tag from HEAD so an exactly-tagged HEAD can never
# be selected as its own "previous release". Only the exact tag is excluded —
# unlike an explicitly supplied current tag, the inference must not filter out
# newer tags, because the no-argument contract is "latest release overall".
# Callers that know their tag (release CI) should still pass it explicitly.
exclude_tag=""
if [ -z "$current_tag" ]; then
  inferred=$(git describe --tags --exact-match 2>/dev/null || true)
  if [ -n "$inferred" ] && is_release_tag "$inferred"; then
    echo "No current tag supplied; HEAD is tagged $inferred — excluding it from prior selection" >&2
    exclude_tag="$inferred"
  fi
fi

prior=""
while IFS= read -r tag; do
  [ -n "$tag" ] || continue
  if [ "$tag" = "$current_tag" ] || [ "$tag" = "$exclude_tag" ]; then
    continue
  fi
  if is_release_tag "$tag"; then
    if [ -n "$current_tag" ] && [ "$(semver_compare "$tag" "$current_tag")" -ge 0 ]; then
      # The version sort may contain tags from a future branch. Compare with
      # the current tag instead of trusting position alone.
      continue
    fi
    prior="$tag"
    break
  fi
done <<EOF
$(git tag --sort=-version:refname)
EOF

if [ -z "$prior" ]; then
  prior="$fallback"
  git rev-parse --verify "$prior^{commit}" >/dev/null
  echo "No previous published release tag; using immutable fallback $prior" >&2
else
  echo "Using previous published release $prior ($(git rev-parse "$prior"))" >&2
fi

printf '%s\n' "$prior"
