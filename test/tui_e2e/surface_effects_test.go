//go:build linux

package tui_e2e

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTUILocalForwardCarriesTrafficAndClosesIt proves the local-only path at
// the boundary a user cares about: the compiled TUI creates the connection,
// the real supervisor starts the listener, bytes cross it, and the listener is
// gone after the TUI-driven close. A plan screen or a completed label alone
// would not establish any of those effects.
func TestTUILocalForwardCarriesTrafficAndClosesIt(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		_, _ = w.Write([]byte("forward-fixture-ok"))
	}))
	defer server.Close()

	remote := server.Listener.Addr().(*net.TCPAddr)
	localPort := freeTCPPort(t)

	s := f.startTUI(120, 40)
	s.waitFor("Nothing is published yet.")
	s.send("n")
	s.waitFor("NEW CONNECTION")
	s.chooseOutcome("Forward a local port")
	s.waitFor("Name this connection")
	s.send("ctrl-u")
	s.typeText("forward-effects")
	s.send("enter")
	s.waitFor("Local listening port:")
	s.typeText(strconv.Itoa(localPort))
	s.send("enter")
	s.waitFor("Remote host:")
	s.typeText("127.0.0.1")
	s.send("enter")
	s.waitFor("Remote port:")
	s.typeText(strconv.Itoa(remote.Port))
	s.send("enter")
	s.waitFor("Protocol:")
	// Both directions are physical terminal navigation. UDP is visibly offered
	// but disabled, so a human can see why TCP is the one that can be selected.
	s.send("down")
	s.send("enter")
	s.waitFor("UDP")
	if !strings.Contains(strings.ToLower(s.screen()), "cannot forward udp") {
		t.Fatalf("UDP choice was not explained as unavailable:\n%s", s.debug())
	}
	s.send("up")
	s.send("enter")
	s.waitFor("REVIEW")
	s.send("enter")
	s.waitFor("CONNECTION CREATED")
	s.waitFor("Connection saved (closed)")
	s.send("enter")
	s.waitFor("CONNECTIONS")
	s.waitFor("forward-effects")

	// Open through Home, approve the visible plan, then use the resulting
	// loopback endpoint as a real HTTP client.
	s.send(" ")
	s.waitFor("EXACTLY THESE STEPS")
	s.waitFor("Verify")
	s.send("enter")
	s.waitFor("Status: Completed")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(localPort) + "/through-forward")
	if err != nil {
		t.Fatalf("HTTP did not cross the TUI-created forward: %v\n%s", err, s.debug())
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || string(body) != "forward-fixture-ok" {
		t.Fatalf("forward response = %q, read error = %v", body, readErr)
	}
	select {
	case got := <-requests:
		if got != "GET /through-forward" {
			t.Fatalf("fixture saw %q, want forwarded request", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote fixture saw no forwarded request")
	}

	// Return to Home using Escape, then close with the same plan/apply flow.
	s.send("esc")
	s.waitFor("CONNECTIONS")
	s.send(" ")
	s.waitFor("EXACTLY THESE STEPS")
	s.send("enter")
	s.waitFor("Status: Completed")

	closedClient := &http.Client{Timeout: 500 * time.Millisecond}
	_, err = closedClient.Get("http://127.0.0.1:" + strconv.Itoa(localPort) + "/after-close")
	if err == nil {
		t.Fatal("a TUI-driven close left the local forward accepting traffic")
	}
	s.assertNoOverflow()
	s.send("ctrl-c")
	s.waitExit()
}

// TestTUIProviderSetupSecretAndSettingsNavigation proves that provider setup
// fields are physically typeable, secrets remain masked and redacted from the
// PTY transcript, guidance providers are not presented as fake forms, and a
// settings choice survives a supervisor restart.
func TestTUIProviderSetupSecretAndSettingsNavigation(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	s := f.startTUI(200, 60)
	s.waitFor("Nothing is published yet.")

	// Providers is a real hierarchical list. The selected provider marker moves
	// with arrows and vi keys, and the account actions are visible in the footer.
	// The terminal is large because the assertion is that every catalogued
	// provider is on one screen: at 120x40 the list legitimately needs paging,
	// and a truncated list would make this completeness claim unfalsifiable.
	s.send("p")
	s.waitFor("PROVIDERS")
	for _, provider := range []string{
		"Client-mediated MCP transport", "Cloudflare", "Mock Provider",
		"Local port forward", "Tailscale", "zrok",
	} {
		s.waitFor(provider)
	}
	if !strings.Contains(s.screen(), "> Client-mediated MCP transport") {
		t.Fatalf("provider cursor is not visible on the first row:\n%s", s.debug())
	}
	s.send("down")
	s.send("j")
	s.send("up")
	s.send("k")
	s.send("?")
	s.waitFor("HELP")
	s.send("esc")
	s.waitFor("PROVIDERS")
	// Empty provider rows expose disabled account actions with reasons rather
	// than making their keys silently inert.
	for _, check := range []struct {
		key, reason string
	}{
		{"v", "Verify is not available"},
		{"c", "Replace credential is not available"},
		{"x", "Remove account is not available"},
	} {
		s.send(check.key)
		s.waitFor(check.reason)
	}

	// Select Cloudflare and open its provider-declared setup form.
	s.send("down")
	s.waitFor("> Cloudflare")
	s.send("a")
	s.waitFor("SET UP CLOUDFLARE")
	canary := "TUI-SECRET-CANARY-DO-NOT-PRINT"
	s.waitFor("API token")
	s.typeSecretText(canary)
	// The screen proves presence without exposing the value. The mask is the
	// field's bullet echo (the contract NewSecretField declares); the raw PTY
	// stream and the command artifact must also remain clean.
	s.waitFor("••••••••")
	if strings.Contains(s.screen(), canary) || strings.Contains(string(s.rawOutput()), canary) {
		t.Fatal("provider secret appeared in the visible or raw PTY output")
	}
	// Escape from the first field exits setup and clears the secret rather than
	// leaving it resident in the provider form.
	s.send("esc")
	s.waitFor("PROVIDERS")
	if strings.Contains(s.screen(), canary) || strings.Contains(string(s.rawOutput()), canary) {
		t.Fatal("provider secret survived leaving setup")
	}

	// Re-enter the form to prove the token-first flow's discovery contract
	// end to end: enter validates the credential through the real IPC path,
	// discovery answers the identity question (single account resolves
	// without a choice list), and the zone question offers the no-zone
	// answer that says what it gives up. Backing out must leave nothing
	// stored.
	s.send("a")
	s.waitFor("API token")
	s.typeSecretText(canary)
	s.send("enter")
	s.waitFor("No zone")
	if strings.Contains(s.screen(), canary) || strings.Contains(string(s.rawOutput()), canary) {
		t.Fatal("provider secret survived validation")
	}
	s.send("enter") // no zone: temporary addresses only
	s.waitFor("Confirm")
	s.waitFor("Dev Account")
	s.send("esc")
	s.waitFor("Zone")
	s.send("esc") // past a discovery question rewinds to the credential
	s.waitFor("API token")
	s.send("esc")
	s.waitFor("PROVIDERS")
	if strings.Contains(s.screen(), canary) || strings.Contains(string(s.rawOutput()), canary) {
		t.Fatal("provider secret survived leaving setup")
	}

	// Setup's Tailscale route is guidance-only. It accepts no credential input,
	// which is materially different from a broken or unfinished form.
	s.send("esc")
	s.waitFor("Nothing is published yet.")
	s.send("s")
	s.waitFor("SET UP")
	// The setup screen's report action is physically driven. Read the generated
	// artifact through the path the TUI printed and verify it remains redacted.
	s.send("E")
	s.page("pgdown")
	s.waitFor("Wrote ")
	reportScreen := s.screen()
	start := strings.Index(reportScreen, "Wrote ") + len("Wrote ")
	end := strings.Index(reportScreen[start:], " —")
	if end < 0 {
		end = strings.Index(reportScreen[start:], " -")
	}
	if start == len("Wrote ") || end < 0 {
		t.Fatalf("support export did not expose a readable path:\n%s", s.debug())
	}
	reportPath := reportScreen[start : start+end]
	report, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read TUI support report %q: %v", reportPath, err)
	}
	t.Cleanup(func() { _ = os.Remove(reportPath) })
	if strings.Contains(string(report), canary) {
		t.Fatal("provider secret appeared in the support report")
	}
	s.send("home")
	s.waitFor("SET UP")

	// Select the Tailscale row by following the visible cursor. Each press is
	// followed by a wait for an actual repaint, so the loop cannot outrun the
	// terminal and walk past the row it is looking for.
	foundTailscale := false
	for range 12 {
		if strings.Contains(s.screen(), "> ✓ Tailscale") {
			foundTailscale = true
			break
		}
		before := s.screen()
		s.send("j")
		s.waitForScreenChange(before)
		if strings.Contains(s.screen(), "> ✓ Tailscale") {
			foundTailscale = true
			break
		}
	}
	if !foundTailscale {
		t.Fatalf("could not navigate the setup cursor to Tailscale:\n%s", s.debug())
	}
	s.send("enter")
	s.waitFor("SET UP TAILSCALE")
	s.waitFor("Portico holds no Tailscale credential")
	s.typeText("must-not-be-collected")
	if strings.Contains(s.screen(), "must-not-be-collected") {
		t.Fatal("read-only provider guidance accepted typed credential material")
	}
	s.send("?")
	s.waitFor("HELP")
	s.send("esc")
	// Closing the guidance form walks back through the screens it was shown
	// over: the accounts list, the Providers list, Setup, then Home. escUntil
	// follows the visible screens rather than assuming the stack depth.
	s.escUntil("Nothing is published yet.")

	// Settings are changed through the selected row, then read back after an
	// explicit supervisor restart. K rotates the installation key through the
	// same surface and reports the durable version without exposing key bytes.
	s.send("S")
	s.waitFor("SETTINGS")
	s.waitFor("At startup")
	s.send("enter")
	s.waitFor("nothing opens by itself")
	s.send("K")
	s.waitFor("Rotate the installation encryption key?")
	s.send("enter")
	s.waitFor("encryption key rotated")
	s.assertNoOverflow()
	s.send("q")
	s.waitExit()
	runCLI(t, f, "supervisor", "stop")
	runCLI(t, f, "supervisor", "start")

	s2 := f.startTUI(70, 20)
	s2.waitFor("Nothing is published yet.")
	s2.send("S")
	s2.waitFor("SETTINGS")
	s2.waitFor("nothing opens by itself")
	s2.send("ctrl-c")
	s2.waitExit()

}

// TestTUIWizardDirectoryAndCommandFields proves the two Portico-owned source
// forms that are easy to mistake for placeholders: a directory and a command.
// Both are created from the TUI, every source-specific field is filled there,
// and the result is inspected through the TUI rather than by reading the
// request object in a component test.
func TestTUIWizardDirectoryAndCommandFields(t *testing.T) {
	requireE2E(t)

	t.Run("directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("directory-fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		f := newFixture(t)
		s := f.startTUI(120, 40)
		s.waitFor("Nothing is published yet.")
		s.send("n")
		s.waitFor("NEW CONNECTION")
		for range 2 {
			s.send("down")
		}
		s.send("enter")
		s.waitFor("Name this connection")
		s.send("ctrl-u")
		s.typeText("directory-closed")
		s.send("enter")
		s.waitFor("Enter the directory path to serve:")
		s.pasteText(root)
		s.send("enter")
		s.waitFor("How should Portico serve this directory?")
		// Both directory modes are reachable. Select the second, then return to
		// the read-only static mode so the subsequent SPA choice is exercised.
		s.send("down")
		s.send("up")
		s.send("enter")
		s.waitFor("Enable SPA fallback")
		s.send("down")
		s.send("up")
		s.send("enter")
		s.waitFor("How should it be reachable?")
		s.send("enter")
		s.waitFor("Who should be able to reach it?")
		s.send("enter")
		s.waitFor("Which provider should carry the connection?")
		s.send("enter")
		s.waitFor("REVIEW")
		s.page("pgdown")
		s.waitFor("Save it, closed")
		s.send("enter")
		s.waitFor("CONNECTION CREATED")
		s.waitFor("Connection saved (closed)")
		s.send("enter")
		s.waitFor("CONNECTIONS")
		s.waitFor("directory-closed")
		s.send("enter")
		s.waitFor("CONNECTION DETAILS")
		s.waitFor("Portico")
		s.assertNoOverflow()
		s.send("ctrl-c")
		s.waitExit()
	})

	t.Run("command", func(t *testing.T) {
		workdir := t.TempDir()
		f := newFixture(t)
		s := f.startTUI(120, 40)
		s.waitFor("Nothing is published yet.")
		s.send("n")
		s.waitFor("NEW CONNECTION")
		s.chooseOutcome("Something else (choose the source yourself)")
		s.waitFor("What should be reachable?")
		for range 2 {
			s.send("down")
		}
		s.send("enter")
		s.waitFor("Name this connection")
		s.send("ctrl-u")
		s.pasteText("command-closed")
		s.send("enter")
		s.waitFor("Enter the command executable to run:")
		port := freeTCPPort(t)
		s.pasteText("python3 -m http.server " + strconv.Itoa(port) + " --bind 127.0.0.1")
		s.send("enter")
		s.waitFor("Local port for the command (required):")
		s.typeText(strconv.Itoa(port))
		s.send("enter")
		s.waitFor("Command arguments (space separated; quote any argument containing spaces; empty to skip):")
		s.pasteText("-m http.server " + strconv.Itoa(port) + " --bind 127.0.0.1")
		s.send("enter")
		s.waitFor("Working directory (empty to use Portico's):")
		s.pasteText(workdir)
		s.send("enter")
		s.waitFor("How should Portico run this command?")
		// Keep direct execution selected; the shell option is still physically
		// reachable and then deliberately rejected because argv is present.
		s.send("down")
		s.send("enter")
		s.waitFor("cannot also receive")
		s.send("up")
		s.send("enter")
		s.waitFor("Environment for the command (NAME=VALUE, comma separated; empty for none):")
		s.pasteText("PORTICO_E2E_FIELD=ok")
		s.send("enter")
		s.waitFor("How should it be reachable?")
		s.send("enter")
		s.waitFor("Who should be able to reach it?")
		s.send("enter")
		s.waitFor("Which provider should carry")
		s.send("enter")
		s.waitFor("REVIEW")
		s.page("pgdown")
		s.waitFor("Save it, closed")
		s.send("enter")
		s.waitFor("CONNECTION CREATED")
		s.waitFor("Connection saved (closed)")
		s.send("enter")
		s.waitFor("CONNECTIONS")
		s.waitFor("command-closed")
		s.assertNoOverflow()
		s.send("ctrl-c")
		s.waitExit()
	})
}

