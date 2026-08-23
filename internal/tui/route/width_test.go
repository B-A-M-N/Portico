package route

import (
	"strings"
	"testing"
)

// The canvas is a grid of cells, so anything measured for it must be measured
// in cells. These pin the three cases a rune count gets wrong.

func TestDisplayWidthCountsCellsNotRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"ascii", "hello", 5},
		{"cjk is two cells per ideograph", "日本語", 6},
		{"combining mark adds no cell", "e\u0301", 1},
		{"emoji presentation is two cells", "⚡\ufe0f", 2},
		{"zwj sequence is one cluster", "👨\u200d👩\u200d👧", 2},
		{"mixed", "ab日", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DisplayWidth(tc.in); got != tc.want {
				t.Fatalf("DisplayWidth(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// A wide character occupies two cells, so text after it must start two columns
// later. Advancing one column per rune overlapped them.
func TestCanvasTextAdvancesByDisplayWidth(t *testing.T) {
	c := NewCanvas(10, 1)
	c.Text(0, 0, "日x", StyleNormal)

	// 日 occupies columns 0-1 (lead at 0, trailing cell blank), x lands at 2.
	if got := c.Cells[c.idx(0, 0)].Rune; got != '日' {
		t.Fatalf("column 0 = %q, want 日", got)
	}
	if got := c.Cells[c.idx(2, 0)].Rune; got != 'x' {
		t.Fatalf("column 2 = %q, want x — a wide character must occupy two cells", got)
	}
	if got := c.Cells[c.idx(1, 0)].Rune; got != 0 {
		t.Fatalf("column 1 = %q, want the blank trailing half of 日", got)
	}
}

// A combining mark belongs to the cell before it and must not consume one of
// its own, or everything after it shifts right.
func TestCanvasTextFoldsZeroWidthMarks(t *testing.T) {
	c := NewCanvas(10, 1)
	c.Text(0, 0, "e\u0301x", StyleNormal)

	if got := c.Cells[c.idx(1, 0)].Rune; got != 'x' {
		t.Fatalf("column 1 = %q, want x — a combining mark occupies no cell", got)
	}
}

// Clipping happens on a grapheme boundary, so a wide character is never split
// into a broken half and a mark is never orphaned.
func TestTruncateToWidthClipsOnGraphemeBoundary(t *testing.T) {
	if got := truncateToWidth("日本語", 4); DisplayWidth(got) > 4 {
		t.Fatalf("truncateToWidth(日本語, 4) = %q (width %d), want at most 4 cells",
			got, DisplayWidth(got))
	}
	// An odd budget cannot fit half of a wide character, so it must drop it
	// rather than emit a broken cell.
	got := truncateToWidth("日本語", 3)
	if DisplayWidth(got) > 3 {
		t.Fatalf("truncateToWidth(日本語, 3) = %q (width %d), want at most 3", got, DisplayWidth(got))
	}
	if strings.Contains(got, "\ufffd") {
		t.Fatalf("truncation produced a replacement character: %q", got)
	}
}

// Centring divides the display width, not the rune count.
func TestCentreOffsetUsesDisplayWidth(t *testing.T) {
	// " 日本 " is 6 cells, so centred on column 10 it starts at 7.
	if got := centreOffset(10, " 日本 "); got != 7 {
		t.Fatalf("centreOffset(10, \" 日本 \") = %d, want 7", got)
	}
	// Clamped rather than negative.
	if got := centreOffset(1, " 日本語 "); got != 0 {
		t.Fatalf("centreOffset(1, …) = %d, want 0", got)
	}
}

func TestRightAlignOffsetUsesDisplayWidth(t *testing.T) {
	if got := rightAlignOffset(10, "日本"); got != 6 {
		t.Fatalf("rightAlignOffset(10, 日本) = %d, want 6", got)
	}
	if got := rightAlignOffset(2, "日本語"); got != 0 {
		t.Fatalf("rightAlignOffset(2, …) = %d, want 0", got)
	}
}

// A rendered route must not exceed the width it was given, whatever the labels
// contain. A rune-counted label overflowed the canvas and wrapped.
func TestRenderRouteRespectsWidthWithWideLabels(t *testing.T) {
	vm := RouteVM{
		LocalLabel:    "127.0.0.1:8080",
		ProviderLabel: "クラウドフレア",
		EndpointLabel: "日本語.example.com",
		State:         RouteOpen,
	}
	const width = 60
	out := RenderRoute(vm, width, false)
	for i, line := range strings.Split(out, "\n") {
		if w := DisplayWidth(line); w > width {
			t.Fatalf("line %d is %d cells wide, exceeding the %d-cell canvas: %q",
				i, w, width, line)
		}
	}
}
