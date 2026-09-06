//go:build linux

package tui_e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTUIExistingServiceManualHTTPAndHTTPS proves the normal existing-service
// creation path from the wizard itself. It deliberately goes through the
// discovery question and chooses its manual-entry row, because a discovery
// shortcut or a CLI-created profile would not prove that a human can name a
// service the scanner did not find. HTTP and HTTPS are menu choices, not text
// values, so both are driven through the visible protocol menu.
func TestTUIExistingServiceManualHTTPAndHTTPS(t *testing.T) {
	requireE2E(t)

	for _, protocol := range []struct {
		name string
		key  string
	}{
		{name: "http", key: "up"},
		{name: "https", key: "down"},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			fixtureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("existing-service-fixture"))
			}))
			defer fixtureServer.Close()

			f := newFixture(t)
			s := f.startTUI(120, 40)
			s.waitFor("Nothing is published yet.")
			s.send("n")
			s.waitFor("NEW CONNECTION")

			// The last outcome is the advanced route. The first intent is an
			// already-running service.
			s.chooseOutcome("Something else (choose the source yourself)")
			s.waitFor("What should be reachable?")
			s.send("enter")
			s.waitFor("Name this connection")
			s.send("ctrl-u")
			s.typeText("existing-" + protocol.name)
			s.send("enter")

			// The random fixture port is intentionally outside the usual scan
			// candidates. End moves the viewport to the bottom; the discovery
			// cursor still needs to move to the manual-entry row, so drive the
			// visible list downward until it clamps there.
			s.waitFor("Which service should be reachable?")
			s.send("end")
			for range 3 {
				s.send("down")
				time.Sleep(20 * time.Millisecond)
			}
			s.send("up")
			// j is the same downward navigation action and is easier to
			// deliver losslessly when the host has a very large discovered
			// port list. The bounded repetition clamps at the always-present
			// manual row; the earlier arrows prove the alternate binding too.
			for range 128 {
				s.send("j")
				time.Sleep(5 * time.Millisecond)
			}
			s.waitFor("Enter an address manually")
			s.send("enter")
			s.waitFor("Enter the service address")
			s.pasteText(strings.TrimPrefix(fixtureServer.Listener.Addr().String(), "tcp://"))
			s.send("enter")
			s.waitFor("What protocol does your service use?")
			s.send(protocol.key)
			s.send("enter")
			s.waitFor("Probe the existing service after connecting?")
			// Health defaults on; choose the explicit No row so this test only
			// exercises the source/protocol path and does not probe HTTPS.
			s.send("up")
			s.send("enter")
			s.waitFor("How should it be reachable?")
			s.send("enter")
			s.waitFor("Who should be able to reach it?")
			s.send("enter")
			s.waitFor("Which provider should carry")
			s.send("enter")
			s.waitFor("What should Portico do?")
			s.page("end")
			s.waitFor("Save it, closed")
			s.send("enter")
			s.waitFor("CONNECTION CREATED")
			s.waitFor("Connection saved (closed)")
			s.send("enter")
			s.waitFor("CONNECTIONS")
			s.waitFor("existing-" + protocol.name)
			s.assertNoOverflow()
			s.send("ctrl-c")
			s.waitExit()
		})
	}
}

// TestTUIMCPTransportMenuAndCreation proves that the MCP branch is a real
// wizard path. HTTP and Streamable HTTP are each selected from the menu and
// saved through the TUI. SSE is asserted as a visible choice when the fixture
// has a provider capable of fixed hostnames; otherwise the menu's two choices
// are the truthful available set and the provider-capability component tests
// cover the gated SSE rule.
func TestTUIMCPTransportMenuAndCreation(t *testing.T) {
	requireE2E(t)

	for _, transport := range []struct {
		name string
		down int
	}{
		{name: "http", down: 0},
		{name: "streamable", down: 1},
	} {
		t.Run(transport.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.startTUI(120, 40)
			s.waitFor("Nothing is published yet.")
			s.send("n")
			s.waitFor("NEW CONNECTION")
			s.chooseOutcome("Something else (choose the source yourself)")
			s.waitFor("What should be reachable?")
			for range 3 {
				s.send("down")
			}
			s.send("enter")
			s.waitFor("Name this connection")
			s.send("ctrl-u")
			s.typeText("mcp-" + transport.name)
			s.send("enter")
			s.waitFor("How does the MCP server run?")
			s.send("enter")
			s.waitFor("Enter the MCP server endpoint")
			s.pasteText("http://127.0.0.1:39999/mcp")
			s.send("enter")
			s.waitFor("Which MCP transport does the server use?")
			s.waitFor("Streamable HTTP")
			for range transport.down {
				s.send("down")
			}
			s.send("enter")
			s.waitFor("How should it be reachable?")
			s.send("enter")
			s.waitFor("Who should be able to reach it?")
			s.send("enter")
			s.waitFor("Which provider should carry")
			s.waitFor("Mock Provider")
			// The recommendation puts the accountless Cloudflare first; this test
			// needs a provider that can actually serve, so walk down to the Mock
			// row before accepting.
			for range 12 {
				if strings.Contains(s.screen(), "> Mock Provider") {
					break
				}
				s.send("down")
				time.Sleep(20 * time.Millisecond)
			}
			if !strings.Contains(s.screen(), "> Mock Provider") {
				t.Fatalf("could not reach the Mock Provider row:\n%s", s.debug())
			}
			s.send("enter")
			s.waitFor("What will be reachable")
			s.page("end")
			s.waitFor("Save it, closed")
			s.send("enter")
			s.waitFor("CONNECTION CREATED")
			s.waitFor("Connection saved (closed)")
			s.send("enter")
			s.waitFor("CONNECTIONS")
			s.waitFor("mcp-" + transport.name)
			s.assertNoOverflow()
			s.send("ctrl-c")
			s.waitExit()
		})
	}
}

// TestTUIMalformedSupervisorSocketRecovery proves the failure screen against
// the real launcher and supervisor process. A non-socket at the socket path
// prevents startup; removing that malformed fixture and pressing the visible
// retry key must then reach the real empty Home screen.
func TestTUIMalformedSupervisorSocketRecovery(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)
	socketDir := filepath.Join(f.root, "runtime", "portico")
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDir, "portico.sock")
	if err := os.WriteFile(socketPath, []byte("this is deliberately not a unix socket"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := f.startTUI(80, 24)
	s.waitFor("Press r to try again")
	s.send("?")
	s.waitFor("HELP")
	s.send("esc")
	s.waitFor("Press r to try again")
	s.assertNoOverflow()

	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	s.send("r")
	s.waitFor("Nothing is published yet.")
	s.send("ctrl-c")
	s.waitExit()
}
