//go:build linux

package tui_e2e

import (
	"strings"
	"testing"
)

// narrowSizes are the audit's six supported terminal sizes, exercised as the
// compiled binary draws them. The unit suite already proves prose
// reconstructs across every size; this exists because the regression the
// audit found shipped through a real terminal the unit renderer got wrong.
// It drives the screens where truncated prose hurts most — fresh Home and
// Providers — and asserts two things no-overflow alone cannot promise: no row
// exceeds the real inner width, and a full instructional sentence survives
// unamputated.
//
// Critical-sentence reconstruction is the audit's finding-5 bar: at 60x18 a
// beginner's setup instruction that ends in "…PORTICO_ENABLE…" has been
// drained of exactly the variable they need. These assertions make the whole
// sentence present, not just its prefix.
var narrowSizes = []struct{ width, height int }{
	{200, 60}, {120, 40}, {100, 30}, {80, 24}, {70, 20}, {60, 18},
}

func TestTUICriticalScreensSurviveEveryNarrowSize(t *testing.T) {
	requireE2E(t)
	for _, size := range narrowSizes {
		size := size
		t.Run(fmtSize(size.width, size.height), func(t *testing.T) {
			f := newFixture(t)

			// A fresh install's Home must offer the paths with their full
			// explanations. The task body sentence is long enough that some
			// sizes wrap it; the whole sentence must still be present, which
			// means no wrapped tail was amputated by a mis-measured width.
			s := f.startTUI(size.width, size.height)
			s.waitFor("Make something reachable")
			s.assertNoOverflow()
			assertProseSurvives(t, s,
				"Describe what you want reachable and who should be able to reach it. Portico picks the provider and the steps.")

			// Providers is where a beginner reads setup instructions. The
			// catalog is taller than any viewport, so scroll to the end — the
			// least-tested edge — and pin that no drawn row exceeds the real
			// width. That is the finding-3/finding-5 gate: instructions wrap
			// within the usable region instead of being amputated by a
			// renderer that counts the full terminal width as available
			// content.
			s.send("p")
			s.waitFor("PROVIDERS")
			// Scroll to the bottom (the least-tested edge). The wait is on the
			// last provider's row, which at all sizes only renders once the
			// scroll reaches it; it is both the settled-frame signal and the
			// guarantee that assertNoOverflow reads the true bottom frame, not a
			// pre-scroll snapshot.
			s.send("end")
			s.waitFor("zrok")
			s.assertNoOverflow()
			// One page back up reveals the client-supported provider's setup
			// actions. Finding-5's bar is that a beginner instruction ends
			// with the exact variable they need (CONTROL_PLANE_API_KEY), so
			// the whole sentence must reconstruct — no-overflow alone cannot
			// promise a full instructional sentence survived unamputated.
			s.send("pgup")
			s.waitFor("CONTROL_PLANE_API_KEY")
			assertProseSurvives(t, s,
				"Export CONTROL_PLANE_API_KEY before starting the supervisor")
			s.send("ctrl-c")
			s.waitExit()
		})
	}
}

// assertProseSurvives proves a complete sentence is reconstructable from the
// PTY screen: joining the rendered lines and removing break whitespace must
// contain every word of the sentence in order. A clipped tail quietly fails
// this even though no row overflows.
func assertProseSurvives(t *testing.T, s *session, sentence string) {
	t.Helper()
	want := strings.Fields(sentence)
	got := normalizePTYProse(s.screen())
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("a word (%q) of a critical sentence is missing from the screen: %q\n%s", w, sentence, s.debug())
		}
	}
}

// normalizePTYProse joins the screen's rows into a single whitespace-run,
// mirroring the unit suite's normalization.
func normalizePTYProse(screen string) string {
	return strings.Join(strings.Fields(screen), " ")
}

func fmtSize(w, h int) string {
	return itoa(w) + "x" + itoa(h)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
