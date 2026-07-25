package store

import (
	"context"
	"testing"
	"time"

	"github.com/paoloanzn/portico/internal/core"
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
				{TunnelID: "tun-123", Token: "secret-token"},
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
				{TunnelID: "tun-dup", Token: "secret"},
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

	blob, err := encryptCredential(s.secretStore, "topsecret", "conn:tun")
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
