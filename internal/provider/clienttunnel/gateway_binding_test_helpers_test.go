package clienttunnel

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	legacy "github.com/B-A-M-N/portico/internal/provider/openaitunnel"
)

type fakeProcess struct{}

func testProvider(t *testing.T) (*Provider, *fakeProcess) {
	t.Helper()
	return legacy.New("tunnel-client", nil), &fakeProcess{}
}

func tunnelProfile(desired core.DesiredConnectionState) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID: "conn-mcp", Name: "mcp", Kind: core.ConnectionClientTunnel,
		Spec: core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
			Client:   core.ClientOpenAISecureMCPTunnel,
			MCP:      core.MCPServiceSpec{Endpoint: "http://127.0.0.1:8787/mcp"},
			TunnelID: "tunnel_0123456789abcdef0123456789abcdef",
		}},
		Driver: core.DriverSelection{ProviderID: "openai_tunnel"}, Desired: desired,
	}
}
