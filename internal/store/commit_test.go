package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// CommitStepResult must persist the terminal event, resources, credentials,
// and lifecycle changes in one transaction.
func TestCommitStepResultAtomicPersist(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	seedOperation(t, s, "op-1", "plan-1", p.ID)

	req := core.StepCommitRequest{
		OperationID:  "op-1",
		ConnectionID: p.ID,
		Provider:     "mock",
		Step:         core.PlanStep{ID: "step-1", Kind: core.StepCreateTunnel, Summary: "Create tunnel"},
		Result: core.StepResult{
			StepID:    "step-1",
			Succeeded: true,
			Resources: []core.ProviderResource{
				{
					ConnectionID: p.ID,
					ProviderID:   "mock",
					Type:         core.ResourceTunnel,
					ExternalID:   "tun-123",
					Ownership:    core.OwnershipManaged,
				},
			},
			CredentialMutations: []core.CredentialMutation{
				{TunnelID: "tun-123", Secret: []byte("secret-token")},
			},
		},
	}
	if err := s.CommitStepResult(ctx, req); err != nil {
		t.Fatalf("CommitStepResult: %v", err)
	}

	// Resource persisted.
	res, err := s.LoadResource(ctx, "mock", core.ResourceTunnel, "tun-123")
	if err != nil {
		t.Fatalf("LoadResource: %v", err)
	}
	if res.ConnectionID != p.ID || res.Ownership != core.OwnershipManaged {
		t.Fatalf("unexpected resource: %+v", res)
	}

	// Credential persisted and decryptable.
	tunnelID, token, err := s.LoadTunnelCredential(ctx, p.ID)
	if err != nil {
		t.Fatalf("LoadTunnelCredential: %v", err)
	}
	if tunnelID != "tun-123" || token != "secret-token" {
		t.Fatalf("unexpected credential: %s / %s", tunnelID, token)
	}

	// Event persisted.
	var count int
	if err := s.DB().QueryRow(
		"SELECT COUNT(*) FROM operation_events WHERE operation_id = 'op-1' AND step_id = 'step-1'").Scan(&count); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one terminal step event, got %d", count)
	}
	durableEvents, err := s.GetDurableEventsSince(ctx, 0, 10)
	if err != nil {
		t.Fatalf("GetDurableEventsSince: %v", err)
	}
	if len(durableEvents) != 2 || durableEvents[0].Event.Type != "operation.created" || durableEvents[1].Event.Type != core.EventOperationStepSucceeded || durableEvents[1].OperationID != "op-1" || durableEvents[1].ConnectionID != p.ID {
		t.Fatalf("unexpected unified durable event: %#v", durableEvents)
	}
	results, err := s.GetStepResults(ctx, "op-1")
	if err != nil {
		t.Fatalf("GetStepResults: %v", err)
	}
	if len(results) != 1 || results[0].Status != string(StepSucceeded) || results[0].Result.StepID != "step-1" {
		t.Fatalf("unexpected durable step result: %#v", results)
	}
	if len(results[0].Result.CredentialMutations) != 0 {
		t.Fatal("recovery ledger must not serialize credential secret bytes")
	}
}

func TestBeginStepAndOutcomeUseSingleRecoveryRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatal(err)
	}
	seedOperation(t, s, "op-ledger", "plan-ledger", p.ID)
	step := core.PlanStep{ID: "step-ledger", Kind: core.StepVerifyEndpoint, Summary: "Verify endpoint"}
	if err := s.BeginStep(ctx, "op-ledger", p.ID, step); err != nil {
		t.Fatalf("BeginStep: %v", err)
	}
	if err := s.CommitStepOutcome(ctx, core.StepCommitRequest{
		OperationID: "op-ledger", ConnectionID: p.ID, Provider: "mock", Step: step,
		Result: core.StepResult{StepID: step.ID, Succeeded: false, Error: context.DeadlineExceeded},
	}); err != nil {
		t.Fatalf("CommitStepOutcome: %v", err)
	}
	results, err := s.GetStepResults(ctx, "op-ledger")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != string(StepFailed) || results[0].Result.Error == nil {
		t.Fatalf("unexpected ledger result: %#v", results)
	}
	var count int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM operation_step_results WHERE operation_id = ? AND step_id = ?", "op-ledger", step.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("got %d recovery rows, want 1", count)
	}
}

