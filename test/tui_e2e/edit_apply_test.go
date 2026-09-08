//go:build linux

package tui_e2e

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTUIEditAppliesAndSurvivesRestart closes the audit's coverage gap: the
// edit matrix test previewed a change and cancelled it, so a plan that could
// never be applied looked covered. This drives the compiled binary through
// edit → preview → APPLY, then verifies the durable result three ways — the
// screen after the operation, the CLI's independent JSON view, and a full
// supervisor restart followed by a fresh TUI reading the same state.
func TestTUIEditAppliesAndSurvivesRestart(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)
	fixtureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fixture"))
	}))
	defer fixtureServer.Close()

	runCLI(t, f, "create", "edit-apply-demo", "--provider", "mock",
		"--source", "http://"+fixtureServer.Listener.Addr().String(),
		"--source-type", "existing_service", "--health-enabled=false")

	s := f.startTUI(120, 40)
	s.waitFor("CONNECTIONS")
	s.waitFor("edit-apply-demo")

	// Open the edit, change the name through the real text field.
	s.send("enter")
	s.waitFor("CONNECTION DETAILS")
	s.send("e")
	s.waitFor("EDIT CONNECTION")
	s.waitFor("Name:")
	s.send("enter")
	s.send("ctrl-u")
	s.typeText("edit-apply-renamed")
	s.send("enter")
	s.waitFor("edit-apply-renamed")

	// Preview, then approve. The applied plan runs through the real
	// supervisor: the operation screen must reach Completed on its own.
	s.send("p")
	s.waitFor("EXACTLY THESE STEPS")
	s.send("enter")
	s.waitFor("Status: Completed")

	// Escape returns to the edit, which shows the applied value; escaping
	// from there reaches Inspect, where the supervisor's own detail view must
	// agree, and again to the list.
	s.send("esc")
	s.waitFor("EDIT CONNECTION")
	s.waitFor("edit-apply-renamed")
	s.send("esc")
	s.waitFor("CONNECTION DETAILS")
	s.waitFor("edit-apply-renamed")
	s.send("esc")
	s.waitFor("CONNECTIONS")
	s.send("ctrl-c")
	s.waitExit()

	// The CLI's independent view of durable state agrees with the screen.
	out := runCLI(t, f, "list", "--json")
	if !contains(out, "edit-apply-renamed") {
		t.Fatalf("the applied rename is absent from the CLI's connection list:\n%s", out)
	}
	if contains(out, "edit-apply-demo") {
		t.Fatalf("the pre-edit name survived an applied plan:\n%s", out)
	}

	// Restart the supervisor and open a fresh TUI: the applied change must be
	// the state a new process reconciles, not a runtime-only mutation.
	runCLI(t, f, "supervisor", "stop")
	runCLI(t, f, "supervisor", "start")

	s2 := f.startTUI(120, 40)
	s2.waitFor("CONNECTIONS")
	s2.waitFor("edit-apply-renamed")
	if contains(s2.screen(), "edit-apply-demo") {
		t.Fatalf("the pre-edit name returned after a supervisor restart:\n%s", s2.debug())
	}
	s2.send("ctrl-c")
	s2.waitExit()

	// The restarted supervisor's CLI view agrees too.
	out = runCLI(t, f, "list", "--json")
	if !contains(out, "edit-apply-renamed") {
		t.Fatalf("the rename did not survive the supervisor restart:\n%s", out)
	}
}

// TestTUIToggleOpenAppliesAndReconciles drives the open/close mutation through
// the real surface: opening a connection applies its plan, the durable desired
// state must survive a supervisor restart, and the restarted supervisor must
// reconcile the connection back open — the startup recovery contract the
// closure matrix exercises on the CLI side.
func TestTUIToggleOpenAppliesAndReconciles(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)
	fixtureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fixture"))
	}))
	defer fixtureServer.Close()

	runCLI(t, f, "create", "toggle-demo", "--provider", "mock",
		"--source", "http://"+fixtureServer.Listener.Addr().String(),
		"--source-type", "existing_service", "--health-enabled=false")

	s := f.startTUI(120, 40)
	s.waitFor("CONNECTIONS")
	s.waitFor("toggle-demo")

	// Space opens the selected connection: preview, then approve. The key
	// bytes for the space action are a literal space. Waiting for the
	// operation status rather than the word "Open": the plan preview's own
	// footer contains that word before the plan is ever approved.
	s.send(" ")
	s.waitFor("EXACTLY THESE STEPS")
	s.send("enter")
	s.waitFor("Status: Completed")
	s.send("esc")
	s.waitFor("CONNECTIONS")
	s.waitFor("toggle-demo")
	s.waitFor("Open")
	s.send("q")
	s.waitExit()

	// Restart. Desired-open is durable, so the restarted supervisor reopens it
	// and a fresh TUI must show the reconciled state.
	runCLI(t, f, "supervisor", "stop")
	runCLI(t, f, "supervisor", "start")

	s2 := f.startTUI(120, 40)
	s2.waitFor("CONNECTIONS")
	s2.waitFor("toggle-demo")
	s2.waitForEither("Open", "Opening")
	s2.send("ctrl-c")
	s2.waitExit()
}
