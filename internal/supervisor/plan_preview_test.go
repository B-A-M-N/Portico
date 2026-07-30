package supervisor

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

func previewProfile(mode core.ExposureMode, protection core.ProtectionSpec) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID:   "conn-1",
		Name: "demo",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "127.0.0.1:3000"},
				},
				Exposure:   core.ExposureSpec{Mode: mode, RequestedAddress: "demo.example.com"},
				Protection: protection,
			},
		},
		Driver: core.DriverSelection{ProviderID: "cloudflare"},
	}
}

// TestPlanPreviewStatesOutcomeAndConsequences pins audit item 16. The preview
// previously listed step summaries only, leaving the user to work out what the
// plan would achieve and what it would change.
func TestPlanPreviewStatesOutcomeAndConsequences(t *testing.T) {
	plan := &core.OperationPlan{
		ID: "plan-1", ConnectionID: "conn-1", Intent: core.IntentOpen, Provider: "cloudflare",
		Expected: core.ExpectedOutcome{State: core.RuntimeOpen, PublicAddress: "demo.example.com"},
		Steps: []core.PlanStep{
			{ID: "s1", Kind: core.StepCreateTunnel, Summary: "Create tunnel"},
			{ID: "s2", Kind: core.StepCreateDNSRecord, Summary: "Create DNS"},
			{ID: "s3", Kind: core.StepCreateAccessApp, Summary: "Create Access"},
			{ID: "s4", Kind: core.StepStartConnector, Summary: "Start connector"},
		},
	}
	profile := previewProfile(core.ExposurePermanent, core.ProtectionSpec{
		Kind: core.ProtectionEmailOTP, AllowedEmails: []string{"alice@example.com"},
	})

	dto := planToDTO(plan, profile)

	if !strings.Contains(dto.Outcome, "127.0.0.1:3000") || !strings.Contains(dto.Outcome, "demo.example.com") {
		t.Fatalf("outcome does not state the origin and the address: %q", dto.Outcome)
	}
	if !strings.Contains(dto.Access, "alice@example.com") {
		t.Fatalf("access does not name who can reach it: %q", dto.Access)
	}
	local := strings.Join(dto.LocalChanges, " | ")
	if !strings.Contains(local, "connector process") {
		t.Fatalf("local changes = %q", local)
	}
	provider := strings.Join(dto.ProviderChanges, " | ")
	for _, want := range []string{"managed tunnel", "DNS record", "access policy"} {
		if !strings.Contains(provider, want) {
			t.Fatalf("provider changes omit %q: %q", want, provider)
		}
	}
	rev := strings.Join(dto.Reversibility, " | ")
	if !strings.Contains(rev, "Closing stops the connector") {
		t.Fatalf("reversibility = %q", rev)
	}
	// The exact technical plan must survive alongside the summaries.
	if len(dto.Steps) != 4 || dto.Steps[0].Kind != string(core.StepCreateTunnel) {
		t.Fatalf("technical steps were lost: %#v", dto.Steps)
	}
}

// TestPlanPreviewDescribesUnrestrictedAccessPlainly ensures an unprotected
// public exposure says so.
func TestPlanPreviewDescribesUnrestrictedAccessPlainly(t *testing.T) {
	plan := &core.OperationPlan{
		ID: "plan-1", ConnectionID: "conn-1", Intent: core.IntentOpen,
		Steps: []core.PlanStep{{ID: "s1", Kind: core.StepStartConnector}},
	}
	dto := planToDTO(plan, previewProfile(core.ExposureTemporary, core.ProtectionSpec{Kind: core.ProtectionNone}))
	if !strings.Contains(dto.Access, "Anyone with this temporary link") {
		t.Fatalf("access = %q", dto.Access)
	}
	if !strings.Contains(strings.Join(dto.ProviderChanges, " "), "No provider resources") {
		t.Fatalf("provider changes = %q", dto.ProviderChanges)
	}
}

// TestDeletePreviewStatesIrreversibility ensures a destructive plan says so.
func TestDeletePreviewStatesIrreversibility(t *testing.T) {
	plan := &core.OperationPlan{
		ID: "plan-1", ConnectionID: "conn-1", Intent: core.IntentDelete,
		Steps: []core.PlanStep{
			{ID: "s1", Kind: core.StepDeleteTunnel, Summary: "Delete tunnel", Destructive: true, Irreversible: true},
		},
	}
	dto := planToDTO(plan, previewProfile(core.ExposurePermanent, core.ProtectionSpec{Kind: core.ProtectionNone}))
	rev := strings.Join(dto.Reversibility, " | ")
	if !strings.Contains(rev, "cannot be undone") {
		t.Fatalf("delete preview does not state irreversibility: %q", rev)
	}
	if !strings.Contains(strings.Join(dto.ProviderChanges, " "), "Delete the managed tunnel") {
		t.Fatalf("provider changes = %q", dto.ProviderChanges)
	}
}

// TestPlanPreviewWithoutProfileInventsNothing ensures a missing profile yields
// fewer statements rather than fabricated ones.
func TestPlanPreviewWithoutProfileInventsNothing(t *testing.T) {
	plan := &core.OperationPlan{
		ID: "plan-1", ConnectionID: "conn-1", Intent: core.IntentOpen,
		Steps: []core.PlanStep{{ID: "s1", Kind: core.StepStartConnector}},
	}
	dto := planToDTO(plan, nil)
	if dto.Access != "" {
		t.Fatalf("access was invented without a profile: %q", dto.Access)
	}
	if dto == nil || dto.ID != "plan-1" {
		t.Fatal("plan identity was lost")
	}
}
