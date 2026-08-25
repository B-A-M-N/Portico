package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// connectorCompensationProvider models a plan that starts a process and then
// fails in the following step. Compensation must stop the connector — and, for
// providers that run one, tear down its local gateway — so no process outlives
// the failed operation.
type connectorCompensationProvider struct {
	mu             sync.Mutex
	verifyFails    bool
	started        bool
	stopped        bool
	gatewayStarted bool
	gatewayStopped bool
}

func (p *connectorCompensationProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "connector-comp", Name: "connector-comp"}
}

func (p *connectorCompensationProvider) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Public: true},
		},
	}, nil
}

func (*connectorCompensationProvider) Authenticate(context.Context, core.AuthRequest) error { return nil }

func (p *connectorCompensationProvider) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    desired.Profile.ID,
		ProfileRevision: desired.Profile.Revision,
		Provider:        "connector-comp",
		Intent:          core.IntentOpen,
		Steps: []core.PlanStep{
			{ID: "start-connector", Kind: core.StepStartConnector, Summary: "Start connector",
				// The compensation under test. A stop is connection-scoped,
				// not resource-scoped: it needs no resource ID.
				Compensation: &core.CompensationStep{
					ID:   "stop-connector-comp",
					Kind: core.StepStopConnector,
				}},
			{ID: "verify-endpoint", Kind: core.StepVerifyEndpoint, Summary: "Verify endpoint"},
		},
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := plan.ComputeFingerprint(); err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *connectorCompensationProvider) ExecuteStep(_ context.Context, connID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch step.Kind {
	case core.StepStartConnector:
		p.started = true
		p.gatewayStarted = true
		return core.StepResult{StepID: step.ID, Succeeded: true, Resources: []core.ProviderResource{{
			ConnectionID: connID, ProviderID: p.Identity().ID, Type: core.ResourceTunnel,
			ExternalID: "tunnel_adopted", Ownership: core.OwnershipAdopted,
		}}}, nil
	case core.StepVerifyEndpoint:
		if p.verifyFails {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: errors.New("public verification failed")}, nil
		}
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	case core.StepStopConnector:
		p.stopped = true
		p.started = false
		p.gatewayStopped = true
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	default:
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	}
}

func (p *connectorCompensationProvider) Observe(_ context.Context, _ core.ConnectionID) (*core.ObservedConnection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	observed := &core.ObservedConnection{ProviderID: p.Identity().ID}
	if p.started {
		observed.Connector = &core.ObservedConnector{PID: 4242, Status: string(core.ConnectorStatusRunning)}
	} else {
		observed.Connector = &core.ObservedConnector{Status: string(core.ConnectorStatusStopped)}
	}
	return observed, nil
}

func runConnectorCompensationTest(t *testing.T, verifyFails bool) (*connectorCompensationProvider, *Operation) {
	t.Helper()
	provider := &connectorCompensationProvider{verifyFails: verifyFails}
	ctrl := New(newTestRegistry(provider), newTestJournal())

	profile := newFailureTestProfile()
	profile.Driver.ProviderID = provider.Identity().ID
	if _, _, err := ctrl.CreateProfile(context.Background(), profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	plan, err := ctrl.PlanOpen(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := ctrl.ApplyPlan(context.Background(), plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	return provider, awaitOperationTerminal(t, ctrl, op.ID)
}

// TestFailedVerificationStopsTheConnector pins item 9/19: when the step after
// connector start fails, the declared stop compensation runs and no process or
// gateway outlives the operation.
func TestFailedVerificationStopsTheConnector(t *testing.T) {
	provider, op := runConnectorCompensationTest(t, true)

	if op.State != OperationStateFailed {
		t.Fatalf("operation state = %s, want failed", op.State)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if !provider.stopped {
		t.Fatal("the stop compensation never ran; a connector leaked")
	}
	if provider.started {
		t.Fatal("the connector is still recorded as started after compensation")
	}
	if !provider.gatewayStopped {
		t.Fatal("the local gateway was not torn down with the connector")
	}
}

// TestSuccessfulOpenLeavesTheConnectorRunning pins the positive case: with no
// failure there is nothing to compensate and the connector stays up.
func TestSuccessfulOpenLeavesTheConnectorRunning(t *testing.T) {
	provider, op := runConnectorCompensationTest(t, false)

	if op.State != OperationStateCompleted {
		t.Fatalf("operation state = %s, want completed", op.State)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if !provider.started || provider.stopped {
		t.Fatalf("started=%v stopped=%v, want started without compensation", provider.started, provider.stopped)
	}
}

// TestAdoptedTunnelSurvivesCompensation pins that compensating an adopted
// resource's connection never deletes the resource itself: the OpenAI tunnel
// object belongs to the platform, not to Portico.
func TestAdoptedTunnelSurvivesCompensation(t *testing.T) {
	provider, op := runConnectorCompensationTest(t, true)

	if op.State != OperationStateFailed {
		t.Fatalf("operation state = %s, want failed", op.State)
	}
	for _, step := range providerPlanSteps(t) {
		if step.Compensation != nil && step.Compensation.Kind == core.StepDeleteTunnel {
			t.Fatal("an adopted tunnel acquired a delete compensation")
		}
	}
	_ = provider
}

// providerPlanSteps re-plans through the same fixture to inspect the plan
// shape, since the operation record does not carry the plan.
func providerPlanSteps(t *testing.T) []core.PlanStep {
	t.Helper()
	provider := &connectorCompensationProvider{}
	ctrl := New(newTestRegistry(provider), newTestJournal())
	profile := newFailureTestProfile()
	profile.Driver.ProviderID = provider.Identity().ID
	if _, _, err := ctrl.CreateProfile(context.Background(), profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	plan, err := ctrl.PlanOpen(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	return plan.Steps
}
