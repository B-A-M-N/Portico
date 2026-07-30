package supervisor

import (
	"fmt"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// Segment status values. A segment reports what the available evidence
// supports and nothing more: "unknown" is a legitimate answer and is never
// upgraded to "ok" merely because no failure was recorded.
const (
	segmentOK        = "ok"
	segmentDegraded  = "degraded"
	segmentFailed    = "failed"
	segmentUnknown   = "unknown"
	segmentNotInUse  = "not_applicable"
	segmentPending   = "pending"
	segmentNotOpened = "not_opened"
)

// computeRouteSegments describes the connection as a chain of segments rather
// than a single open/failed verdict.
//
// A connection that is "failed" tells the user nothing about where. Reporting
// per-segment state lets Portico say which hop is broken — the local service,
// the connector, the provider resource, DNS, protection, or the public
// endpoint — and which hops are known good.
func computeRouteSegments(
	profile *core.ConnectionProfile,
	rt *core.ConnectionRuntime,
	resources []core.ProviderResource,
) []ipc.RouteSegmentDTO {
	if profile == nil {
		return nil
	}

	// Findings are the authoritative record of a diagnosed fault, so a segment
	// with an open finding is reported from that rather than inferred.
	findingBySegment := map[core.RouteSegmentID]core.DiagnosticFinding{}
	if rt != nil {
		for _, f := range rt.Diagnostics {
			if f.ResolvedAt == nil {
				findingBySegment[f.Segment] = f
			}
		}
	}

	closed := profile.Desired == core.DesiredClosed
	spec := profile.Spec.ServiceExposure

	segment := func(id core.RouteSegmentID, label, status, detail string) ipc.RouteSegmentDTO {
		seg := ipc.RouteSegmentDTO{ID: string(id), Label: label, Status: status, Error: detail}
		if f, ok := findingBySegment[id]; ok {
			seg.Status = segmentFailed
			if f.Severity == core.SeverityWarning {
				seg.Status = segmentDegraded
			}
			seg.Error = f.Summary
		}
		return seg
	}

	var segments []ipc.RouteSegmentDTO

	// Local service.
	originLabel := "Local service"
	if origin := originDescription(profile); origin != "" {
		originLabel = "Local service at " + origin
	}
	segments = append(segments, segment(core.SegmentLocalService, originLabel, segmentUnknown,
		"Portico has not probed the local service since the last change."))

	// Connector process. Process identity is concrete evidence.
	connectorStatus, connectorDetail := segmentUnknown, ""
	if rt != nil {
		switch rt.Connector.Status {
		case core.ConnectorStatusRunning:
			connectorStatus = segmentOK
			connectorDetail = fmt.Sprintf("PID %d", rt.Connector.PID)
		case core.ConnectorStatusStarting:
			connectorStatus = segmentPending
		case core.ConnectorStatusStopped:
			if closed {
				connectorStatus = segmentNotOpened
			} else {
				connectorStatus = segmentFailed
				connectorDetail = "the connector is not running"
			}
		case core.ConnectorStatusUnstable:
			connectorStatus = segmentDegraded
			connectorDetail = fmt.Sprintf("restarted %d times", rt.Connector.Restarts)
		case core.ConnectorStatusCrashed:
			connectorStatus = segmentFailed
			connectorDetail = rt.Connector.LastError
		}
	}
	segments = append(segments, segment(core.SegmentConnector, "Connector process", connectorStatus, connectorDetail))

	// Provider edge: the managed tunnel.
	segments = append(segments, segment(core.SegmentProviderEdge, "Provider tunnel",
		resourceStatus(resources, core.ResourceTunnel, closed),
		resourceDetail(resources, core.ResourceTunnel)))

	// Address: a DNS record only exists for a permanent hostname.
	if spec != nil && spec.Exposure.Mode == core.ExposurePermanent {
		segments = append(segments, segment(core.SegmentAddress,
			"DNS record for "+spec.Exposure.RequestedAddress,
			resourceStatus(resources, core.ResourceDNSRecord, closed),
			resourceDetail(resources, core.ResourceDNSRecord)))
	} else {
		segments = append(segments, segment(core.SegmentAddress, "Address",
			segmentNotInUse, "this connection uses a temporary address"))
	}

	// Protection applies only when the profile asks for it.
	if spec != nil && spec.Protection.Kind != "" && spec.Protection.Kind != core.ProtectionNone {
		segments = append(segments, segment(core.SegmentProtection,
			fmt.Sprintf("Access protection (%s)", spec.Protection.Kind),
			resourceStatus(resources, core.ResourceAccessApp, closed),
			resourceDetail(resources, core.ResourceAccessApp)))
	} else {
		segments = append(segments, segment(core.SegmentProtection, "Access protection",
			segmentNotInUse, "this connection is not protected; anyone with the address can reach it"))
	}

	// Public endpoint.
	endpointStatus, endpointDetail := segmentUnknown, ""
	switch {
	case closed:
		endpointStatus, endpointDetail = segmentNotOpened, "the connection is closed"
	case rt != nil && rt.Endpoint.PublicAddress != "":
		endpointStatus = segmentOK
		endpointDetail = rt.Endpoint.PublicAddress
	case rt != nil && rt.State == core.RuntimeError:
		endpointStatus, endpointDetail = segmentFailed, "no public address was established"
	}
	segments = append(segments, segment(core.SegmentEndpoint, "Public endpoint", endpointStatus, endpointDetail))

	return segments
}

// resourceStatus reports a provider resource segment from the durable record of
// what Portico created, including resources an authoritative lookup found had
// been removed outside Portico.
func resourceStatus(resources []core.ProviderResource, kind core.ResourceType, closed bool) string {
	for _, r := range resources {
		if r.Type != kind {
			continue
		}
		switch r.Lifecycle {
		case core.LifecycleExternallyRemoved:
			return segmentFailed
		case core.LifecycleRemoved, core.LifecycleRemovalPending:
			return segmentNotOpened
		case core.LifecycleRemovalFailed, core.LifecycleOrphaned:
			return segmentDegraded
		}
		return segmentOK
	}
	if closed {
		return segmentNotOpened
	}
	return segmentUnknown
}

func resourceDetail(resources []core.ProviderResource, kind core.ResourceType) string {
	for _, r := range resources {
		if r.Type == kind {
			if r.Lifecycle == core.LifecycleExternallyRemoved {
				return fmt.Sprintf("%s was deleted outside Portico", r.ExternalID)
			}
			return r.ExternalID
		}
	}
	return ""
}
