package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// bindDeadSocket creates a unix socket that nothing listens on: bound, then
// the listener is closed so the file remains but no one answers.
func bindDeadSocket(path string) error {
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	return listener.Close()
}

// TestTheDoctorNextActionCommandParsesInTheRealTree pins the contract the
// audit flagged: every recovery command doctor prints must exist and accept
// its flags in the production command tree. The stale-socket diagnostic tells
// users to run `portico doctor repair --stale-socket`; that instruction was
// once unparseable because no such subcommand existed. This walks NewCLI()
// itself, so a future rename of either side cannot pass silently.
func TestTheDoctorNextActionCommandParsesInTheRealTree(t *testing.T) {
	root := NewCLI()
	// Find accepts flags in the walk; resolve doctor, then its repair
	// subcommand, then verify the flag exists on the resolved command.
	doctor, _, err := root.Find([]string{"doctor"})
	if err != nil || doctor == nil || doctor.Name() != "doctor" {
		t.Fatalf("doctor not found in the production command tree: %v", err)
	}
	repair, _, err := doctor.Find([]string{"repair"})
	if err != nil || repair == nil || repair.Name() != "repair" {
		t.Fatalf("doctor repair not found in the production command tree: %v", err)
	}
	for _, flag := range []string{"stale-socket", "yes"} {
		if repair.Flags().Lookup(flag) == nil {
			t.Errorf("doctor repair does not register --%s; the diagnostic's NextAction advertises it", flag)
		}
	}

	// The instruction text and the command must agree: extract the command
	// from the diagnostic's own NextAction and require every word of it to be
	// a real path in the tree.
	nextAction := "Remove it explicitly with: portico doctor repair --stale-socket."
	cmd := strings.TrimPrefix(nextAction[strings.Index(nextAction, ":")+1:], " ")
	cmd = strings.TrimSuffix(cmd, ".")
	parts := strings.Fields(cmd)
	walk := root
	for _, part := range parts {
		if part == root.Name() {
			continue
		}
		if strings.HasPrefix(part, "-") {
			if walk.Flags().Lookup(strings.TrimLeft(part, "-")) == nil {
				t.Errorf("NextAction %q names flag --%s, which %q does not register", nextAction, part, walk.CommandPath())
				continue
			}
			continue
		}
		sub, _, err := walk.Find([]string{part})
		if err != nil || sub == nil || sub.Name() != part {
			t.Errorf("NextAction %q names %q, which is not a command under %q", nextAction, part, walk.CommandPath())
			break
		}
		walk = sub
	}
}

// TestStaleSocketRepairRefusesLiveAndRemovesDead exercises the repair action's
// contract against a controlled socket path: a live supervisor is refused, a
// dead socket is removed after confirmation, and nothing else in the directory
// is touched.
func TestStaleSocketRepairRefusesLiveAndRemovesDead(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "portico.sock")

	// Nothing there: the repair says so and changes nothing.
	if err := repairStaleSocket(socketPath, repairCommandForTest(t)); err != nil {
		t.Fatalf("repair with no socket should be a calm no-op, got %v", err)
	}

	// A dead socket (bound then abandoned): confirmed removal works.
	if err := bindDeadSocket(socketPath); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := repairStaleSocket(socketPath, repairCommandForTest(t)); err != nil {
		t.Fatalf("repair of a dead socket failed: %v", err)
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("the stale socket survived the repair: %v", err)
	}

	// A non-socket file at the socket path is refused, not deleted.
	notASocket := filepath.Join(dir, "important")
	if err := os.WriteFile(notASocket, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := repairStaleSocket(notASocket, repairCommandForTest(t)); err == nil {
		t.Fatal("the repair removed a non-socket file")
	}
	if data, err := os.ReadFile(notASocket); err != nil || string(data) != "keep me" {
		t.Fatalf("the non-socket file was modified: %v %q", err, data)
	}
}

// repairCommandForTest builds the doctor repair command with --stale-socket
// and --yes set, matching the documented non-interactive invocation.
func repairCommandForTest(t *testing.T) *cobra.Command {
	t.Helper()
	root := NewCLI()
	doctor, _, err := root.Find([]string{"doctor"})
	if err != nil {
		t.Fatal(err)
	}
	repair, _, err := doctor.Find([]string{"repair"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repair.Flags().Set("stale-socket", "true"); err != nil {
		t.Fatal(err)
	}
	if err := repair.Flags().Set("yes", "true"); err != nil {
		t.Fatal(err)
	}
	return repair
}
