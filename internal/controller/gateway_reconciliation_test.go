package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// healthyObserver is a provider stub whose observation always reports the
// connector running, isolating the gateway-presence rule from connector state.
type healthyObserver struct{}

func (p *healthyObserver) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "mock", Name: "mock"}
}

func (p *healthyObserver) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
		CustomHostnames:    core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Public: true},
		},
		BuiltInProtection: []core.ProtectionCapability{
			{Kind: core.ProtectionNone, Supported: true, Stability: core.StabilityStable},
		},
		Streaming: core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
	}, nil
}

func (p *healthyObserver) Authenticate(context.Context, core.AuthRequest) error { return nil }

func (p *healthyObserver) Plan(context.Context, core.DesiredConnection) (*core.OperationPlan, error) {
	return nil, errors.New("not needed for reconciliation tests")
}

func (p *healthyObserver) ExecuteStep(
	context.Context, core.ConnectionID, core.PlanStep) (core.StepResult, error) {
	return core.StepResult{Succeeded: true}, nil
}

func (p *healthyObserver) Observe(context.Context, core.ConnectionID) (*core.ObservedConnection, error) {
	return &core.ObservedConnection{
		Connector: &core.ObservedConnector{PID: 4242, Status: string(core.ConnectorStatusRunning)},
	}, nil
}

// Audit P1-17: a connection whose profile requires a gateway must not remain
// open/healthy if the gateway dies while the connector process is still
// running. Reconcile must classify that state as needing repair.

func TestReconcileFlagsRepairWhenRequiredGatewayDies(t *testing.T) {
	current := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	current.ProfileKind = core.ProfileOpenAICompatible
	c := New(newTestRegistry(&healthyObserver{}), newTestJournal())
	c.RestoreProfile(current)

	// Healthy-looking open runtime, but the gateway projection is gone
	// (it died independently of the connector process).
	rt := &core.ConnectionRuntime{
		ConnectionID: current.ID,
		State:        core.RuntimeOpen,
	}
	rt.Connector.Status = core.ConnectorStatusRunning
	// rt.Gateway deliberately nil.
	c.RestoreRuntime(rt)

	action, err := c.Reconcile(context.Background(), current.ID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if action != "repair" {
		t.Fatalf("reconcile action = %q, want repair (a required gateway is gone)", action)
	}
}

func TestReconcileStaysOpenWhenRequiredGatewayRuns(t *testing.T) {
	current := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	current.ProfileKind = core.ProfileOpenAICompatible
	c := New(newTestRegistry(&healthyObserver{}), newTestJournal())
	c.RestoreProfile(current)

	rt := &core.ConnectionRuntime{
		ConnectionID: current.ID,
		State:        core.RuntimeOpen,
		Gateway: &core.GatewayRuntime{
			Endpoint:    "http://127.0.0.1:49152",
			AuthEnabled: true,
		},
	}
	rt.Connector.Status = core.ConnectorStatusRunning
	c.RestoreRuntime(rt)

	action, err := c.Reconcile(context.Background(), current.ID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if action != "" {
		t.Fatalf("reconcile action = %q, want none (gateway present and healthy)", action)
	}
}

func TestReconcileIgnoresGatewayForProfilesThatDoNotNeedOne(t *testing.T) {
	current := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	// Plain web_service: no gateway requirement.
	c := New(newTestRegistry(&healthyObserver{}), newTestJournal())
	c.RestoreProfile(current)

	rt := &core.ConnectionRuntime{ConnectionID: current.ID, State: core.RuntimeOpen}
	rt.Connector.Status = core.ConnectorStatusRunning
	c.RestoreRuntime(rt)

	action, err := c.Reconcile(context.Background(), current.ID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if action != "" {
		t.Fatalf("reconcile action = %q for a profile with no gateway requirement; want none", action)
	}
}
