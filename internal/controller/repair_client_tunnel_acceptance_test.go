package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// Acceptance evidence for "client tunnels have a repair path": the repair
// plan replays the provider's open steps (verify origin, validate, start,
// verify ready), never invents remote resources, and reports nothing to do
// for a healthy tunnel. The port-forward repair test does not certify this
// kind.

type clientTunnelRepairProvider struct {
	mu        sync.Mutex
	started   bool
	running   bool
	failStart bool
}

func (p *clientTunnelRepairProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "ct-repair", Name: "ct-repair"}
}

func (p *clientTunnelRepairProvider) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		Kinds: []core.ConnectionKind{core.ConnectionClientTunnel},
		PrivateExposure: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityExperimental,
		},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Private: true},
		},
	}, nil
}

func (*clientTunnelRepairProvider) Authenticate(context.Context, core.AuthRequest) error {
	return nil
}

func (p *clientTunnelRepairProvider) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    desired.Profile.ID,
		ProfileRevision: desired.Profile.Revision,
		Provider:        "ct-repair",
		Intent:          core.IntentOpen,
		Steps: []core.PlanStep{
			{ID: "verify-origin", Kind: core.StepVerifyOrigin, Summary: "Verify origin"},
			{ID: "start-client", Kind: core.StepStartConnector, Summary: "Start client",
				Compensation: &core.CompensationStep{ID: "stop-comp", Kind: core.StepStopConnector}},
			{ID: "verify-ready", Kind: core.StepVerifyConnector, Summary: "Verify ready"},
		},
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := plan.ComputeFingerprint(); err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *clientTunnelRepairProvider) ExecuteStep(_ context.Context, _ core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch step.Kind {
	case core.StepStartConnector:
		if p.failStart {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: errors.New("injected")}, nil
		}
		p.started = true
		p.running = true
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	default:
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	}
}

func (p *clientTunnelRepairProvider) Observe(_ context.Context, _ core.ConnectionID) (*core.ObservedConnection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := core.ConnectorStatusStopped
	if p.running {
		status = core.ConnectorStatusRunning
	}
	return &core.ObservedConnection{
		Connector: &core.ObservedConnector{
			PID: 4242, Status: string(status),
			LastError: func() string {
				if !p.running && p.started {
					return "crashed after start"
				}
				return ""
			}(),
		},
	}, nil
}

func newClientTunnelRepairFixture(t *testing.T, failStart bool) *Controller {
	t.Helper()
	prov := &clientTunnelRepairProvider{failStart: failStart}
	ctrl := New(newTestRegistry(prov), newTestJournal())

	profile := &core.ConnectionProfile{
		Name: "chatgpt-mcp", Kind: core.ConnectionClientTunnel,
		Desired: core.DesiredOpen,
		Spec: core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
			Client:   core.ClientOpenAISecureMCPTunnel,
			TunnelID: "tunnel_0123456789abcdef0123456789abcdef",
			MCP:      core.MCPServiceSpec{Transport: core.MCPTransportStreamable, Endpoint: "http://127.0.0.1:8000/mcp"},
		}},
		Driver: core.DriverSelection{ProviderID: prov.Identity().ID},
	}
	if _, _, err := ctrl.CreateProfile(context.Background(), profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	return ctrl
}

// TestAClientTunnelCanBeRepaired pins that a crashed client tunnel produces a
// repair plan built from the provider's own open steps.
func TestAClientTunnelCanBeRepaired(t *testing.T) {
	ctrl := newClientTunnelRepairFixture(t, false)

	ctx := context.Background()
	profiles := ctrl.ListProfiles()
	if len(profiles) == 0 {
		t.Fatal("fixture created no profile")
	}

	plan, err := ctrl.PlanRepair(ctx, profiles[0].ID)
	if err != nil {
		t.Fatalf("PlanRepair for a crashed client tunnel: %v", err)
	}
	kinds := map[core.StepKind]bool{}
	for _, step := range plan.Steps {
		kinds[step.Kind] = true
	}
	if !kinds[core.StepStartConnector] || !kinds[core.StepVerifyConnector] {
		t.Fatalf("the repair plan is not a causal open replay: %+v", plan.Steps)
	}
	for _, step := range plan.Steps {
		switch step.Kind {
		case core.StepCreateTunnel, core.StepCreateDNSRecord, core.StepCreateAccessApp:
			t.Fatalf("the repair plan invents remote resource work: %s", step.Kind)
		}
	}
}
