package tui

// Responsive layout.
//
// The breakpoints existed and almost nothing consulted them. Home drew the same
// full-width list at every size, ConnectionListWidth was computed and never
// called, and the only width-dependent behaviour was the emergency screen below
// sixty columns. A terminal at 110 columns and one at 61 got the same layout,
// which at 61 meant a route strip drawn wider than the terminal.
//
// A Layout is the answer to "what fits here" — computed once from the width, and
// consulted rather than re-derived. The alternative is each renderer deciding for
// itself, which is how the breakpoints came to be scaffolding.

// LayoutBreakpoint defines responsive layout breakpoints.
type LayoutBreakpoint int

const (
	LayoutEmergency LayoutBreakpoint = iota // < 60 cols
	LayoutCompact                           // 60–79 cols
	LayoutStandard                          // 80–109 cols
	LayoutWide                              // >= 110 cols
)

// String names a breakpoint, for tests and for the technical detail on screen.
func (b LayoutBreakpoint) String() string {
	switch b {
	case LayoutWide:
		return "wide"
	case LayoutStandard:
		return "standard"
	case LayoutCompact:
		return "compact"
	default:
		return "emergency"
	}
}

// Breakpoint returns the layout breakpoint for a given width.
func Breakpoint(width int) LayoutBreakpoint {
	switch {
	case width >= 110:
		return LayoutWide
	case width >= 80:
		return LayoutStandard
	case width >= 60:
		return LayoutCompact
	default:
		return LayoutEmergency
	}
}

// Layout is what the current terminal size affords.
//
// It is computed from the width and height once per render, so every part of a
// screen agrees about how much room it has. Nothing here decides content: it
// decides space.
type Layout struct {
	Breakpoint LayoutBreakpoint
	Width      int
	Height     int

	// SideBySide reports that the connection list and the selected connection's
	// detail are drawn as two columns rather than stacked.
	SideBySide bool
	// ListWidth is the columns available to the connection list, and DetailWidth
	// the columns available beside it. DetailWidth is zero when the layout is
	// stacked.
	ListWidth   int
	DetailWidth int
	// ListRows is how many connections can be listed before the list scrolls.
	ListRows int
	// ShowRoute reports that there is room for the route strip at all.
	ShowRoute bool
	// CompactRoute reports that the route must be drawn on one line.
	CompactRoute bool
	// ShowMeasurements reports that there is room for the traffic figures.
	ShowMeasurements bool
}

// gutter is the space between the two columns of a side-by-side layout.
const gutter = 2

// reservedRows is what a Home render spends on things other than the list: the
// header, the section title, the route, the status line and the footer.
const reservedRows = 10

// NewLayout computes what fits at this size.
func NewLayout(width, height int) Layout {
	l := Layout{
		Breakpoint: Breakpoint(width),
		Width:      width,
		Height:     height,
	}

	switch l.Breakpoint {
	case LayoutWide:
		// The list on the left, the selected connection beside it: at this width
		// there is room to see the whole set and one member of it at once.
		l.SideBySide = true
		l.ListWidth = 34
		l.DetailWidth = width - l.ListWidth - gutter
		l.ShowRoute = true
		l.ShowMeasurements = true
	case LayoutStandard:
		// A limited list with the selected connection's route below it. Two
		// columns at eighty would leave neither wide enough to read.
		l.ListWidth = width - gutter
		l.ShowRoute = true
		l.ShowMeasurements = true
	case LayoutCompact:
		// The selection, a one-line route, and the actions. The list is still
		// shown but shorter: what matters at this width is the connection in
		// hand rather than the whole inventory.
		l.ListWidth = width - gutter
		l.ShowRoute = true
		l.CompactRoute = true
		l.ShowMeasurements = false
	default:
		// Below sixty columns nothing can be laid out. The emergency screen says
		// so and keeps working: a resize back up must recover, so this is a
		// narrower rendering rather than a refusal to render.
		l.ListWidth = maxInt(width-2, 10)
		l.ShowRoute = false
	}

	l.ListRows = listRowsFor(l)
	return l
}

// listRowsFor is how many connections fit, given what else the screen spends
// vertical space on.
func listRowsFor(l Layout) int {
	if l.Height <= 0 {
		// No size has been reported yet. A conservative row count is better than
		// a computed negative one.
		return 5
	}
	rows := l.Height - reservedRows
	if l.Breakpoint == LayoutCompact {
		// The compact layout spends fewer rows on the route, so the list gets
		// them back.
		rows += 2
	}
	if l.SideBySide {
		// Side by side, the detail column takes no rows from the list.
		rows += 3
	}
	if rows < 3 {
		return 3
	}
	return rows
}

// ConnectionListWidth returns the connection list width in columns.
//
// It is kept because it names something a caller wants, and now delegates to the
// layout so there is one computation rather than two that can disagree.
func ConnectionListWidth(width int) int {
	return NewLayout(width, 0).ListWidth
}

// HeaderHeight returns the reserved header height.
func HeaderHeight() int { return 1 }

// FooterHeight returns the reserved footer height for help text.
func FooterHeight() int { return 1 }

// ContentHeight returns the available content height.
func ContentHeight(height int) int {
	return height - HeaderHeight() - FooterHeight()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ListDetailLayout is the compact-layout subset used by Discovery, Providers,
// and Operations screens. It reuses the same row budgeting so the selection
// viewport and the renderer always agree.
type ListDetailLayout struct {
	Breakpoint  LayoutBreakpoint
	ListWidth   int
	DetailWidth int
	ListRows    int
	SideBySide  bool
}

// NewListDetailLayout computes what fits for Discovery-style screens.
func NewListDetailLayout(width, height int) ListDetailLayout {
	l := ListDetailLayout{
		Breakpoint: Breakpoint(width),
		ListWidth:  width,
	}

	// Compact layout: 8 list rows at 60+ cols, same as selectionRowsForAt.
	if l.Breakpoint == LayoutCompact {
		l.DetailWidth = 0                 // stacked
		l.ListRows = maxInt(height-10, 3) // height-10 mirrors reservedRows + compact bonus
	} else if l.Breakpoint == LayoutStandard {
		l.DetailWidth = 0 // still stacked at standard
		l.ListRows = maxInt(height-8, 3)
	} else {
		// Wide: split columns.
		l.SideBySide = true
		l.DetailWidth = width - 36 - gutter // 34 list + 2 gutter
		if l.DetailWidth < 10 {
			l.DetailWidth = 10
		}
		l.ListWidth = 34
		l.ListRows = maxInt(height-8, 3)
	}

	return l
}