func TestCommitStepOutcomeCascadesEveryTrackedAccessPolicy(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatal(err)
	}
	seedOperation(t, s, "op-access-delete", "plan-access-delete", p.ID)

	app := &core.ProviderResource{
		ConnectionID: p.ID, ProviderID: "mock", Type: core.ResourceAccessApp,
		ExternalID: "app-1", Ownership: core.OwnershipManaged,
	}
	policyOne := &core.ProviderResource{
		ConnectionID: p.ID, ProviderID: "mock", Type: core.ResourceAccessPolicy,
		ExternalID: "policy-1", Ownership: core.OwnershipManaged,
		Metadata: map[string]string{"app_id": "app-1"},
	}
	policyTwo := &core.ProviderResource{
		ConnectionID: p.ID, ProviderID: "mock", Type: core.ResourceAccessPolicy,
		ExternalID: "policy-2", Ownership: core.OwnershipManaged,
		Metadata: map[string]string{"app_id": "app-1"},
	}
	for _, resource := range []*core.ProviderResource{app, policyOne, policyTwo} {
		if err := s.SaveResource(ctx, resource); err != nil {
			t.Fatal(err)
		}
	}

	step := core.PlanStep{ID: "delete-app", Kind: core.StepDeleteAccessApp, Summary: "Delete access app"}
	if err := s.CommitStepOutcome(ctx, core.StepCommitRequest{
		OperationID: "op-access-delete", ConnectionID: p.ID, Provider: "mock", Step: step,
		Result: core.StepResult{StepID: step.ID, Succeeded: true},
		Lifecycle: []core.LifecycleMark{{
			ProviderID: "mock", ResourceType: core.ResourceAccessApp, ExternalID: "app-1", NewLifecycle: core.LifecycleRemoved,
		}},
		RemovedAccessApps: []string{"app-1"},
	}); err != nil {
		t.Fatalf("CommitStepOutcome: %v", err)
	}
	for _, id := range []string{"policy-1", "policy-2"} {
		resource, err := s.LoadResource(ctx, "mock", core.ResourceAccessPolicy, id)
		if err != nil {
			t.Fatal(err)
		}
		if resource.Lifecycle != core.LifecycleRemoved {
			t.Fatalf("policy %s lifecycle = %q, want removed", id, resource.Lifecycle)
		}
	}
}

