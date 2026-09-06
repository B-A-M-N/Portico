package supervisor

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// Audit P0-9 verticals: client-tunnel edits reach the runtime end-to-end —
// the request union carries them, applyEditRequest applies them, and the
// resulting plan restarts the client.

func tunnelEditProfile() *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID: "tunnel-edit", Name: "secure mcp", Revision: 1,
		Kind:    core.ConnectionClientTunnel,
		Desired: core.DesiredClosed,
		Spec: core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
			Client:   core.ClientOpenAISecureMCPTunnel,
			TunnelID: "tunnel_0123456789abcdef0123456789abcdef",
			MCP:      core.MCPServiceSpec{Transport: core.MCPTransportHTTP, Endpoint: "http://127.0.0.1:9000/mcp"},
		}},
		Driver: core.DriverSelection{ProviderID: "gw-target"},
	}
}

// tunnelCapableProvider declares client_tunnel support so the edit path's
// provider validation accepts the fixture.
type tunnelCapableProvider struct {
	inner *targetCapturingProvider
}

func (p *tunnelCapableProvider) Identity() core.ProviderIdentity { return p.inner.Identity() }

func (p *tunnelCapableProvider) Capabilities(ctx context.Context) (core.Capabilities, error) {
	caps, err := p.inner.Capabilities(ctx)
	if err != nil {
		return caps, err
	}
	caps.Kinds = append(caps.Kinds, core.ConnectionClientTunnel)
	return caps, nil
}

func (p *tunnelCapableProvider) Plan(ctx context.Context, d core.DesiredConnection) (*core.OperationPlan, error) {
	return p.inner.Plan(ctx, d)
}

func (p *tunnelCapableProvider) ExecuteStep(
	ctx context.Context, id core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	return p.inner.ExecuteStep(ctx, id, step)
}

func (p *tunnelCapableProvider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	return p.inner.Observe(ctx, id)
}

func (p *tunnelCapableProvider) Authenticate(ctx context.Context, req core.AuthRequest) error {
	return p.inner.Authenticate(ctx, req)
}

// TestClientTunnelEditEndpointChangeProducesRestartPlan drives the full
// HandlePlanEdit path for a CLOSED connection.
func TestClientTunnelEditEndpointChangeProducesRestartPlan(t *testing.T) {
	sup := testSupervisorWithController(t, &tunnelCapableProvider{inner: &targetCapturingProvider{}})
	ctx := context.Background()
	profile := tunnelEditProfile() // driver already "gw-target"
	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	handler := &supervisorHandler{sup: sup}
	req := ipc.UpdateConnectionRequest{
		ClientTunnel: &ipc.ClientTunnelSpecDTO{
			MCP: ipc.MCPSourceDTO{Endpoint: "http://127.0.0.1:9100/mcp"},
		},
	}
	dto, err := handler.HandlePlanEdit("tunnel-edit", req)
	if err != nil {
		t.Fatalf("HandlePlanEdit: %v", err)
	}
	if dto.Noop {
		t.Fatal("an MCP endpoint change was classified as a no-op")
	}
	found := false
	for _, step := range dto.Steps {
		if step.Kind == string(core.StepApplyProfile) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the preview has no apply-profile step: %+v", dto.Steps)
	}
	if !strings.Contains(dto.Outcome, "MCP endpoint") {
		t.Fatalf("unexpected outcome text: %q", dto.Outcome)
	}
}

// TestOpenClientTunnelEditOrdersStopApplyReopen pins the lifecycle ordering
// for an edit of a connection that is already open.
func TestOpenClientTunnelEditOrdersStopApplyReopen(t *testing.T) {
	sup := testSupervisorWithController(t, &tunnelCapableProvider{inner: &targetCapturingProvider{}})
	ctx := context.Background()
	profile := tunnelEditProfile()
	profile.Desired = core.DesiredOpen
	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	sup.controller.RestoreRuntime(&core.ConnectionRuntime{
		ConnectionID: profile.ID,
		State:        core.RuntimeOpen,
		Connector:    core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
	})

	handler := &supervisorHandler{sup: sup}
	dto, err := handler.HandlePlanEdit("tunnel-edit", ipc.UpdateConnectionRequest{
		ClientTunnel: &ipc.ClientTunnelSpecDTO{
			MCP: ipc.MCPSourceDTO{Endpoint: "http://127.0.0.1:9100/mcp"},
		},
	})
	if err != nil {
		t.Fatalf("HandlePlanEdit: %v", err)
	}

	want := []string{
		string(core.StepStopConnector),
		string(core.StepApplyProfile),
		string(core.StepStartConnector),
	}
	if len(dto.Steps) != len(want) {
		t.Fatalf("open client-tunnel edit steps = %#v, want exactly %#v", dto.Steps, want)
	}
	for i, step := range dto.Steps {
		if step.Kind != want[i] {
			t.Fatalf("step %d kind = %q, want %q; steps = %#v", i, step.Kind, want[i], dto.Steps)
		}
	}
}

