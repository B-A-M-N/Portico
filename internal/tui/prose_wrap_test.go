package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Prose must survive the terminal it is drawn in.
//
// The empty Home wrote each sentence as one physical line and let the
// terminal wrap it: at 80x24 the tails ("... and kee", "... Port") landed on
// rows the renderer counted as other lines, and the sentence was amputated.
// No-overflow cannot catch that — a line wrapped by the terminal still fits
// the PTY; the words are simply gone from the logical rows. The criterion is
// therefore reconstruction: at every supported size, joining the wrapped rows
// and removing the line breaks must restore the complete sentence, word for
// word.

// auditSizes are the sizes the audit manually reproduced the defect at, plus
// the wide end. 60x18 is the smallest supported terminal.
var auditSizes = []struct{ width, height int }{
	{200, 60}, {120, 40}, {100, 30}, {80, 24}, {70, 20}, {60, 18},
}

// ansiPattern matches styling sequences, which must be stripped before the
// words of a sentence are compared: an escape glued to the first word of a
// row otherwise becomes part of that word.
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

func normalizeProse(s string) string {
	return strings.Join(strings.Fields(ansiPattern.ReplaceAllString(s, "")), " ")
}

func reconstructedProse(lines []string) string {
	return normalizeProse(strings.Join(lines, " "))
}

// TestWrapProsePreservesTheSentence pins the facility itself: whatever the
// width and indent, the wrapped rows carry every word of the original.
func TestWrapProsePreservesTheSentence(t *testing.T) {
	sentence := "Portico makes something on this machine reachable from somewhere else, " +
		"and keeps it that way after you close this window. CONTROL_PLANE_API_KEY must survive intact."
	for _, size := range auditSizes {
		for _, indent := range []int{0, 2, 4, 7, 8} {
			rows := screens.WrapProse(sentence, size.width, indent)
			if got, want := reconstructedProse(rows), normalizeProse(sentence); got != want {
				t.Fatalf("%dx%d indent %d: the sentence did not survive\n got: %s\nwant: %s",
					size.width, size.height, indent, got, want)
			}
			for i, row := range rows {
				if w := displayWidth(row); w > size.width {
					t.Fatalf("%dx%d indent %d: row %d is %d cells, over the %d-cell terminal",
						size.width, size.height, indent, i, w, size.width)
				}
			}
		}
	}
}

// TestEmptyHomeProseSurvivesEveryAuditSize drives the real empty-Home
// renderer at every audit size and reconstructs each task's sentence from the
// physical rows the renderer produced — not from the model.
func TestEmptyHomeProseSurvivesEveryAuditSize(t *testing.T) {
	sentences := []string{
		"Describe what you want reachable and who should be able to reach it. Portico picks the provider and the steps.",
		"Look for services listening on this machine and publish one of them without typing an address.",
		"See what Portico can publish through and add an account. You do not need one for a temporary address.",
	}
	for _, size := range auditSizes {
		t.Run(string(rune(size.width))+"x"+string(rune(size.height)), func(t *testing.T) {
			m := readyModel(&fakeClient{}, ipc.SnapshotDTO{})
			m.width, m.height = size.width, size.height
			m.screen = ScreenHome
			view := m.renderEmptyHome()

			if w, ok := widestLine(view); ok && w > size.width {
				t.Fatalf("%dx%d: a produced row is %d cells wide\n%s", size.width, size.height, w, view)
			}
			for _, want := range sentences {
				if !strings.Contains(normalizeProse(view), normalizeProse(want)) {
					// Reconstruction: every wrapped row is joined by spaces in
					// the view itself, so the normalized view must contain the
					// normalized sentence whenever no row was clipped. A miss
					// means the renderer dropped words.
					t.Fatalf("%dx%d: the sentence was amputated\nwant: %s\ngot:\n%s",
						size.width, size.height, want, view)
				}
			}
		})
	}
}

// widestLine reports the widest row in cell width, ignoring ANSI styling.
func widestLine(view string) (int, bool) {
	widest := 0
	for _, line := range strings.Split(view, "\n") {
		if w := displayWidth(line); w > widest {
			widest = w
		}
	}
	return widest, true
}

// TestProviderInstructionProseSurvivesNarrowTerminals pins the Providers
// screen: setup actions and unusable reasons are the instructions a beginner
// cannot act on if clipped, and CONTROL_PLANE_API_KEY once lost its tail.
func TestProviderInstructionProseSurvivesNarrowTerminals(t *testing.T) {
	longAction := "Export CONTROL_PLANE_API_KEY before starting the supervisor, then restart it so the provider registers"
	longReason := "This machine is on the tailnet and reachability is decided by tailnet membership, which Portico neither stores nor applies"
	snap := ipc.SnapshotDTO{
		Providers: []ipc.ProviderDTO{{
			ID: "client_tunnel", DisplayName: "Client tunnel",
			Availability: "client_missing", Readiness: "client_missing",
			SetupActions: []string{longAction},
			Capabilities: &ipc.CapabilitySetDTO{},
			LastError:    longReason,
		}},
	}
	for _, size := range auditSizes {
		m := readyModel(&fakeClient{}, snap)
		m.width, m.height = size.width, size.height
		m.screen = ScreenProviders
		view := m.renderProviders()

		if w, ok := widestLine(view); ok && w > size.width {
			t.Fatalf("%dx%d: a produced row is %d cells wide\n%s", size.width, size.height, w, view)
		}
		for _, want := range []string{longAction, longReason} {
			if !strings.Contains(normalizeProse(view), normalizeProse(want)) {
				t.Fatalf("%dx%d: instructional prose was amputated\nwant: %s\ngot:\n%s",
					size.width, size.height, want, view)
			}
		}
	}
}

// TestWizardExplanationSurvivesNarrowTerminals pins the wizard outcome
// explanation the audit watched clip mid-word ("Anyone with the link ca").
func TestWizardExplanationSurvivesNarrowTerminals(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	for _, size := range auditSizes {
		m.width, m.height = size.width, size.height
		// m.client satisfies the wizard's ConnectionCreator, exactly as in
		// production dispatch.
		m.wizard = screens.NewWizard(m.client, m.providerSnapshot())
		m.wizard.WithASCII(true).SetSize(size.width, size.height)
		view := m.wizard.View()

		if w, ok := widestLine(view); ok && w > size.width {
			t.Fatalf("%dx%d: a produced row is %d cells wide\n%s", size.width, size.height, w, view)
		}
		// The generated-address explanation is the highlighted outcome's
		// consequences — the complete sentences must be reconstructable.
		if !strings.Contains(normalizeProse(view), normalizeProse(
			"Portico creates a temporary address. Anyone with the link can reach the service while the connection is open, and the address changes each time you open it.")) {
			t.Fatalf("%dx%d: the wizard explanation was amputated\n%s", size.width, size.height, view)
		}
	}
}
