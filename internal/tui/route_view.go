package tui

import (
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/route"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Building the route from the authoritative segments.
//
// RouteVM carried Segments, Protection, DNSDrift, ActiveFinding and
// TrafficParticles, and the only fields anything set were the three labels and
// the overall state. The supervisor normalises the route into segments with a
// status each — the interpretation the textual Inspect route already uses — and
// the drawing ignored all of it, so a route with one failing hop was drawn as a
// uniformly degraded line with no indication of where the break was.
//
// One interpretation, from ConnectionDetailDTO.Segments, feeding both the drawing
// and the text. Where the detail has not been loaded, the summary is used and the
// route is drawn without segment detail rather than with invented segments.

// routeViewModel is the one construction of a route for display.
func (m *Model) routeViewModel(conn *ipc.ConnectionDTO) route.RouteVM {
	if conn == nil {
		return route.RouteVM{}
	}

	vm := route.RouteVM{
		LocalLabel:    conn.Name,
		ProviderLabel: routeMiddleLabel(conn.Kind, conn.ProviderID),
		EndpointLabel: routeEndpointLabel(conn),
		State:         routeStateFor(conn.UserState),
	}

	// The detail is only used for the connection it belongs to. Reading whichever
	// detail happened to be loaded would draw one connection's segments against
	// another's summary.
	detail := m.connectionDetail
	if detail == nil || detail.Summary.ID != conn.ID {
		return vm
	}

	vm.Segments = routeSegments(detail.Segments)
	vm.Protection = routeProtection(detail)
	vm.DNSDrift = routeHasDNSDrift(detail)
	vm.ActiveFinding = routeActiveFinding(detail)
	return vm
}

// routeSegments maps the supervisor's normalised segments onto the drawing.
//
// The statuses are taken as given. Deriving them here from addresses and process
// state would be a second interpretation of route health beside the supervisor's,
// and the one that drifted would be the one nobody was looking at.
func routeSegments(segments []ipc.RouteSegmentDTO) []route.RouteSegmentVM {
	if len(segments) == 0 {
		return nil
	}
	out := make([]route.RouteSegmentVM, 0, len(segments))
	for _, seg := range segments {
		out = append(out, route.RouteSegmentVM{
			ID:     core.RouteSegmentID(seg.ID),
			Status: routeSegmentStatus(seg.Status),
			Label:  seg.Label,
			Error:  seg.Error,
		})
	}
	return out
}

// routeSegmentStatus maps a wire status onto a drawable one.
//
// An unrecognised status is unknown, not healthy: a segment Portico cannot
// interpret must not be drawn as working.
func routeSegmentStatus(status string) route.SegmentStatus {
	switch status {
	case "healthy", "ok", "pass":
		return route.SegmentHealthy
	case "failed", "fail", "broken":
		return route.SegmentFailed
	case "degraded", "warning":
		return route.SegmentDegraded
	default:
		return route.SegmentUnknown
	}
}

// routeProtection is the access checkpoint, when the connection has one.
//
// Protection is a real point on the route — a place where a request is stopped
// and asked who it is — so it is drawn as one rather than mentioned in a caption.
func routeProtection(detail *ipc.ConnectionDetailDTO) *route.CheckpointVM {
	exposed := detail.DesiredSpec.ServiceExposure
	if exposed == nil {
		return nil
	}
	kind := exposed.Protection.Kind
	if kind == "" || kind == "none" {
		return nil
	}
	return &route.CheckpointVM{
		Label:  protectionCheckpointLabel(kind),
		Active: true,
	}
}

// routeHasDNSDrift reports that the address no longer resolves where Portico put
// it.
//
// This is a displaced route rather than a broken one: everything works, and it
// works somewhere other than where the user's hostname points. Drawing it as
// healthy hides the problem; drawing it as failed misstates it.
func routeHasDNSDrift(detail *ipc.ConnectionDetailDTO) bool {
	for _, finding := range detail.Findings {
		if finding.Segment == string(core.SegmentAddress) &&
			(finding.Severity == "warning" || finding.Severity == "error") {
			return true
		}
	}
	return false
}

// routeActiveFinding is the most serious thing currently wrong, for the caption
// under the break.
func routeActiveFinding(detail *ipc.ConnectionDetailDTO) *route.FindingVM {
	var chosen *ipc.DiagnosticDTO
	for i, finding := range detail.Findings {
		switch finding.Severity {
		case "error":
			// An error outranks anything already chosen.
			chosen = &detail.Findings[i]
		case "warning":
			if chosen == nil {
				chosen = &detail.Findings[i]
			}
		}
		if chosen != nil && chosen.Severity == "error" {
			break
		}
	}
	if chosen == nil {
		return nil
	}
	return &route.FindingVM{
		SegmentID:   core.RouteSegmentID(chosen.Segment),
		Summary:     chosen.Summary,
		Explanation: chosen.Explanation,
	}
}

// routeStateFor maps the connection's user-facing state onto the route's.
func routeStateFor(userState string) route.RouteState {
	switch userState {
	case "Open":
		return route.RouteOpen
	case "Closed":
		return route.RouteClosed
	case "Unstable", "Degraded":
		return route.RouteDegraded
	default:
		return route.RouteUnknown
	}
}

// routeEndpointLabel names where the connection ends.
//
// Reading only the public address drew a blank endpoint for every port forward
// and client tunnel, neither of which has one by construction.
func routeEndpointLabel(conn *ipc.ConnectionDTO) string {
	if conn.PublicAddress != "" {
		return conn.PublicAddress
	}
	if conn.PrivateAddress != "" {
		return conn.PrivateAddress
	}
	return screens.ConnectionKindLabel(conn.Kind)
}

// protectionCheckpointLabel names the checkpoint in the words a user would use.
//
// "email_otp" is the wire value. What the checkpoint does is ask the caller to
// prove who they are.
func protectionCheckpointLabel(kind string) string {
	switch kind {
	case "email_otp":
		return "sign-in"
	case "private_network":
		return "private network"
	case "service_token":
		return "service token"
	default:
		return kind
	}
}
