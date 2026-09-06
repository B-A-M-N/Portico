package screens

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Wrapping prose to the terminal's cells.
//
// WrapText used to split on newlines and do nothing else, so a sentence longer
// than the terminal wrapped in the PTY itself: the terminal moved the overflow
// onto physical rows the renderer believed belonged to the next line, and the
// next frame's differential update never cleared them. The captured screen
// showed two screens merged — the exact corruption the PTY suite caught at
// 60x18. Byte and rune counts are equally wrong for width: "café中" is five
// runes, four cells.
//
// ansi.Wrap measures grapheme cells, keeps ANSI styling intact across the
// break, and breaks on word boundaries, so what this returns is what a
// terminal will actually fit on one row.

// WrapText wraps prose to terminal cells, preserving explicit blank lines.
// Width <= 0 means "no constraint known yet": the text is returned unwrapped
// rather than destroyed, and the caller clips elsewhere.
func WrapText(text string, width int) []string {
	if width <= 0 {
		return strings.Split(text, "\n")
	}
	paragraphs := strings.Split(text, "\n")
	lines := make([]string, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		if paragraph == "" {
			lines = append(lines, "")
			continue
		}
		// Breakpoints " -" lets a long compound word break at an existing
		// hyphen; "-" alone is always a breakpoint in ansi.Wrap anyway.
		lines = append(lines, strings.Split(ansi.Wrap(paragraph, width, "-"), "\n")...)
	}
	return lines
}