func TestCreateManagedResourceRevivesOnlyManagedResource(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	managed := &core.ProviderResource{
		ConnectionID: "conn-managed", ProviderID: "mock", Type: core.ResourceTunnel,
		ExternalID: "tunnel-managed", Ownership: core.OwnershipManaged,
	}
	if err := s.SaveResource(ctx, managed); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkResourceRemoved(ctx, managed.ConnectionID, managed.ProviderID, managed.Type, managed.ExternalID); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateManagedResource(ctx, managed); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadResource(ctx, managed.ProviderID, managed.Type, managed.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Lifecycle != core.LifecyclePresent {
		t.Fatalf("managed resource lifecycle = %q, want present", got.Lifecycle)
	}

	external := &core.ProviderResource{
		ConnectionID: "conn-external", ProviderID: "mock", Type: core.ResourceTunnel,
		ExternalID: "tunnel-external", Ownership: core.OwnershipExternal,
	}
	if err := s.SaveResource(ctx, external); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkResourceRemoved(ctx, external.ConnectionID, external.ProviderID, external.Type, external.ExternalID); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateManagedResource(ctx, external); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadResource(ctx, external.ProviderID, external.Type, external.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ownership != core.OwnershipExternal || got.Lifecycle != core.LifecycleRemoved {
		t.Fatalf("external resource changed unexpectedly: %#v", got)
	}
}

func TestCommitStepOutcomeRevivesManagedResourceAfterRecreate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatal(err)
	}
	seedOperation(t, s, "op-recreate", "plan-recreate", p.ID)
	resource := &core.ProviderResource{
		ConnectionID: p.ID, ProviderID: "mock", Type: core.ResourceTunnel,
		ExternalID: "tunnel-recreated", Ownership: core.OwnershipManaged,
	}
	if err := s.SaveResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkResourceRemoved(ctx, resource.ConnectionID, resource.ProviderID, resource.Type, resource.ExternalID); err != nil {
		t.Fatal(err)
	}
	step := core.PlanStep{ID: "recreate-tunnel", Kind: core.StepRecreateTunnel, Summary: "Recreate tunnel"}
	if err := s.CommitStepOutcome(ctx, core.StepCommitRequest{
		OperationID: "op-recreate", ConnectionID: p.ID, Provider: "mock", Step: step,
		Result: core.StepResult{StepID: step.ID, Succeeded: true, Resources: []core.ProviderResource{*resource}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadResource(ctx, resource.ProviderID, resource.Type, resource.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Lifecycle != core.LifecyclePresent {
		t.Fatalf("recreated resource lifecycle = %q, want present", got.Lifecycle)
	}
}

func TestCommitStepOutcomeAtomicallyReplacesExternallyRemovedAccessResources(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatal(err)
	}
	seedOperation(t, s, "op-access-repair", "plan-access-repair", p.ID)
	for _, resource := range []*core.ProviderResource{
		{ConnectionID: p.ID, ProviderID: "mock", Type: core.ResourceAccessApp, ExternalID: "old-app", Ownership: core.OwnershipManaged},
		{ConnectionID: p.ID, ProviderID: "mock", Type: core.ResourceAccessPolicy, ExternalID: "old-policy", Ownership: core.OwnershipManaged, Metadata: map[string]string{"app_id": "old-app"}},
	} {
		if err := s.SaveResource(ctx, resource); err != nil {
			t.Fatal(err)
		}
	}
	step := core.PlanStep{ID: "repair-access", Kind: core.StepCreateAccessApp, Summary: "Recreate Access application"}
	if err := s.CommitStepOutcome(ctx, core.StepCommitRequest{
		OperationID: "op-access-repair", ConnectionID: p.ID, Provider: "mock", Step: step,
		Result: core.StepResult{StepID: step.ID, Succeeded: true, Resources: []core.ProviderResource{
			{ConnectionID: p.ID, ProviderID: "mock", Type: core.ResourceAccessApp, ExternalID: "new-app", Ownership: core.OwnershipManaged},
			{ConnectionID: p.ID, ProviderID: "mock", Type: core.ResourceAccessPolicy, ExternalID: "new-policy", Ownership: core.OwnershipManaged, Metadata: map[string]string{"app_id": "new-app"}},
		}},
		Lifecycle: []core.LifecycleMark{
			{ProviderID: "mock", ResourceType: core.ResourceAccessApp, ExternalID: "old-app", NewLifecycle: core.LifecycleExternallyRemoved},
			{ProviderID: "mock", ResourceType: core.ResourceAccessPolicy, ExternalID: "old-policy", NewLifecycle: core.LifecycleExternallyRemoved},
		},
	}); err != nil {
		t.Fatalf("CommitStepOutcome: %v", err)
	}
	for _, check := range []struct {
		typeID     core.ResourceType
		externalID string
		lifecycle  core.ResourceLifecycle
	}{
		{core.ResourceAccessApp, "old-app", core.LifecycleExternallyRemoved},
		{core.ResourceAccessPolicy, "old-policy", core.LifecycleExternallyRemoved},
		{core.ResourceAccessApp, "new-app", core.LifecyclePresent},
		{core.ResourceAccessPolicy, "new-policy", core.LifecyclePresent},
	} {
		resource, err := s.LoadResource(ctx, "mock", check.typeID, check.externalID)
		if err != nil {
			t.Fatal(err)
		}
		if resource.Lifecycle != check.lifecycle {
			t.Fatalf("%s lifecycle = %q, want %q", check.externalID, resource.Lifecycle, check.lifecycle)
		}
	}
}

// A cross-connection resource conflict must roll back the entire commit:
// no event, no credential, no resource row may survive.
func TestCommitStepResultConflictRollsBack(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Seed the resource under a different connection.
	other := &core.ProviderResource{
		ConnectionID: "other-conn",
		ProviderID:   "mock",
		Type:         core.ResourceTunnel,
		ExternalID:   "tun-dup",
		Ownership:    core.OwnershipManaged,
	}
	if err := s.SaveResource(ctx, other); err != nil {
		t.Fatalf("seed resource: %v", err)
	}
	seedOperation(t, s, "op-2", "plan-2", "test-conn-1")

	req := core.StepCommitRequest{
		OperationID:  "op-2",
		ConnectionID: "test-conn-1",
		Provider:     "mock",
		Step:         core.PlanStep{ID: "step-2", Kind: core.StepCreateTunnel, Summary: "Create tunnel"},
		Result: core.StepResult{
			StepID:    "step-2",
			Succeeded: true,
			Resources: []core.ProviderResource{
				{
					ConnectionID: "test-conn-1",
					ProviderID:   "mock",
					Type:         core.ResourceTunnel,
					ExternalID:   "tun-dup",
					Ownership:    core.OwnershipManaged,
				},
			},
			CredentialMutations: []core.CredentialMutation{
				{TunnelID: "tun-dup", Secret: []byte("secret")},
			},
		},
	}
	err := s.CommitStepResult(ctx, req)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	var conflict *ResourceAssociationConflict
	if !asConflict(err, &conflict) {
		t.Fatalf("expected ResourceAssociationConflict, got %v", err)
	}

	// Nothing from the failed transaction may persist.
	var count int
	if err := s.DB().QueryRow(
		"SELECT COUNT(*) FROM operation_events WHERE operation_id = 'op-2'").Scan(&count); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected rollback of step event, found %d rows", count)
	}
	if durable, durableErr := s.GetDurableEventsSince(ctx, 0, 10); durableErr != nil || len(durable) != 1 || durable[0].Event.Type != "operation.created" {
		t.Fatalf("unified journal must roll back with the step: events=%#v err=%v", durable, durableErr)
	}
	if tid, tok, err := s.LoadTunnelCredential(ctx, "test-conn-1"); err != nil || tid != "" || tok != "" {
		t.Fatalf("expected no credential after rollback, got %q/%q err=%v", tid, tok, err)
	}
	// The seeded resource must retain its original owner.
	res, err := s.LoadResource(ctx, "mock", core.ResourceTunnel, "tun-dup")
	if err != nil {
		t.Fatalf("LoadResource: %v", err)
	}
	if res.ConnectionID != "other-conn" {
		t.Fatalf("ownership must not transfer; got %s", res.ConnectionID)
	}
}

// Terminal open/close commits must return exact committed values;
// repair must not change the profile revision.
func TestRuntimeCommitResults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	rt := testRuntime()
	if err := s.SaveRuntime(ctx, rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}

	openRes, err := s.CommitOpenSuccess(ctx, p.ID, "op-open", rt.LastTransition)
	if err != nil {
		t.Fatalf("CommitOpenSuccess: %v", err)
	}
	if openRes.ProfileRevision != p.Revision+1 {
		t.Fatalf("expected revision %d, got %d", p.Revision+1, openRes.ProfileRevision)
	}
	if openRes.DesiredState != core.DesiredOpen || openRes.RuntimeState != core.RuntimeOpen {
		t.Fatalf("unexpected open commit result: %+v", openRes)
	}

	repairRes, err := s.CommitRepairSuccess(ctx, p.ID, "op-repair")
	if err != nil {
		t.Fatalf("CommitRepairSuccess: %v", err)
	}
	if repairRes.ProfileRevision != openRes.ProfileRevision {
		t.Fatalf("repair must not change revision: %d -> %d", openRes.ProfileRevision, repairRes.ProfileRevision)
	}

	closeRes, err := s.CommitCloseSuccess(ctx, p.ID, "op-close")
	if err != nil {
		t.Fatalf("CommitCloseSuccess: %v", err)
	}
	if closeRes.ProfileRevision != repairRes.ProfileRevision+1 {
		t.Fatalf("close must bump revision: %d -> %d", repairRes.ProfileRevision, closeRes.ProfileRevision)
	}
	if closeRes.DesiredState != core.DesiredClosed || closeRes.RuntimeState != core.RuntimeClosed {
		t.Fatalf("unexpected close commit result: %+v", closeRes)
	}
}

// A tampered modern blob must fail decryption rather than silently
// downgrading to the legacy machine-ID key.
func TestDecryptCredentialNoSilentDowngrade(t *testing.T) {
	s := newTestStore(t)

	blob, err := encryptCredential(s.secretStore, []byte("topsecret"), "conn:tun")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Wrong context must fail, not fall back.
	if _, err := decryptCredential(s.secretStore, blob, "conn:other"); err == nil {
		t.Fatal("expected context-mismatch failure, got silent success")
	}
	// Correct context round-trips.
	got, err := decryptCredential(s.secretStore, blob, "conn:tun")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "topsecret" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func asConflict(err error, target **ResourceAssociationConflict) bool {
	c, ok := err.(*ResourceAssociationConflict)
	if ok {
		*target = c
	}
	return ok
}

func seedOperation(t *testing.T, s *Store, opID core.OperationID, planID core.PlanID, connID core.ConnectionID) {
	t.Helper()
	ctx := context.Background()
	plan := &core.OperationPlan{
		ID:              planID,
		ConnectionID:    connID,
		ProfileRevision: 1,
		Provider:        "mock",
		Intent:          core.IntentOpen,
		Steps: []core.PlanStep{
			{ID: "seed-step", Kind: core.StepCreateTunnel, Summary: "seed"},
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := s.SavePlan(ctx, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if err := s.SaveOperation(ctx, opID, planID, connID, "running", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
}

func TestGetOperationAndEventsAreDurable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	opID := core.OperationID("operation-query")
	seedOperation(t, s, opID, "plan-query", "connection-query")
	if err := s.AppendOperationEvent(ctx, core.Event{
		Type: core.EventOperationStepStarted,
		Data: core.OperationEvent{StepID: "seed-step", Stage: core.StageStarted, Message: "seed"},
	}, opID); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := s.CompleteOperation(ctx, opID, "completed"); err != nil {
		t.Fatalf("complete operation: %v", err)
	}
	op, err := s.GetOperation(ctx, opID)
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if op.State != "completed" || op.CompletedAt == "" {
		t.Fatalf("unexpected durable operation: %+v", op)
	}
	events, err := s.GetOperationEvents(ctx, opID)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	if len(events) != 1 || events[0].Sequence <= 0 || events[0].StepID != "seed-step" {
		t.Fatalf("unexpected durable events: %+v", events)
	}
	unified, err := s.GetDurableEventsForOperation(ctx, opID)
	if err != nil {
		t.Fatalf("get unified operation events: %v", err)
	}
	if len(unified) != 3 || unified[1].Event.Sequence <= 0 || unified[1].Event.Type != core.EventOperationStepStarted || unified[1].ConnectionID != "connection-query" || unified[2].Event.Type != core.EventOperationCompleted {
		t.Fatalf("unexpected unified operation events: %#v", unified)
	}
}

func TestRecordCleanupItemSurvivesMissingResourceInventory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	item := CleanupItem{
		OperationID: "cleanup-operation", ConnectionID: "cleanup-connection", ProviderID: "mock",
		ResourceType: core.ResourceTunnel, ExternalID: "external-tunnel", State: "outcome_unknown", LastError: "commit failed",
	}
	if err := s.RecordCleanupItem(ctx, item); err != nil {
		t.Fatalf("record cleanup item: %v", err)
	}
	item.State = "compensation_failed"
	if err := s.RecordCleanupItem(ctx, item); err != nil {
		t.Fatalf("refresh cleanup item: %v", err)
	}
	var count int
	var state string
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*), cleanup_state FROM resource_cleanup_items WHERE external_id = ?`, item.ExternalID).Scan(&count, &state); err != nil {
		t.Fatalf("query cleanup item: %v", err)
	}
	if count != 1 || state != "compensation_failed" {
		t.Fatalf("unexpected cleanup ledger state: count=%d state=%q", count, state)
	}
}

func TestCompensationLedgerTransitions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	opID := core.OperationID("compensation-operation")
	seedOperation(t, s, opID, "compensation-plan", "compensation-connection")
	step := core.PlanStep{ID: "rollback-tunnel", Kind: core.StepDeleteTunnel, Summary: "Rollback tunnel"}
	if err := s.BeginCompensation(ctx, opID, "compensation-connection", step); err != nil {
		t.Fatalf("begin compensation: %v", err)
	}
	results, err := s.GetStepResults(ctx, opID)
	if err != nil || len(results) != 1 || results[0].Status != string(StepCompensationPending) {
		t.Fatalf("unexpected pending ledger: results=%+v err=%v", results, err)
	}
	if err := s.CommitCompensationOutcome(ctx, opID, "compensation-connection", step, core.StepResult{StepID: step.ID, Succeeded: true}); err != nil {
		t.Fatalf("commit compensation: %v", err)
	}
	results, err = s.GetStepResults(ctx, opID)
	if err != nil || results[0].Status != string(StepCompensated) {
		t.Fatalf("unexpected terminal ledger: results=%+v err=%v", results, err)
	}
}

// --------------- CAS runtime revision tests ---------------

func TestSaveRuntimeCASBumpsRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	rt := testRuntime()
	if err := s.SaveRuntime(ctx, rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}

	// Load the runtime to get the initial revision.
	loaded, err := s.LoadRuntime(ctx, p.ID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if loaded.RuntimeRevision != 0 {
		t.Fatalf("expected initial revision 0, got %d", loaded.RuntimeRevision)
	}

	// SaveRuntimeCAS bumps the persisted revision and mirrors in struct.
	if err := s.SaveRuntimeCAS(ctx, loaded); err != nil {
		t.Fatalf("SaveRuntimeCAS: %v", err)
	}
	if loaded.RuntimeRevision != 1 {
		t.Fatalf("expected mirrored revision 1, got %d", loaded.RuntimeRevision)
	}

	// Verify persisted revision is 1.
	loaded2, err := s.LoadRuntime(ctx, p.ID)
	if err != nil {
		t.Fatalf("LoadRuntime after CAS: %v", err)
	}
	if loaded2.RuntimeRevision != 1 {
		t.Fatalf("expected persisted revision 1, got %d", loaded2.RuntimeRevision)
	}
}

func TestSaveRuntimeCASRejectsStaleRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	rt := testRuntime()
	if err := s.SaveRuntime(ctx, rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}

	// Load at revision 0.
	rt1, err := s.LoadRuntime(ctx, p.ID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}

	// First CAS succeeds: 0 → 1.
	if err := s.SaveRuntimeCAS(ctx, rt1); err != nil {
		t.Fatalf("SaveRuntimeCAS first: %v", err)
	}

	// rt1 still has revision 1 (mirrored). Persist a second change with same revision.
	rt1.Endpoint.PublicAddress = "https://changed.example.com"
	if err := s.SaveRuntimeCAS(ctx, rt1); err != nil {
		t.Fatalf("SaveRuntimeCAS second: %v", err)
	}
	// Now persisted is 2, rt1 has 2.

	// Load rt3 — also at revision 2. Make rt1 advance first, then rt3 will be stale.
	rt3, err := s.LoadRuntime(ctx, p.ID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	// rt3 has revision 2, rt1 also has 2, persisted is 2.

	// Make rt1 change it (succeeds: 2→3).
	rt1.Endpoint.PublicAddress = "https://rt1-changed.example.com"
	if err := s.SaveRuntimeCAS(ctx, rt1); err != nil {
		t.Fatalf("SaveRuntimeCAS rt1: %v", err)
	}
	// Persisted is 3, rt1 has 3.

	// rt3 still has revision 2. CAS should fail.
	rt3.Endpoint.PublicAddress = "https://rt3-stale.example.com"
	err = s.SaveRuntimeCAS(ctx, rt3)
	if !errors.Is(err, ErrRuntimeRevisionMismatch) {
		t.Fatalf("expected ErrRuntimeRevisionMismatch, got %v", err)
	}

	// Persisted row should be unchanged (still 3).
	after, err := s.LoadRuntime(ctx, p.ID)
	if err != nil {
		t.Fatalf("LoadRuntime after stale CAS: %v", err)
	}
	if after.RuntimeRevision != 3 {
		t.Fatalf("expected persisted revision 3 unchanged, got %d", after.RuntimeRevision)
	}
	// The value should be rt1's, not rt3's.
	if after.Endpoint.PublicAddress != "https://rt1-changed.example.com" {
		t.Fatalf("persisted row was overwritten: %q", after.Endpoint.PublicAddress)
	}
}

func TestCommitOpenSuccessBumpsRuntimeRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	rt := testRuntime()
	if err := s.SaveRuntime(ctx, rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}

	// Manually set runtime_revision to 0 (migration 17 back-fills 0).
	_, err := s.db.Exec("UPDATE connection_runtime SET runtime_revision = 0 WHERE connection_id = ?", p.ID)
	if err != nil {
		t.Fatalf("set initial revision: %v", err)
	}

	res, err := s.CommitOpenSuccess(ctx, p.ID, "op-open", time.Now().UTC())
	if err != nil {
		t.Fatalf("CommitOpenSuccess: %v", err)
	}
	if res.RuntimeState != core.RuntimeOpen {
		t.Fatalf("unexpected runtime state: %+v", res)
	}

	var rev int64
	err = s.db.QueryRow("SELECT runtime_revision FROM connection_runtime WHERE connection_id = ?", p.ID).Scan(&rev)
	if err != nil {
		t.Fatalf("query revision: %v", err)
	}
	if rev != 1 {
		t.Fatalf("expected runtime_revision 1 after open, got %d", rev)
	}
}

func TestCommitCloseSuccessBumpsRuntimeRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	rt := testRuntime()
	if err := s.SaveRuntime(ctx, rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}
	// Open first (bumps to 1).
	_, err := s.db.Exec("UPDATE connection_runtime SET runtime_revision = 0 WHERE connection_id = ?", p.ID)
	if err != nil {
		t.Fatalf("reset revision: %v", err)
	}
	s.CommitOpenSuccess(ctx, p.ID, "op-open", time.Now().UTC())
	// Now revision is 1.

	res, err := s.CommitCloseSuccess(ctx, p.ID, "op-close")
	if err != nil {
		t.Fatalf("CommitCloseSuccess: %v", err)
	}
	if res.RuntimeState != core.RuntimeClosed {
		t.Fatalf("unexpected runtime state: %+v", res)
	}

	var rev int64
	err = s.db.QueryRow("SELECT runtime_revision FROM connection_runtime WHERE connection_id = ?", p.ID).Scan(&rev)
	if err != nil {
		t.Fatalf("query revision: %v", err)
	}
	if rev != 2 {
		t.Fatalf("expected runtime_revision 2 after close, got %d", rev)
	}
}

func TestCommitRepairSuccessBumpsRuntimeRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	rt := testRuntime()
	if err := s.SaveRuntime(ctx, rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}
	// Reset and open first.
	_, err := s.db.Exec("UPDATE connection_runtime SET runtime_revision = 0 WHERE connection_id = ?", p.ID)
	if err != nil {
		t.Fatalf("reset revision: %v", err)
	}
	s.CommitOpenSuccess(ctx, p.ID, "op-open", time.Now().UTC())

	res, err := s.CommitRepairSuccess(ctx, p.ID, "op-repair")
	if err != nil {
		t.Fatalf("CommitRepairSuccess: %v", err)
	}
	if res.RuntimeState != core.RuntimeOpen {
		t.Fatalf("unexpected runtime state: %+v", res)
	}

	var rev int64
	err = s.db.QueryRow("SELECT runtime_revision FROM connection_runtime WHERE connection_id = ?", p.ID).Scan(&rev)
	if err != nil {
		t.Fatalf("query revision: %v", err)
	}
	if rev != 2 {
		t.Fatalf("expected runtime_revision 2 after repair, got %d", rev)
	}
}

func TestCommitEditSuccessBumpsRuntimeRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	rt := testRuntime()
	if err := s.SaveRuntime(ctx, rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}
	// Create a running operation to be cleared by CommitEditSuccess.
	opID := core.OperationID("op-edit")
	// Save a plan first (foreign key constraint).
	if err := s.SavePlan(ctx, &core.OperationPlan{
		ID:           "plan-edit",
		ConnectionID: p.ID,
		Provider:     p.Driver.ProviderID,
		Intent:       core.IntentEdit,
	}); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if err := s.SaveOperation(ctx, opID, "plan-edit", p.ID, "running", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
	// Reset revision to a known value.
	_, err := s.db.Exec("UPDATE connection_runtime SET runtime_revision = 0 WHERE connection_id = ?", p.ID)
	if err != nil {
		t.Fatalf("reset revision: %v", err)
	}

	res, err := s.CommitEditSuccess(ctx, p.ID, opID)
	if err != nil {
		t.Fatalf("CommitEditSuccess: %v", err)
	}
	// Desired state preserved.
	if res.DesiredState != p.Desired {
		t.Fatalf("desired state = %q, want %q", res.DesiredState, p.Desired)
	}
	// Runtime state preserved.
	if res.RuntimeState != rt.State {
		t.Fatalf("runtime state = %q, want %q", res.RuntimeState, rt.State)
	}

	// runtime_revision bumped.
	var rev int64
	err = s.db.QueryRow("SELECT runtime_revision FROM connection_runtime WHERE connection_id = ?", p.ID).Scan(&rev)
	if err != nil {
		t.Fatalf("query revision: %v", err)
	}
	if rev != 1 {
		t.Fatalf("expected runtime_revision 1 after edit, got %d", rev)
	}

	// Active operation cleared.
	var activeOpID *string
	err = s.db.QueryRow("SELECT active_operation_id FROM connection_runtime WHERE connection_id = ?", p.ID).Scan(&activeOpID)
	if err != nil {
		t.Fatalf("query active op: %v", err)
	}
	if activeOpID != nil {
		t.Fatalf("expected active_operation_id NULL, got %q", *activeOpID)
	}

	// Operation marked completed.
	var opState string
	err = s.db.QueryRow("SELECT state FROM operations WHERE id = ?", opID).Scan(&opState)
	if err != nil {
		t.Fatalf("query op state: %v", err)
	}
	if opState != "completed" {
		t.Fatalf("expected operation state 'completed', got %q", opState)
	}
}

func TestMigration17AddsRuntimeRevision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test_m17.db")

	// Open a fresh store — migrations 1..16 run first, then 17 adds runtime_revision.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Save a profile + runtime so the table exists.
	ctx := context.Background()
	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	rt := testRuntime()
	rt.ConnectionID = p.ID
	if err := s.SaveRuntime(ctx, rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}

	// Verify column exists via PRAGMA table_info.
	rows, err := s.db.Query("PRAGMA table_info(connection_runtime)")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	found := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan column info: %v", err)
		}
		if name == "runtime_revision" {
			found = true
			if ctype != "INTEGER" {
				t.Fatalf("expected type INTEGER, got %q", ctype)
			}
		}
	}
	rows.Close()
	if !found {
		t.Fatal("runtime_revision column not found in connection_runtime")
	}

	// Verify default back-fills to 0.
	var rev int64
	err = s.db.QueryRow("SELECT runtime_revision FROM connection_runtime WHERE connection_id = ?", p.ID).Scan(&rev)
	if err != nil && err != sql.ErrNoRows {
		t.Fatalf("query revision: %v", err)
	}
	if rev != 0 {
		t.Fatalf("expected back-filled revision 0, got %d", rev)
	}
}