// TestApplyEditRequestAppliesClientTunnelFields pins that each field of the
// request arm lands on the proposed profile.
func TestApplyEditRequestAppliesClientTunnelFields(t *testing.T) {
	current := tunnelEditProfile()
	tunnelID := "tunnel_ffffffffffffffffffffffffffffffff"
	endpoint := "http://127.0.0.1:9200/mcp"
	transport := "sse"

	proposed, err := applyEditRequest(current, ipc.UpdateConnectionRequest{
		ClientTunnel: &ipc.ClientTunnelSpecDTO{
			TunnelID: tunnelID,
			Profile:  "work",
			MCP:      ipc.MCPSourceDTO{Endpoint: endpoint, Transport: transport},
		},
	})
	if err != nil {
		t.Fatalf("applyEditRequest: %v", err)
	}
	spec := proposed.Spec.ClientTunnel
	switch {
	case spec.TunnelID != tunnelID:
		t.Fatalf("tunnel ID = %q", spec.TunnelID)
	case spec.MCP.Endpoint != endpoint:
		t.Fatalf("endpoint = %q", spec.MCP.Endpoint)
	case spec.MCP.Transport != core.MCPTransportSSE:
		t.Fatalf("transport = %q", spec.MCP.Transport)
	}
}

func TestApplyEditRequestAppliesClientTunnelMCPCommand(t *testing.T) {
	current := tunnelEditProfile()
	command := &ipc.CommandSourceDTO{
		Executable: "uvx",
		Args:       []string{"mcp-server", "--stdio"},
		WorkingDir: "/srv/mcp",
		Env:        map[string]string{"MODE": "production"},
		Port:       8080,
		Protocol:   "http",
	}

	proposed, err := applyEditRequest(current, ipc.UpdateConnectionRequest{
		ClientTunnel: &ipc.ClientTunnelSpecDTO{
			MCP: ipc.MCPSourceDTO{Command: command},
		},
	})
	if err != nil {
		t.Fatalf("applyEditRequest: %v", err)
	}
	got := proposed.Spec.ClientTunnel.MCP.Command
	if got == nil {
		t.Fatal("the requested MCP command was silently ignored")
	}
	if proposed.Spec.ClientTunnel.MCP.Endpoint != "" {
		t.Fatalf("endpoint = %q, want empty when command is selected", proposed.Spec.ClientTunnel.MCP.Endpoint)
	}
	if got.Executable != command.Executable || got.WorkingDir != command.WorkingDir ||
		got.Port != command.Port || got.Protocol != core.Protocol(command.Protocol) || got.UseShell != command.UseShell {
		t.Fatalf("command metadata = %#v, want %#v", got, command)
	}
	if len(got.Args) != len(command.Args) || got.Args[0] != command.Args[0] || got.Args[1] != command.Args[1] {
		t.Fatalf("command args = %#v, want %#v", got.Args, command.Args)
	}
	if got.Env["MODE"] != "production" {
		t.Fatalf("command environment = %#v, want production mode", got.Env)
	}
}

func TestApplyEditRequestEndpointReplacesClientTunnelMCPCommand(t *testing.T) {
	current := tunnelEditProfile()
	current.Spec.ClientTunnel.MCP.Endpoint = ""
	current.Spec.ClientTunnel.MCP.Command = &core.CommandSpec{Executable: "mcp-server"}

	proposed, err := applyEditRequest(current, ipc.UpdateConnectionRequest{
		ClientTunnel: &ipc.ClientTunnelSpecDTO{
			MCP: ipc.MCPSourceDTO{Endpoint: "http://127.0.0.1:9300/mcp"},
		},
	})
	if err != nil {
		t.Fatalf("applyEditRequest: %v", err)
	}
	if proposed.Spec.ClientTunnel.MCP.Command != nil {
		t.Fatal("the old MCP command was retained when an endpoint was requested")
	}
	if proposed.Spec.ClientTunnel.MCP.Endpoint != "http://127.0.0.1:9300/mcp" {
		t.Fatalf("endpoint = %q, want requested endpoint", proposed.Spec.ClientTunnel.MCP.Endpoint)
	}
}

// TestApplyEditRequestRefusesClientTunnelArmOnOtherKinds pins tagged-union
// validation: the arm against a non-tunnel connection is refused, not dropped.
func TestApplyEditRequestRefusesClientTunnelArmOnOtherKinds(t *testing.T) {
	current := gatewayVerticalProfile() // service exposure
	_, err := applyEditRequest(current, ipc.UpdateConnectionRequest{
		ClientTunnel: &ipc.ClientTunnelSpecDTO{TunnelID: "tunnel_0123456789abcdef0123456789abcdef"},
	})
	if err == nil {
		t.Fatal("a client-tunnel edit arm was silently accepted for a service exposure")
	}
}
