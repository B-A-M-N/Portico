//go:build linux

package tui_e2e

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// terminal is deliberately small, but stateful: it reconstructs the last
// semantic screen from the raw PTY stream. Assertions therefore inspect what a
// user sees after cursor movement, clears, alternate-screen changes and ANSI
// styling, rather than matching an append-only log of render deltas.
type terminal struct {
	width  int
	height int
	row    int
	col    int
	// pendingWrap is the DECAWM "last column flag". A real terminal does not
	// wrap when a glyph fills the final column: it keeps the cursor there and
	// defers the wrap until the next printable character forces it. Wrapping
	// eagerly left this parser one row ahead of a real terminal after every
	// full-width line, and every later relative write then landed on shifted
	// rows — which is how two screens came to share one display in the
	// captured artifacts while the same bytes rendered correctly in xterm.
	pendingWrap bool
	// savedCursor is DECSC/DECRC state.
	savedRow, savedCol int
	savedPendingWrap   bool
	cells              [][]string
	state              terminalState
	csi                []byte
	osc                []byte
	utf8               []byte
}

type terminalState uint8

const (
	terminalNormal terminalState = iota
	terminalEscape
	terminalCSI
	terminalOSC
	terminalOSCEscape
)

func newTerminal(width, height int) *terminal {
	t := &terminal{}
	t.resize(width, height)
	return t
}

func (t *terminal) resize(width, height int) {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	t.width, t.height = width, height
	t.cells = make([][]string, height)
	for row := range t.cells {
		t.cells[row] = make([]string, width)
		for col := range t.cells[row] {
			t.cells[row][col] = " "
		}
	}
	t.row, t.col = 0, 0
	t.pendingWrap = false
	t.state = terminalNormal
	t.csi = nil
	t.osc = nil
	t.utf8 = nil
}

func (t *terminal) feed(data []byte) {
	if len(t.utf8) > 0 {
		data = append(append([]byte(nil), t.utf8...), data...)
		t.utf8 = nil
	}

	for i := 0; i < len(data); i++ {
		b := data[i]
		switch t.state {
		case terminalEscape:
			t.handleEscape(b)
			continue
		case terminalCSI:
			if b >= 0x40 && b <= 0x7e {
				t.handleCSI(b, string(t.csi))
				t.csi = nil
				t.state = terminalNormal
			} else {
				t.csi = append(t.csi, b)
			}
			continue
		case terminalOSC:
			if b == 0x07 {
				t.state = terminalNormal
				t.osc = nil
			} else if b == 0x1b {
				t.state = terminalOSCEscape
			} else {
				t.osc = append(t.osc, b)
			}
			continue
		case terminalOSCEscape:
			if b == '\\' {
				t.state = terminalNormal
				t.osc = nil
			} else {
				t.state = terminalOSC
				t.osc = append(t.osc, 0x1b, b)
			}
			continue
		}

		switch b {
		case 0x1b:
			t.flushUTF8()
			t.state = terminalEscape
		case '\r':
			t.flushUTF8()
			t.col = 0
			t.pendingWrap = false
		case '\n':
			t.flushUTF8()
			t.lineFeed()
		case '\b':
			t.flushUTF8()
			if t.col > 0 {
				t.col--
			}
			t.pendingWrap = false
		case 0x07, '\t':
			t.flushUTF8()
			if b == '\t' {
				t.col = minInt(t.width-1, ((t.col/8)+1)*8)
				t.pendingWrap = false
			}
		case 0x00:
			t.flushUTF8()
		default:
			if b < 0x20 || b == 0x7f {
				continue
			}
			if b < utf8.RuneSelf {
				t.putRune(rune(b))
				continue
			}
			r, size := utf8.DecodeRune(data[i:])
			if r == utf8.RuneError && size == 1 {
				t.utf8 = append(t.utf8, data[i:]...)
				break
			}
			t.putRune(r)
			i += size - 1
		}
	}
	t.flushUTF8()
}

