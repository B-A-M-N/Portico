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

// centreOffset returns the starting column at which a string of the given
// display width is centred on a target column, clamped to zero.
//
// This is what the route renderer needs to put a provider label under the
// gateway node. It divided a rune count by two, so a label containing any wide
// character was drawn off-centre by half its width.
func centreOffset(centre int, s string) int {
	x := centre - displayWidth(s)/2
	if x < 0 {
		return 0
	}
	return x
}

// rightAlignOffset returns the starting column at which a string ends at the
// given column, clamped to zero.
func rightAlignOffset(end int, s string) int {
	x := end - displayWidth(s)
	if x < 0 {
		return 0
	}
	return x
}
