//go:build linux

package tui_e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestFreshInstallTUIHomeHelpAndNavigation(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)
	s := f.startTUI(80, 24)

	s.waitFor("Nothing is published yet.")
	s.assertNoOverflow()

	// Help, scrolling, and back are driven through terminal escape sequences,
	// not Bubble Tea messages or model methods.
	s.send("?")
	s.waitFor("HELP")
	s.send("pgdown")
	s.waitFor("Describe something you want reachable")
	s.send("home")
	s.waitFor("HELP")
	for _, size := range [][2]int{{200, 60}, {120, 40}, {100, 30}, {80, 24}, {70, 20}, {60, 18}} {
		s.resize(size[0], size[1])
		s.send("home")
		s.waitFor("HELP")
		s.send("pgdown")
		s.send("pgup")
		s.send("home")
		s.send("end")
		s.assertNoOverflow()
	}
	s.send("esc")
	s.waitFor("Nothing is published yet.")

	// Exercise each fresh-install Home route that is advertised and return by
	// the same physical Back key a user has. This catches route wiring errors
	// that component tests can miss while still keeping the fixture empty.
	for _, route := range []struct {
		key, heading string
	}{
		{"a", "DISCOVER LOCAL SERVICES"},
		{"p", "PROVIDERS"},
		{"s", "Launch mode:"},
		{"S", "SETTINGS"},
		{"o", "HISTORY"},
		{"n", "NEW CONNECTION"},
	} {
		s.send(route.key)
		s.waitFor(route.heading)
		// Drive both physical arrow keys and the vi aliases on every list-like
		// screen, including empty states where the action is visibly disabled.
		// A human must be able to try the obvious movement keys without being
		// stranded or changing the meaning of the next keypress.
		for _, key := range []string{"up", "k", "down", "j"} {
			s.send(key)
		}
		// Help and paging are global navigation even when a screen-local form
		// owns the rest of the keyboard. Exercise the route from each screen and
		// return through the same physical Escape key.
		s.send("?")
		s.waitFor("HELP")
		s.send("esc")
		s.waitFor(route.heading)
		for _, key := range []string{"pgdown", "pgup", "home", "end"} {
			s.send(key)
		}
		s.assertNoOverflow()
		s.send("esc")
		s.waitFor("Nothing is published yet.")
	}

	// q is the Home quit action; Ctrl+C is separately tested as the universal
	// emergency quit path in the harness cleanup and lifecycle test.
	s.send("q")
	s.waitExit()
}

func TestTUIPlanCancelApplyOpenAndSupervisorRestart(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)
	fixtureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fixture"))
	}))
	defer fixtureServer.Close()

	port := fixtureServer.Listener.Addr().String()
	runCLI(t, f, "create", "demo", "--provider", "mock", "--source", port,
		"--source-type", "existing_service", "--health-enabled=false")

	s := f.startTUI(120, 40)
	s.waitFor("CONNECTIONS")
	s.waitFor("demo")
	s.assertNoOverflow()

	// The same connection enters inspect, previews an open plan, cancels it,
	// previews again, then applies it. The assertion follows the visible screen
	// and the post-operation state, rather than inspecting IPC responses.
	s.send("enter")
	s.waitFor("demo")
	for _, tab := range []struct {
		key, marker string
	}{
		{"right", "[Route]"},
		{"right", "[Activity]"},
		{"right", "[Technical]"},
		{"right", "[Logs]"},
		{"left", "[Technical]"},
		{"h", "[Activity]"},
		{"l", "[Technical]"},
		{"left", "[Activity]"},
		{"left", "[Route]"},
	} {
		s.send(tab.key)
		s.waitFor(tab.marker)
	}
	s.send("?")
	s.waitFor("HELP")
	s.send("esc")
	s.waitFor("[Route]")
	s.send(" ")
	s.waitFor("EXACTLY THESE STEPS")
	s.waitFor("Through:    Mock Provider")
	s.send("esc")
	s.waitFor("demo")
	s.send(" ")
	s.waitFor("EXACTLY THESE STEPS")
	// Resize immediately after approval while the operation is being launched;
	// the completed state is asserted only after the resized screen is observed.
	s.send("enter")
	s.resize(100, 30)
	s.waitFor("Status: Completed")
	s.assertNoOverflow()

	// Exercise the operation screen's terminal navigation while it is live. The
	// real PTY receives terminal bytes even when an action is unavailable here.
	s.send("right")
	s.send("right")
	s.send("left")
	s.waitFor("Status: Completed")
	s.assertNoOverflow()
	s.send("esc")
	s.waitFor("demo")
	s.send("esc")
	s.waitFor("CONNECTIONS")
	s.waitFor("demo")

	// Closing the TUI leaves the supervisor alive. A new real TUI process must
	// reconcile against the same durable profile after the supervisor restarts.
	s.send("q")
	s.waitExit()
	runCLI(t, f, "supervisor", "stop")
	runCLI(t, f, "supervisor", "start")

	s2 := f.startTUI(80, 24)
	s2.waitFor("CONNECTIONS")
	s2.waitFor("demo")
	s2.waitFor("Open")
	s2.assertNoOverflow()
	s2.send("ctrl-c")
	s2.waitExit()
}

