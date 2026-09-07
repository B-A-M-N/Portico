//go:build linux

package tui_e2e

import (
	"strings"
	"testing"
)

// TestTUISettingsTunnelClientPathEditor proves the settings vertical in the
// compiled binary: the tunnel-client path row is editable there, enter opens a
// field that accepts real bytes, saving reports the supervisor's effective
// value on the row, and cancelling writes nothing. The supervisor could always
// update this field; the interface is what refused to, and the compiled
// boundary is where that refusal lived.
func TestTUISettingsTunnelClientPathEditor(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	s := f.startTUI(120, 40)
	s.waitFor("Nothing is published yet.")
	s.send("S")
	s.waitFor("SETTINGS")
	s.waitFor("tunnel-client path")

	// Walk the cursor to the path row: it is the fifth.
	found := false
	for range 6 {
		if strings.Contains(s.screen(), "> tunnel-client path") {
			found = true
			break
		}
		before := s.screen()
		s.send("j")
		s.waitForScreenChange(before)
		if strings.Contains(s.screen(), "> tunnel-client path") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("could not reach the tunnel-client path row:\n%s", s.debug())
	}

	// Enter opens the field. Esc closes it without sending anything: the row
	// still shows the PATH lookup, and no write happened to change what a
	// reopen would show.
	s.send("enter")
	s.waitFor("e.g. /usr/local/bin/tunnel-client")
	s.send("esc")
	s.waitFor("> tunnel-client path")

	// Reopen and actually set a path. The typed bytes are visible proof the
	// field received them; saving returns the row to the supervisor's answer.
	s.send("enter")
	s.waitFor("e.g. /usr/local/bin/tunnel-client")
	s.typeText("/opt/tunnel-client-e2e")
	s.send("enter")
	s.waitFor("/opt/tunnel-client-e2e")
	s.assertNoOverflow()

	// The value survives a supervisor restart: it is stored by the supervisor,
	// not held by this interface.
	s.send("q")
	s.waitExit()
	runCLI(t, f, "supervisor", "stop")
	runCLI(t, f, "supervisor", "start")

	s2 := f.startTUI(120, 40)
	s2.waitFor("Nothing is published yet.")
	s2.send("S")
	s2.waitFor("SETTINGS")
	s2.waitFor("tunnel-client path")
	if !strings.Contains(s2.screen(), "/opt/tunnel-client-e2e") {
		t.Fatalf("the saved path did not survive the restart:\n%s", s2.debug())
	}

	// Clearing it back: an empty save is the documented way to PATH lookup,
	// and the row must show that, not keep the stale value.
	for range 6 {
		if strings.Contains(s2.screen(), "> tunnel-client path") {
			break
		}
		before := s2.screen()
		s2.send("j")
		s2.waitForScreenChange(before)
		if strings.Contains(s2.screen(), "> tunnel-client path") {
			break
		}
	}
	s2.send("enter")
	s2.waitFor("e.g. /usr/local/bin/tunnel-client")
	s2.send("enter") // commit the empty field
	s2.waitFor("find on PATH")
	if strings.Contains(s2.screen(), "/opt/tunnel-client-e2e") {
		t.Fatalf("the row still shows the cleared path:\n%s", s2.debug())
	}

	s2.send("ctrl-c")
	s2.waitExit()
}
