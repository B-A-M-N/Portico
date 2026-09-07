//go:build linux

package tui_e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
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

// canaryOrigin is an HTTP fixture with a fixed, unusual body so the proof is
// that these exact bytes came back, not that any 200 did.
func canaryOrigin(t *testing.T) (string, string) {
	t.Helper()
	body := "PORTICO-PROOF-" + strconv.FormatInt(int64(os.Getpid()), 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, body
}

// dialUntil waits for the forward's listening socket to appear. The connector
// starts asynchronously, so a fixed sleep would race the very state the test
// is proving.
func dialUntil(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(defaultTimeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 250*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the forward never opened a listening socket on 127.0.0.1:%d", port)
}

// dialGone proves the listening socket is closed. A forward that claims to be
// closed but still accepts connections would be lying at the only layer that
// matters.
func dialGone(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(defaultTimeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 250*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("127.0.0.1:%d still accepts connections after the close plan completed", port)
}

// TestTUILocalForwardMovesRealBytes opens a local forward through the TUI's
// normal-user path, then asserts the physical effect independently of
// Portico's own reporting: bytes sent to the forward's listening port come
// back from the origin, and closing through the TUI stops the transport.
// "Status: Completed" alone is not evidence that anything was forwarded.
func TestTUILocalForwardMovesRealBytes(t *testing.T) {
	requireE2E(t)
	originURL, body := canaryOrigin(t)
	originHost, originPortStr, _ := net.SplitHostPort(strings.TrimPrefix(originURL, "http://"))
	originPort, _ := strconv.Atoi(originPortStr)
	localPort := freeTCPPort(t)

	f := newFixture(t)
	s := f.startTUI(100, 30)
	s.waitFor("Nothing is published yet.")
	s.send("n")
	s.waitFor("NEW CONNECTION")
	s.chooseOutcome("Forward a local port")
	s.waitFor("Name this connection")
	s.send("ctrl-u")
	s.typeText("forward-proof")
	s.send("enter")
	s.waitFor("Local listening port:")
	s.typeText(strconv.Itoa(localPort))
	s.send("enter")
	s.waitFor("Remote host:")
	s.typeText(originHost)
	s.send("enter")
	s.waitFor("Remote port:")
	s.typeText(strconv.Itoa(originPort))
	s.send("enter")
	s.waitFor("Protocol:")
	s.send("enter")
	s.waitFor("REVIEW")
	s.page("pgdown")
	s.waitFor("Save it, closed")
	s.send("enter")
	s.waitFor("CONNECTION CREATED")
	s.send("enter")
	s.waitFor("CONNECTIONS")
	s.waitFor("forward-proof")

	// Open it through the same screen a user uses.
	s.send("enter")
	s.waitFor("forward-proof")
	s.send(" ")
	s.waitFor("EXACTLY THESE STEPS")
	s.send("enter")
	s.waitFor("Status: Completed")
	// The operation view stays up after completion; back to the details
	// screen where the connection's own status is rendered.
	s.send("esc")

	// Independent effect #1: the forward's listening port exists and carries
	// bytes to the origin. The exact canary body must come back.
	dialUntil(t, localPort)
	got := fetchWithRetry(t, localPort)
	if got != body {
		t.Fatalf("the forwarded port returned %q, want the canary body %q", got, body)
	}

	// The TUI agrees the connection is open — but the byte proof above is the
	// authority, not this screen. The details screen must also name the
	// listening address it told the connector to open: "none yet" next to a
	// working forward is a lie by omission.
	s.waitFor("State:      Open")
	if screen := s.screen(); strings.Contains(screen, "Address:    none yet") {
		t.Errorf("the connection is open and forwarding bytes, but Inspect shows no address:\n%s", screen)
	}

	// Independent effect #2: closing through the TUI stops the transport.
	s.send(" ")
	s.waitFor("EXACTLY THESE STEPS")
	s.send("enter")
	s.waitFor("Status: Completed")
	dialGone(t, localPort)
	s.send("esc")
	s.waitFor("State:      Closed")

	s.send("esc")
	s.waitFor("CONNECTIONS")
	s.assertNoOverflow()

	// Clean up through the CLI so the fixture leaves nothing behind.
	runCLI(t, f, "delete", "--yes", findConnectionID(t, f, "forward-proof"))
	s.send("q")
	s.waitExit()
}

// fetchWithRetry sends one HTTP request through the forward and returns the
// body, retrying briefly while the proxy accepts but the origin side warms up.
func fetchWithRetry(t *testing.T, port int) string {
	t.Helper()
	deadline := time.Now().Add(defaultTimeout)
	var lastErr string
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err == nil {
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err == nil {
				return string(b)
			}
			lastErr = err.Error()
		} else {
			lastErr = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no HTTP response through the forward: %s", lastErr)
	return ""
}

// findConnectionID resolves a connection name to its ID through the CLI, the
// same JSON surface an operator would use.
func findConnectionID(t *testing.T, f *fixture, name string) string {
	t.Helper()
	out := runCLI(t, f, "list", "--json")
	var ids []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &ids); err != nil {
		t.Fatalf("list --json did not parse: %v\n%s", err, out)
	}
	for _, c := range ids {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("connection %q not found in list output: %s", name, out)
	return ""
}

// TestTUIProvidersMultiProviderMultiAccount proves the compiled binary renders
// the Providers screen correctly when several providers each have accounts —
// the exact shape the unit renderer got wrong (each provider's payload was
// re-emitted once per its account rows). The unit suite now pins the counts,
// but the compiled binary through a real terminal is the boundary the last
// regression shipped through.
func TestTUIProvidersMultiProviderMultiAccount(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	// The mock provider exists to make multi-account shapes reachable without
	// live credentials. Configure two accounts on it so the screen has
	// provider rows interleaved with account rows from more than one provider.
	// (Fixture shape mirrors TestTUIProviderSetupSecretAndSettingsNavigation.)
	s := f.startTUI(200, 60)
	s.waitFor("Nothing is published yet.")
	s.send("p")
	s.waitFor("PROVIDERS")
	// Wait until the last catalogued provider is on screen so the render is
	// complete before anything is counted.
	s.waitFor("zrok")

	// Every catalogued provider's header appears exactly once. The N+1
	// regression duplicated provider payloads per account row; counting the
	// header row ("Name  State") catches it through the real binary. Matching
	// the full header rather than the bare name avoids counting provider names
	// that legitimately appear inside action labels ("Add a Cloudflare account").
	for _, header := range []string{"Cloudflare  Ready", "Local port forward  Ready", "Tailscale  ready • beta"} {
		if got := strings.Count(s.screen(), header); got != 1 {
			t.Fatalf("provider header %q rendered %d times, want exactly 1:\n%s", header, got, s.debug())
		}
	}

	// Walking the whole flattened list keeps the cursor visible at every row:
	// down to the end and back up.
	for range 8 {
		s.send("down")
	}
	s.send("up")
	s.send("up")
	s.send("up")
	s.waitFor("Cloudflare")
	// The cursor is back on a provider row, which selects no account. The
	// footer is the screen's action set, so the account actions are drawn
	// disabled there rather than the bar claiming a selection exists: pressing
	// one names the refusal instead of targeting an account nobody chose.
	s.send("x")
	s.waitFor("Remove account is not available")
	s.send("?")
	s.waitFor("HELP")
	s.send("esc")
}
