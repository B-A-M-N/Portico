package core

import (
	"testing"
	"time"
)

func TestComputeFingerprint_StepOrderChangesFingerprint(t *testing.T) {
	now := time.Now()
	plan := OperationPlan{
		ID:              "test",
		ConnectionID:    "conn-1",
		ProfileRevision: 1,
		Provider:        ProviderID("mock"),
		Intent:          IntentOpen,
		Steps: []PlanStep{
			{ID: "step-a", Summary: "Create tunnel", Kind: StepCreateTunnel},
			{ID: "step-b", Summary: "Create DNS", Kind: StepCreateDNSRecord},
		},
		CreatedAt: now,
	}

	// Compute fingerprint for original order
	if err := plan.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	fp1 := plan.Fingerprint

	// Swap steps
	plan.Steps[0], plan.Steps[1] = plan.Steps[1], plan.Steps[0]
	if err := plan.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	fp2 := plan.Fingerprint

	if fp1 == fp2 {
		t.Fatal("fingerprint did not change after swapping steps — execution order is not canonicalized")
	}
}

func TestComputeFingerprint_MapOrderDoesNotChangeFingerprint(t *testing.T) {
	now := time.Now()
	plan := OperationPlan{
		ID:              "test",
		ConnectionID:    "conn-1",
		ProfileRevision: 1,
		Provider:        ProviderID("mock"),
		Intent:          IntentOpen,
		Steps: []PlanStep{
			{
				ID:      "step-1",
				Summary: "Create tunnel",
				Kind:    StepCreateTunnel,
				Technical: TechnicalOperation{
					Parameters: map[string]string{
						"name":    "my-tunnel",
						"region":  "us-east",
						"visible": "true",
					},
				},
			},
		},
		CreatedAt: now,
	}

	if err := plan.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	fp1 := plan.Fingerprint

	// Maps in Go don't guarantee order — changing insertion order shouldn't
	// change fingerprint. json.Marshal sorts map keys, so this should be stable.
	plan.Steps[0].Technical.Parameters = map[string]string{
		"visible": "true",
		"region":  "us-east",
		"name":    "my-tunnel",
	}
	if err := plan.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	fp2 := plan.Fingerprint

	if fp1 != fp2 {
		t.Fatal("fingerprint changed after map reordering — maps should be canonicalized by JSON encoding")
	}
}

func TestComputeFingerprint_ExcludesIDAndFingerprint(t *testing.T) {
	now := time.Now()

	base := OperationPlan{
		ID:              "plan-a",
		ConnectionID:    "conn-1",
		ProfileRevision: 1,
		Provider:        ProviderID("mock"),
		Intent:          IntentOpen,
		Steps:           []PlanStep{{ID: "step-1", Summary: "test"}},
		CreatedAt:       now,
	}

	if err := base.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}

	// Same plan, different ID and fingerprint
	clone := base
	clone.ID = "plan-b"
	clone.Fingerprint = ""
	if err := clone.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}

	if base.Fingerprint != clone.Fingerprint {
		t.Fatal("fingerprints differ despite same content (ID should be excluded)")
	}
}

func TestVerifyFingerprint(t *testing.T) {
	now := time.Now()
	plan := OperationPlan{
		ID:              "test",
		ConnectionID:    "conn-1",
		ProfileRevision: 1,
		Provider:        ProviderID("mock"),
		Intent:          IntentOpen,
		Steps:           []PlanStep{{ID: "step-1", Summary: "test"}},
		CreatedAt:       now,
	}

	if err := plan.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}

	if err := plan.VerifyFingerprint(); err != nil {
		t.Fatalf("VerifyFingerprint for correct fingerprint: %v", err)
	}

	// Tamper with a field
	plan.ProfileRevision = 2
	if err := plan.VerifyFingerprint(); err == nil {
		t.Fatal("VerifyFingerprint should fail after tampering")
	}
}
