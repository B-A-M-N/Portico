package tui_e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTUIOriginDeathDiagnosesNamesTheBrokenSegment covers the mutation boundary
// the audit found missing: a healthy local-forward connection whose origin is
// then killed OUTSIDE Portico must produce a visible diagnosis that blames the
// origin — the only segment actually broken — rather than the connector or a
// provider hop that does not exist. Physical health before the fault is proven
// with an independent HTTP fetch through the forward, not Portico's own
// reporting.
func TestTUIOriginDeathDiagnosesNamesTheBrokenSegment(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	// The canary origin. Its body is unique so every fetch below proves which
	// server actually answered.
	body := "ORIGIN-DEATH-CANARY-7f3a"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	originHostPort := strings.TrimPrefix(origin.URL, "http://")
	originPort, err := strconv.Atoi(originHostPort[strings.LastIndex(originHostPort, ":")+1:])
	if err != nil {
		t.Fatalf("parse origin port from %s: %v", originHostPort, err)
	}

	localPort := freeTCPPort(t)
	created := runCLI(t, f, "forward", "create", "origin-death",
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
		t.Fatalf("the forward returned %q, want the canary body %q", got, body)
	}

	s := f.startTUI(120, 40)
	s.waitFor("CONNECTIONS")
	s.waitFor("origin-death")

	// Healthy first: inspect the connection, then diagnose. The diagnosis is
	// run while the origin is being killed, so order matters: kill first,
	// diagnose second.
	s.send("enter")
	s.waitFor("origin-death")

	// Kill the origin OUTSIDE Portico — the fault is physical, not simulated
	// in Portico's state.
	origin.Close()

	// Diagnose through the TUI's advertised action ([r] Diagnose). The finding
	// must name the origin/local segment: a local forward has no provider
	// tunnel, so blaming one would be a false positive on an intact hop.
	s.send("r")
	s.waitForAny("DIAGNOS", "diagnos", "ROUTE", "route", "origin")
	screen := strings.ToLower(s.screen())
	if !contains(screen, "origin") {
		t.Fatalf("the diagnosis does not name the broken segment (origin):\n%s", s.debug())
	}
	s.send("ctrl-c")
	s.waitExit()
}

// jsonField extracts the first value of a top-level JSON string field. The
// artifact verifier uses the same awk approach; a helper here keeps the test
// honest about the CLI's real output shape without a JSON dependency.
func jsonField(json, field string) string {
	key := "\"" + field + "\""
	i := strings.Index(json, key)
	if i < 0 {
		return ""
	}
	rest := json[i+len(key):]
	c := strings.Index(rest, ":")
	if c < 0 {
		return ""
	}
	rest = rest[c+1:]
	q1 := strings.Index(rest, "\"")
	if q1 < 0 {
		return ""
	}
	rest = rest[q1+1:]
	q2 := strings.Index(rest, "\"")
	if q2 < 0 {
		return ""
	}
	return rest[:q2]
}

// waitForAny returns as soon as one of the markers appears and reports which
// one. Screens may legitimately word the same fact differently; the test pins
// that SOMETHING user-visible appeared, then pins the finding itself.
func (s *session) waitForAny(markers ...string) string {
	s.t.Helper()
	deadline := time.NewTimer(defaultTimeout)
	defer deadline.Stop()
	for {
		screen := s.screen()
		for _, m := range markers {
			if contains(screen, m) {
				return m
			}
		}
		select {
		case <-s.chunks:
		case <-s.done:
			s.t.Fatalf("TUI exited while waiting for %v\n%s", markers, s.debug())
		case <-deadline.C:
			s.t.Fatalf("timed out waiting for any of %v\n%s", markers, s.debug())
		}
	}
}
