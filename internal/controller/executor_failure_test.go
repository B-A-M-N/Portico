package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider/mock"
)

// --------------- fakes ---------------

type fakeStepCommitter struct {
	mu       sync.Mutex
	requests []core.StepCommitRequest
	failOn   int // fail on the Nth call (1-based); 0 = never fail
	calls    int
}

func (f *fakeStepCommitter) CommitStepResult(ctx context.Context, req core.StepCommitRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failOn > 0 && f.calls == f.failOn {
		return fmt.Errorf("injected step commit failure")
	}
	f.requests = append(f.requests, req)
	return nil
}

type fakeRuntimeCommitter struct {
	mu           sync.Mutex
	openResult   *core.RuntimeCommitResult
	failOpen     bool
	failureCalls int
}

func (f *fakeRuntimeCommitter) CommitOpenSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID, startedAt time.Time) (*core.RuntimeCommitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOpen {
		return nil, fmt.Errorf("injected terminal commit failure")
	}
	return f.openResult, nil
}

func (f *fakeRuntimeCommitter) CommitCloseSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) (*core.RuntimeCommitResult, error) {
	return nil, nil
}

func (f *fakeRuntimeCommitter) CommitRepairSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) (*core.RuntimeCommitResult, error) {
	return nil, nil
}

func (f *fakeRuntimeCommitter) CommitEditSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) (*core.RuntimeCommitResult, error) {
	return nil, nil
}

func (f *fakeRuntimeCommitter) CommitOperationFailure(ctx context.Context, connID core.ConnectionID, opID core.OperationID, errMsg string, provider core.ProviderID, retryable bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failureCalls++
	return nil
}

func (f *fakeRuntimeCommitter) CommitDeleteSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) error {
	return nil
}

func (f *fakeRuntimeCommitter) failureCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failureCalls
}

// --------------- helpers ---------------

func newFailureTestProfile() *core.ConnectionProfile {
	return &core.ConnectionProfile{
		Name: "failure-test",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind: core.SourceExisting,
					Existing: &core.ExistingServiceSpec{
						Address:  "localhost:8080",
						Protocol: core.ProtocolHTTP,
					},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver: core.DriverSelection{ProviderID: "mock"},
		Lifecycle: core.LifecycleSpec{
			AutoStart:    true,
			OnDisconnect: core.DisconnectKeepAlive,
		},
		Desired: core.DesiredOpen,
	}
}

