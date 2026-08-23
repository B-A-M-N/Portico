package route

import (
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
)

// Drawing the route the supervisor described.
//
// RouteVM carried Segments, Protection, DNSDrift and ActiveFinding, and the
// renderer used none of them: it drew a fixed local → gateway → endpoint shape
// with one style for the whole line. So a connection with a working tunnel and a
// broken DNS record was drawn identically to one whose connector had never
// started, and every connection kind got a provider gateway — including the port
// forward and the client tunnel, which do not have one.
//
// Here each normalised segment owns a span of the line, its own status decides how
// that span is drawn, a failure physically breaks the line where it failed, and
// protection is a checkpoint standing on the route rather than a caption beside it.

// RenderSegmentedRoute draws a route from its segments.
//
// It returns the empty string when there are no segments, so the caller falls
// back to the summary drawing rather than this inventing a topology.
func RenderSegmentedRoute(vm RouteVM, width int, useASCII bool) string {
	if len(vm.Segments) == 0 || width < 24 {
		return ""
	}

	glyphs := routeGlyphs(useASCII)
	spans := segmentSpans(vm, width, glyphs)
	if len(spans) == 0 {
		return ""
	}

	canvas := NewCanvas(width, 3)

	// Row 0 is the line, row 1 the segment names, row 2 the finding.
	//
	// Drawing stops at the first failure. Everything past a broken hop is
	// unreachable, so continuing to draw it — even correctly, per segment — would
	// show a route that carries traffic beyond the point where it does not.
	broken := false
	for i, span := range spans {
		if broken {
			break
		}
		drawSpan(canvas, span, glyphs)
		if span.status == SegmentFailed {
			broken = true
			// The endpoint glyph becomes unknown rather than open: beyond a break
			// Portico cannot say what state the far end is in.
			spans = spans[:i+1]
		}
	}

	// The endpoints sit at the ends of the line. Their glyph follows the overall
	// state, because that is what the endpoints are: where the connection starts
	// and where it comes out.
	canvas.Set(0, 0, glyphForState(vm.State, useASCII, false), styleForState(vm.State))
	if !broken {
		if last := spans[len(spans)-1]; last.end <= width {
			canvas.Set(width-1, 0, glyphForState(vm.State, useASCII, true), styleForState(vm.State))
		}
	}

	// The names, under the spans wide enough to carry one.
	for _, span := range spans {
		if span.label == "" {
			continue
		}
		available := span.end - span.start
		if available < 4 {
			continue
		}
		label := truncateToWidth(span.label, available)
		canvas.Text(centreOffset((span.start+span.end)/2, label), 1, label, span.labelStyle())
	}

	// The checkpoint is named next to the glyph that draws it, so a user can see
	// what the barrier on the route actually is rather than only that there is
	// one. It goes on the finding row when nothing is wrong, which is otherwise
	// empty.
	if vm.Protection != nil && vm.Protection.Label != "" && vm.ActiveFinding == nil && !broken {
		note := truncateToWidth(vm.Protection.Label, width)
		canvas.Text(rightAlignOffset(width-1, note), 2, note, StyleGateway)
	}

	// What is wrong, under the break.
	if vm.ActiveFinding != nil {
		summary := truncateToWidth(vm.ActiveFinding.Summary, width)
		at := 0
		for _, span := range spans {
			if span.id == vm.ActiveFinding.SegmentID {
				at = centreOffset((span.start+span.end)/2, summary)
			}
		}
		canvas.Text(at, 2, summary, StyleFailed)
	}

	return strings.TrimRight(canvasToString(canvas), " \n")
}

// span is one segment's territory on the line.
type span struct {
	id     core.RouteSegmentID
	status SegmentStatus
	label  string
	start  int
	end    int
	// checkpoint marks the span as a place traffic is stopped and questioned
	// rather than a length of route it travels along.
	checkpoint bool
	// displaced marks a span that works but points somewhere other than where
	// Portico put it — a DNS record that has drifted.
	displaced bool
}

