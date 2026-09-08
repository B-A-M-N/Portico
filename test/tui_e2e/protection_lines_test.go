package tui_e2e

import (
	"strings"
	"testing"
)

// TestTUIProtectionLinesMatchDeclarations drives the compiled binary to the
// Providers screen and pins that each provider's access line says what its own
// capability declaration carries. The audit caught the screen telling a local
// forward and Tailscale that "account setup required" would produce protection
// — for a loopback listener and tailnet membership, neither of which any
// Portico account can change. Assertions are per row, against the screen a
// user actually reads.
func TestTUIProtectionLinesMatchDeclarations(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	s := f.startTUI(120, 40)
	s.waitFor("Nothing is published yet.")
	s.send("p")
	s.waitFor("PROVIDERS")
	s.waitFor("Local port forward")
	s.waitFor("Tailscale")
	s.assertNoOverflow()
	// The catalog is taller than the viewport now that no instructional line
	// is truncated; jump to the end so the tail providers' lines are visible.
	s.send("end")
	s.waitForScreenChange(s.screen())

	screen := s.screen()
	// Provider sections run in catalog order; split on the header lines so
	// each provider's claim is checked against its own declaration. The mock
	// provider legitimately gates protection on setup — it declares an
	// email-OTP policy Portico would apply after an account exists — so the
	// forbidden phrase is checked per provider, not globally.
	sectionOf := func(name string) string {
		start := strings.Index(screen, "\n  "+name+"  ")
		if start < 0 {
			t.Fatalf("provider %q is not on the providers screen:\n%s", name, s.debug())
		}
		rest := screen[start+len("\n  "+name+"  "):]
		// Sections are separated by a blank line; a two-space cut would also
		// match the four-space detail rows and amputate the very line being
		// checked.
		if end := strings.Index(rest, "\n\n  "); end >= 0 {
			rest = rest[:end]
		}
		return rest
	}

	forward := sectionOf("Local port forward")
	if !strings.Contains(forward, "not applicable") {
		t.Fatalf("the local forward's protection line does not say it is not applicable:\n%s", s.debug())
	}
	if strings.Contains(forward, "account setup required") {
		t.Fatalf("the local forward still promises account-gated protection:\n%s", forward)
	}

	tailnet := sectionOf("Tailscale")
	if !strings.Contains(tailnet, "membership") {
		t.Fatalf("the Tailscale protection line does not say reachability is membership:\n%s", s.debug())
	}
	if strings.Contains(tailnet, "account setup required") {
		t.Fatalf("Tailscale still promises account-gated protection:\n%s", tailnet)
	}

	s.send("ctrl-c")
	s.waitExit()
}