// TestTUIFailedCommandOperationAndDiagnosisNavigation proves that an origin
// failure is a visible terminal state, not a spinner that traps the user.
// The command exits before it can listen, so the real supervisor fails the
// operation through the same path a bad executable or occupied port would use.
func TestTUIFailedCommandOperationAndDiagnosisNavigation(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)
	s := f.startTUI(120, 40)
	s.waitFor("Nothing is published yet.")
	s.send("n")
	s.waitFor("NEW CONNECTION")
	s.chooseOutcome("Something else (choose the source yourself)")
	s.waitFor("What should be reachable?")
	for range 2 {
		s.send("down")
	}
	s.send("enter")
	s.waitFor("Name this connection")
	s.send("ctrl-u")
	s.typeText("failed-command")
	s.send("enter")
	s.waitFor("Enter the command executable to run:")
	s.pasteText("sh -c 'exit 7'")
	s.send("enter")
	s.waitFor("Local port for the command (required):")
	s.typeText(strconv.Itoa(freeTCPPort(t)))
	s.send("enter")
	s.waitFor("Command arguments (space separated; quote any argument containing spaces; empty to skip):")
	s.send("enter")
	s.waitFor("Working directory (empty to use Portico's):")
	s.send("enter")
	s.waitFor("How should Portico run this command?")
	s.send("enter")
	s.waitFor("Environment for the command (NAME=VALUE, comma separated; empty for none):")
	s.send("enter")
	s.waitFor("How should it be reachable?")
	s.send("enter")
	s.waitFor("Who should be able to reach it?")
	s.send("enter")
	s.waitFor("Which provider should carry")
	s.send("enter")
	s.waitFor("REVIEW")
	s.send("enter")
	s.waitFor("CONNECTION CREATED")
	s.waitFor("Connection saved (closed)")
	s.send("enter")
	s.waitFor("CONNECTIONS")
	s.waitFor("failed-command")

	s.send(" ")
	s.waitFor("EXACTLY THESE STEPS")
	s.send("enter")
	s.waitFor("Status: Failed")
	if !strings.Contains(strings.ToLower(s.screen()), "error") {
		t.Fatalf("failed operation did not expose an error:\n%s", s.debug())
	}

	// Escape returns to Home, where the failed connection remains selectable;
	// the repair key must open diagnosis rather than leave the user at a dead
	// end. Exercise help, paging and both return paths on the no-data outcome.
	s.send("esc")
	s.waitFor("CONNECTIONS")
	s.send("r")
	s.waitFor("REPAIR: failed-command")
	s.waitFor("No findings")
	s.send("?")
	s.waitFor("HELP")
	s.send("esc")
	s.waitFor("REPAIR: failed-command")
	s.send("pgdown")
	s.send("pgup")
	s.send("home")
	s.send("end")
	s.assertNoOverflow()
	s.send("esc")
	s.waitFor("CONNECTIONS")
	s.send("ctrl-c")
	s.waitExit()
}
