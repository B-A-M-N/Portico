#!/usr/bin/env bash
#
# Verify that every acceptance matrix row points at a test that exists and
# passes.
#
# The matrix is written by hand and cites test names. Nothing checked that the
# names were real: the first draft of ACCEPTANCE_MATRIX.md cited ten rows whose
# -run patterns matched no test at all, because pipes escaped for the markdown
# table became literal backslash-pipes in Go's regexp. Those rows looked
# authoritative and verified nothing.
#
# A matrix that cites tests which do not run is worse than no matrix, so this
# runs it rather than trusting it.

set -uo pipefail

MATRICES=(docs/ACCEPTANCE_MATRIX.md docs/AUDIT_ACCEPTANCE_MATRIX.md)
failures=0
commands=0
missing_names=0

report_dir="${ACCEPTANCE_REPORT_DIR:-.}"
report="${report_dir}/acceptance-report.txt"

{
  echo "Portico acceptance matrix verification"
  echo "commit:     $(git rev-parse HEAD 2>/dev/null || echo unknown)"
  echo "go version: $(go version)"
  echo "date:       $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
} > "$report"

# --- every cited test name must exist ---------------------------------------
#
# A name in the Test column that no longer exists is a row describing behaviour
# nothing holds.
echo "Checking that every cited test name exists..."
all_tests="$(go test ./... -list '.*' 2>/dev/null | grep -E '^Test' | sort -u)"

for matrix in "${MATRICES[@]}"; do
  [ -f "$matrix" ] || continue
  # Test names are cited in backticks, always starting with Test.
  grep -o '`Test[A-Za-z0-9_]*`' "$matrix" | tr -d '`' | sort -u | while read -r name; do
    if ! grep -qx "$name" <<< "$all_tests"; then
      echo "  MISSING: $matrix cites $name, which does not exist"
    fi
  done
done > /tmp/acceptance_missing.$$ 2>&1

if [ -s /tmp/acceptance_missing.$$ ]; then
  cat /tmp/acceptance_missing.$$ | tee -a "$report"
  missing_names=$(wc -l < /tmp/acceptance_missing.$$)
fi
rm -f /tmp/acceptance_missing.$$

# --- every cited command must run and pass ----------------------------------
echo "Running every command the matrix cites..."
for matrix in "${MATRICES[@]}"; do
  [ -f "$matrix" ] || continue
  grep -o '`go test [^`]*`' "$matrix" | tr -d '`' | sort -u | while read -r cmd; do
    echo "CMD $cmd"
  done
done | sort -u > /tmp/acceptance_cmds.$$

while read -r line; do
  cmd="${line#CMD }"
  commands=$((commands + 1))
  out="$(eval "$cmd" 2>&1)"
  status=$?
  if [ $status -ne 0 ]; then
    echo "  FAILED: $cmd" | tee -a "$report"
    echo "$out" | grep -E '^(---|FAIL|.*\.go:)' | head -3 | sed 's/^/      /' | tee -a "$report"
    failures=$((failures + 1))
  elif grep -q 'no tests to run' <<< "$out"; then
    # A command that matches nothing passes trivially and proves nothing.
    echo "  MATCHES NOTHING: $cmd" | tee -a "$report"
    failures=$((failures + 1))
  else
    echo "  ok: $cmd" >> "$report"
  fi
done < /tmp/acceptance_cmds.$$
commands=$(wc -l < /tmp/acceptance_cmds.$$)
rm -f /tmp/acceptance_cmds.$$

# Counted from the report, not from shell variables. An increment inside a
# loop that turns out to be a subshell is silently lost, and a verification
# script that reports success while listing failures is worse than none.
problems=$(grep -cE '^  (FAILED|MATCHES NOTHING|  MISSING|MISSING):' "$report" || true)

{
  echo
  echo "commands run:      $commands"
  echo "problems found:    $problems"
} >> "$report"

echo
echo "Ran $commands matrix commands."
echo "Report written to $report"

if [ "$problems" -ne 0 ]; then
  echo
  echo "The acceptance matrix does not match the tests: $problems problems."
  echo "See $report."
  exit 1
fi

echo "Every matrix row points at a test that exists and passes."
