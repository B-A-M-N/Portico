package route

import (
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// Terminal cell measurement for the route canvas.
//
// The canvas is a grid of cells, so anything drawn on it has to be measured in
// cells. Counting runes is not that measurement: a CJK ideograph occupies two
// cells, a combining accent occupies none, and an emoji ZWJ sequence is one
// cluster of several runes occupying two. The canvas iterated runes and
// advanced one column each, so every label containing any of those was drawn
// at the wrong width and pushed whatever followed it out of place.

// grapheme is one user-perceived character with the width it occupies.
type grapheme struct {
	// lead is the first rune of the cluster, which is what the canvas stores.
	// A cell holds one rune, so a multi-rune cluster is represented by its
	// base character; the alternative is a cell type that holds a string,
	// which the canvas does not need for the labels it draws.
	lead rune
	// width is the number of cells the cluster occupies: 0 for a combining
	// mark, 1 for a normal character, 2 for a wide one.
	width int
}

// graphemes splits a string into grapheme clusters with their display widths.
func graphemes(s string) []grapheme {
	out := make([]grapheme, 0, len(s))
	state := -1
	rest := s
	for len(rest) > 0 {
		var cluster string
		var width int
		cluster, rest, width, state = uniseg.FirstGraphemeClusterInString(rest, state)
		if cluster == "" {
			break
		}
		lead := []rune(cluster)
		if len(lead) == 0 {
			continue
		}
		out = append(out, grapheme{lead: lead[0], width: width})
	}
	return out
}

// DisplayWidth returns how many terminal cells a string occupies. It is the
// same measurement lipgloss uses for its own layout, so a label measured here
// and a frame measured there agree.
func DisplayWidth(s string) int {
	return ansi.StringWidth(s)
}

// truncateToWidth clips a string to at most width cells on a grapheme
// boundary, so a wide character is never split in half and a combining mark is
// never separated from its base.
func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if DisplayWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "")
}

// centreOffset returns the column at which a string is centred on a target
// column, clamped to zero. Dividing a rune count by two put any label with a
// wide character off-centre by half its own width.
func centreOffset(centre int, s string) int {
	x := centre - DisplayWidth(s)/2
	if x < 0 {
		return 0
	}
	return x
}

// rightAlignOffset returns the column at which a string ends at the given
// column, clamped to zero.
func rightAlignOffset(end int, s string) int {
	x := end - DisplayWidth(s)
	if x < 0 {
		return 0
	}
	return x
}