// labelStyle is how a span's name is drawn: the same distinction as the line, so
// a degraded segment reads as degraded in both places.
func (s span) labelStyle() StyleID {
	switch {
	case s.status == SegmentFailed:
		return StyleFailed
	case s.displaced:
		return StyleFailed
	case s.status == SegmentDegraded:
		return StyleFailed
	case s.status == SegmentUnknown:
		return StyleMuted
	default:
		return StyleNormal
	}
}

// segmentSpans divides the width among the segments.
//
// Every segment gets a share, so a route with five hops shows five and a route
// with two shows two: the drawing follows the connection's actual shape instead
// of a fixed three-part picture.
func segmentSpans(vm RouteVM, width int, glyphs glyphSet) []span {
	segments := vm.Segments
	// The endpoint glyphs occupy the first and last column.
	usable := width - 2
	if usable < len(segments) {
		return nil
	}

	spans := make([]span, 0, len(segments))
	each := usable / len(segments)
	cursor := 1
	for i, seg := range segments {
		end := cursor + each
		if i == len(segments)-1 {
			// The last segment absorbs the remainder, so the line reaches the
			// right edge exactly.
			end = width - 1
		}
		s := span{
			id:     seg.ID,
			status: seg.Status,
			label:  segmentLabel(seg),
			start:  cursor,
			end:    end,
		}
		if seg.ID == core.SegmentProtection {
			s.checkpoint = true
		}
		if seg.ID == core.SegmentAddress && vm.DNSDrift {
			s.displaced = true
		}
		spans = append(spans, s)
		cursor = end
	}

	// Protection may be a checkpoint on the route without being one of the
	// supervisor's segments. It is inserted where it belongs: after the provider
	// edge, before the endpoint.
	if vm.Protection != nil && !hasSegment(segments, core.SegmentProtection) {
		spans = insertCheckpoint(spans, vm.Protection.Label)
	}
	return spans
}

// hasSegment reports whether the supervisor already described a segment.
func hasSegment(segments []RouteSegmentVM, id core.RouteSegmentID) bool {
	for _, seg := range segments {
		if seg.ID == id {
			return true
		}
	}
	return false
}

// insertCheckpoint marks the last span as carrying a checkpoint.
//
// The checkpoint is drawn on the boundary rather than given a span of its own:
// splitting the width again to accommodate it would shrink every other segment,
// and a checkpoint is a point, not a distance.
func insertCheckpoint(spans []span, label string) []span {
	if len(spans) == 0 {
		return spans
	}
	last := len(spans) - 1
	spans[last].checkpoint = true
	if spans[last].label == "" {
		spans[last].label = label
	}
	return spans
}

// segmentLabel names a segment for the row beneath the line.
func segmentLabel(seg RouteSegmentVM) string {
	if seg.Label != "" {
		return seg.Label
	}
	return segmentName(seg.ID)
}

// segmentName is the plain-language name of a route segment.
func segmentName(id core.RouteSegmentID) string {
	switch id {
	case core.SegmentLocalService:
		return "your service"
	case core.SegmentLocalRoute:
		return "local route"
	case core.SegmentConnector:
		return "connector"
	case core.SegmentProviderEdge:
		return "provider"
	case core.SegmentAddress:
		return "address"
	case core.SegmentProtection:
		return "who may enter"
	case core.SegmentEndpoint:
		return "endpoint"
	default:
		return strings.ReplaceAll(string(id), "_", " ")
	}
}

// glyphSet is the characters the route is drawn with.
type glyphSet struct {
	healthy    rune
	degraded   rune
	unknown    rune
	broken     rune
	checkpoint rune
	displaced  rune
}

// routeGlyphs chooses the glyphs, honouring the ASCII fallback for terminals
// that cannot render the box-drawing set.
func routeGlyphs(useASCII bool) glyphSet {
	if useASCII {
		return glyphSet{
			healthy: '=', degraded: '-', unknown: '.', broken: 'X',
			checkpoint: '|', displaced: '?',
		}
	}
	return glyphSet{
		healthy: '━', degraded: '┅', unknown: '┄', broken: '╳',
		checkpoint: '┃', displaced: '⇥',
	}
}

