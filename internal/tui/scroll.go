package tui

import (
	"fmt"
	"strings"
)

// Scrolling long screens.
//
// Only the connection list could scroll. Every other screen rendered its full
// content and let the terminal clip it, so anything past the bottom row was
// unreachable: a connector's log tail, a plan with many steps, the diagnostic
// findings for a broken connection, the operation history. The information was
// fetched, formatted, and then thrown away by the terminal, with nothing on
// screen to say there was more.
//
// This clips at the point of render instead, so what is cut off can be reached.
// It is deliberately one mechanism for every screen rather than per-screen
// scroll state, which is how the class of defect arose.
//
// The connection list keeps its own offset because it solves a different
// problem: it follows the selection so the highlighted row stays visible. This
// one is free scrolling, driven by the user rather than by a cursor.

// scrollState is the vertical offset for the screen currently shown.
type scrollState struct {
	// offset is the first content line rendered, in lines from the top.
	offset int
	// height is the viewport height last rendered, used to size a page.
	height int
	// overflow reports whether the last render had more content than fit.
	overflow bool
	// screen is the screen this offset belongs to. Navigation happens through
	// pushScreen, popScreen and direct assignment in a dozen places, so the
	// change is detected at render rather than hooked at each site — otherwise
	// opening a screen would inherit wherever the previous one was scrolled to.
	screen ScreenID
}

// follow resets the offset when the screen has changed since the last render.
func (s *scrollState) follow(screen ScreenID) {
	if s.screen != screen {
		s.screen = screen
		s.reset()
	}
}

// reset returns to the top, which is where a newly opened screen starts.
func (s *scrollState) reset() {
	s.offset = 0
	s.overflow = false
}

// scrollBy moves the viewport, clamping at the top. The bottom is clamped at
// render time, where the content length is known.
func (s *scrollState) scrollBy(lines int) {
	s.offset += lines
	if s.offset < 0 {
		s.offset = 0
	}
}

// page returns the number of lines a page-up or page-down moves, leaving two
// lines of context so the user can see where they were.
func (s *scrollState) page() int {
	if s.height > 3 {
		return s.height - 2
	}
	return 1
}

// scrollsFreely reports whether a screen is scrolled by this mechanism.
//
// The home screen manages its own offset to follow the selected connection, and
// applying both would move it twice for one keypress.
func scrollsFreely(screen ScreenID) bool {
	return screen != ScreenHome && screen != ScreenBoot && screen != ScreenQuit
}

// clipToViewport trims rendered content to the visible height and reports what
// was cut, so a screen never silently hides the rest of its own output.
//
// The returned content always fills at most height lines including the
// indicator, so adding the indicator cannot itself push a line off the bottom.
func clipToViewport(content string, height int, state *scrollState) string {
	if height <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")

	state.height = height
	if len(lines) <= height {
		state.offset = 0
		state.overflow = false
		return content
	}
	state.overflow = true

	// One line is spent on the indicator that says there is more.
	visible := height - 1
	maxOffset := len(lines) - visible
	if state.offset > maxOffset {
		state.offset = maxOffset
	}
	if state.offset < 0 {
		state.offset = 0
	}

	window := lines[state.offset : state.offset+visible]
	return strings.Join(window, "\n") + "\n" + scrollIndicator(state.offset, visible, len(lines))
}

// scrollIndicator says where the viewport is and how to move it. A user cannot
// act on hidden content they have not been told about.
func scrollIndicator(offset, visible, total int) string {
	above := offset
	below := total - offset - visible
	switch {
	case above > 0 && below > 0:
		return fmt.Sprintf("  ↑ %d more above · %d more below — pgup/pgdn to scroll", above, below)
	case below > 0:
		return fmt.Sprintf("  ↓ %d more below — pgdn to scroll", below)
	case above > 0:
		return fmt.Sprintf("  ↑ %d more above — pgup to scroll", above)
	default:
		return ""
	}
}
