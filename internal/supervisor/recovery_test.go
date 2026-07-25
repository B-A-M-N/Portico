package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paoloanzn/portico/internal/core"
	"github.com/paoloanzn/portico/internal/store"
)

// --------------- test fixtures ---------------

func newRecoveryTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func recoveryTestProfile(connID core.ConnectionID) *core.ConnectionProfile {
	now := time.Now().UTC().Truncate(time.Second)
	return &core.ConnectionProfile{
		ID:       connID,
		Name:     "recovery-test",
		Revision: 1,
		Source: core.SourceSpec{
			Kind: core.SourceExisting,
			Existing: &core.ExistingServiceSpec{
				Network:  "tcp",
				Address:  "localhost",
				Protocol: core.ProtocolHTTP,
			},
		},
		Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
		Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
		Provider:   core.ProviderSelection{ProviderID: core.ProviderID("mock")},
		Lifecycle:  core.LifecycleSpec{OnDisconnect: core.DisconnectKeepAlive},
		Desired:    core.DesiredOpen,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func recoveryTestRuntime(connID core.ConnectionID) *core.ConnectionRuntime {
	now := time.Now().UTC().Truncate(time.Second)
	return &core.ConnectionRuntime{
		ConnectionID:   connID,
		State:          core.RuntimeOpening,
		LastObservedAt: now,
		LastTransition: now,
		Provider:       core.ProviderRuntime{ProviderID: core.ProviderID("mock")},
	}
}

func recoveryTestPlan(t *testing.T, connID core.ConnectionID, intent core.OperationIntent, steps []core.PlanStep) *core.OperationPlan {
	t.Helper()
	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    connID,
		ProfileRevision: 1,
		Provider:        core.ProviderID("mock"),
		Intent:          intent,
		Steps:           steps,
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}
	if err := plan.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	return plan
}

// seedInterruptedOperation persists a profile, runtime, plan, and an
// operation left in 'running' state, mimicking a supervisor crash.
func seedInterruptedOperation(t *testing.T, st *store.Store, connID core.ConnectionID,
	intent core.OperationIntent, steps []core.PlanStep) (*core.OperationPlan, core.OperationID) {
	t.Helper()
	ctx := context.Background()

	if err := st.SaveProfile(ctx, recoveryTestProfile(connID)); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	if err := st.SaveRuntime(ctx, recoveryTestRuntime(connID)); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}
	plan := recoveryTestPlan(t, connID, intent, steps)
	if err := st.SavePlan(ctx, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	opID := core.NewOperationID()
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if err := st.SaveOperation(ctx, opID, plan.ID, connID, "running", startedAt); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
	return plan, opID
}

func appendStepEvent(t *testing.T, st *store.Store, opID core.OperationID,
	stepID string, kind core.StepKind, stage core.EventStage) {
	t.Helper()
	evtType := core.EventOperationStepStarted
	switch stage {
	case core.StageSucceeded:
		evtType = core.EventOperationStepSucceeded
	case core.StageFailed:
		evtType = core.EventOperationStepFailed
	}
	err := st.AppendOperationEvent(context.Background(), core.Event{
		Type:      evtType,
		Timestamp: time.Now().UTC(),
		Data: core.OperationEvent{
			StepID:    stepID,
			StepKind:  kind,
			Stage:     stage,
			Timestamp: time.Now().UTC(),
		},
	}, opID)
	if err != nil {
		t.Fatalf("AppendOperationEvent(%s/%s): %v", stepID, stage, err)
	}
}

func openSteps() []core.PlanStep {
	return []core.PlanStep{
		{ID: "step-1", Kind: core.StepCreateTunnel, Summary: "Create tunnel"},
		{ID: "step-2", Kind: core.StepCreateDNSRecord, Summary: "Create DNS record"},
	}
}

// runRecovery loads the single seeded non-terminal operation and applies
// the recovery decision exactly the way recoverOperationJournals does.
func runRecovery(t *testing.T, st *store.Store, observed *core.ObservedConnection) recoveryDecision {
	t.Helper()
	ctx := context.Background()

	ops, err := st.ListNonTerminalOperations(ctx)
	if err != nil {
		t.Fatalf("ListNonTerminalOperations: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("expected 1 non-terminal operation, got %d", len(ops))
	}
	op := ops[0]

	plan, err := st.LoadPlan(ctx, op.PlanID)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	events, err := st.GetOperationEvents(ctx, op.ID)
	if err != nil {
		t.Fatalf("GetOperationEvents: %v", err)
	}
	resources, err := st.ListResourcesByConnection(ctx, op.ConnectionID)
	if err != nil {
		t.Fatalf("ListResourcesByConnection: %v", err)
	}

	decision := classifyOperationRecovery(plan, events, resources, observed)
	if err := applyRecoveryDecision(ctx, st, op, plan, decision); err != nil {
		t.Fatalf("applyRecoveryDecision: %v", err)
	}
	return decision
}

// --------------- end-to-end recovery against a real temp store ---------------

// TestRecoveryCompletesFullyCommittedOperation: every step has a durable
// terminal succeeded event -> recovery finishes the operation with the
// normal terminal success commit.
func TestRecoveryCompletesFullyCommittedOperation(t *testing.T) {
	st := newRecoveryTestStore(t)
	ctx := context.Background()
	connID := core.ConnectionID("conn-recovery-complete")

	_, opID := seedInterruptedOperation(t, st, connID, core.IntentOpen, openSteps())
	appendStepEvent(t, st, opID, "step-1", core.StepCreateTunnel, core.StageStarted)
	appendStepEvent(t, st, opID, "step-1", core.StepCreateTunnel, core.StageSucceeded)
	appendStepEvent(t, st, opID, "step-2", core.StepCreateDNSRecord, core.StageStarted)
	appendStepEvent(t, st, opID, "step-2", core.StepCreateDNSRecord, core.StageSucceeded)
	if err := st.SaveResource(ctx, &core.ProviderResource{
		ConnectionID: connID,
		ProviderID:   "mock",
		Type:         core.ResourceTunnel,
		ExternalID:   "tun-1",
		Ownership:    core.OwnershipManaged,
	}); err != nil {
		t.Fatalf("SaveResource: %v", err)
	}

	decision := runRecovery(t, st, nil)
	if decision.Outcome != recoveryComplete {
		t.Fatalf("expected outcome %q, got %q", recoveryComplete, decision.Outcome)
	}

	ops, err := st.ListNonTerminalOperations(ctx)
	if err != nil {
		t.Fatalf("ListNonTerminalOperations: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("expected operation to be terminal after recovery, still non-terminal: %+v", ops)
	}

	rt, err := st.LoadRuntime(ctx, connID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if rt.State != core.RuntimeOpen {
		t.Errorf("expected runtime state %q, got %q", core.RuntimeOpen, rt.State)
	}
	if rt.Error != nil {
		t.Errorf("expected no runtime error, got %+v", rt.Error)
	}
	if rt.ActiveOperation != nil {
		t.Errorf("expected active operation cleared, got %v", *rt.ActiveOperation)
	}
}

// TestRecoveryFailsUntouchedOperationSafely: an operation with no journal
// events never reached the provider -> recovery fails it cleanly without a
// recovery finding.
func TestRecoveryFailsUntouchedOperationSafely(t *testing.T) {
	st := newRecoveryTestStore(t)
	ctx := context.Background()
	connID := core.ConnectionID("conn-recovery-untouched")

	_, opID := seedInterruptedOperation(t, st, connID, core.IntentOpen, openSteps())

	decision := runRecovery(t, st, nil)
	if decision.Outcome != recoveryFailSafe {
		t.Fatalf("expected outcome %q, got %q", recoveryFailSafe, decision.Outcome)
	}

	ops, err := st.ListNonTerminalOperations(ctx)
	if err != nil {
		t.Fatalf("ListNonTerminalOperations: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("expected operation to be terminal after recovery, still non-terminal: %+v", ops)
	}

	rt, err := st.LoadRuntime(ctx, connID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if rt.State != core.RuntimeError {
		t.Errorf("expected runtime state %q, got %q", core.RuntimeError, rt.State)
	}
	if rt.Error == nil {
		t.Fatal("expected a runtime error to be recorded")
	}
	if rt.Error.Code == "PTO-OP-RECOVERY-REQUIRED" {
		t.Errorf("untouched operation must not be marked recovery-required, got %+v", rt.Error)
	}

	findings, err := st.ListFindingsByConnection(ctx, connID)
	if err != nil {
		t.Fatalf("ListFindingsByConnection: %v", err)
	}
	for _, f := range findings {
		if strings.HasPrefix(string(f.ID), "recovery-") {
			t.Errorf("clean failure must not create a recovery finding, got %+v", f)
		}
	}

	// No step should have been recorded as needing recovery.
	results, err := st.GetStepResults(ctx, opID)
	if err != nil {
		t.Fatalf("GetStepResults: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no step results for untouched operation, got %+v", results)
	}
}

// TestRecoveryMarksUncertainOperationRecoveryRequired: one step committed,
// the next started but never terminated -> recovery marks the operation
// failed with PTO-OP-RECOVERY-REQUIRED, records the uncertain step as
// outcome_unknown + recovery_required, and creates a finding with the exact
// evidence.
func TestRecoveryMarksUncertainOperationRecoveryRequired(t *testing.T) {
	st := newRecoveryTestStore(t)
	ctx := context.Background()
	connID := core.ConnectionID("conn-recovery-required")

	_, opID := seedInterruptedOperation(t, st, connID, core.IntentOpen, openSteps())
	// step-1 committed (terminal event + persisted resource).
	appendStepEvent(t, st, opID, "step-1", core.StepCreateTunnel, core.StageStarted)
	appendStepEvent(t, st, opID, "step-1", core.StepCreateTunnel, core.StageSucceeded)
	if err := st.SaveResource(ctx, &core.ProviderResource{
		ConnectionID: connID,
		ProviderID:   "mock",
		Type:         core.ResourceTunnel,
		ExternalID:   "tun-1",
		Ownership:    core.OwnershipManaged,
	}); err != nil {
		t.Fatalf("SaveResource: %v", err)
	}
	// step-2 started, no terminal event, no persisted resource.
	appendStepEvent(t, st, opID, "step-2", core.StepCreateDNSRecord, core.StageStarted)

	// The provider reports a DNS record that was never persisted: it must
	// be listed as evidence, never silently adopted or orphaned.
	observed := &core.ObservedConnection{
		ConnectionID: connID,
		ProviderID:   "mock",
		Tunnel:       &core.ObservedTunnel{ID: "tun-1"},
		DNSRecords:   []core.ObservedDNSRecord{{ID: "dns-ghost"}},
	}

	decision := runRecovery(t, st, observed)
	if decision.Outcome != recoveryRequired {
		t.Fatalf("expected outcome %q, got %q", recoveryRequired, decision.Outcome)
	}
	if decision.LastStartedStepID != "step-2" {
		t.Errorf("expected last started step step-2, got %q", decision.LastStartedStepID)
	}

	// Operation must be terminal.
	ops, err := st.ListNonTerminalOperations(ctx)
	if err != nil {
		t.Fatalf("ListNonTerminalOperations: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("expected operation to be terminal after recovery, still non-terminal: %+v", ops)
	}

	// Runtime must carry the recovery-required error code.
	rt, err := st.LoadRuntime(ctx, connID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if rt.State != core.RuntimeError {
		t.Errorf("expected runtime state %q, got %q", core.RuntimeError, rt.State)
	}
	if rt.Error == nil || rt.Error.Code != "PTO-OP-RECOVERY-REQUIRED" {
		t.Fatalf("expected runtime error PTO-OP-RECOVERY-REQUIRED, got %+v", rt.Error)
	}

	// The uncertain step must be durably recorded.
	results, err := st.GetStepResults(ctx, opID)
	if err != nil {
		t.Fatalf("GetStepResults: %v", err)
	}
	var found bool
	for _, r := range results {
		if r.StepID == "step-2" {
			found = true
			if r.Status != string(store.StepOutcomeUnknown) {
				t.Errorf("expected step-2 status %q, got %q", store.StepOutcomeUnknown, r.Status)
			}
			if !r.RecoveryRequired {
				t.Error("expected step-2 to be marked recovery_required")
			}
		}
	}
	if !found {
		t.Fatalf("expected a step result row for step-2, got %+v", results)
	}

	// A finding must preserve operation, last started step, and exact IDs.
	findings, err := st.ListFindingsByConnection(ctx, connID)
	if err != nil {
		t.Fatalf("ListFindingsByConnection: %v", err)
	}
	var finding *core.DiagnosticFinding
	for i := range findings {
		if findings[i].ID == core.FindingID("recovery-"+string(opID)) {
			finding = &findings[i]
		}
	}
	if finding == nil {
		t.Fatalf("expected recovery finding recovery-%s, got %+v", opID, findings)
	}
	if len(finding.Evidence) != 1 {
		t.Fatalf("expected 1 evidence entry, got %+v", finding.Evidence)
	}
	data := finding.Evidence[0].Data
	if data["operation_id"] != string(opID) {
		t.Errorf("evidence operation_id = %q, want %q", data["operation_id"], opID)
	}
	if data["last_started_step"] != "step-2" {
		t.Errorf("evidence last_started_step = %q, want step-2", data["last_started_step"])
	}
	if !strings.Contains(data["known_resources"], "tunnel/tun-1") {
		t.Errorf("evidence known_resources = %q, want to contain tunnel/tun-1", data["known_resources"])
	}
	if !strings.Contains(data["possibly_unpersisted"], "dns_record/dns-ghost") {
		t.Errorf("evidence possibly_unpersisted = %q, want to contain dns_record/dns-ghost", data["possibly_unpersisted"])
	}
	if !strings.Contains(data["unknown_steps"], "step-2") {
		t.Errorf("evidence unknown_steps = %q, want to contain step-2", data["unknown_steps"])
	}
}

// --------------- pure classification tests ---------------

func TestClassifyOperationRecovery(t *testing.T) {
	plan := recoveryTestPlan(t, "conn-classify", core.IntentOpen, openSteps())

	started := func(stepID string, kind core.StepKind) store.OperationJournalEvent {
		return store.OperationJournalEvent{StepID: stepID, EventType: string(kind), Stage: string(core.StageStarted)}
	}
	succeededEvt := func(stepID string, kind core.StepKind) store.OperationJournalEvent {
		return store.OperationJournalEvent{StepID: stepID, EventType: string(kind), Stage: string(core.StageSucceeded)}
	}
	failedEvt := func(stepID string, kind core.StepKind) store.OperationJournalEvent {
		return store.OperationJournalEvent{StepID: stepID, EventType: string(kind), Stage: string(core.StageFailed)}
	}
	tunnelRes := core.ProviderResource{
		ConnectionID: "conn-classify", ProviderID: "mock",
		Type: core.ResourceTunnel, ExternalID: "tun-1",
		Ownership: core.OwnershipManaged,
	}

	cases := []struct {
		name      string
		events    []store.OperationJournalEvent
		resources []core.ProviderResource
		observed  *core.ObservedConnection
		want      recoveryOutcome
		wantStep2 stepRecoveryClass
	}{
		{
			name: "all steps committed",
			events: []store.OperationJournalEvent{
				started("step-1", core.StepCreateTunnel), succeededEvt("step-1", core.StepCreateTunnel),
				started("step-2", core.StepCreateDNSRecord), succeededEvt("step-2", core.StepCreateDNSRecord),
			},
			want:      recoveryComplete,
			wantStep2: stepRecoveryCommitted,
		},
		{
			name:      "no steps started",
			want:      recoveryFailSafe,
			wantStep2: stepRecoveryNotStarted,
		},
		{
			name: "first step failed terminally, nothing committed",
			events: []store.OperationJournalEvent{
				started("step-1", core.StepCreateTunnel), failedEvt("step-1", core.StepCreateTunnel),
			},
			want:      recoveryFailSafe,
			wantStep2: stepRecoveryNotStarted,
		},
		{
			name: "committed then started-unterminated is recovery required",
			events: []store.OperationJournalEvent{
				started("step-1", core.StepCreateTunnel), succeededEvt("step-1", core.StepCreateTunnel),
				started("step-2", core.StepCreateDNSRecord),
			},
			resources: []core.ProviderResource{tunnelRes},
			want:      recoveryRequired,
			wantStep2: stepRecoveryUnknown,
		},
		{
			name: "started-unterminated with persisted resource counts as committed",
			events: []store.OperationJournalEvent{
				// step-1 started, no terminal event, but the tunnel resource
				// was durably persisted: the commit landed.
				started("step-1", core.StepCreateTunnel),
				started("step-2", core.StepCreateDNSRecord), succeededEvt("step-2", core.StepCreateDNSRecord),
			},
			resources: []core.ProviderResource{tunnelRes},
			want:      recoveryComplete,
			wantStep2: stepRecoveryCommitted,
		},
		{
			name: "observed unpersisted resource forces recovery required",
			observed: &core.ObservedConnection{
				ConnectionID: "conn-classify",
				ProviderID:   "mock",
				Tunnel:       &core.ObservedTunnel{ID: "tun-ghost"},
			},
			want:      recoveryRequired,
			wantStep2: stepRecoveryNotStarted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := classifyOperationRecovery(plan, tc.events, tc.resources, tc.observed)
			if d.Outcome != tc.want {
				t.Errorf("outcome = %q, want %q", d.Outcome, tc.want)
			}
			if len(d.Steps) != len(plan.Steps) {
				t.Fatalf("classified %d steps, want %d", len(d.Steps), len(plan.Steps))
			}
			if d.Steps[1].Class != tc.wantStep2 {
				t.Errorf("step-2 class = %q, want %q", d.Steps[1].Class, tc.wantStep2)
			}
		})
	}
}