func (t *terminal) flushUTF8() {
	if len(t.utf8) == 0 {
		return
	}
	if r, _ := utf8.DecodeRune(t.utf8); r != utf8.RuneError {
		t.putRune(r)
	}
	t.utf8 = nil
}

func (t *terminal) handleEscape(b byte) {
	switch b {
	case '[':
		t.state = terminalCSI
		t.csi = nil
	case ']':
		t.state = terminalOSC
		t.osc = nil
	case 'M':
		if t.row > 0 {
			t.row--
		}
		t.state = terminalNormal
	case '7':
		// DECSC saves the cursor, including the pending-wrap flag: a restore
		// that lost it would redraw one row off after a full-width line.
		t.savedRow, t.savedCol = t.row, t.col
		t.savedPendingWrap = t.pendingWrap
		t.state = terminalNormal
	case '8':
		// DECRC.
		t.row, t.col = t.savedRow, t.savedCol
		t.pendingWrap = t.savedPendingWrap
		t.state = terminalNormal
	case '=', '>', '(', ')', 'c', 'D', 'E', 'H':
		t.state = terminalNormal
	default:
		t.state = terminalNormal
	}
}

func (t *terminal) handleCSI(final byte, params string) {
	// Private mode, style and keyboard-protocol CSI sequences affect terminal
	// state outside the semantic text and are intentionally ignored.
	params = strings.TrimLeft(params, "? >")
	if strings.ContainsAny(params, "? >") {
		return
	}
	values := parseCSIInts(params)
	first := func(defaultValue int) int {
		if len(values) == 0 || values[0] == 0 {
			return defaultValue
		}
		return values[0]
	}
	second := func(defaultValue int) int {
		if len(values) < 2 || values[1] == 0 {
			return defaultValue
		}
		return values[1]
	}

	switch final {
	case 'A':
		t.row = maxInt(0, t.row-first(1))
		t.pendingWrap = false
	case 'B', 'e':
		t.row = minInt(t.height-1, t.row+first(1))
		t.pendingWrap = false
	case 'C', 'a':
		t.col = minInt(t.width-1, t.col+first(1))
		t.pendingWrap = false
	case 'D':
		t.col = maxInt(0, t.col-first(1))
		t.pendingWrap = false
	case 'G', '`':
		t.col = clamp(first(1)-1, 0, t.width-1)
		t.pendingWrap = false
	case 'd':
		t.row = clamp(first(1)-1, 0, t.height-1)
		t.pendingWrap = false
	case 'H', 'f':
		t.row = clamp(first(1)-1, 0, t.height-1)
		t.col = clamp(second(1)-1, 0, t.width-1)
		t.pendingWrap = false
	case 'J':
		t.eraseDisplay(first(0))
		t.pendingWrap = false
	case 'K':
		t.eraseLine(first(0))
		t.pendingWrap = false
	case 'P':
		t.deleteChars(first(1))
	case '@':
		t.insertChars(first(1))
	case 'L':
		t.insertLines(first(1))
	case 'M':
		t.deleteLines(first(1))
	case 'S':
		// SU — scroll up: content moves toward the top, new blanks at the
		// bottom. Bubble Tea uses this to shift the screen under the cursor
		// without rewriting every row; ignoring it shifted every later frame.
		t.scrollUp(first(1))
		t.pendingWrap = false
	case 'T':
		// SD — scroll down: content moves toward the bottom, new blanks at
		// the top.
		t.scrollDown(first(1))
		t.pendingWrap = false
	case 'm', 'h', 'l', 'n', 'r', 's', 'u', 't', 'W', 'X':
		// Styling, mode toggles, device queries and tab-clear operations do not
		// change the semantic screen content used by these tests.
	}
}

func parseCSIInts(params string) []int {
	if params == "" {
		return nil
	}
	parts := strings.Split(params, ";")
	values := make([]int, len(parts))
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		values[i] = value
	}
	return values
}

