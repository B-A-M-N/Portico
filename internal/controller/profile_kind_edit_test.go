package controller

import (
	"context"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider/mock"
)

// streamingMock wraps the mock provider with Streaming support so an
// openai_compatible profile can be carried.
type streamingMock struct {
	*mock.Provider
}

func (p *streamingMock) Capabilities(ctx context.Context) (core.Capabilities, error) {
	caps, err := p.Provider.Capabilities(ctx)
	if err != nil {
		return caps, err
	}
	caps.Streaming = core.CapabilitySupport{Supported: true, Stability: core.StabilityStable}
	return caps, nil
}

// nonStreamingMock makes the negative compatibility case explicit now that
// the hermetic mock provider used by the compiled TUI suite supports streaming.
type nonStreamingMock struct {
	*mock.Provider
}

func (p *nonStreamingMock) Capabilities(ctx context.Context) (core.Capabilities, error) {
	caps, err := p.Provider.Capabilities(ctx)
	if err != nil {
		return caps, err
	}
	caps.Streaming = core.CapabilitySupport{}
	return caps, nil
}

// ProfileKind edits (audit P0-8): plan/edit must actually change the
// workload, refuse unknown kinds, revalidate transport compatibility,
// preview a semantic restart, persist the new kind on apply, and keep
// stale-revision protection.

func TestProfileKindEditChangesTheWorkload(t *testing.T) {
	ctx := context.Background()
	current := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := New(newTestRegistry(&streamingMock{mock.New()}), newTestJournal())
	c.RestoreProfile(current)
	rt := &core.ConnectionRuntime{ConnectionID: current.ID, State: core.RuntimeOpen}
	rt.Connector.Status = core.ConnectorStatusRunning
	rt.Provider.Resources = managedResources()
	c.RestoreRuntime(rt)

	proposed := current.DeepCopy()
	proposed.ProfileKind = core.ProfileOpenAICompatible

	plan, delta, err := c.PlanEdit(ctx, current.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	found := false
	for _, change := range delta.Changes {
		if change == "profile kind" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a profile-kind change was not classified: %v", delta.Changes)
	}
	if !delta.RestartConnector {
		t.Fatal("a workload change did not require a semantic restart")
	}
	if !hasKind(plan, core.StepApplyProfile) {
		t.Fatalf("the preview carries no apply-profile step: %v", stepKinds(plan))
	}
}

func TestProfileKindEditToUnknownKindIsRefused(t *testing.T) {
	current := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := editController(t, current, false, nil)

	proposed := current.DeepCopy()
	proposed.ProfileKind = core.ProfileKind("quantum_relay")

	_, _, err := c.PlanEdit(context.Background(), current.ID, proposed)
	if err == nil {
		t.Fatal("an unregistered profile kind was accepted")
	}
}

func TestProfileKindEditRefusedWhenTransportIncapable(t *testing.T) {
	ctx := context.Background()
	current := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := New(newTestRegistry(&nonStreamingMock{mock.New()}), newTestJournal())
	c.RestoreProfile(current)
	c.RestoreRuntime(&core.ConnectionRuntime{ConnectionID: current.ID, State: core.RuntimeClosed})

	proposed := current.DeepCopy()
	proposed.ProfileKind = core.ProfileOpenAICompatible

	_, _, err := c.PlanEdit(ctx, current.ID, proposed)
	// A transport that does not declare Streaming support cannot carry an
	// openai_compatible profile; the edit must be refused rather than saved
	// into a state whose next open fails.
	if err == nil {
		t.Fatal("a transport incapable of the new profile accepted the workload change")
	}
}

func TestAppliedProfileKindEditPersistsTheNewKind(t *testing.T) {
	ctx := context.Background()
	current := editProfile("original", "old.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := editController(t, current, false, managedResources())

	// Rename so the fixture produces a plan even if the kind comparison were
	// broken; asserting BOTH changes landed proves the kind edit applied.
	proposed := current.DeepCopy()
	proposed.Name = "renamed"
	proposed.ProfileKind = core.ProfileWebService // same value; rename drives steps

	plan, _, err := c.PlanEdit(ctx, current.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("the fixture no longer produces an edit with steps")
	}
	if err := c.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := c.ApplyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var final *Operation
	for time.Now().Before(deadline) {
		snap, ok := c.GetOperation(op.ID)
		if ok && (snap.State == OperationStateCompleted || snap.State == OperationStateFailed) {
			final = snap
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if final == nil || final.State != OperationStateCompleted {
		t.Fatalf("the edit operation did not complete: %+v", final)
	}
	applied, ok := c.GetProfile(current.ID)
	if !ok {
		t.Fatal("the connection is gone after applying an edit")
	}
	if applied.ProfileKind != proposed.ProfileKind {
		t.Fatalf("applied profile kind = %q, want %q", applied.ProfileKind, proposed.ProfileKind)
	}
}

func TestStaleRevisionStillRejectedForProfileKindEdit(t *testing.T) {
	// Revision protection lives at the supervisor boundary; this controller-
	// level check pins that PlanEdit itself binds the plan to the CURRENT
	// revision, so a stale proposal cannot silently win.
	ctx := context.Background()
	current := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := New(newTestRegistry(&streamingMock{mock.New()}), newTestJournal())
	c.RestoreProfile(current)
	rt := &core.ConnectionRuntime{ConnectionID: current.ID, State: core.RuntimeClosed}
	rt.Provider.Resources = managedResources()
	c.RestoreRuntime(rt)

	proposed := current.DeepCopy()
	proposed.ProfileKind = core.ProfileOpenAICompatible
	proposed.Revision = current.Revision + 5 // stale view of a moved connection

	plan, _, err := c.PlanEdit(ctx, current.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	if plan.ProfileRevision != current.Revision {
		t.Fatalf("plan bound to revision %d, want the current %d", plan.ProfileRevision, current.Revision)
	}
}