// drawSpan draws one segment's stretch of line.
//
// A failed segment is drawn as a break: the line stops, the break glyph marks
// where, and the rest of that span is left empty. That is the point — a route
// with a broken hop is not a continuous line, and drawing it as one in a
// different colour asks the user to notice a shade rather than see a gap.
func drawSpan(canvas *Canvas, s span, glyphs glyphSet) {
	switch {
	case s.status == SegmentFailed:
		mid := (s.start + s.end) / 2
		if mid > s.start {
			canvas.HLine(s.start, mid-1, 0, glyphs.healthy, StyleActive)
		}
		canvas.Set(mid, 0, glyphs.broken, StyleFailed)
		// Everything past the break is left blank: nothing beyond a broken hop
		// is reachable, and drawing it would say otherwise.
		return
	case s.displaced:
		// The route works and arrives somewhere other than where it should. It
		// is drawn as continuous but diverted, because both halves of that are
		// true.
		mid := (s.start + s.end) / 2
		if mid > s.start {
			canvas.HLine(s.start, mid-1, 0, glyphs.healthy, StyleActive)
		}
		canvas.Set(mid, 0, glyphs.displaced, StyleFailed)
		if s.end > mid+1 {
			canvas.HLine(mid+1, s.end-1, 0, glyphs.degraded, StyleFailed)
		}
	case s.status == SegmentDegraded:
		canvas.HLine(s.start, s.end-1, 0, glyphs.degraded, StyleFailed)
	case s.status == SegmentUnknown:
		// Unknown is drawn distinctly from both working and broken: Portico does
		// not know, and a dotted line says that where a solid or a broken one
		// would each claim something.
		canvas.HLine(s.start, s.end-1, 0, glyphs.unknown, StyleMuted)
	default:
		canvas.HLine(s.start, s.end-1, 0, glyphs.healthy, StyleActive)
	}

	if s.checkpoint {
		// The checkpoint stands on the route at the span's end, so traffic
		// visibly passes through it.
		canvas.Set(s.end-1, 0, glyphs.checkpoint, StyleGateway)
	}
}

// RenderCompactRoute draws a route on one line, for narrow terminals.
//
// It is the exported entry the compact layout uses. The internal
// renderCompactRoute remains what RenderRoute falls back to below its own
// minimum width.
func RenderCompactRoute(vm RouteVM, width int, useASCII bool) string {
	if len(vm.Segments) == 0 {
		return renderCompactRoute(vm, useASCII)
	}

	glyphs := routeGlyphs(useASCII)
	var b strings.Builder
	b.WriteRune(glyphForState(vm.State, useASCII, false))
	for _, seg := range vm.Segments {
		switch seg.Status {
		case SegmentFailed:
			b.WriteRune(glyphs.broken)
			// The line stops at the break. What follows it is not reachable, so
			// it is not drawn.
			return truncateToWidth(b.String()+" "+failureNote(vm), width)
		case SegmentDegraded:
			b.WriteRune(glyphs.degraded)
		case SegmentUnknown:
			b.WriteRune(glyphs.unknown)
		default:
			b.WriteRune(glyphs.healthy)
		}
	}
	if vm.Protection != nil {
		b.WriteRune(glyphs.checkpoint)
	}
	b.WriteRune(glyphForState(vm.State, useASCII, true))
	if vm.EndpointLabel != "" {
		b.WriteString(" " + vm.EndpointLabel)
	}
	return truncateToWidth(b.String(), width)
}

// failureNote is the one-line reason a compact route is broken.
func failureNote(vm RouteVM) string {
	if vm.ActiveFinding != nil && vm.ActiveFinding.Summary != "" {
		return vm.ActiveFinding.Summary
	}
	for _, seg := range vm.Segments {
		if seg.Status == SegmentFailed {
			if seg.Error != "" {
				return seg.Error
			}
			return segmentName(seg.ID) + " is not working"
		}
	}
	return ""
}

var _ = fmt.Sprintf