func (t *terminal) putRune(r rune) {
	if t.row < 0 || t.row >= t.height || t.col < 0 || t.col >= t.width {
		return
	}
	width := runewidth.RuneWidth(r)
	if width <= 0 {
		// Combining marks occupy no cells. Attach them to the preceding visible
		// cell, skipping the empty continuation cell of a wide glyph.
		for col := t.col - 1; col >= 0; col-- {
			if t.cells[t.row][col] != "" {
				t.cells[t.row][col] += string(r)
				break
			}
		}
		return
	}
	if width > 2 {
		width = 1
	}
	// DECAWM wrap is deferred: a glyph that fills the last column leaves the
	// cursor there with the wrap pending, and the wrap happens when the NEXT
	// printable character arrives — not now.
	if t.pendingWrap {
		t.pendingWrap = false
		t.col = 0
		t.lineFeed()
		if t.row >= t.height {
			return
		}
	}
	// A glyph wider than the remaining space on the row wraps early, as a real
	// terminal does; it is never split across rows.
	if t.col+width > t.width {
		t.col = 0
		t.lineFeed()
		if t.row >= t.height {
			return
		}
	}
	// A cursor can legally land on the continuation cell of a wide glyph after
	// a CSI movement. Replace the complete glyph rather than leaving its lead
	// cell visible beside the new character.
	t.clearCell(t.row, t.col)
	if width == 2 {
		t.clearCell(t.row, t.col+1)
	}
	t.cells[t.row][t.col] = string(r)
	if width == 2 && t.col+1 < t.width {
		t.cells[t.row][t.col+1] = ""
	}
	t.col += width
	if t.col >= t.width {
		t.col = t.width - 1
		t.pendingWrap = true
	}
}

// lineFeed follows the terminal's default LF behavior: it moves vertically,
// scrolling when already on the bottom row, without implicitly returning to
// column zero. Applications that need CRLF emit the carriage return
// separately. Clamping at the bottom instead of scrolling silently dropped
// every row after a screenful and is not what any real terminal does.
func (t *terminal) lineFeed() {
	t.pendingWrap = false
	if t.row >= t.height-1 {
		t.scrollUp(1)
		return
	}
	t.row++
}

// scrollUp moves every row above the bottom up by count, the scroll region
// being the whole screen, which is all these tests exercise.
func (t *terminal) scrollUp(count int) {
	count = clamp(count, 1, t.height)
	for row := 0; row+count < t.height; row++ {
		t.cells[row] = t.cells[row+count]
	}
	for row := maxInt(t.height-count, 0); row < t.height; row++ {
		t.cells[row] = blankLine(t.width)
	}
}

// scrollDown is SD's inverse: every row moves toward the bottom by count and
// the top receives blanks.
func (t *terminal) scrollDown(count int) {
	count = clamp(count, 1, t.height)
	for row := t.height - 1; row-count >= 0; row-- {
		t.cells[row] = t.cells[row-count]
	}
	for row := 0; row < count && row < t.height; row++ {
		t.cells[row] = blankLine(t.width)
	}
}

func (t *terminal) eraseDisplay(mode int) {
	switch mode {
	case 2, 3:
		for row := range t.cells {
			for col := range t.cells[row] {
				t.clearCell(row, col)
			}
		}
	case 1:
		for row := 0; row <= t.row && row < t.height; row++ {
			end := t.width
			if row == t.row {
				end = minInt(t.col+1, t.width)
			}
			for col := 0; col < end; col++ {
				t.clearCell(row, col)
			}
		}
	default:
		for row := t.row; row < t.height; row++ {
			start := 0
			if row == t.row {
				start = t.col
			}
			for col := start; col < t.width; col++ {
				t.clearCell(row, col)
			}
		}
	}
}

func (t *terminal) eraseLine(mode int) {
	start, end := 0, t.width
	switch mode {
	case 0:
		start = t.col
	case 1:
		end = minInt(t.col+1, t.width)
	case 2:
		// full line
	default:
		return
	}
	for col := start; col < end; col++ {
		t.clearCell(t.row, col)
	}
}

