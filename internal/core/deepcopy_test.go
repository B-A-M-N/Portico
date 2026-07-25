package core

import (
	"testing"
	"time"
)

// Mutating a deep copy must never affect the original aggregate.

func TestOperationPlanDeepCopyIsolation(t *testing.T) {
	comp := &CompensationStep{
		ID:   "comp-1",
		Kind: StepDeleteTunnel,
		Technical: TechnicalOperation{
			Parameters: map[string]string{"key": "orig"},
		},
	}
	plan := &OperationPlan{
		ID:           "plan-1",
		ConnectionID: "conn-1",
		Intent:       IntentOpen,
		Steps: []PlanStep{
			{
				ID:   "step-1",
				Kind: StepCreateTunnel,
				Technical: TechnicalOperation{
					Parameters: map[string]string{"name": "orig"},
				},
				Compensation: comp,
			},
		},
		Preconditions: []Precondition{{Type: "check", Checks: []string{"orig"}}},
	}

	cp := plan.DeepCopy()
	cp.Steps[0].Technical.Parameters["name"] = "mutated"
	cp.Steps[0].Compensation.Technical.Parameters["key"] = "mutated"
	cp.Preconditions[0].Checks[0] = "mutated"

	if plan.Steps[0].Technical.Parameters["name"] != "orig" {
		t.Error("step parameters shared between copies")
	}
	if plan.Steps[0].Compensation.Technical.Parameters["key"] != "orig" {
		t.Error("compensation parameters shared between copies")
	}
	if plan.Steps[0].Compensation == cp.Steps[0].Compensation {
		t.Error("compensation pointer shared between copies")
	}
	if plan.Preconditions[0].Checks[0] != "orig" {
		t.Error("preconditions shared between copies")
	}
}

func TestConnectionRuntimeDeepCopyIsolation(t *testing.T) {
	rt := &ConnectionRuntime{
		ConnectionID: "conn-1",
		State:        RuntimeOpen,
		Provider: ProviderRuntime{
			ProviderID: "cloudflare",
			Resources: []ProviderResource{
				{
					Type:       ResourceTunnel,
					ExternalID: "tun-1",
					Metadata:   map[string]string{"k": "orig"},
				},
			},
			AccountInfo: map[string]string{"account": "orig"},
		},
		Diagnostics: []DiagnosticFinding{
			{
				ID:       "f-1",
				Summary:  "orig",
				Evidence: []Evidence{{Message: "orig"}},
			},
		},
	}

	cp := rt.DeepCopy()
	cp.Provider.Resources[0].Metadata["k"] = "mutated"
	cp.Provider.Resources[0].ExternalID = "mutated"
	cp.Provider.AccountInfo["account"] = "mutated"
	cp.Diagnostics[0].Evidence[0].Message = "mutated"

	if rt.Provider.Resources[0].Metadata["k"] != "orig" {
		t.Error("resource metadata shared between copies")
	}
	if rt.Provider.Resources[0].ExternalID != "tun-1" {
		t.Error("resource slice shared between copies")
	}
	if rt.Provider.AccountInfo["account"] != "orig" {
		t.Error("account info shared between copies")
	}
	if rt.Diagnostics[0].Evidence[0].Message != "orig" {
		t.Error("diagnostic evidence shared between copies")
	}
}

func TestProviderResourceCloneIsolation(t *testing.T) {
	res := ProviderResource{
		Type:       ResourceAccessPolicy,
		ExternalID: "pol-1",
		Metadata:   map[string]string{"app_id": "app-1"},
	}
	cp := res.Clone()
	cp.Metadata["app_id"] = "mutated"
	if res.Metadata["app_id"] != "app-1" {
		t.Error("metadata shared between clone and original")
	}
}

func TestDiagnosticFindingCloneIsolation(t *testing.T) {
	now := time.Now()
	f := DiagnosticFinding{
		ID:         "f-1",
		Summary:    "orig",
		Evidence:   []Evidence{{Message: "orig"}},
		ObservedAt: now,
		RepairOptions: []RepairOption{
			{Summary: "orig", Steps: []PlanStep{{ID: "s-1"}}},
		},
	}
	cp := f.Clone()
	cp.Evidence[0].Message = "mutated"
	cp.RepairOptions[0].Summary = "mutated"

	if f.Evidence[0].Message != "orig" {
		t.Error("evidence shared between clone and original")
	}
	if f.RepairOptions[0].Summary != "orig" {
		t.Error("repair options shared between clone and original")
	}
}
