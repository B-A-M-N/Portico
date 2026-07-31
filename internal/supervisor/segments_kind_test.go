package supervisor

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

func portForwardProfile() *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID: "pf-1", Name: "database", Kind: core.ConnectionPortForward,
		Desired: core.DesiredOpen,
		Spec: core.ConnectionSpec{PortForward: &core.PortForwardSpec{
			LocalPort: 5432, RemoteHost: "db.internal", RemotePort: 5432,
			Direction: core.PortForwardLocal, Protocol: core.ProtocolTCP,
		}},
	}
}

func clientTunnelProfile() *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID: "ct-1", Name: "mcp", Kind: core.ConnectionClientTunnel,
		Desired: core.DesiredOpen,
		Spec: core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
			Client:   core.ClientOpenAISecureMCPTunnel,
			TunnelID: "tun-abc",
			MCP: core.MCPServiceSpec{
				Transport: core.MCPTransportStreamable, Endpoint: "http://127.0.0.1:3000/mcp",
			},
		}},
	}
}

func privateNetworkProfile() *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID: "pn-1", Name: "mesh", Kind: core.ConnectionPrivateNetwork,
		Desired: core.DesiredOpen,
		Spec: core.ConnectionSpec{PrivateNetwork: &core.PrivateNetworkSpec{
			NetworkID: "net-1", Mode: core.PrivateNetworkJoin,
		}},
	}
}

func segmentText(segs []ipc.RouteSegmentDTO) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.ID + " " + s.Status + " " + s.Label + " " + s.Error + "\n")
	}
	return b.String()
}

// TestAPortForwardIsNotDescribedAsAnExposedService pins audit finding 8.
//
// The route builder read Spec.ServiceExposure and, finding nil, fell through to
// the else branch of every kind-specific decision. A port forward that binds
// 127.0.0.1 was reported as being on a temporary address, having a provider
// tunnel, and — the part that matters — "not protected; anyone with the address
// can reach it". Every one of those is false, and the last one describes the
// safest connection Portico can make as the most dangerous.
func TestAPortForwardIsNotDescribedAsAnExposedService(t *testing.T) {
	profile := portForwardProfile()
	rt := &core.ConnectionRuntime{
		ConnectionID: profile.ID, State: core.RuntimeOpen,
		Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning, PID: 91},
	}
	rt.Endpoint.PrivateAddress = "127.0.0.1:5432"

	segs := computeRouteSegments(profile, rt, nil)
	text := segmentText(segs)

	for _, claim := range []string{
		"anyone with the address can reach it",
		"temporary address",
		"Provider tunnel",
		"Public endpoint",
	} {
		if strings.Contains(text, claim) {
			t.Errorf("a port forward is described with %q:\n%s", claim, text)
		}
	}

	// It should describe what a port forward actually is: a local listener
	// carrying traffic to a remote endpoint.
	if !strings.Contains(text, "127.0.0.1:5432") {
		t.Errorf("the local listener is not named:\n%s", text)
	}
	if !strings.Contains(text, "db.internal:5432") {
		t.Errorf("the remote endpoint is not named:\n%s", text)
	}
}

// TestAClientTunnelIsNeverDescribedAsPublic pins the kind whose entire reason
// for existing is that it has no public address. Describing it as one would
// invite the user to look for a link that will never exist, and to believe
// their MCP server is exposed when it is not.
func TestAClientTunnelIsNeverDescribedAsPublic(t *testing.T) {
	profile := clientTunnelProfile()
	rt := &core.ConnectionRuntime{
		ConnectionID: profile.ID, State: core.RuntimeOpen,
		Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning, PID: 92},
	}

	text := segmentText(computeRouteSegments(profile, rt, nil))

	for _, claim := range []string{"Public endpoint", "DNS", "temporary address", "anyone with the address"} {
		if strings.Contains(text, claim) {
			t.Errorf("a client tunnel is described with %q:\n%s", claim, text)
		}
	}
	if !strings.Contains(text, "127.0.0.1:3000/mcp") {
		t.Errorf("the local MCP server is not named:\n%s", text)
	}
}

