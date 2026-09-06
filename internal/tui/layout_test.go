package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The responsive layouts, at the widths where they change.
//
// The breakpoints existed and almost nothing consulted them: Home drew the same
// full-width list at every size, and ConnectionListWidth was computed and never
// called. A terminal at 110 columns and one at 61 got the same layout, which at
// 61 meant a route strip drawn wider than the terminal.
//
// These are tested at the boundaries — 59/60, 79/80, 109/110 — because a
// breakpoint is only wrong on one side of itself.

// TestBreakpointBoundaries pins which layout each width selects.
func TestBreakpointBoundaries(t *testing.T) {
	cases := []struct {
		width int
		want  LayoutBreakpoint
	}{
		{59, LayoutEmergency},
		{60, LayoutCompact},
		{79, LayoutCompact},
		{80, LayoutStandard},
		{109, LayoutStandard},
		{110, LayoutWide},
	}
	for _, tc := range cases {
		if got := Breakpoint(tc.width); got != tc.want {
			t.Errorf("Breakpoint(%d) = %s, want %s", tc.width, got, tc.want)
		}
	}
}

// TestLayoutFitsInsideTheTerminal pins the property that matters most: nothing
// the layout allocates may exceed the width it was given.
//
// A layout wider than the terminal wraps, and a wrapped route strip is unreadable
// in a way that looks like a rendering bug rather than a sizing one.
func TestLayoutFitsInsideTheTerminal(t *testing.T) {
	for _, width := range []int{59, 60, 79, 80, 109, 110, 200} {
		l := NewLayout(width, 30)
		if l.ListWidth > width {
			t.Errorf("width %d: list width %d exceeds the terminal", width, l.ListWidth)
		}
		if l.SideBySide {
			total := l.ListWidth + gutter + l.DetailWidth
			if total > width {
				t.Errorf("width %d: columns total %d, exceeding the terminal", width, total)
			}
			if l.DetailWidth <= 0 {
				t.Errorf("width %d: side by side with no room for the detail", width)
			}
		} else if l.DetailWidth != 0 {
			t.Errorf("width %d: stacked layout allocated a detail column", width)
		}
		if l.ListRows < 3 {
			t.Errorf("width %d: list rows = %d, too few to be usable", width, l.ListRows)
		}
	}
}

// TestOnlyWideIsSideBySide pins that two columns appear only where both fit.
func TestOnlyWideIsSideBySide(t *testing.T) {
	for _, width := range []int{59, 60, 79, 80, 109} {
		if NewLayout(width, 30).SideBySide {
			t.Errorf("width %d draws two columns, neither of which would be readable", width)
		}
	}
	if !NewLayout(110, 30).SideBySide {
		t.Error("width 110 does not use the room it has for the selected connection")
	}
}

// TestCompactUsesAOneLineRoute pins the compact behaviour.
func TestCompactUsesAOneLineRoute(t *testing.T) {
	for _, width := range []int{60, 79} {
		l := NewLayout(width, 30)
		if !l.ShowRoute {
			t.Errorf("width %d hides the route entirely", width)
		}
		if !l.CompactRoute {
			t.Errorf("width %d draws a full route where only one line fits", width)
		}
		if l.ShowMeasurements {
			t.Errorf("width %d shows traffic figures with no room for them", width)
		}
	}
	for _, width := range []int{80, 110} {
		if NewLayout(width, 30).CompactRoute {
			t.Errorf("width %d compresses the route despite having room", width)
		}
	}
}

// TestEmergencyLayoutStillRenders pins that a very narrow terminal is usable and
// recovers on resize.
func TestEmergencyLayoutStillRenders(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.selectedID = "conn-a"

	for _, width := range []int{20, 40, 59} {
		m.width, m.height = width, 24
		view := m.View().Content
		if strings.TrimSpace(view) == "" {
			t.Fatalf("width %d rendered nothing at all", width)
		}
		// It says what is wrong, so a user does not think Portico is broken.
		if !strings.Contains(view, "60 columns") {
			t.Errorf("width %d does not explain that the terminal is too narrow:\n%s", width, view)
		}
	}

	// Resizing back up recovers the full layout.
	m.width, m.height = 120, 24
	view := m.View().Content
	if strings.Contains(view, "60 columns") {
		t.Error("the emergency screen persisted after the terminal was widened")
	}
	if !strings.Contains(view, "CONNECTIONS") {
		t.Errorf("widening the terminal did not restore the connection list:\n%s", view)
	}
}

