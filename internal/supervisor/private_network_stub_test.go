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
