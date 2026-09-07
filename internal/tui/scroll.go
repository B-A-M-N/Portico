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
//
// Every field is written during Update and only read during View. The first
// version of this mutated the state from inside View, which has a value
// receiver — so the height, the screen and the clamped offset were written to a
// copy that was discarded, and the live model never learned the viewport height
// at all. Page Down incremented an offset that the next render reset to zero,
// and the feature did not work. The unit tests passed because they set the
// height by hand, which is setup production never performs.
type scrollState struct {
	// offset is the first content line rendered, in lines from the top.
	offset int
	// height is the viewport height, set from the window size message.
	height int
	// contentLines is how many lines the current screen last produced, which is
	// what the offset is clamped against.
	contentLines int
	// screen is the screen this offset belongs to.
	screen ScreenID
}

// setHeight records the terminal height and re-clamps.
func (s *scrollState) setHeight(height int) {
	s.height = height
	s.clamp()
}

// setContentLines records how long the current screen's content is and
// re-clamps, so an offset cannot survive the content shrinking beneath it.
func (s *scrollState) setContentLines(lines int) {
	s.contentLines = lines
	s.clamp()
}

// resetFor moves to the top for a newly shown screen.
func (s *scrollState) resetFor(screen ScreenID) {
	s.screen = screen
	s.offset = 0
}

// scrollBy moves the viewport and clamps at both ends.
func (s *scrollState) scrollBy(lines int) {
	s.offset += lines
	s.clamp()
}

// toTop and toBottom are the Home and End motions.
func (s *scrollState) toTop()    { s.offset = 0 }
func (s *scrollState) toBottom() { s.offset = s.maxOffset() }

// clamp keeps the offset inside the content.
func (s *scrollState) clamp() {
	if s.offset > s.maxOffset() {
		s.offset = s.maxOffset()
	}
	if s.offset < 0 {
		s.offset = 0
	}
}

// maxOffset is the furthest the viewport can move down.
func (s *scrollState) maxOffset() int {
	visible := s.visibleLines()
	if visible <= 0 || s.contentLines <= visible {
		return 0
	}
	return s.contentLines - visible
}

// visibleLines is how many content lines fit, reserving one for the indicator
// when there is more than fits.
func (s *scrollState) visibleLines() int {
	if s.height <= 0 {
		return 0
	}
	if s.contentLines > s.height {
		return s.height - 1
	}
	return s.height
}

// page is how far a page key moves, leaving two lines of context.
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

// clipToViewport trims rendered content to the visible height and appends an
// indicator saying what was cut.
//
// It is pure: it reads the offset and returns a string. Clamping is the
// caller's job, done during Update where the result can be kept.
func clipToViewport(content string, height, offset int) string {
	return clipToViewportPinned(content, height, offset, "")
}

// clipToViewportPinned is clipToViewport with a pinned line: when pin is
// non-empty it takes the indicator's own line — the one row that is visible at
// every offset — instead of the "what was cut" text. A message answering the
// key just pressed must survive scrolling; written into scrollable content,
// a blocked action's explanation was rendered and then cut, so the dimmed
// action looked inert. Replacing rather than appending keeps the frame exactly
// as tall as before, so the renderer's line accounting does not change.
func clipToViewportPinned(content string, height, offset int, pin string) string {
	if height <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= height {
		// Nothing is cut, so there is no indicator line to borrow. When the
		// content leaves room the pin rides after it; when it does not, the
		// pin displaces the last line rather than growing the frame past the
		// terminal.
		if pin == "" {
			return content
		}
		if len(lines) < height {
			return content + "\n" + pin
		}
		lines = lines[:height-1]
		return strings.Join(lines, "\n") + "\n" + pin
	}

	visible := height - 1
	maxOffset := len(lines) - visible
	if offset > maxOffset {
		offset = maxOffset
	}
	if offset < 0 {
		offset = 0
	}

	window := lines[offset : offset+visible]
	indicator := scrollIndicator(offset, visible, len(lines))
	if pin != "" {
		indicator = pin
	}
	return strings.Join(window, "\n") + "\n" + indicator
}

// scrollIndicator says where the viewport is and how to move it. A user cannot
// act on hidden content they have not been told about.
func scrollIndicator(offset, visible, total int) string {
	above := offset
	below := total - offset - visible
	switch {
	case above > 0 && below > 0:
		return fmt.Sprintf("  ↑ %d above · %d below — pgup/pgdn, home/end", above, below)
	case below > 0:
		return fmt.Sprintf("  ↓ %d more below — pgdn to scroll, end for the last", below)
	case above > 0:
		return fmt.Sprintf("  ↑ %d more above — pgup to scroll, home for the first", above)
	default:
		return ""
	}
}

// countLines is how many lines a rendered screen occupies.
func countLines(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}
