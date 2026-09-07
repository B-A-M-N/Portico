package diagnostics

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// TestAHealthyPortForwardIsNotDiagnosedAsABrokenTunnel pins the defect an
// adversarial review of the connection-kind work surfaced.
//
// The engine is kind-blind. Its provider-edge check treated "open, but no
// public address" as a fault, which is the permanent and correct state of every
// port forward, client tunnel and private-only exposure. Each therefore
// produced a severity-error finding — "Provider edge unavailable" — that was
// persisted, shown on the detail screen, and offered to the user as something
// to repair.
//
// The finding was also keyed to the provider_edge segment, a hop the port
// forward route does not emit, so the route reported the connection healthy
// while the findings list reported its edge broken.
func TestAHealthyPortForwardIsNotDiagnosedAsABrokenTunnel(t *testing.T) {
	f := healthyFixture()
	f.profile.Kind = core.ConnectionPortForward
	f.profile.Spec = core.ConnectionSpec{PortForward: &core.PortForwardSpec{
		LocalPort: 15432, RemoteHost: "db.internal", RemotePort: 5432,
		Direction: core.PortForwardLocal, Protocol: core.ProtocolTCP,
	}}
	f.runtime.Endpoint = core.EndpointRuntime{PrivateAddress: "127.0.0.1:15432"}

	for _, finding := range diagnose(t, f) {
		if finding.Segment == core.SegmentProviderEdge {
			t.Fatalf("a healthy port forward is diagnosed as a broken tunnel: %+v", finding)
		}
	}
}

// TestAHealthyClientTunnelIsNotDiagnosedAsABrokenTunnel covers the kind whose
// defining property is that it never has a public address.
func TestAHealthyClientTunnelIsNotDiagnosedAsABrokenTunnel(t *testing.T) {
	f := healthyFixture()
	f.profile.Kind = core.ConnectionClientTunnel
	f.profile.Spec = core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
		Client: core.ClientOpenAISecureMCPTunnel, TunnelID: "tun-abc",
		MCP: core.MCPServiceSpec{Transport: core.MCPTransportStreamable, Endpoint: "http://127.0.0.1:3000/mcp"},
	}}
	f.runtime.Endpoint = core.EndpointRuntime{PrivateAddress: "private tunnel to OpenAI"}

	for _, finding := range diagnose(t, f) {
		if finding.Segment == core.SegmentProviderEdge {
			t.Fatalf("a healthy client tunnel is diagnosed as a broken tunnel: %+v", finding)
		}
	}
}

// TestAPrivateOnlyExposureIsNotDiagnosedAsABrokenTunnel covers the same defect
// one level in: a service exposure asked for as private_only is the right kind
// and still has no public address by design.
func TestAPrivateOnlyExposureIsNotDiagnosedAsABrokenTunnel(t *testing.T) {
	f := healthyFixture()
	f.profile.Spec.ServiceExposure.Exposure.Mode = core.ExposurePrivate
	f.runtime.Endpoint = core.EndpointRuntime{PrivateAddress: "10.0.0.4:3000"}

	for _, finding := range diagnose(t, f) {
		if finding.Segment == core.SegmentProviderEdge {
			t.Fatalf("a private-only exposure is diagnosed as a broken tunnel: %+v", finding)
		}
	}
}

// TestAPublishedServiceWithNoAddressIsStillAFault guards the opposite error.
// Suppressing the finding for the kinds that never have a public address must
// not suppress it for the one that should.
func TestAPublishedServiceWithNoAddressIsStillAFault(t *testing.T) {
	for _, mode := range []core.ExposureMode{"", core.ExposureTemporary, core.ExposurePermanent} {
		f := healthyFixture()
		f.profile.Spec.ServiceExposure.Exposure.Mode = mode
		f.runtime.Endpoint = core.EndpointRuntime{}

		var found bool
		for _, finding := range diagnose(t, f) {
			if finding.Segment == core.SegmentProviderEdge {
				found = true
			}
		}
		if !found {
			t.Errorf("exposure mode %q: a published service with no address is no longer reported", mode)
		}
	}
}

// TestARefusedClientTunnelCredentialIsDiagnosedAsTheCredential pins the
// distinction the audit demanded: the client answers /readyz 200 while the
// control plane refuses its key, so the first failing segment must be the
// credential — not the connector a restart would target — and the repair must
// be key replacement, not another restart of a healthy process.
func TestARefusedClientTunnelCredentialIsDiagnosedAsTheCredential(t *testing.T) {
	f := healthyFixture()
	f.profile.Kind = core.ConnectionClientTunnel
	f.profile.Spec = core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
		Client: core.ClientOpenAISecureMCPTunnel, TunnelID: "tun-abc",
		MCP: core.MCPServiceSpec{Transport: core.MCPTransportStreamable, Endpoint: "http://127.0.0.1:3000/mcp"},
	}}
	f.runtime.Endpoint = core.EndpointRuntime{PrivateAddress: "private tunnel to OpenAI"}
	f.provider.observed = &core.ObservedConnection{
		ConnectionID: "conn-1", ProviderID: "client_tunnel",
		Connector: &core.ObservedConnector{
			PID: 100, Status: string(core.ConnectorStatusUnstable),
			LastError: "the control plane rejected the stored credential (invalid_api_key); replace the runtime key",
		},
	}

	findings := diagnose(t, f)
	if len(findings) == 0 {
		t.Fatal("a refused credential produced no finding")
	}
	finding := findings[0]
	if finding.Summary != "The control plane rejected the stored credential" {
		t.Fatalf("summary = %q, want the credential finding", finding.Summary)
	}
	if len(finding.RepairOptions) != 1 || finding.RepairOptions[0].Summary != "Replace the runtime key" {
		t.Fatalf("repair options = %+v, want key replacement", finding.RepairOptions)
	}
	if finding.Evidence[0].Type != "control_plane_refusal" {
		t.Fatalf("evidence = %+v, want the control plane's own refusal record", finding.Evidence[0])
	}
}

// TestAnUnreadyClientWithoutRefusalEvidenceIsNotACredentialFinding guards the
// boundary: a client that is merely not ready yet (startup, control plane
// unreachable) must keep the ordinary connector finding, so a network outage
// is never misreported as a rejected key.
func TestAnUnreadyClientWithoutRefusalEvidenceIsNotACredentialFinding(t *testing.T) {
	f := healthyFixture()
	f.profile.Kind = core.ConnectionClientTunnel
	f.profile.Spec = core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
		Client: core.ClientOpenAISecureMCPTunnel, TunnelID: "tun-abc",
		MCP: core.MCPServiceSpec{Transport: core.MCPTransportStreamable, Endpoint: "http://127.0.0.1:3000/mcp"},
	}}
	f.runtime.Endpoint = core.EndpointRuntime{PrivateAddress: "private tunnel to OpenAI"}
	f.provider.observed = &core.ObservedConnection{
		ConnectionID: "conn-1", ProviderID: "client_tunnel",
		Connector: &core.ObservedConnector{
			PID: 100, Status: string(core.ConnectorStatusUnstable),
			LastError: "the tunnel client is running but not ready (control plane unreachable or credential rejected)",
		},
	}

	findings := diagnose(t, f)
	for _, finding := range findings {
		if finding.Summary == "The control plane rejected the stored credential" {
			t.Fatalf("an unready client without refusal evidence was diagnosed as a refused credential: %+v", finding)
		}
	}
}
