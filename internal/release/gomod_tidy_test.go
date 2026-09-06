package release

import (
	"os"
	"os/exec"
	"testing"
)

// TestGoModTidyClean asserts that `go mod tidy -diff` reports no changes.
// make release-check runs this gate; this test keeps it green between
// releases so a stray indirect/direct reclassification cannot silently
// break the release pipeline.
func TestGoModTidyClean(t *testing.T) {
	if _, err := os.Stat("go.mod"); err != nil {
		t.Skipf("go.mod not found from test working directory: %v", err)
	}
	cmd := exec.Command("go", "mod", "tidy", "-diff")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			t.Fatalf("go mod tidy would change the module:\n%s", out)
		}
		t.Fatalf("go mod tidy -diff failed: %v\n%s", err, out)
	}
}