// TestAPrivateNetworkIsNotDescribedAsUnprotected pins the same inversion for a
// private network, which is reachable only by members.
func TestAPrivateNetworkIsNotDescribedAsUnprotected(t *testing.T) {
	profile := privateNetworkProfile()
	rt := &core.ConnectionRuntime{
		ConnectionID: profile.ID, State: core.RuntimeOpen,
		Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning, PID: 93},
	}
	rt.Endpoint.PrivateAddress = "100.64.0.3"

	text := segmentText(computeRouteSegments(profile, rt, nil))

	if strings.Contains(text, "anyone with the address can reach it") {
		t.Errorf("a private network is described as unprotected:\n%s", text)
	}
	if strings.Contains(text, "Public endpoint") {
		t.Errorf("a private network is described as having a public endpoint:\n%s", text)
	}
	if !strings.Contains(text, "100.64.0.3") {
		t.Errorf("the private address is not named:\n%s", text)
	}
}

// TestAServiceExposureStillDescribesItsPublicRoute guards against fixing the
// other kinds by removing the segments the exposed kind genuinely needs.
func TestAServiceExposureStillDescribesItsPublicRoute(t *testing.T) {
	profile := previewProfile(core.ExposurePermanent, core.ProtectionSpec{Kind: core.ProtectionNone})
	profile.Desired = core.DesiredOpen
	rt := &core.ConnectionRuntime{
		ConnectionID: profile.ID, State: core.RuntimeOpen,
		Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning, PID: 94},
	}
	rt.Endpoint.PublicAddress = "https://app.example.com"

	segs := computeRouteSegments(profile, rt, nil)
	for _, id := range []core.RouteSegmentID{
		core.SegmentConnector, core.SegmentProviderEdge, core.SegmentAddress,
		core.SegmentProtection, core.SegmentEndpoint,
	} {
		if _, ok := segmentByID(segs, id); !ok {
			t.Errorf("service exposure lost its %s segment", id)
		}
	}
	text := segmentText(segs)
	if !strings.Contains(text, "anyone with the address can reach it") {
		t.Errorf("an unprotected public service no longer says so:\n%s", text)
	}
}

// TestAnOpenFindingStillOverridesEveryKind ensures the diagnosed-fault override
// was not lost when the chain became kind-specific.
func TestAnOpenFindingStillOverridesEveryKind(t *testing.T) {
	for _, profile := range []*core.ConnectionProfile{
		portForwardProfile(), clientTunnelProfile(), privateNetworkProfile(),
	} {
		rt := &core.ConnectionRuntime{
			ConnectionID: profile.ID, State: core.RuntimeDegraded,
			Connector:   core.ConnectorRuntime{Status: core.ConnectorStatusRunning, PID: 95},
			Diagnostics: []core.DiagnosticFinding{{Segment: core.SegmentConnector, Severity: core.SeverityError, Summary: "the forwarder exited"}},
		}
		segs := computeRouteSegments(profile, rt, nil)
		seg, ok := segmentByID(segs, core.SegmentConnector)
		if !ok {
			t.Fatalf("%s has no connector segment", profile.Kind)
		}
		if seg.Status != segmentFailed || seg.Error != "the forwarder exited" {
			t.Errorf("%s ignored an open finding: %#v", profile.Kind, seg)
		}
	}
}

// TestAKindlessProfileIsReadFromItsSpec covers profiles stored before Kind was
// recorded. The spec union already says which kind it is; falling back to the
// exposed chain would reintroduce the defect for exactly the rows least likely
// to be re-saved.
func TestAKindlessProfileIsReadFromItsSpec(t *testing.T) {
	profile := portForwardProfile()
	profile.Kind = ""

	text := segmentText(computeRouteSegments(profile, nil, nil))
	if strings.Contains(text, "anyone with the address can reach it") {
		t.Errorf("a kindless port forward fell back to the exposed chain:\n%s", text)
	}
}