func TestTUIEditCopyAndDeletePlanNavigation(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)
	fixtureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fixture"))
	}))
	defer fixtureServer.Close()

	runCLI(t, f, "create", "demo", "--provider", "mock",
		"--source", fixtureServer.Listener.Addr().String(),
		"--source-type", "existing_service", "--health-enabled=false")

	s := f.startTUI(120, 40)
	s.waitFor("CONNECTIONS")
	s.waitFor("demo")
	s.send("enter")
	s.waitFor("CONNECTION DETAILS")

	// Edit the typed Name field with real text-input controls, preview the
	// change, then cancel it. The value must remain visible until the user
	// explicitly discards it.
	s.send("e")
	s.waitFor("EDIT CONNECTION")
	s.waitFor("Name:")
	s.send("enter")
	s.send("ctrl-u")
	s.typeText("demo-renamed")
	s.send("enter")
	s.waitFor("demo-renamed")
	s.send("p")
	s.waitFor("EXACTLY THESE STEPS")
	s.send("esc")
	s.waitFor("EDIT CONNECTION")
	s.send("esc")
	s.waitFor("unsaved changes")
	s.send("y")
	s.waitFor("CONNECTION DETAILS")

	// Copy uses its own typed field and returns to Inspect after the supervisor
	// creates the closed copy. Escape then returns to the list where the new
	// identity is visible.
	s.send("c")
	s.waitFor("COPY CONNECTION")
	s.waitFor("Name for the copy")
	s.send("ctrl-u")
	s.typeText("demo-copy")
	s.send("enter")
	s.waitFor("CONNECTION DETAILS")
	s.send("esc")
	s.waitFor("CONNECTIONS")
	s.waitFor("demo-copy")

	// The delete plan is applied directly. The operation must complete and the
	// row must disappear from the real Home snapshot.
	s.send("d")
	s.waitFor("EXACTLY THESE STEPS")
	s.send("enter")
	s.waitFor("Status: Completed")
	s.send("esc")
	s.waitFor("CONNECTIONS")
	if strings.Contains(s.screen(), "demo-copy") {
		t.Fatalf("deleted copy remained visible after the applied plan:\n%s", s.debug())
	}
	s.assertNoOverflow()
	s.send("q")
	s.waitExit()
}

func TestTUIWizardCreatesClosedLocalForward(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)
	localPort := freeTCPPort(t)

	s := f.startTUI(100, 30)
	s.waitFor("Nothing is published yet.")
	s.send("n")
	s.waitFor("NEW CONNECTION")
	s.send("?")
	s.waitFor("HELP")
	s.send("esc")
	s.waitFor("NEW CONNECTION")

	// Choose the provider-neutral local-forward recipe through the visible
	// outcome menu, then fill every question with physical terminal bytes.
	s.chooseOutcome("Forward a local port")
	s.waitFor("Name this connection")
	s.send("ctrl-u")
	s.typeText("local-forward")
	s.send("enter")
	s.waitFor("Local listening port:")
	s.typeText(strconv.Itoa(localPort))
	s.send("enter")
	s.waitFor("Remote host:")
	s.typeText("127.0.0.1")
	s.send("enter")
	s.waitFor("Remote port:")
	s.typeText("8080")
	s.send("enter")
	s.waitFor("Protocol:")
	// This is a choice menu, not a text field: move to the unavailable UDP
	// option and back with physical arrows before accepting TCP.
	s.send("down")
	s.send("up")
	s.send("enter")
	s.waitFor("REVIEW")
	s.page("pgdown")
	s.waitFor("Save it, closed")
	s.send("enter")
	s.waitFor("CONNECTION CREATED")
	s.waitFor("Connection saved (closed)")
	s.send("enter")
	s.waitFor("CONNECTIONS")
	s.waitFor("local-forward")
	s.assertNoOverflow()
	s.send("q")
	s.waitExit()
}

func TestLiveTUIExternalProviderRequiresExplicitOptIn(t *testing.T) {
	if os.Getenv(e2eEnabledEnv) != "1" {
		t.Skipf("real PTY acceptance is opt-in; run with %s=1", e2eEnabledEnv)
	}
	if os.Getenv("PORTICO_E2E_LIVE") != "1" {
		t.Skip("live provider traffic is not part of deterministic tui-e2e")
	}

	provider := strings.TrimSpace(os.Getenv("PORTICO_TUI_E2E_LIVE_PROVIDER"))
	source := strings.TrimSpace(os.Getenv("PORTICO_TUI_E2E_LIVE_SOURCE"))
	if provider == "" || source == "" {
		t.Fatal("live TUI E2E requires PORTICO_TUI_E2E_LIVE_PROVIDER and PORTICO_TUI_E2E_LIVE_SOURCE")
	}

	f := newFixture(t)
	name := "live-tui-" + strconv.FormatInt(int64(os.Getpid()), 10)
	sourceType := os.Getenv("PORTICO_TUI_E2E_LIVE_SOURCE_TYPE")
	if sourceType == "" {
		sourceType = "existing_service"
	}
	args := []string{"create", name, "--provider", provider, "--source", source,
		"--source-type", sourceType, "--health-enabled=false"}
	runCLI(t, f, args...)

	s := f.startTUI(120, 40)
	s.waitFor("CONNECTIONS")
	s.waitFor(name)
	s.send("enter")
	s.waitFor(name)
	s.send(" ")
	s.waitFor("EXACTLY THESE STEPS")
	s.send("enter")
	s.waitFor("Status: Completed")
	f.t.Logf("live TUI provider flow completed for %s (%s)", name, provider)
}

func TestTerminalParserRejectsImpossibleDimensions(t *testing.T) {
	// This is a small local invariant test for the semantic screen layer. It is
	// intentionally not the acceptance gate; the tests above are the ones that
	// launch the compiled binary and drive a PTY.
	tm := newTerminal(10, 3)
	tm.feed([]byte("hello\x1b[2J\x1b[Hworld"))
	if got := tm.text(); got != "world" {
		t.Fatalf("terminal semantic text = %q, want %q", got, "world")
	}
	if strings.Contains(tm.debug(), fmt.Sprintf("%d,-", 0)) {
		t.Fatal("terminal debug contained an impossible coordinate")
	}
}
