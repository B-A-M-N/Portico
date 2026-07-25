package tui

// LayoutBreakpoint defines responsive layout breakpoints.
type LayoutBreakpoint int

const (
	LayoutEmergency LayoutBreakpoint = iota // < 60 cols
	LayoutCompact                           // 60–79 cols
	LayoutStandard                          // 80–109 cols
	LayoutWide                              // >= 110 cols
)

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

// ConnectionListWidth returns the connection list width in columns.
func ConnectionListWidth(width int) int {
	bp := Breakpoint(width)
	switch bp {
	case LayoutWide:
		return 30
	case LayoutStandard:
		return min(width-2, 30)
	default:
		return max(width-2, 20)
	}
}

// HeaderHeight returns the reserved header height.
func HeaderHeight() int { return 1 }

// FooterHeight returns the reserved footer height for help text.
func FooterHeight() int { return 1 }

// ContentHeight returns the available content height.
func ContentHeight(height int) int {
	return height - HeaderHeight() - FooterHeight()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