func (t *terminal) deleteChars(count int) {
	count = clamp(count, 1, t.width)
	row := t.cells[t.row]
	copy(row[t.col:], row[minInt(t.col+count, t.width):])
	for col := maxInt(t.width-count, t.col); col < t.width; col++ {
		row[col] = " "
	}
	t.normalizeWideRow(t.row)
}

func (t *terminal) insertChars(count int) {
	count = clamp(count, 1, t.width)
	row := t.cells[t.row]
	for col := t.width - 1; col >= t.col+count; col-- {
		row[col] = row[col-count]
	}
	for col := t.col; col < minInt(t.col+count, t.width); col++ {
		row[col] = " "
	}
	t.normalizeWideRow(t.row)
}

func (t *terminal) insertLines(count int) {
	count = clamp(count, 1, t.height)
	for row := t.height - 1; row >= t.row+count; row-- {
		t.cells[row] = t.cells[row-count]
	}
	for row := t.row; row < minInt(t.row+count, t.height); row++ {
		t.cells[row] = blankLine(t.width)
	}
}

func (t *terminal) deleteLines(count int) {
	count = clamp(count, 1, t.height)
	for row := t.row; row+count < t.height; row++ {
		t.cells[row] = t.cells[row+count]
	}
	for row := maxInt(t.height-count, t.row); row < t.height; row++ {
		t.cells[row] = blankLine(t.width)
	}
}

// clearCell erases a complete display glyph. Wide glyphs occupy a lead cell
// followed by an empty continuation cell in this model, so clearing either
// coordinate must clear both halves. This matters when CSI cursor movement,
// erase, insert, or delete lands in the middle of a wide glyph.
func (t *terminal) clearCell(row, col int) {
	if row < 0 || row >= t.height || col < 0 || col >= t.width {
		return
	}
	if t.cells[row][col] == "" {
		if col > 0 && runewidth.StringWidth(t.cells[row][col-1]) == 2 {
			t.cells[row][col-1] = " "
		}
		t.cells[row][col] = " "
		return
	}
	if runewidth.StringWidth(t.cells[row][col]) == 2 && col+1 < t.width && t.cells[row][col+1] == "" {
		t.cells[row][col+1] = " "
	}
	t.cells[row][col] = " "
}

// normalizeWideRow removes orphaned continuation cells and incomplete wide
// glyphs after cell-based CSI insert/delete operations. The parser keeps a
// valid cell grid even when an escape sequence cuts through a grapheme.
func (t *terminal) normalizeWideRow(row int) {
	if row < 0 || row >= t.height {
		return
	}
	for col := 0; col < t.width; col++ {
		cell := t.cells[row][col]
		if cell == "" {
			if col == 0 || runewidth.StringWidth(t.cells[row][col-1]) != 2 {
				t.cells[row][col] = " "
			}
			continue
		}
		if runewidth.StringWidth(cell) == 2 && (col+1 >= t.width || t.cells[row][col+1] != "") {
			t.cells[row][col] = " "
		}
	}
}

func blankLine(width int) []string {
	line := make([]string, width)
	for col := range line {
		line[col] = " "
	}
	return line
}

// text returns the semantic screen as trimmed lines. It intentionally leaves
// internal spaces intact because alignment and status columns are useful in
// failure artifacts, while trailing terminal blanks are not.
func (t *terminal) text() string {
	lines := make([]string, len(t.cells))
	last := -1
	for row, cells := range t.cells {
		line := strings.TrimRight(strings.Join(cells, ""), " ")
		lines[row] = line
		if line != "" {
			last = row
		}
	}
	if last < 0 {
		return ""
	}
	return strings.Join(lines[:last+1], "\n")
}

func (t *terminal) debug() string {
	return fmt.Sprintf("%dx%d cursor=%d,%d\n%s", t.width, t.height, t.row, t.col, t.text())
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
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
