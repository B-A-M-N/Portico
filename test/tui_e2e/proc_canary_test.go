//go:build linux

package tui_e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTUISecretNeverEntersProcessCommandLines is the /proc canary row of the
// audit's security matrix. A canary secret is configured through the real
// provider-credential path, a connection that uses it is created, and while
// the supervisor is alive its /proc/[pid]/cmdline and every connector it may
// have spawned are scanned for the canary. A secret that leaked into a command
// line would be visible to any local user via /proc regardless of file
// permissions — argv is not a safe place for secrets.
//
// Command arguments are already refused at the CLI boundary; this proves the
// stored-value path stays out too: configuring the secret from stdin and
// carrying it through account resolution never rebuilds a process argv
// containing it. The log and a support export are scanned as the adjacent
// surfaces that capture a leaked secret.
func TestTUISecretNeverEntersProcessCommandLines(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	account := "acct-proc-canary"
	canary := "PROC-CANARY-7f2b9a4e11"
	runCLIWithStdin(t, f, canary+"\n",
		"provider", "login", "mock", "--set", "account_id="+account,
		"--credential-stdin")

	// Bind a connection to the account and open it, so the account's
	// credential is resolved and in use while the process scan runs.
	fixtureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fixture"))
	}))
	defer fixtureServer.Close()
	runCLI(t, f, "create", "proc-canary-demo", "--provider", "mock",
		"--source", "http://"+fixtureServer.Listener.Addr().String(),
		"--source-type", "existing_service", "--health-enabled=false",
		"--account-id", account)
	connID := findConnectionID(t, f, "proc-canary-demo")
	runCLI(t, f, "open", connID, "--yes")

	// The supervisor for THIS fixture is the one whose environment carries the
	// fixture's XDG_DATA_HOME. A stray supervisor from another fixture shares
	// the binary but not the data root, so the scan keys on isolation.
	dataRoot := xdgEnv(f, "XDG_DATA_HOME")
	if dataRoot == "" {
		t.Fatalf("fixture env did not set XDG_DATA_HOME")
	}
	supPID := findSupervisorForDataRoot(t, dataRoot)
	if supPID == 0 {
		t.Fatalf("no supervisor process found for data root %s", dataRoot)
	}

	cmdline := procCmdline(t, supPID)
	if strings.Contains(cmdline, canary) {
		t.Fatalf("the canary secret leaked into supervisor argv (pid %d):\n%s", supPID, cmdline)
	}

	// Any connector subprocess (cloudflared and friends) that could carry the
	// secret in its own args must be clean too.
	for _, pid := range connectorPIDs(t) {
		line := procCmdline(t, pid)
		if strings.Contains(line, canary) {
			t.Fatalf("the canary secret leaked into connector argv (pid %d):\n%s", pid, line)
		}
	}

	// The supervisor log is a second leak surface: a secret logged is a secret
	// captured in support bundles and rotated diagnostics.
	if logPath := supervisorLogPath(t, f); logPath != "" {
		if data, err := os.ReadFile(logPath); err == nil && strings.Contains(string(data), canary) {
			t.Fatalf("the canary secret appeared in the supervisor log:\n%s", logPath)
		}
	}

	// A support export must not carry the secret either.
	exportPath := filepath.Join(t.TempDir(), "report.json")
	runCLI(t, f, "support", "export", "--output", exportPath)
	if data, err := os.ReadFile(exportPath); err == nil && strings.Contains(string(data), canary) {
		t.Fatalf("the canary secret appeared in the support export:\n%s", exportPath)
	}
}

// --- helpers ---

func xdgEnv(f *fixture, key string) string {
	prefix := key + "="
	for _, item := range f.env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

// findSupervisorForDataRoot returns the PID of the live `portico supervisor
// run` process whose XDG_DATA_HOME equals the fixture's, 0 if none.
func findSupervisorForDataRoot(t *testing.T, dataRoot string) int {
	t.Helper()
	for _, pid := range procPIDs(t, "portico") {
		if envValue(pid, "XDG_DATA_HOME") == dataRoot {
			return pid
		}
	}
	return 0
}

// envValue reads one environment variable of a live process from /proc.
func envValue(pid int, key string) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, item := range strings.Split(string(data), "\x00") {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

// connectorPIDs returns the PIDs of connector binaries Portico could spawn.
// The mock provider runs in-process, so on the common path this is empty; the
// scan still guards the day a real connector is configured.
func connectorPIDs(t *testing.T) []int {
	t.Helper()
	return procPIDs(t, "cloudflared")
}

// procCmdline reads and NUL-normalizes /proc/<pid>/cmdline.
func procCmdline(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(data), "\x00", " ")
}

// procPIDs returns the PIDs of live processes whose executable basename is
// name.
func procPIDs(t *testing.T, name string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("list /proc: %v", err)
	}
	var pids []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid == 0 {
			continue
		}
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			continue
		}
		if filepath.Base(exe) == name {
			pids = append(pids, pid)
		}
	}
	return pids
}

// supervisorLogPath locates the fixture's supervisor.log under its root.
func supervisorLogPath(t *testing.T, f *fixture) string {
	t.Helper()
	p, err := findFile(f.root, "supervisor.log")
	if err != nil {
		return ""
	}
	return p
}