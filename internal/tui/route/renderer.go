package route

import (
	"fmt"
	"strings"

	"github.com/paoloanzn/portico/internal/core"
)

// RouteVM is the view model for the route renderer.
// It contains already-interpreted state — no I/O, no clock, no provider calls.
type RouteVM struct {
	LocalLabel       string
	EndpointLabel    string
	ProviderLabel    string
	State            RouteState
	Segments         []RouteSegmentVM
	Protection       *CheckpointVM
	DNSDrift         bool
	ActiveFinding    *FindingVM
	TrafficParticles int
}

// RouteState describes the overall route state.
type RouteState string

const (
	RouteOpen     RouteState = "open"
	RouteClosed   RouteState = "closed"
	RouteDegraded RouteState = "degraded"
	RouteUnknown  RouteState = "unknown"
)

// RouteSegmentVM is a view model for one route segment.
type RouteSegmentVM struct {
	ID     core.RouteSegmentID
	Status SegmentStatus
	Label  string
	Error  string
}

// SegmentStatus describes the status of a segment.
type SegmentStatus string

const (
	SegmentHealthy  SegmentStatus = "healthy"
	SegmentFailed   SegmentStatus = "failed"
	SegmentDegraded SegmentStatus = "degraded"
	SegmentUnknown  SegmentStatus = "unknown"
)

// CheckpointVM describes a protection checkpoint.
type CheckpointVM struct {
	Active bool
	Label  string
}

// FindingVM describes an active finding at a segment.
type FindingVM struct {
	SegmentID   core.RouteSegmentID
	Summary     string
	Explanation string
}

// --------------- cell canvas ---------------

// StyleID identifies a cell style.
type StyleID int

const (
	StyleNormal  StyleID = 0
	StyleMuted   StyleID = 1
	StyleActive  StyleID = 2
	StyleFailed  StyleID = 3
	StyleAccent  StyleID = 4
	StyleGateway StyleID = 5
)

// Cell represents one terminal cell.
type Cell struct {
	Rune  rune
	Style StyleID
}

// Canvas is a small terminal cell grid for route rendering.
type Canvas struct {
	Width  int
	Height int
	Cells  []Cell
}

// NewCanvas creates a new cell canvas.
func NewCanvas(width, height int) *Canvas {
	return &Canvas{
		Width:  width,
		Height: height,
		Cells:  make([]Cell, width*height),
	}
}

func (c *Canvas) idx(x, y int) int {
	return y*c.Width + x
}

// Set places a rune at (x, y).
func (c *Canvas) Set(x, y int, r rune, style StyleID) {
	if x < 0 || x >= c.Width || y < 0 || y >= c.Height {
		return
	}
	c.Cells[c.idx(x, y)] = Cell{Rune: r, Style: style}
}

// Text places a string at (x, y), clipping to canvas width.
func (c *Canvas) Text(x, y int, s string, style StyleID) {
	for i, r := range s {
		c.Set(x+i, y, r, style)
	}
}

// HLine draws a horizontal line from x1 to x2 at row y.
func (c *Canvas) HLine(x1, x2, y int, r rune, style StyleID) {
	for x := x1; x <= x2; x++ {
		c.Set(x, y, r, style)
	}
}

// VLine draws a vertical line from y1 to y2 at column x.
func (c *Canvas) VLine(x, y1, y2 int, r rune, style StyleID) {
	for y := y1; y <= y2; y++ {
		c.Set(x, y, r, style)
	}
}

// --------------- rendering ---------------

// glyphs for route states
var (
	openGlyph     = '●'
	unstableGlyph = '◐'
	closedGlyph   = '○'
	unknownGlyph  = '◌'
	gatewayGlyph  = '◈'
	breakGlyph    = '╳'
	activeLine    = '━'
)

// ASCII fallbacks
var (
	asciiOpen     = '*'
	asciiClosed   = 'O'
	asciiUnstable = 'o'
	asciiUnknown  = '.'
)

