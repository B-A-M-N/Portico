package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/B-A-M-N/portico/internal/ipc"
)

func numberedLines(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%d", i)
	}
	return strings.Join(lines, "\n")
}

// TestContentTallerThanTheTerminalCanBeReached pins audit finding 14.
//
// Only the connection list scrolled. Every other screen rendered its full
// content and let the terminal clip it, so a connector's log tail, a plan with
// many steps, or the findings for a broken connection were fetched, formatted,
// and then discarded by the terminal with nothing on screen to say so.
func TestContentTallerThanTheTerminalCanBeReached(t *testing.T) {
	var state scrollState
	content := numberedLines(100)

	top := clipToViewport(content, 10, &state)
	if !strings.Contains(top, "line-0") {
		t.Fatalf("the top of the content is not shown:\n%s", top)
	}
	if strings.Contains(top, "line-50") {
		t.Fatalf("content past the viewport was rendered:\n%s", top)
	}
	if !state.overflow {
		t.Fatal("overflow was not reported")
	}

	state.scrollBy(50)
	middle := clipToViewport(content, 10, &state)
	if !strings.Contains(middle, "line-50") {
		t.Fatalf("scrolling did not reach line 50:\n%s", middle)
	}
	if strings.Contains(middle, "line-0\n") {
		t.Fatalf("the viewport did not move:\n%s", middle)
	}
}

// TestTheUserIsToldThereIsMore pins that hidden content is announced. A user
// cannot act on content they were never told exists.
func TestTheUserIsToldThereIsMore(t *testing.T) {
	var state scrollState
	view := clipToViewport(numberedLines(100), 10, &state)
	if !strings.Contains(view, "more below") {
		t.Fatalf("nothing said there was more content:\n%s", view)
	}

	state.scrollBy(50)
	view = clipToViewport(numberedLines(100), 10, &state)
	if !strings.Contains(view, "more above") || !strings.Contains(view, "more below") {
		t.Fatalf("the indicator does not report both directions:\n%s", view)
	}
}

// TestTheViewportNeverExceedsTheTerminalHeight pins that the indicator cannot
// itself push a line off the bottom.
func TestTheViewportNeverExceedsTheTerminalHeight(t *testing.T) {
	for _, height := range []int{3, 5, 10, 24, 50} {
		var state scrollState
		view := clipToViewport(numberedLines(200), height, &state)
		if got := len(strings.Split(view, "\n")); got > height {
			t.Errorf("height %d: rendered %d lines", height, got)
		}
	}
}

// TestScrollingStopsAtTheEnd pins that the viewport cannot run off the bottom
// into blank space.
func TestScrollingStopsAtTheEnd(t *testing.T) {
	var state scrollState
	content := numberedLines(30)

	state.scrollBy(1000)
	view := clipToViewport(content, 10, &state)
	if !strings.Contains(view, "line-29") {
		t.Fatalf("over-scrolling lost the end of the content:\n%s", view)
	}
	if strings.Count(view, "\n") > 9 {
		t.Fatalf("over-scrolling produced blank space:\n%q", view)
	}
}

// TestShortContentIsNotClipped pins that content which fits is untouched, with
// no indicator and no reserved line.
func TestShortContentIsNotClipped(t *testing.T) {
	var state scrollState
	content := numberedLines(5)
	if got := clipToViewport(content, 20, &state); got != content {
		t.Fatalf("content that fits was modified:\n%s", got)
	}
	if state.overflow {
		t.Fatal("content that fits reported overflow")
	}
}

// TestOpeningAScreenStartsAtTheTop pins that a screen does not inherit the
// scroll position of the one before it — which would open a fresh screen part
// way down for no reason the user could see.
func TestOpeningAScreenStartsAtTheTop(t *testing.T) {
	var state scrollState
	state.follow(ScreenOperations)
	state.scrollBy(40)

	state.follow(ScreenInspect)
	if state.offset != 0 {
		t.Fatalf("a newly opened screen started at offset %d", state.offset)
	}
}

