package controller

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// TestDiffProfilesClassifiesMCPEndpointChange pins item 33: changing where a
// client tunnel forwards to is a real edit that requires a semantic restart,
// not an invisible metadata change the executor would skip.
func TestDiffProfilesClassifiesMCPEndpointChange(t *testing.T) {
	current := &core.ConnectionProfile{
		ID: "ct-edit", Name: "mcp", Kind: core.ConnectionClientTunnel,
		Desired: core.DesiredOpen,
		Spec: core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
			Client:   core.ClientOpenAISecureMCPTunnel,
			TunnelID: "tunnel_0123456789abcdef0123456789abcdef",
			MCP: core.MCPServiceSpec{
				Transport: core.MCPTransportStreamable,
				Endpoint:  "http://127.0.0.1:8000/mcp",
			},
		}},
	}
	proposed := current.DeepCopy()
	proposed.Spec.ClientTunnel.MCP.Endpoint = "http://127.0.0.1:9001/mcp"

	delta, err := DiffProfiles(current, proposed)
	if err != nil {
		t.Fatalf("DiffProfiles: %v", err)
	}
	found := false
	for _, c := range delta.Changes {
		if c == "MCP endpoint" {
			found = true
		}
	}
	if !found {
		t.Fatalf("an MCP endpoint change was not classified: %+v", delta.Changes)
	}
	if !delta.RestartConnector {
		t.Fatal("an MCP endpoint change did not require a connector restart")
	}
}

// TestDiffProfilesClassifiesMCPTransportChange pins the transport arm of the
// same contract.
func TestDiffProfilesClassifiesMCPTransportChange(t *testing.T) {
	current := &core.ConnectionProfile{
		ID: "ct-edit", Name: "mcp", Kind: core.ConnectionClientTunnel,
		Desired: core.DesiredOpen,
		Spec: core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
			Client:   core.ClientOpenAISecureMCPTunnel,
			TunnelID: "tunnel_0123456789abcdef0123456789abcdef",
			MCP:      core.MCPServiceSpec{Transport: core.MCPTransportStreamable, Endpoint: "http://127.0.0.1:8000/mcp"},
		}},
	}
	proposed := current.DeepCopy()
	proposed.Spec.ClientTunnel.MCP.Transport = core.MCPTransportSSE

	delta, err := DiffProfiles(current, proposed)
	if err != nil {
		t.Fatalf("DiffProfiles: %v", err)
	}
	found := false
	for _, c := range delta.Changes {
		if c == "MCP transport" {
			found = true
		}
	}
	if !found || !delta.RestartConnector {
		t.Fatalf("an MCP transport change was not treated as restart-worthy: %+v restart=%v", delta.Changes, delta.RestartConnector)
	}
}

// TestDiffProfilesIgnoresLegacyClientProfile pins that the persisted native
// client profile is read-compatible history, not an effective tunnel setting.
func TestDiffProfilesIgnoresLegacyClientProfile(t *testing.T) {
	current := tunnelEditDiffProfile()
	proposed := current.DeepCopy()
	proposed.Spec.ClientTunnel.Profile = "native-profile"

	delta, err := DiffProfiles(current, proposed)
	if err != nil {
		t.Fatalf("DiffProfiles: %v", err)
	}
	if !delta.Empty() {
		t.Fatalf("a legacy client-profile change became an effective edit: %+v", delta)
	}
}

// tunnelEditDiffProfile returns a minimal client tunnel profile used for
// DiffProfiles regression tests.
func tunnelEditDiffProfile() *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID: "ct-edit", Name: "tunnel", Kind: core.ConnectionClientTunnel,
		Desired: core.DesiredOpen,
		Spec: core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
			Client:   core.ClientOpenAISecureMCPTunnel,
			TunnelID: "tunnel_0123456789abcdef0123456789abcdef",
			Profile:  "legacy-native",
			MCP:      core.MCPServiceSpec{Transport: core.MCPTransportStreamable, Endpoint: "http://127.0.0.1:8000/mcp"},
		}},
	}
}
