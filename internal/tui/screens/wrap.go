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

// WrapProse is WrapText with a hanging indent, and is the one way explanatory
// prose reaches the screen. Indent is the fixed leading spaces the finished
// rows must carry; the same count is subtracted from the available width
// before wrapping, so a line drawn under an indent is wrapped to the cells
// that remain after it. Wrapping at the full width and indenting afterwards is
// the defect this prevents: every physical row then overflows by the indent,
// and the terminal moved the overflow onto rows the renderer counted as other
// lines.
//
// A width of indent or less means no constraint is known (or the terminal is
// too narrow to hold any prose): the text is returned as one row per source
// line rather than wrapped into slivers of a cell.
func WrapProse(text string, width, indent int) []string {
	if indent < 0 {
		indent = 0
	}
	if width <= indent {
		return strings.Split(text, "\n")
	}
	wrapped := WrapText(text, width-indent)
	out := make([]string, 0, len(wrapped))
	pad := strings.Repeat(" ", indent)
	for _, line := range wrapped {
		if line == "" {
			// A blank source line stays blank: indenting an empty row would
			// draw trailing spaces and change nothing visible.
			out = append(out, "")
			continue
		}
		out = append(out, pad+line)
	}
	return out
}

// InnerWidth is the width prose may occupy after the container's fixed
// indentation, and is the one answer to "how wide is the inside of this
// screen". Every prose renderer passes its row's indent here rather than
// deriving its own width, so a container's padding is subtracted exactly once.
// A terminal narrower than the indent leaves nothing: the answer is zero, and
// WrapProse treats zero as "no constraint yet" rather than destroying text.
func InnerWidth(width, indent int) int {
	inner := width - indent
	if inner < 0 {
		return 0
	}
	return inner
}
