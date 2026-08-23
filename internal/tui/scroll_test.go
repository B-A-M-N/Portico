package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Scrolling is tested through the real model lifecycle.
//
// The first version of these tests set the viewport height by hand and called
// the screen-follow helper directly, then asserted on the clipping function.
// Both passed while the feature did not work at all: the state they set up was
// written during View, which has a value receiver, so production never kept it
// and Page Down moved an offset the next render reset. A test that performs
// setup production cannot perform proves nothing about production.

// operationsModel returns a ready model showing more operations than fit.
func operationsModel(t *testing.T, height int) Model {
	t.Helper()
	// Each operation names a distinct connection, so a row is identifiable in the
	// rendered output. The connection is looked up in the snapshot and named, so
	// the snapshot carries one per operation: rows lead with the connection's
	// name now, not its ID.
	ops := make([]ipc.OperationDTO, 40)
	snap := testSnapshot()
	snap.Connections = nil
	for i := range ops {
		id := fmt.Sprintf("marker-%02d", i)
		ops[i] = ipc.OperationDTO{
			ID:           fmt.Sprintf("op-%02d", i),
			ConnectionID: id,
			State:        ipc.OperationCompleted,
			Intent:       "open",
		}
		snap.Connections = append(snap.Connections, ipc.ConnectionDTO{
			ID: id, Name: id, DesiredState: "closed", UserState: "Closed",
		})
	}
	m := readyModel(&fakeClient{}, snap)
	m.operations = ops
	m.operationsAvailable = true

	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: height})
	m = next.(Model)
	next, _ = m.Update(keyMsg("o"))
	return next.(Model)
}

// TestContentTallerThanTheTerminalCanBeReached pins audit finding 14 through
// the path a user takes: size the window, open a screen, press Page Down.
func TestContentTallerThanTheTerminalCanBeReached(t *testing.T) {
	m := operationsModel(t, 20)

	first := m.View().Content
	if !strings.Contains(first, "more below") {
		t.Fatalf("a screen taller than the terminal does not say so:\n%s", first)
	}

	next, _ := m.Update(keyMsg("pgdown"))
	m = next.(Model)
	second := m.View().Content

	if first == second {
		t.Fatal("page down did not move the viewport")
	}
	if m.scroll.offset == 0 {
		t.Fatal("the offset was not kept: the model never learned it moved")
	}
}

// TestEndReachesTheBottomAndHomeReturns pins that the whole content is
// reachable, not merely some of it.
func TestEndReachesTheBottomAndHomeReturns(t *testing.T) {
	m := operationsModel(t, 20)

	next, _ := m.Update(keyMsg("end"))
	m = next.(Model)
	bottom := m.View().Content
	if !strings.Contains(bottom, "marker-39") {
		t.Fatalf("the end of the content is unreachable:\n%s", bottom)
	}
	if strings.Contains(bottom, "more below") {
		t.Fatalf("the bottom still reports content below it:\n%s", bottom)
	}

	next, _ = m.Update(keyMsg("home"))
	m = next.(Model)
	if m.scroll.offset != 0 {
		t.Fatalf("home left the offset at %d", m.scroll.offset)
	}
}

// TestPagingDownRepeatedlyStopsAtTheEnd pins that the offset is clamped against
// real content rather than running off into blank space.
func TestPagingDownRepeatedlyStopsAtTheEnd(t *testing.T) {
	m := operationsModel(t, 20)
	for i := 0; i < 50; i++ {
		next, _ := m.Update(keyMsg("pgdown"))
		m = next.(Model)
	}
	view := m.View().Content
	if !strings.Contains(view, "marker-39") {
		t.Fatalf("over-paging lost the end of the content:\n%s", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines > 20 {
		t.Fatalf("the view is %d lines in a 20 line terminal", lines)
	}
}

// TestViewIsPure pins the rule the whole design rests on: rendering must not
// change the program. It was violated twice — the scroll state was written to a
// discarded copy, and the inspect model was written through a live pointer.
func TestViewIsPure(t *testing.T) {
	for _, screen := range []ScreenID{
		ScreenHome, ScreenOperations, ScreenInspect, ScreenProviders, ScreenHelp,
	} {
		m := operationsModel(t, 20)
		m.selectedID = "conn-1"
		m.transitionTo(screen)
		m.measureViewport()

		before := m.scroll
		// A deep comparison, because the inspect model holds slices and the
		// defect was assignment through a pointer into the live model.
		var inspectBefore string
		if m.inspect != nil {
			inspectBefore = fmt.Sprintf("%#v", *m.inspect)
		}

		first := m.View().Content
		second := m.View().Content

		if m.scroll != before {
			t.Errorf("%s: View changed the scroll state: %+v -> %+v", screen, before, m.scroll)
		}
		if m.inspect != nil && fmt.Sprintf("%#v", *m.inspect) != inspectBefore {
			t.Errorf("%s: View mutated the live inspect model", screen)
		}
		if first != second {
			t.Errorf("%s: two identical renders differ", screen)
		}
	}
}

// TestAScreenOpensAtTheTop pins that a newly shown screen does not inherit the
// offset of the one before it.
func TestAScreenOpensAtTheTop(t *testing.T) {
	m := operationsModel(t, 20)
	next, _ := m.Update(keyMsg("end"))
	m = next.(Model)
	if m.scroll.offset == 0 {
		t.Fatal("the fixture no longer scrolls")
	}

	next, _ = m.Update(keyMsg("esc"))
	m = next.(Model)
	next, _ = m.Update(keyMsg("o"))
	m = next.(Model)

	if m.scroll.offset != 0 {
		t.Fatalf("reopening a screen started at offset %d", m.scroll.offset)
	}
}

// TestShortContentIsNotClipped pins that content which fits is untouched.
func TestShortContentIsNotClipped(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 200})
	m = next.(Model)
	next, _ = m.Update(keyMsg("o"))
	m = next.(Model)

	view := m.View().Content
	if strings.Contains(view, "more below") || strings.Contains(view, "more above") {
		t.Fatalf("content that fits reported an overflow:\n%s", view)
	}
}

