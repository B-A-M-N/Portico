package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// accessCompensationProvider models the Cloudflare application/policy pair at
// the controller boundary. The application ID is created at runtime; policy
// creation fails; compensation must use that exact ID.
type accessCompensationProvider struct {
	mu            sync.Mutex
	deleteFails   bool
	liveApp       bool
	policyAppID   string
	deletedAppIDs []string
}

func (p *accessCompensationProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "access-compensation", Name: "access-compensation"}
}

func (p *accessCompensationProvider) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Public: true},
		},
	}, nil
}

func (*accessCompensationProvider) Authenticate(context.Context, core.AuthRequest) error { return nil }

func (p *accessCompensationProvider) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    desired.Profile.ID,
		ProfileRevision: desired.Profile.Revision,
		Provider:        "access-compensation",
		Intent:          core.IntentOpen,
		Steps: []core.PlanStep{
			{ID: "create-app", Kind: core.StepCreateAccessApp, Summary: "Create Access application", Compensation: &core.CompensationStep{
				ID: "delete-app", Kind: core.StepDeleteAccessApp,
			}},
			{ID: "create-policy", Kind: core.StepCreateAccessPolicy, Summary: "Create Access policy"},
		},
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := plan.ComputeFingerprint(); err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *accessCompensationProvider) ExecuteStep(_ context.Context, connID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch step.Kind {
	case core.StepCreateAccessApp:
		p.liveApp = true
		return core.StepResult{StepID: step.ID, Succeeded: true, Resources: []core.ProviderResource{{
			ConnectionID: connID, ProviderID: p.Identity().ID, Type: core.ResourceAccessApp, ExternalID: "app-123", Ownership: core.OwnershipManaged,
		}}}, nil
	case core.StepCreateAccessPolicy:
		p.policyAppID = step.Technical.Parameters["app_id"]
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: errors.New("policy creation failed")}, nil
	case core.StepDeleteAccessApp:
		p.deletedAppIDs = append(p.deletedAppIDs, step.Technical.ResourceID)
		if p.deleteFails {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: errors.New("application deletion failed")}, nil
		}
		if step.Technical.ResourceID != "app-123" {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: errors.New("wrong application ID")}, nil
		}
		p.liveApp = false
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	default:
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	}
}

func (p *accessCompensationProvider) Observe(context.Context, core.ConnectionID) (*core.ObservedConnection, error) {
	return &core.ObservedConnection{ProviderID: p.Identity().ID}, nil
}

type cleanupItem struct {
	resourceType core.ResourceType
	externalID   string
	state        string
}

type cleanupRecorder struct {
	items []cleanupItem
}

func (r *cleanupRecorder) RecordCleanupItem(_ context.Context, _ core.OperationID, _ core.ConnectionID, _ core.ProviderID, _ core.ProviderAccountID, resourceType core.ResourceType, externalID, state, _ string) error {
	r.items = append(r.items, cleanupItem{resourceType: resourceType, externalID: externalID, state: state})
	return nil
}

func runAccessCompensationTest(t *testing.T, deleteFails bool) (*accessCompensationProvider, *cleanupRecorder, *Operation) {
	t.Helper()
	provider := &accessCompensationProvider{deleteFails: deleteFails}
	ctrl := New(newTestRegistry(provider), newTestJournal())
	cleanup := &cleanupRecorder{}
	ctrl.SetCleanupRecorder(cleanup)

	profile := newFailureTestProfile()
	profile.Driver.ProviderID = provider.Identity().ID
	if _, _, err := ctrl.CreateProfile(context.Background(), profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	plan, err := ctrl.PlanOpen(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	if got := plan.Steps[1].Technical.Parameters["app_id"]; got != "" {
		t.Fatalf("preview guessed an application ID: %q", got)
	}
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := ctrl.ApplyPlan(context.Background(), plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	return provider, cleanup, awaitOperationTerminal(t, ctrl, op.ID)
}

func TestAccessPolicyFailureCompensatesExactApplicationID(t *testing.T) {
	provider, cleanup, operation := runAccessCompensationTest(t, false)
	if operation.State != OperationStateFailed {
		t.Fatalf("operation state = %s, want failed", operation.State)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.policyAppID != "app-123" {
		t.Fatalf("policy used application ID %q, want app-123", provider.policyAppID)
	}
	if len(provider.deletedAppIDs) != 1 || provider.deletedAppIDs[0] != "app-123" {
		t.Fatalf("compensation deleted IDs = %#v, want [app-123]", provider.deletedAppIDs)
	}
	if provider.liveApp {
		t.Fatal("application remained live after successful compensation")
	}
	if len(cleanup.items) != 0 {
		t.Fatalf("successful compensation recorded cleanup obligations: %#v", cleanup.items)
	}
}

func TestFailedAccessCompensationRetainsCleanupObligation(t *testing.T) {
	provider, cleanup, operation := runAccessCompensationTest(t, true)
	if operation.State != OperationStateFailed {
		t.Fatalf("operation state = %s, want failed", operation.State)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.liveApp == false {
		t.Fatal("failed compensation pretended the application was removed")
	}
	if len(provider.deletedAppIDs) != 1 || provider.deletedAppIDs[0] != "app-123" {
		t.Fatalf("failed compensation deleted IDs = %#v, want [app-123]", provider.deletedAppIDs)
	}
	if len(cleanup.items) != 1 || cleanup.items[0].externalID != "app-123" || cleanup.items[0].state != "compensation_failed" {
		t.Fatalf("cleanup obligation = %#v, want app-123/compensation_failed", cleanup.items)
	}
}
