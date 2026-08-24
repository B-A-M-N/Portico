package supervisor

import (
	"context"

	"github.com/B-A-M-N/portico/internal/core"
)

// stubPrivateNetwork is a private-network provider that answers without a client.
//
// The Tailscale adapter has its own tests against a recorded client. What these
// supervisor tests need is a provider that declares the private-network kind so the
// registry accepts the profile — the point being exercised is the supervisor's create,
// route and reconstruction paths, not the adapter's.
type stubPrivateNetwork struct{}

func (*stubPrivateNetwork) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID: "stub_private_network", Name: "stub_private_network",
		DisplayName: "Stub private network",
	}
}

func (*stubPrivateNetwork) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		Kinds:              []core.ConnectionKind{core.ConnectionPrivateNetwork},
		TemporaryAddresses: core.CapabilitySupport{Supported: false},
		CustomHostnames:    core.CapabilitySupport{Supported: false},
		ManagedDNS:         core.CapabilitySupport{Supported: false},
		PrivateExposure:    core.CapabilitySupport{Supported: true, Stability: core.StabilityBeta},
		BuiltInProtection: []core.ProtectionCapability{
			{Kind: core.ProtectionPrivateNet, Supported: true, Stability: core.StabilityBeta},
		},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Private: true},
			core.ProtocolTCP:  {Supported: true, Private: true},
		},
		Redundancy: core.RedundancyCapability{Supported: false, MaxConnectors: 1},
	}, nil
}

func (*stubPrivateNetwork) Authenticate(context.Context, core.AuthRequest) error { return nil }

func (*stubPrivateNetwork) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	profile := desired.Profile
	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    profile.ID,
		ProfileRevision: profile.Revision,
		Provider:        "stub_private_network",
		Intent:          core.IntentOpen,
	}
	if profile.Desired == core.DesiredClosed {
		plan.Intent = core.IntentClose
	}
	plan.Steps = []core.PlanStep{{
		ID: "stub-step", Kind: core.StepValidateAccount,
		Summary: "Confirm the machine is on the private network",
		Technical: core.TechnicalOperation{
			Provider: "stub_private_network", Type: "verify_membership",
		},
	}}
	plan.Expected.State = core.RuntimeOpen
	if plan.Intent == core.IntentClose {
		plan.Expected.State = core.RuntimeClosed
	}
	plan.Expected.PrivateAddress = "machine.private.example"
	if err := plan.ComputeFingerprint(); err != nil {
		return nil, err
	}
	return plan, nil
}

func (*stubPrivateNetwork) ExecuteStep(context.Context, core.ConnectionID,
	core.PlanStep) (core.StepResult, error) {
	return core.StepResult{StepID: "stub-step", Succeeded: true}, nil
}

func (*stubPrivateNetwork) Observe(_ context.Context,
	id core.ConnectionID) (*core.ObservedConnection, error) {
	return &core.ObservedConnection{
		ConnectionID: id, ProviderID: "stub_private_network",
		Connector: &core.ObservedConnector{Status: string(core.ConnectorStatusStopped)},
	}, nil
}

// stubClientTunnel declares the client-tunnel kind so the registry accepts the profile.
//
// The OpenAI adapter has its own tests. What the supervisor tests need is a provider that
// says it can deliver the kind, so the create and reconstruction paths can be exercised
// without a tunnel client on the machine.
type stubClientTunnel struct{}

func (*stubClientTunnel) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID: "stub_client_tunnel", Name: "stub_client_tunnel",
		DisplayName: "Stub client tunnel",
	}
}

func (*stubClientTunnel) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		Kinds:              []core.ConnectionKind{core.ConnectionClientTunnel},
		TemporaryAddresses: core.CapabilitySupport{Supported: false},
		CustomHostnames:    core.CapabilitySupport{Supported: false},
		ManagedDNS:         core.CapabilitySupport{Supported: false},
		PrivateExposure:    core.CapabilitySupport{Supported: true},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Private: true},
		},
		Redundancy: core.RedundancyCapability{Supported: false, MaxConnectors: 1},
	}, nil
}

func (*stubClientTunnel) Authenticate(context.Context, core.AuthRequest) error { return nil }

func (*stubClientTunnel) Plan(_ context.Context,
	desired core.DesiredConnection) (*core.OperationPlan, error) {
	plan := &core.OperationPlan{
		ID: core.NewPlanID(), ConnectionID: desired.Profile.ID,
		ProfileRevision: desired.Profile.Revision,
		Provider:        "stub_client_tunnel", Intent: core.IntentOpen,
		Steps: []core.PlanStep{{
			ID: "stub-tunnel-step", Kind: core.StepStartConnector,
			Summary: "Start the tunnel client",
			Technical: core.TechnicalOperation{
				Provider: "stub_client_tunnel", Type: "start_client",
			},
		}},
	}
	plan.Expected.State = core.RuntimeOpen
	if err := plan.ComputeFingerprint(); err != nil {
		return nil, err
	}
	return plan, nil
}

func (*stubClientTunnel) ExecuteStep(context.Context, core.ConnectionID,
	core.PlanStep) (core.StepResult, error) {
	return core.StepResult{StepID: "stub-tunnel-step", Succeeded: true}, nil
}

func (*stubClientTunnel) Observe(_ context.Context,
	id core.ConnectionID) (*core.ObservedConnection, error) {
	return &core.ObservedConnection{
		ConnectionID: id, ProviderID: "stub_client_tunnel",
		Connector: &core.ObservedConnector{Status: string(core.ConnectorStatusStopped)},
	}, nil
}
