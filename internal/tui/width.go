package tui

import "github.com/charmbracelet/x/ansi"

// Terminal display width, measured rather than guessed.
//
// Several places counted runes and called the result a width. A rune is not a
// cell: a CJK ideograph occupies two, a combining accent occupies none, and an
// emoji with a variation selector or a ZWJ sequence is one grapheme cluster
// spanning two cells across several runes. Counting runes therefore
// mis-measured every one of those, and the errors compound in the two places
// width is actually load-bearing — clipping a label to fit, and centring one
// under a route node.
//
// ansi.StringWidth is the same measurement lipgloss uses to lay out its own
// boxes, so using it here means the route canvas and the surrounding frame
// agree about how wide a string is. It also skips ANSI escape sequences, which
// a rune count included in the total.

// displayWidth returns how many terminal cells a string occupies.
func displayWidth(s string) int {
	return ansi.StringWidth(s)
}

// truncateToWidth clips a string to at most width cells, cutting on a grapheme
// boundary so a wide character is never split into a broken half-cell and a
// combining mark is never separated from the character it modifies.
func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if displayWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "")
}

// Positioning helpers live in the route package, next to the canvas that needs
// them: centring a label under a node and right-aligning one against an edge are
// canvas operations, and this package draws with lipgloss rather than on a grid.
// Copies here were unused, and a second implementation of a measurement is the
// one that drifts.