// TestTheConnectionListIsNotScrolledTwice pins that the home screen, which
// already tracks an offset to keep the selected connection visible, is not also
// moved by this mechanism.
func TestTheConnectionListIsNotScrolledTwice(t *testing.T) {
	if scrollsFreely(ScreenHome) {
		t.Fatal("the connection list would be scrolled by two mechanisms at once")
	}
}

// TestScrollKeysReachHiddenContent pins the keys end to end through the model.
func TestScrollKeysReachHiddenContent(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenOperations
	m.height = 10
	m.scroll.follow(ScreenOperations)
	m.scroll.height = 10

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = next.(Model)

	if m.scroll.offset == 0 {
		t.Fatal("page down did not move the viewport")
	}

	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = next.(Model)
	if m.scroll.offset != 0 {
		t.Fatalf("page up did not return to the top: offset %d", m.scroll.offset)
	}
}

// TestScrollKeysDoNotFireWhileTyping pins that page keys reach the field rather
// than the viewport when a question is being answered, and that the wizard's
// own keyboard ownership is not broken by adding a global key.
func TestScrollKeysDoNotFireWhileTyping(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenNewConnection
	m.height = 10

	before := m.scroll.offset
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = next.(Model)

	if m.scroll.offset != before {
		t.Fatal("a page key scrolled the screen while the wizard owned the keyboard")
	}
}

// TestLeavingThePreviewWithQAbandonsThePlan pins audit finding 19 where it
// overlaps finding 1.
//
// Abandoning the in-flight plan was written into the esc handler alone, so
// leaving the preview with q left the request live — and the reply reopened the
// preview behind the user, on a screen they had already dismissed.
func TestLeavingThePreviewWithQAbandonsThePlan(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.selectedID = "conn-a"
	m.planOpenCmd("conn-a")
	generation := m.planRequests.current
	m.screen = ScreenPlanPreview

	next, _ := m.Update(keyMsg("q"))
	m = next.(Model)
	if m.screen != ScreenHome {
		t.Fatalf("q did not leave the preview: screen = %q", m.screen)
	}

	next, _ = m.Update(planLoadedMsg{
		Generation: generation, ConnectionID: "conn-a",
		Plan: &ipc.PlanDTO{ID: "plan-1", ConnectionID: "conn-a", Intent: "open"},
	})
	m = next.(Model)

	if m.screen == ScreenPlanPreview {
		t.Fatal("a dismissed plan reopened the preview behind the user")
	}
}

// TestAJumpKeyDoesNotFireDuringADecision pins that keys meaning "go somewhere
// else" do not leave a screen that is asking a question. s and p jumped to
// setup and providers from a plan preview awaiting approval.
func TestAJumpKeyDoesNotFireDuringADecision(t *testing.T) {
	for _, screen := range []ScreenID{ScreenPlanPreview, ScreenRepair, ScreenOperationProgress} {
		for _, key := range []string{"s", "p"} {
			m := readyModel(&fakeClient{}, testSnapshot())
			m.screen = screen

			next, _ := m.Update(keyMsg(key))
			m = next.(Model)

			if m.screen != screen {
				t.Errorf("%q on %s navigated to %s mid-decision", key, screen, m.screen)
			}
		}
	}
}

// TestAJumpKeyStillWorksWhereItShould guards the opposite error.
func TestAJumpKeyStillWorksWhereItShould(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenHome

	next, _ := m.Update(keyMsg("p"))
	m = next.(Model)
	if m.screen != ScreenProviders {
		t.Fatalf("p from home went to %s, want the providers screen", m.screen)
	}
}

// TestHelpDoesNotStrandTheUser pins that ? on the help screen does not record
// help as the screen to return to.
func TestHelpDoesNotStrandTheUser(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenHome

	next, _ := m.Update(keyMsg("?"))
	m = next.(Model)
	next, _ = m.Update(keyMsg("?"))
	m = next.(Model)

	next, _ = m.Update(keyMsg("esc"))
	m = next.(Model)

	if m.screen != ScreenHome {
		t.Fatalf("esc from help went to %q, want home", m.screen)
	}
}
