package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/B-A-M-N/portico/internal/app"
)

// doctorSocketDiagnostic is a read-only local check that runs when no
// supervisor answers. It distinguishes an ordinary stopped supervisor from a
// stale or hostile socket left behind by a crashed one (audit P1-13).
// Doctor never unlinks anything here — cleanup stays explicit
// (`portico supervisor run --force` / the repair command).
func doctorSocketDiagnostic(socketPath string) {
	_ = printDoctorCheck(os.Stdout, doctorSocketCheck(socketPath))
}

func doctorSocketCheck(socketPath string) DoctorCheck {
	dir := filepath.Dir(socketPath)
	check := DoctorCheck{ID: "socket", Title: "Connection socket"}

	// Runtime directory sanity.
	info, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		check.State = "attention"
		check.Summary = fmt.Sprintf("Runtime directory %s does not exist yet; nothing has started on this account.", dir)
		return check
	case err != nil:
		check.State = "unknown"
		check.Summary = "Portico could not inspect its runtime directory."
		check.Technical = err.Error()
		return check
	case info.Mode()&os.ModeSymlink != 0:
		check.State = "problem"
		check.Summary = fmt.Sprintf("Runtime directory %s is a symlink; refusing to reason about it.", dir)
		check.Classification = "insecure_runtime_directory"
		return check
	case !info.IsDir():
		check.State = "problem"
		check.Summary = fmt.Sprintf("Runtime directory %s is not a directory (%s).", dir, info.Mode())
		check.Classification = "insecure_runtime_directory"
		return check
	}

	// The socket itself.
	socketInfo, err := os.Lstat(socketPath)
	switch {
	case os.IsNotExist(err):
		check.State = "attention"
		check.Summary = "Socket is not present; the supervisor is simply stopped."
		return check
	case err != nil:
		check.State = "unknown"
		check.Summary = "Portico could not inspect its socket."
		check.Technical = err.Error()
		return check
	case socketInfo.Mode()&os.ModeSocket == 0:
		check.State = "problem"
		check.Summary = fmt.Sprintf("Socket path %s exists but is not a unix socket (%s).", socketPath, socketInfo.Mode().Type())
		check.Detail = "Something else created a file there; the supervisor cannot start until it is removed."
		check.Classification = "stale_socket"
		check.NextAction = "Inspect it, then remove it by hand or run: portico supervisor run --force."
		return check
	}

	if mode := socketInfo.Mode().Perm(); mode&0o077 != 0 {
		check.State = "problem"
		check.Summary = fmt.Sprintf("Socket permissions are %04o; expected 0600. Other users on this machine may reach it.", mode)
		check.Classification = "insecure_socket"
		check.NextAction = "Restart the supervisor, which recreates the socket with the right permissions."
		return check
	}

	// Probe: if something still answers on the socket, the supervisor IS
	// alive and only the health request failed.
	if answersOnSocket(socketPath) {
		check.State = "attention"
		check.Summary = "Socket is present and answering connections; the health check failed for another reason."
		return check
	}

	// A socket file nobody is listening on = stale.
	lockPath := filepath.Join(dir, "portico-supervisor.lock")
	stale := "no lock evidence"
	if lockBytes, err := os.ReadFile(lockPath); err == nil && len(lockBytes) > 0 {
		stale = fmt.Sprintf("lock file names %s", strings.TrimSpace(string(lockBytes)))
	}
	check.State = "problem"
	check.Classification = "stale_socket"
	check.Summary = fmt.Sprintf("Stale socket detected: nothing is listening on %s (%s).", socketPath, stale)
	check.NextAction = "Remove it explicitly with: portico doctor repair --stale-socket."
	return check
}

func answersOnSocket(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// handleRepairStaleSocket is the explicit repair action for a stale supervisor
// socket (audit P1-14). It previews exactly what will be removed, refuses
// when a live supervisor is answering on the socket, and never touches
// provider resources or durable state.
func handleRepairStaleSocket(cmd *cobra.Command) error {
	if ok, _ := cmd.Flags().GetBool("stale-socket"); !ok {
		return fmt.Errorf("name what to repair: portico doctor repair --stale-socket")
	}
	launcher := app.NewLauncher()
	return repairStaleSocket(launcher.GetPaths().SocketPath, cmd)
}

// repairStaleSocket performs the check-and-remove against an explicit socket
// path so tests can exercise it without touching the user's real runtime dir.
func repairStaleSocket(socketPath string, cmd *cobra.Command) error {
	info, err := os.Lstat(socketPath)
	if os.IsNotExist(err) {
		fmt.Println("Nothing to repair: no socket exists at")
		fmt.Printf("  %s\n", socketPath)
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		fmt.Printf("The path exists but is NOT a unix socket:\n  %s (%s)\n", socketPath, info.Mode().Type())
		fmt.Println("Refusing to remove it automatically: inspect it by hand first.")
		return fmt.Errorf("refusing to remove a non-socket file at the socket path")
	}

	// A live supervisor answers here — removal would break every client.
	if answersOnSocket(socketPath) {
		fmt.Println("A supervisor is answering on this socket; there is nothing stale to repair.")
		return fmt.Errorf("supervisor appears to be running")
	}

	fmt.Printf("About to remove the stale socket:\n  %s\n", socketPath)
	fmt.Println("No other state (database, credentials, keys, logs) will be touched.")
	skipConfirm, _ := cmd.Flags().GetBool("yes")
	if !skipConfirm && !confirmPrompt("Remove the stale socket?") {
		fmt.Println("Aborted. Nothing was changed.")
		return nil
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	fmt.Println("Stale socket removed. Start the supervisor with: portico supervisor run")
	return nil
}

func confirmPrompt(question string) bool {
	fmt.Printf("%s [y/N]: ", question)
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