func awaitOperationTerminal(t *testing.T, ctrl *Controller, opID core.OperationID) *Operation {
	t.Helper()
	// Fifteen seconds, not five: under the full-suite race gate the machine is
	// loaded enough that a healthy operation that finishes in half a second
	// otherwise outruns the poll and the test reports a false failure.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok := ctrl.GetOperation(opID)
		if !ok {
			t.Fatal("operation disappeared")
		}
		if snap.State == OperationStateCompleted || snap.State == OperationStateFailed {
			return snap
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("operation did not reach terminal state")
	return nil
}

// --------------- tests ---------------

// A step-commit failure after a successful provider mutation must fail the
// operation (never report success with unpersisted provider state).
func TestExecutor_StepCommitFailureFailsOperation(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	journal := newTestJournal()
	ctrl := New(reg, journal)
	ctx := context.Background()

	committer := &fakeStepCommitter{failOn: 1}
	ctrl.SetStepCommitter(committer)

	profile := newFailureTestProfile()
	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	plan, err := ctrl.PlanOpen(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := ctrl.ApplyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	final := awaitOperationTerminal(t, ctrl, op.ID)
	if final.State != OperationStateFailed {
		t.Fatalf("expected failed operation after step commit failure, got %s", final.State)
	}

	rt, ok := ctrl.GetRuntime(profile.ID)
	if !ok {
		t.Fatal("runtime missing")
	}
	if rt.State != core.RuntimeError {
		t.Fatalf("expected runtime error state, got %s", rt.State)
	}
}

// A successful step commit must carry the terminal step data atomically:
// operation ID, connection ID, step, and result all present in one request.
func TestExecutor_StepCommitCarriesAtomicPayload(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	journal := newTestJournal()
	ctrl := New(reg, journal)
	ctx := context.Background()

	committer := &fakeStepCommitter{}
	ctrl.SetStepCommitter(committer)

	profile := newFailureTestProfile()
	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	plan, err := ctrl.PlanOpen(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := ctrl.ApplyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	final := awaitOperationTerminal(t, ctrl, op.ID)
	if final.State != OperationStateCompleted {
		t.Fatalf("expected completed, got %s", final.State)
	}

	committer.mu.Lock()
	defer committer.mu.Unlock()
	if len(committer.requests) != len(plan.Steps) {
		t.Fatalf("expected %d step commits, got %d", len(plan.Steps), len(committer.requests))
	}
	for _, req := range committer.requests {
		if req.OperationID != op.ID {
			t.Errorf("commit request has wrong operation ID: %s", req.OperationID)
		}
		if req.ConnectionID != profile.ID {
			t.Errorf("commit request has wrong connection ID: %s", req.ConnectionID)
		}
		if !req.Result.Succeeded {
			t.Errorf("commit request for step %s not marked succeeded", req.Step.ID)
		}
	}
}

func TestReplacementLifecycleMarksRecordOnlyExactMissingAccessResources(t *testing.T) {
	marks := replacementLifecycleMarks("cloudflare", core.PlanStep{Technical: core.TechnicalOperation{Parameters: map[string]string{
		"replaces_access_app_id":    "old-app",
		"replaces_access_policy_id": "old-policy",
	}}})
	if len(marks) != 2 {
		t.Fatalf("marks = %#v", marks)
	}
	if marks[0].ResourceType != core.ResourceAccessApp || marks[0].ExternalID != "old-app" || marks[0].NewLifecycle != core.LifecycleExternallyRemoved {
		t.Fatalf("unexpected application mark: %#v", marks[0])
	}
	if marks[1].ResourceType != core.ResourceAccessPolicy || marks[1].ExternalID != "old-policy" || marks[1].NewLifecycle != core.LifecycleExternallyRemoved {
		t.Fatalf("unexpected policy mark: %#v", marks[1])
	}
}

func TestApplyResourceOutcomeRetiresReplacedResourceInMemory(t *testing.T) {
	resources := applyResourceOutcome(
		[]core.ProviderResource{{ProviderID: "cloudflare", Type: core.ResourceAccessPolicy, ExternalID: "old-policy", Lifecycle: core.LifecyclePresent}},
		[]core.ProviderResource{{ProviderID: "cloudflare", Type: core.ResourceAccessPolicy, ExternalID: "new-policy", Ownership: core.OwnershipManaged}},
		[]core.LifecycleMark{{ProviderID: "cloudflare", ResourceType: core.ResourceAccessPolicy, ExternalID: "old-policy", NewLifecycle: core.LifecycleExternallyRemoved}},
	)
	if len(resources) != 2 {
		t.Fatalf("resources = %#v", resources)
	}
	if resources[0].Lifecycle != core.LifecycleExternallyRemoved || resources[1].Lifecycle != core.LifecyclePresent {
		t.Fatalf("replacement inventory = %#v", resources)
	}
}

// Terminal commit failure must fail the operation AND durably record the
// runtime failure via CommitOperationFailure.
func TestExecutor_TerminalCommitFailureCommitsRuntimeFailure(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	journal := newTestJournal()
	ctrl := New(reg, journal)
	ctx := context.Background()

	rc := &fakeRuntimeCommitter{failOpen: true}
	ctrl.SetRuntimeCommitter(rc)

	profile := newFailureTestProfile()
	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	plan, err := ctrl.PlanOpen(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := ctrl.ApplyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	final := awaitOperationTerminal(t, ctrl, op.ID)
	if final.State != OperationStateFailed {
		t.Fatalf("expected failed operation, got %s", final.State)
	}
	if rc.failureCount() == 0 {
		t.Fatal("expected CommitOperationFailure to be called on terminal commit failure")
	}
	rt, ok := ctrl.GetRuntime(profile.ID)
	if !ok {
		t.Fatal("runtime missing")
	}
	if rt.State != core.RuntimeError {
		t.Fatalf("expected runtime error, got %s", rt.State)
	}
}

// The exact committed values from the store must be installed into memory.
func TestExecutor_InstallsExactCommittedValues(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	journal := newTestJournal()
	ctrl := New(reg, journal)
	ctx := context.Background()

	committedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rc := &fakeRuntimeCommitter{
		openResult: &core.RuntimeCommitResult{
			ProfileRevision: 42,
			DesiredState:    core.DesiredOpen,
			RuntimeState:    core.RuntimeOpen,
			LastTransition:  committedAt,
		},
	}
	ctrl.SetRuntimeCommitter(rc)

	profile := newFailureTestProfile()
	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	plan, err := ctrl.PlanOpen(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := ctrl.ApplyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	final := awaitOperationTerminal(t, ctrl, op.ID)
	if final.State != OperationStateCompleted {
		t.Fatalf("expected completed, got %s", final.State)
	}

	p, ok := ctrl.GetProfile(profile.ID)
	if !ok {
		t.Fatal("profile missing")
	}
	if p.Revision != 42 {
		t.Fatalf("expected committed revision 42, got %d", p.Revision)
	}
	rt, ok := ctrl.GetRuntime(profile.ID)
	if !ok {
		t.Fatal("runtime missing")
	}
	if !rt.LastTransition.Equal(committedAt) {
		t.Fatalf("expected committed transition %v, got %v", committedAt, rt.LastTransition)
	}
	if rt.State != core.RuntimeOpen {
		t.Fatalf("expected open, got %s", rt.State)
	}
}