// TestTheConnectionListIsNotScrolledTwice pins that the home screen, which
// tracks its own offset to keep the selected connection visible, is not also
// moved by this mechanism.
func TestTheConnectionListIsNotScrolledTwice(t *testing.T) {
	if scrollsFreely(ScreenHome) {
		t.Fatal("the connection list would be scrolled by two mechanisms at once")
	}
}

// TestScrollKeysDoNotFireWhileTyping pins that page keys reach the field rather
// than the viewport when a question is being answered.
func TestScrollKeysDoNotFireWhileTyping(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 10})
	m = next.(Model)
	m.transitionTo(ScreenNewConnection)

	before := m.scroll.offset
	next, _ = m.Update(keyMsg("pgdown"))
	m = next.(Model)

	if m.scroll.offset != before {
		t.Fatal("a page key scrolled the screen while the wizard owned the keyboard")
	}
}

// TestCtrlDIsNotAScrollKey pins that end-of-input is not bound to scrolling.
func TestCtrlDIsNotAScrollKey(t *testing.T) {
	m := operationsModel(t, 20)
	before := m.scroll.offset

	next, _ := m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	m = next.(Model)

	if m.scroll.offset != before {
		t.Fatal("ctrl+d scrolled: in a terminal it means end of input")
	}
}

// TestLeavingThePreviewAbandonsThePlan pins audit finding 19 where it overlaps
// finding 1.
//
// Abandoning the in-flight plan must happen however the screen is left, not only
// on the one path that remembered to do it: a plan left live reopens the preview
// behind the user, on a screen they have already dismissed.
//
// Esc is the key that leaves a screen. q quits Portico from here — it used to
// navigate Home from some screens and quit from others while footers advertised
// it identically in both cases.
func TestLeavingThePreviewAbandonsThePlan(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.selectedID = "conn-a"
	m.planOpenCmd("conn-a")
	generation := m.planRequests.current
	m.screen = ScreenPlanPreview

	next, _ := m.Update(keyMsg("esc"))
	m = next.(Model)
	if m.screen != ScreenHome {
		t.Fatalf("esc did not leave the preview: screen = %q", m.screen)
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

// TestQQuitsRatherThanNavigating pins the normalised navigation semantics.
//
// q advertised itself as "quit" and navigated Home from every screen but one,
// which meant the footer described two different behaviours with one word. Esc
// is back; q is quit; ctrl+c is quit unconditionally.
func TestQQuitsRatherThanNavigating(t *testing.T) {
	for _, screen := range []ScreenID{ScreenHome, ScreenPlanPreview, ScreenOperations} {
		m := readyModel(&fakeClient{}, twoConnectionSnapshot())
		m.screen = screen

		next, cmd := m.Update(keyMsg("q"))
		m = next.(Model)
		if cmd == nil {
			t.Fatalf("q on %s produced no command, so it did not quit", screen)
		}
		if m.screen != screen && m.screen != ScreenQuit {
			t.Fatalf("q on %s navigated to %s instead of quitting", screen, m.screen)
		}
	}
}

// TestQIsTextWhileTyping pins that a printable key stays printable.
//
// The screens that collect text own the keyboard, so q in a hostname is the
// letter q. They must also not advertise q as quit, or the footer would offer a
// key the field swallows.
func TestQIsTextWhileTyping(t *testing.T) {
	for _, screen := range []ScreenID{ScreenNewConnection, ScreenEdit, ScreenClone} {
		if !screenTakesTextInput(screen) {
			t.Fatalf("%s collects text but is not treated as a text screen", screen)
		}
		m := readyModel(&fakeClient{}, twoConnectionSnapshot())
		m.screen = screen
		for _, action := range m.actionsFor(screen).Advertised() {
			for _, key := range action.Keys {
				if key == "q" {
					t.Fatalf("%s advertises q, which its text field consumes", screen)
				}
			}
		}
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
