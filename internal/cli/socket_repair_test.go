package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Audit P1-13/14 acceptance: doctor's local diagnostic distinguishes the
// stale-socket case from an ordinary stopped supervisor, and the explicit
// repair removes only a genuine stale socket — never a live one, never a
// non-socket file, and nothing else.

func TestDoctorSocketDiagnosticClassifiesStates(t *testing.T) {
	t.Run("no runtime dir", func(t *testing.T) {
		doctorSocketDiagnostic(filepath.Join(t.TempDir(), "missing", "portico.sock"))
	})

	t.Run("stopped supervisor (no socket)", func(t *testing.T) {
		dir := t.TempDir()
		doctorSocketDiagnostic(filepath.Join(dir, "portico.sock"))
	})

	t.Run("non-socket file at socket path", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "portico.sock")
		if err := os.WriteFile(path, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
		doctorSocketDiagnostic(path)
	})
}

// repairCmdForTest builds a repair command with the confirmation skipped.
func repairCmdForTest(yes bool) *cobra.Command {
	root := newRepairCmd()
	if yes {
		root.Flags().Set("yes", "true")
	}
	return root
}

func TestRepairStaleSocketRemovesOnlyGenuinelyStaleSockets(t *testing.T) {
	t.Run("stale socket removed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "portico.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		// Close WITHOUT unlinking: exactly what a crashed process leaves
		// behind (Go's default is to unlink on Close, so disable that).
		if ul, ok := ln.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false)
		}
		ln.Close()

		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			t.Fatal("fixture failed to create a lingering socket file")
		}

		cmd := repairCmdForTest(true)
		if err := repairStaleSocket(path, cmd); err != nil {
			t.Fatalf("repair: %v", err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("the stale socket was not removed")
		}
	})

	t.Run("live socket refused", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "portico.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()

		cmd := repairCmdForTest(true)
		if err := repairStaleSocket(path, cmd); err == nil {
			t.Fatal("repair removed a socket a live supervisor is answering on")
		}
		if _, err := os.Lstat(path); err != nil {
			t.Fatal("the live socket file was removed")
		}
	})
}

// TestDoctorPrintedCommandExecutes pins the audit finding that Doctor
// advertised `portico doctor repair --stale-socket` and no such command
// existed. It drives the real command tree — doctor's own repair subcommand,
// not a shortcut to the internal function — against an isolated runtime dir,
// with the exact flag spelling the diagnostic prints.
func TestDoctorPrintedCommandExecutes(t *testing.T) {
	dir := t.TempDir()
	sockDir := filepath.Join(dir, "portico")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sockDir, "portico.sock")

	// A crashed supervisor's leftover: a bound-then-closed unix socket.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	ln.Close()
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("fixture failed to create a lingering socket file")
	}

	// The real doctor command tree, run through the root so argument routing
	// is exercised, not just the handler.
	root := NewCLI()
	root.SetArgs([]string{"doctor", "repair", "--stale-socket", "--yes"})

	// Redirect the socket the launcher resolves to, so the test never touches
	// the machine's real runtime directory.
	t.Setenv("XDG_RUNTIME_DIR", dir)

	if err := root.Execute(); err != nil {
		t.Fatalf("the exact command Doctor prints failed: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("the advertised command did not remove the stale socket")
	}
}

// TestDoctorRepairWithoutTargetExplainsItself pins that a bare `doctor repair`
// does not guess: it names the flag the diagnostic tells users to pass.
func TestDoctorRepairWithoutTargetExplainsItself(t *testing.T) {
	root := NewCLI()
	root.SetArgs([]string{"doctor", "repair"})
	err := root.Execute()
	if err == nil {
		t.Fatal("a bare doctor repair did nothing and reported success")
	}
	if !strings.Contains(err.Error(), "--stale-socket") {
		t.Fatalf("the refusal does not name the real flag: %v", err)
	}
}
