//go:build linux

package tui_e2e

import (
	"fmt"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestDebugDeleteChars(t *testing.T) {
	term := newTerminal(10, 1)
	term.feed([]byte("AB界123456"))
	term.feed([]byte("\x1b[3H\x1b[2P"))

	fmt.Println("Cells after 2P (before normalizeWideRow was called inside deleteChars):")
	// Actually normalizeWideRow already ran. Let me trace through it.

	// Simulate normalizeWideRow on the row
	row := term.cells[0]
	fmt.Printf("Row: %v\n", row)

	// Manually trace normalizeWideRow
	for col := 0; col < term.width; col++ {
		cell := row[col]
		fmt.Printf("  normalize col=%d cell=%q\n", col, cell)
		if cell == "" {
			if col == 0 || runewidth.StringWidth(row[col-1]) != 2 {
				fmt.Printf("    -> empty, lead cell width!=2, fill with space\n")
				row[col] = " "
			} else {
				fmt.Printf("    -> empty, orphan continuation of wide glyph, SKIP\n")
			}
			continue
		}
		w := runewidth.StringWidth(cell)
		fmt.Printf("    -> width=%d, col+1=%d\n", w, col+1)
		if w == 2 && (col+1 >= term.width || row[col+1] != "") {
			fmt.Printf("    -> wide with missing continuation, clear\n")
			row[col] = " "
		}
	}
	fmt.Printf("After normalize trace: %v\n", row)
	fmt.Printf("text()=%q\n", term.text())
}
