//go:build linux

package tui_e2e

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTUIRepairAppliesAndRestoresTransport closes the audit's repair gap: the
// existing repair coverage navigated a "No findings" screen, so a repair that
// could never fix anything looked proven. This breaks a healthy local forward
// physically — a squatter takes the forward's local port, and a supervisor
// restart then fails to rebind it — diagnoses through the TUI, previews the
// repair, frees the port, applies, and proves the effect at the only layer
// that matters: the same port carries the origin's canary bytes again, not
// the squatter's.
//
// The squatter is released only after the plan was produced from the broken
// state, so the repair plan describes a real fault rather than one the
// supervisor's reconcile loop had already healed.
func TestTUIRepairAppliesAndRestoresTransport(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	// The canary origin. Its body is unique so every fetch below proves which
	// server actually answered.
	body := "REPAIR-CANARY-91bd"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer origin.Close()
	originHostPort := origin.Listener.Addr().String()
	originPort, err := strconv.Atoi(originHostPort[strings.LastIndex(originHostPort, ":")+1:])
	if err != nil {
		t.Fatalf("parse origin port from %s: %v", originHostPort, err)
	}

	localPort := freeTCPPort(t)
	created := runCLI(t, f, "forward", "create", "repair-demo",
		"--local-port", strconv.Itoa(localPort),
		"--remote-host", "127.0.0.1",
		"--remote-port", strconv.Itoa(originPort), "--json")
	connID := jsonField(created, "id")
	if connID == "" {
		t.Fatalf("forward create returned no connection ID:\n%s", created)
	}
	runCLI(t, f, "open", connID, "--yes")

	// Physical proof the forward relays before the fault.
	dialUntil(t, localPort)
	if got := fetchWithRetry(t, localPort); got != body {
		t.Fatalf("pre-fault fetch returned %q, want the canary body %q", got, body)
	}

	// The fault: the squatter claims the forward's port, and the supervisor
	// restart's startup recovery cannot rebind it. Desired-open survives the
	// restart — startup recovery does not close a connection on failure — so
	// the result is genuinely repairable: a connection Portico promised to
	// keep open whose listener is gone. The squatter answers in the
	// meantime, which is how the fault is proven physically rather than by
	// trusting Portico's own reporting.
	//
	// Stop the supervisor first: its forward's relay owns the port until the
	// process exits. Only once the port is free can the squatter claim it;
	// claiming while the healthy forward holds it would fail the bind, not
	// stage a repair.
	runCLI(t, f, "supervisor", "stop")
	waitForPortFree(t, localPort)

	squatterBody := "NOT-PORTICO-3e5f"
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		t.Fatalf("squatter could not claim port %d: %v", localPort, err)
	}
	squatter := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(squatterBody))
	})}
	go func() { _ = squatter.Serve(listener) }()
	defer func() { _ = squatter.Close() }()

	runCLI(t, f, "supervisor", "start")

	if got := fetchWithRetry(t, localPort); got != squatterBody {
		t.Fatalf("post-fault fetch returned %q, want the squatter body %q — the fault is not physical", got, squatterBody)
	}

	s := f.startTUI(120, 40)
	s.waitFor("CONNECTIONS")
	s.waitFor("repair-demo")

	// Diagnose: the finding must name the broken segment (the forward's
	// listener), not an origin that is actually up.
	s.send("r")
	s.waitFor("REPAIR: repair-demo")
	s.waitForAny("connector", "Connector")

	// Preview the repair from those findings. On the repair screen the
	// preview action is bound to enter.
	s.send("enter")
	s.waitFor("EXACTLY THESE STEPS")

	// Free the port only after the plan was produced from the broken state.
	if err := squatter.Close(); err != nil {
		t.Fatalf("release squatter: %v", err)
	}

	// Approve. The repair runs through the real supervisor; it completes and
	// re-runs the diagnosis fast enough that the operation-progress screen is
	// transient. The durable terminal state is the repair-verification screen
	// comparing the findings the repair was planned from against a fresh
	// diagnosis.
	s.send("enter")
	s.waitForAny("All issues resolved", "Could not verify", "did not resolve", "REPAIR VERIFICATION")

	// Independent proof: the same port carries the origin's canary again.
	// The screen claiming success is not the evidence; these bytes are.
	dialUntil(t, localPort)
	if got := fetchWithRetry(t, localPort); got != body {
		t.Fatalf("post-repair fetch returned %q, want the canary body %q", got, body)
	}

	// The screen's own verdict must agree with the bytes.
	if screen := strings.ToLower(s.screen()); !strings.Contains(screen, "all issues resolved") {
		t.Fatalf("the repair verification does not confirm the fix:\n%s", s.debug())
	}

	// Back out through the operation and repair screens to the connection
	// list. Escape from the verification returns to the operation progress
	// view, whose own escape returns to the repair screen.
	s.escUntil("CONNECTIONS")
	s.send("ctrl-c")
	s.waitExit()
}

// waitForPortFree polls until nothing on loopback accepts the port. Like the
// other physical proofs in this suite, it waits on the observable state
// rather than sleeping a fixed interval.
func waitForPortFree(t *testing.T, port int) {
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
	t.Fatalf("127.0.0.1:%d is still accepting connections; cannot stage the fault", port)
}
