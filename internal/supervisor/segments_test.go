package supervisor

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

func segmentByID(segs []ipc.RouteSegmentDTO, id core.RouteSegmentID) (ipc.RouteSegmentDTO, bool) {
	for _, s := range segs {
		if s.ID == string(id) {
			return s, true
		}
	}
	return ipc.RouteSegmentDTO{}, false
}

// TestSegmentsLocateTheFaultRatherThanReportingOneVerdict pins audit item 25.
// A connection reported only as open or failed says nothing about where the
// break is.
func TestSegmentsLocateTheFaultRatherThanReportingOneVerdict(t *testing.T) {
	profile := previewProfile(core.ExposurePermanent, core.ProtectionSpec{
		Kind: core.ProtectionEmailOTP, AllowedEmails: []string{"a@example.com"},
	})
	profile.Desired = core.DesiredOpen

	rt := &core.ConnectionRuntime{
		ConnectionID: profile.ID,
		State:        core.RuntimeDegraded,
		Connector:    core.ConnectorRuntime{Status: core.ConnectorStatusRunning, PID: 4242},
	}
	resources := []core.ProviderResource{
		{Type: core.ResourceTunnel, ExternalID: "tun-1", Lifecycle: core.LifecyclePresent},
		{Type: core.ResourceDNSRecord, ExternalID: "dns-1", Lifecycle: core.LifecycleExternallyRemoved},
	}

	segs := computeRouteSegments(profile, rt, resources)

	connector, ok := segmentByID(segs, core.SegmentConnector)
	if !ok || connector.Status != segmentOK {
		t.Fatalf("connector segment = %#v, want ok", connector)
	}
	if connector.Error != "PID 4242" {
		t.Fatalf("connector segment does not carry process identity: %q", connector.Error)
	}

	tunnel, ok := segmentByID(segs, core.SegmentProviderEdge)
	if !ok || tunnel.Status != segmentOK {
		t.Fatalf("tunnel segment = %#v, want ok", tunnel)
	}

	// The DNS record was deleted outside Portico; that is a located fault.
	dns, ok := segmentByID(segs, core.SegmentAddress)
	if !ok || dns.Status != segmentFailed {
		t.Fatalf("dns segment = %#v, want failed", dns)
	}
	if dns.Error == "" {
		t.Fatal("failed dns segment gives no reason")
	}
}

// TestSegmentsDoNotClaimHealthWithoutEvidence ensures an unprobed segment
// reports unknown rather than ok.
func TestSegmentsDoNotClaimHealthWithoutEvidence(t *testing.T) {
	profile := previewProfile(core.ExposureTemporary, core.ProtectionSpec{Kind: core.ProtectionNone})
	profile.Desired = core.DesiredOpen

	segs := computeRouteSegments(profile, nil, nil)

	local, ok := segmentByID(segs, core.SegmentLocalService)
	if !ok || local.Status != segmentUnknown {
		t.Fatalf("local service segment = %#v, want unknown without a probe", local)
	}
	connector, _ := segmentByID(segs, core.SegmentConnector)
	if connector.Status == segmentOK {
		t.Fatal("connector reported healthy with no runtime evidence")
	}
	endpoint, _ := segmentByID(segs, core.SegmentEndpoint)
	if endpoint.Status == segmentOK {
		t.Fatal("endpoint reported healthy with no observed address")
	}
}

// TestSegmentsMarkInapplicableHopsExplicitly ensures a temporary connection
// does not report a missing DNS record or absent protection as a fault.
func TestSegmentsMarkInapplicableHopsExplicitly(t *testing.T) {
	profile := previewProfile(core.ExposureTemporary, core.ProtectionSpec{Kind: core.ProtectionNone})
	profile.Desired = core.DesiredOpen

	segs := computeRouteSegments(profile, nil, nil)

	dns, _ := segmentByID(segs, core.SegmentAddress)
	if dns.Status != segmentNotInUse {
		t.Fatalf("dns segment = %#v, want not_applicable for a temporary address", dns)
	}
	protection, _ := segmentByID(segs, core.SegmentProtection)
	if protection.Status != segmentNotInUse {
		t.Fatalf("protection segment = %#v, want not_applicable", protection)
	}
	// An unprotected connection must still say what that means.
	if protection.Error == "" {
		t.Fatal("unprotected connection does not explain who can reach it")
	}
}

// TestOpenFindingsOverrideInferredSegmentState ensures a diagnosed fault wins
// over an optimistic inference.
func TestOpenFindingsOverrideInferredSegmentState(t *testing.T) {
	profile := previewProfile(core.ExposureTemporary, core.ProtectionSpec{Kind: core.ProtectionNone})
	profile.Desired = core.DesiredOpen

	rt := &core.ConnectionRuntime{
		ConnectionID: profile.ID,
		Connector:    core.ConnectorRuntime{Status: core.ConnectorStatusRunning, PID: 10},
		Diagnostics: []core.DiagnosticFinding{{
			Segment:  core.SegmentConnector,
			Severity: core.SeverityError,
			Summary:  "connector is not forwarding traffic",
		}},
	}

	segs := computeRouteSegments(profile, rt, nil)
	connector, _ := segmentByID(segs, core.SegmentConnector)
	if connector.Status != segmentFailed {
		t.Fatalf("a running process with an open error finding reported %q", connector.Status)
	}
	if connector.Error != "connector is not forwarding traffic" {
		t.Fatalf("finding summary was not surfaced: %q", connector.Error)
	}
}

// TestClosedConnectionSegmentsAreNotFailures ensures a deliberately closed
// connection is not rendered as broken.
func TestClosedConnectionSegmentsAreNotFailures(t *testing.T) {
	profile := previewProfile(core.ExposureTemporary, core.ProtectionSpec{Kind: core.ProtectionNone})
	profile.Desired = core.DesiredClosed

	rt := &core.ConnectionRuntime{
		ConnectionID: profile.ID,
		State:        core.RuntimeClosed,
		Connector:    core.ConnectorRuntime{Status: core.ConnectorStatusStopped},
	}
	segs := computeRouteSegments(profile, rt, nil)
	for _, seg := range segs {
		if seg.Status == segmentFailed {
			t.Fatalf("closed connection reports segment %q as failed", seg.ID)
		}
	}
}