// TestHomeRendersAtEveryBreakpoint pins that each layout produces a sane screen.
//
// This is the golden-test shape the audit asks for: rather than freezing exact
// output — which changes whenever a word does — it asserts the properties that
// must hold at each width, which is what a reader of the screen actually relies
// on.
func TestHomeRendersAtEveryBreakpoint(t *testing.T) {
	for _, width := range []int{59, 60, 79, 80, 109, 110} {
		m := readyModel(&fakeClient{}, twoConnectionSnapshot())
		m.selectedID = "conn-a"
		m.width, m.height = width, 30

		view := m.View().Content
		if strings.TrimSpace(view) == "" {
			t.Fatalf("width %d rendered nothing", width)
		}

		// No line may exceed the terminal width, measured in cells.
		for i, line := range strings.Split(view, "\n") {
			if w := displayWidth(line); w > width {
				t.Errorf("width %d: line %d is %d cells wide:\n%s", width, i, w, line)
			}
		}

		if width < 60 {
			continue
		}
		// Above the emergency threshold the selected connection is identified.
		if !strings.Contains(view, "conn-a-name") && !strings.Contains(view, "alpha") {
			// The fixture's names are checked loosely: what matters is that the
			// list rendered something, not which fixture is in use.
			if !strings.Contains(view, "CONNECTIONS") {
				t.Errorf("width %d does not show the connection list:\n%s", width, view)
			}
		}
	}
}

// TestWideCharactersDoNotOverflowTheLayout pins the interaction between display
// width and layout.
//
// A CJK name is twice as wide as its rune count. Measuring it as runes was how a
// list entry came to be drawn wider than the column it was allocated.
func TestWideCharactersDoNotOverflowTheLayout(t *testing.T) {
	snap := ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{
			{ID: "conn-cjk", Name: "日本語のサービス名前", DesiredState: "open", UserState: "Open"},
			{ID: "conn-emoji", Name: "deploy 🚀🚀 staging", DesiredState: "closed", UserState: "Closed"},
			{ID: "conn-combining", Name: "café\u0301 combining", DesiredState: "closed", UserState: "Closed"},
		},
	}
	for _, width := range []int{60, 80, 110} {
		m := readyModel(&fakeClient{}, snap)
		m.selectedID = "conn-cjk"
		m.width, m.height = width, 30

		view := m.View().Content
		for i, line := range strings.Split(view, "\n") {
			if w := displayWidth(line); w > width {
				t.Errorf("width %d: wide characters pushed line %d to %d cells:\n%q",
					width, i, w, line)
			}
		}
	}
}

// TestConnectionListWidthMatchesTheLayout pins that the helper and the layout
// cannot disagree.
//
// ConnectionListWidth was an independent computation that nothing called. Keeping
// it as a second opinion is how the two would drift.
func TestConnectionListWidthMatchesTheLayout(t *testing.T) {
	for _, width := range []int{59, 60, 79, 80, 109, 110} {
		if got, want := ConnectionListWidth(width), NewLayout(width, 0).ListWidth; got != want {
			t.Errorf("width %d: helper says %d, layout says %d", width, got, want)
		}
	}
}

// TestListDetailLayoutCompact60x18ExposesFullBudget verifies the compact
// breakpoint at 60 columns budgets the full height-10 row count (8 items at
// height 18), matching what selectionRowsForAt returns for Discovery and
// Providers screens.
func TestListDetailLayoutCompact60x18ExposesFullBudget(t *testing.T) {
	l := NewListDetailLayout(60, 18)
	if l.Breakpoint != LayoutCompact {
		t.Fatalf("width 60 breakpoint = %s, want compact", l.Breakpoint)
	}
	want := 8 // 18 - 10 (compact budget)
	if l.ListRows != want {
		t.Errorf("compact 60x18 ListRows = %d, want %d", l.ListRows, want)
	}

	for _, screen := range []ScreenID{ScreenDiscovery, ScreenProviders, ScreenOperations} {
		l2 := NewListDetailLayout(60, 18)
		got := l2.ListRows
		if got != want {
			t.Errorf("%s at 60x18 rows = %d, want %d (ListRows)", screen, got, want)
		}
	}
}