// RenderRoute is the pure function that renders a route visualization.
// It receives already-interpreted view model data and returns terminal cell output.
func RenderRoute(vm RouteVM, width int, useASCII bool) string {
	if width < 20 {
		return renderCompactRoute(vm, useASCII)
	}

	canvas := NewCanvas(width, 4)

	localGlyph := glyphForState(vm.State, useASCII, false)
	endpointGlyph := glyphForState(vm.State, useASCII, true)

	localX := 0
	gw := width / 2 // gateway center
	endX := width - 1

	// Draw local endpoint at column 0, row 1
	canvas.Set(localX, 1, localGlyph, styleForState(vm.State))

	// Draw gateway at center, row 1
	canvas.Set(gw, 1, gatewayGlyph, StyleGateway)

	// Draw endpoint at far right, row 1
	canvas.Set(endX, 1, endpointGlyph, styleForState(vm.State))

	// Draw top route: local → gateway
	topRouteEnd := gw - 1
	if topRouteEnd > localX+1 {
		canvas.HLine(localX+1, topRouteEnd, 0, activeLine, StyleActive)
		// Corner
		canvas.Set(topRouteEnd, 1, '╮', StyleActive)
		canvas.Set(topRouteEnd, 0, '╭', StyleActive)
	} else {
		canvas.Set(localX+1, 1, '─', StyleActive)
	}

	// Draw bottom route: gateway → endpoint
	bottomRouteStart := gw + 1
	if bottomRouteStart < endX {
		canvas.HLine(bottomRouteStart, endX-1, 2, activeLine, StyleActive)
		canvas.Set(gw, 2, '╰', StyleActive)
		canvas.Set(gw+1, 2, '━', StyleActive)
	} else {
		canvas.Set(gw, 2, '╰', StyleActive)
	}

	// Handle finding overlay
	if vm.ActiveFinding != nil {
		switch vm.ActiveFinding.SegmentID {
		case core.SegmentConnector:
			// Put break at gateway entry
			canvas.Set(gw, 1, breakGlyph, StyleFailed)
		case core.SegmentProviderEdge:
			canvas.Set(gw+2, 2, breakGlyph, StyleFailed)
			canvas.Set(endX, 1, unknownGlyph, StyleMuted)
		}
	}

	// Render labels below route
	labelY := 3
	label := fmt.Sprintf(" %s ", vm.LocalLabel)
	canvas.Text(0, labelY, label, StyleNormal)

	if vm.ProviderLabel != "" {
		provLabel := fmt.Sprintf(" %s ", vm.ProviderLabel)
		provX := gw - len(provLabel)/2
		if provX < 0 {
			provX = 0
		}
		canvas.Text(provX, labelY, provLabel, StyleMuted)
	}

	if vm.EndpointLabel != "" {
		endLabel := fmt.Sprintf(" %s ", vm.EndpointLabel)
		endX2 := endX - len(endLabel)
		if endX2 < 0 {
			endX2 = 0
		}
		canvas.Text(endX2, labelY, endLabel, StyleNormal)
	}

	return canvasToString(canvas)
}

// renderCompactRoute renders a single-line route for narrow terminals.
func renderCompactRoute(vm RouteVM, useASCII bool) string {
	localG := glyphForState(vm.State, useASCII, false)
	endG := glyphForState(vm.State, useASCII, true)

	localStr := string(localG)
	endStr := string(endG)

	if vm.ActiveFinding != nil {
		route := fmt.Sprintf("%s ╳ %s", localStr, endStr)
		seg := vm.ActiveFinding.Summary
		if len(vm.LocalLabel) > 0 {
			route = fmt.Sprintf("%s %s", vm.LocalLabel, route)
		}
		if len(route)+len(seg)+1 < 60 {
			route = route + " " + seg
		}
		return route
	}

	route := fmt.Sprintf("%s━━━%s━━━%s", localStr, string(gatewayGlyph), endStr)
	if vm.EndpointLabel != "" {
		parts := len(vm.EndpointLabel)
		if parts > 10 {
			parts = 10
		}
		route += " " + vm.EndpointLabel[:parts]
	}
	return route
}

func glyphForState(state RouteState, ascii bool, isEndpoint bool) rune {
	if ascii {
		switch state {
		case RouteOpen:
			return asciiOpen
		case RouteClosed:
			return asciiClosed
		case RouteDegraded:
			return asciiUnstable
		default:
			return asciiUnknown
		}
	}
	switch state {
	case RouteOpen:
		return openGlyph
	case RouteClosed:
		return closedGlyph
	case RouteDegraded:
		return unstableGlyph
	default:
		return unknownGlyph
	}
}

func styleForState(state RouteState) StyleID {
	switch state {
	case RouteOpen:
		return StyleActive
	case RouteClosed:
		return StyleMuted
	case RouteDegraded:
		return StyleFailed
	default:
		return StyleMuted
	}
}

func canvasToString(c *Canvas) string {
	var b strings.Builder
	for y := 0; y < c.Height; y++ {
		for x := 0; x < c.Width; x++ {
			cell := c.Cells[c.idx(x, y)]
			if cell.Rune == 0 {
				b.WriteRune(' ')
			} else {
				b.WriteRune(cell.Rune)
			}
		}
		if y < c.Height-1 {
			b.WriteRune('\n')
		}
	}
	return b.String()
}

// RenderSimple creates a route VM from core types and renders it.
func RenderSimple(
	address string,
	state core.RuntimeState,
	publicAddress string,
	providerLabel string,
	finding *core.DiagnosticFinding,
	width int,
	useASCII bool,
) string {
	routeState := RouteUnknown
	switch state {
	case core.RuntimeOpen:
		routeState = RouteOpen
	case core.RuntimeClosed:
		routeState = RouteClosed
	case core.RuntimeDegraded:
		routeState = RouteDegraded
	}

	vm := RouteVM{
		LocalLabel:    address,
		EndpointLabel: publicAddress,
		ProviderLabel: providerLabel,
		State:         routeState,
	}

	if finding != nil {
		vm.ActiveFinding = &FindingVM{
			SegmentID:   finding.Segment,
			Summary:     finding.Summary,
			Explanation: finding.Explanation,
		}
	}

	return RenderRoute(vm, width, useASCII)
}
