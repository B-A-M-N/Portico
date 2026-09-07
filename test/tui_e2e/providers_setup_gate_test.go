//go:build linux

package tui_e2e

import (
	"strings"
	"testing"
)

// TestTUIProvidersScreenRefusesSetupWhereThereIsNoFlow proves the setup gate
// at the compiled boundary: pressing a on a provider with no Portico setup
// flow — the local port forward, and zrok which Portico names but has no
// adapter for — does nothing but say why. The old screen enabled "Add account"
// for every catalogued provider, so the keystroke fired a request the
// supervisor was guaranteed to refuse: an advertised action whose every use
// fails.
func TestTUIProvidersScreenRefusesSetupWhereThereIsNoFlow(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	s := f.startTUI(120, 40)
	s.waitFor("Nothing is published yet.")
	s.send("p")
	s.waitFor("PROVIDERS")
	s.waitFor("Local port forward")

	// Walk to the local port forward: a working provider that needs no
	// account and declares no setup flow.
	walkTo(t, s, "> Local port forward")

	// The action is advertised but disabled, and pressing it names the reason
	// rather than firing a doomed request. A refused setup would open a form
	// or an error naming the provider; the correct behaviour is the dimmed
	// action's own explanation.
	s.send("a")
	s.waitFor("Set up is not available")

	// Nothing opened: still the providers screen, no setup form.
	if strings.Contains(s.screen(), "SET UP ") && !strings.Contains(s.screen(), "is not available") {
		t.Fatalf("pressing a on the local port forward opened a setup form:\n%s", s.debug())
	}

	// And zrok, which Portico cannot use at all, behaves the same way.
	walkTo(t, s, "> zrok")
	s.send("a")
	s.waitFor("Set up is not available")
	if strings.Contains(s.screen(), "SET UP ZROK") {
		t.Fatalf("pressing a on zrok opened a setup form:\n%s", s.debug())
	}

	s.assertNoOverflow()
	s.send("?")
	s.waitFor("HELP")
	s.send("esc")
	s.send("ctrl-c")
	s.waitExit()
}

// walkTo moves the providers cursor until name is the selected row, failing
// if it cannot be reached within the catalog's length. The cursor's own
// movement scrolls the row into view, so a row below the fold is still
// reachable.
func walkTo(t *testing.T, s *session, name string) {
	t.Helper()
	for range 12 {
		if strings.Contains(s.screen(), name) {
			return
		}
		before := s.screen()
		s.send("j")
		s.waitForScreenChange(before)
		if strings.Contains(s.screen(), name) {
			return
		}
	}
	t.Fatalf("could not reach the %s row:\n%s", name, s.debug())
}
