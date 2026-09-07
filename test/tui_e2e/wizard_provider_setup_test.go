//go:build linux

package tui_e2e

import (
	"strings"
	"testing"
)

// TestTUIWizardProviderSetupIsAScreen proves the setup handoff end to end in
// the compiled binary: the wizard's "[s] Set up this provider" hands the
// rendered surface and the keyboard to provider setup, the form accepts real
// input, and every exit returns to the wizard with its answers intact.
//
// The pre-screen model armed setup behind the wizard screen: the user pressed
// s, kept seeing — and kept typing into — the wizard. These physical bytes
// would have failed against that binary.
func TestTUIWizardProviderSetupIsAScreen(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	s := f.startTUI(120, 40)
	s.waitFor("Nothing is published yet.")

	// New connection, temporary web outcome, a name, a manually typed service
	// address, protocol, and health off — reaching the provider question with
	// several answers already given, so surviving the handoff is observable.
	s.send("n")
	s.waitFor("NEW CONNECTION")
	// The outcome menu paints a frame after the header appears; choosing
	// before it is drawn would race the cursor's only visit to row 0.
	s.waitFor("Share a web app on this computer, temporarily")
	s.chooseOutcome("Share a web app on this computer, temporarily")
	s.waitFor("Name this connection")
	s.send("ctrl-u")
	s.typeText("wizard-setup")
	s.send("enter")
	// The scan runs; answer with a typed address instead of a discovered row,
	// so the test does not depend on what this machine happens to be serving.
	s.waitFor("Which service should be reachable?")
	s.send("m")
	s.waitFor("Enter the service address")
	s.typeText("127.0.0.1:8123")
	s.send("enter")
	s.waitFor("What protocol does your service use?")
	s.send("enter") // http
	// Health probe defaults to Yes; take the No row so the path stays
	// deterministic, mirroring the original local-forward walkthrough.
	s.waitFor("Probe the existing service after connecting?")
	s.send("up")
	s.send("enter")
	s.waitFor("How should it be reachable?")
	s.send("enter") // temporary public address, as the outcome preset
	s.waitFor("Who should be able to reach it?")
	s.send("enter") // no protection, as the outcome preset
	s.waitFor("Which provider should carry the connection?")
	s.waitFor("Which provider should carry the connection?")

	// Walk the visible cursor to the experimental client-tunnel row: it is a
	// provider the catalog lists but cannot use yet, which is exactly the
	// state the setup handoff exists for. The match requires the selected
	// marker — the row is on screen long before the cursor reaches it.
	found := false
	for range 14 {
		if strings.Contains(s.screen(), "> Client-mediated MCP transport") {
			found = true
			break
		}
		before := s.screen()
		s.send("j")
		s.waitForScreenChange(before)
		if strings.Contains(s.screen(), "> Client-mediated MCP transport") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the provider question never listed the unconfigured transport:\n%s", s.debug())
	}

	// The wizard's advertised escape hatch must actually move the user to the
	// setup screen — not arm a flow behind the wizard.
	s.send("s")
	s.waitFor("SET UP CLIENT_TUNNEL")
	s.waitFor("Control plane API key")
	s.assertNoOverflow()

	// Typed bytes must reach the setup field while setup is the current
	// screen. The masked echo is visible proof the field received them —
	// and the confirm summary must show the same mask, never the raw value.
	s.typeText("wizard-setup-key-01")
	s.waitFor("••••••••")
	s.send("enter")
	s.waitFor("Step 2/2: Confirm")
	if !strings.Contains(s.screen(), "••••••••") {
		t.Fatalf("the confirm summary does not show the collected secret as present:\n%s", s.screen())
	}
	if strings.Contains(s.screen(), "wizard-setup-key-01") {
		t.Fatal("the confirm summary echoed the raw secret")
	}

	// Cancel: nothing is stored, and the wizard comes back to the provider
	// question — the exact screen the user left. This is the contract that
	// makes the handoff worth having.
	s.send("esc")
	s.waitFor("API key")
	s.send("esc")
	s.waitFor("Which provider should carry the connection?")

	// The answers survived: stepping back re-asks the earlier questions with
	// the given answers on screen, not empty fields.
	s.send("esc")
	s.waitFor("Who should be able to reach it?")
	s.send("esc")
	s.waitFor("How should it be reachable?")
	s.send("esc")
	s.waitFor("Probe the existing service after connecting?")
	s.send("esc")
	s.waitFor("What protocol does your service use?")
	s.send("esc")
	s.waitFor("Enter the service address")
	// The wizard normalizes host:port, so the restored answer is the host with
	// the port re-derived onto its own question; either shape proves survival.
	if !strings.Contains(s.screen(), "127.0.0.1") {
		t.Fatalf("the service address did not survive the setup round trip:\n%s", s.screen())
	}

	s.send("ctrl-c")
	s.waitExit()
}

// TestTUIWizardSetupGuidanceHelpRoundTrip pins the second P0 at the compiled
// boundary: Help opened from a provider setup screen returns to that exact
// setup state on the first Escape, for a guidance flow as well as a form.
func TestTUIWizardSetupGuidanceHelpRoundTrip(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	s := f.startTUI(120, 40)
	s.waitFor("Nothing is published yet.")

	// Tailscale is guidance-only and reachable from the Setup readiness screen.
	s.send("s")
	s.waitFor("SET UP")
	s.send("E") // support export page also proves the screen is interactive
	s.page("pgup")
	s.send("home")

	// Navigate to the Tailscale row and open its guidance screen.
	found := false
	for range 14 {
		if strings.Contains(s.screen(), "Tailscale") && strings.Contains(s.screen(), "> ") {
			if strings.Contains(s.screen(), "> ✓ Tailscale") {
				found = true
				break
			}
		}
		before := s.screen()
		s.send("j")
		s.waitForScreenChange(before)
		if strings.Contains(s.screen(), "> ✓ Tailscale") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("could not reach the Tailscale readiness row:\n%s", s.debug())
	}
	s.send("enter")
	s.waitFor("SET UP TAILSCALE")
	s.waitFor("Portico holds no Tailscale credential")

	// ? opens Help; one Escape must return to the guidance screen — under the
	// flag model this Escape tore the guide down behind Help instead.
	s.send("?")
	s.waitFor("HELP")
	s.send("esc")
	s.waitFor("SET UP TAILSCALE")
	if !strings.Contains(s.screen(), "Portico holds no Tailscale credential") {
		t.Fatalf("the guidance content did not survive the Help round trip:\n%s", s.screen())
	}

	// And now the guide itself closes in one key, back to the readiness screen.
	s.send("esc")
	s.waitFor("SET UP")
	s.send("q")
	s.waitExit()
}
