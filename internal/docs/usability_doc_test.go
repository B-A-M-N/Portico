package docs_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestUSABILITYInvariantsCitedTestsExist pins the documentation-truth rule the
// audit applied to this document: every test file USABILITY_INVARIANTS.md
// names as covering its invariants must exist. The previous revision cited
// `internal/tui/screens/usability_test.go` and `internal/tui/home_search_test.go`,
// neither of which was ever in the repository — a claim of coverage with no
// coverage behind it.
func TestUSABILITYInvariantsCitedTestsExist(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "USABILITY_INVARIANTS.md"))
	if err != nil {
		t.Fatalf("read USABILITY_INVARIANTS.md: %v", err)
	}

	// Any backticked path ending in _test.go is a claim that this test exists
	// and runs.
	cited := regexp.MustCompile("`([A-Za-z0-9_./-]+_test\\.go)`")
	seen := map[string]bool{}
	for _, m := range cited.FindAllStringSubmatch(string(doc), -1) {
		path := m[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		full := filepath.Join(repoRoot(t), path)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("USABILITY_INVARIANTS.md cites %q, which does not exist in the repository", path)
			continue
		}
		// A cited test must also be a real Go file in this module.
		data, err := os.ReadFile(full)
		if err != nil {
			t.Errorf("read cited test %q: %v", path, err)
			continue
		}
		if !strings.Contains(string(data), "func Test") {
			t.Errorf("USABILITY_INVARIANTS.md cites %q, which contains no tests", path)
		}
	}
	if len(seen) == 0 {
		t.Fatal("USABILITY_INVARIANTS.md names no test files; the coverage claim is unverifiable")
	}
}
