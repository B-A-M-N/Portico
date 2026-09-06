package supervisor

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/origin"
)

// Gateway invariant verticals: repair and edit/reopen paths must preserve the
// gateway boundary exactly as PlanOpen does.
//
// The normal open path pins the symbolic core.GatewayTargetRef for a
// gateway-required profile. The shared reopen helper used to omit gateway
// targeting entirely, so an OpenAI-compatible connection whose connector or
// gateway died would replan pointing at the RAW origin — silently discarding
// authentication and SSE passthrough. These tests hold every replan path to
// the same rule.

// TestRepairPlanNeverTargetsRawOrigin drives PlanRepair for an OpenAI-
// compatible connection with a dead connector and asserts the produced plan
// carries the symbolic gateway reference, never the raw origin.
func TestRepairPlanNeverTargetsRawOrigin(t *testing.T) {
	provider := &targetCapturingProvider{}
	sup := testSupervisorWithController(t, provider)
	sup.controller.SetOriginManager(origin.NewManager())

	profile := gatewayVerticalProfile()
	profile.Driver.ProviderID = provider.Identity().ID
	ctx := context.Background()
	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	// A runtime whose connector is down: exactly the state repair exists for.
	rt := &core.ConnectionRuntime{ConnectionID: profile.ID, State: core.RuntimeDegraded}
	rt.Connector.Status = core.ConnectorStatusStopped
	sup.controller.RestoreRuntime(rt)

	plan, err := sup.controller.PlanRepair(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanRepair: %v", err)
	}
	assertNoRawOriginInSteps(t, profile.ID, plan.Steps)
}

// TestEditReopenPlanNeverTargetsRawOrigin drives the shared reopen helper via
// an edit of an open OpenAI-compatible connection and asserts its reopen steps
// keep the gateway reference.
func TestEditReopenPlanNeverTargetsRawOrigin(t *testing.T) {
	provider := &targetCapturingProvider{}
	sup := testSupervisorWithController(t, provider)
	sup.controller.SetOriginManager(origin.NewManager())

	profile := gatewayVerticalProfile()
	profile.Driver.ProviderID = provider.Identity().ID
	profile.Revision = 1
	ctx := context.Background()
	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	// An open runtime: the edit path reuses open steps when reopening.
	rt := &core.ConnectionRuntime{ConnectionID: profile.ID, State: core.RuntimeOpen}
	rt.Connector.Status = core.ConnectorStatusRunning
	sup.controller.RestoreRuntime(rt)

	proposed := profile.DeepCopy()
	proposed.Name = "llama-box-renamed"
	proposed.Revision++

	editPlan, _, err := sup.controller.PlanEdit(ctx, profile.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	assertNoRawOriginInSteps(t, profile.ID, editPlan.Steps)
}

// assertNoRawOriginInSteps fails when any step carries the raw origin URL in
// an origin_url parameter and fails when no step carries the symbolic gateway
// reference at all — both halves of the invariant.
func assertNoRawOriginInSteps(t *testing.T, connID core.ConnectionID, steps []core.PlanStep) {
	t.Helper()
	rawOrigin := "http://127.0.0.1:8080"
	hasGatewayRef := false
	for _, step := range steps {
		for key, v := range step.Technical.Parameters {
			if v == rawOrigin && strings.Contains(key, "origin") {
				t.Fatalf("%s: step %q carries the RAW origin %q; that bypasses the auth/SSE boundary",
					connID, step.ID, rawOrigin)
			}
			if v == core.GatewayTargetRef {
				hasGatewayRef = true
			}
		}
	}
	if !hasGatewayRef {
		t.Fatalf("%s: no step carries the symbolic gateway target reference; %+v", connID, steps)
	}
}

// TestPlanNeedsGatewayDetectsSymbolicReference pins the apply-side gate: any
// plan whose steps carry the symbolic reference requires gateway startup,
// regardless of intent. This is what makes repair/edit gateways start.
func TestPlanNeedsGatewayDetectsSymbolicReference(t *testing.T) {
	withRef := &core.OperationPlan{Intent: core.IntentRepair, Steps: []core.PlanStep{{
		ID:        "restart",
		Kind:      core.StepStartConnector,
		Technical: core.TechnicalOperation{Parameters: map[string]string{"origin_url": core.GatewayTargetRef}},
	}}}
	if !planNeedsGateway(withRef) {
		t.Fatal("a repair plan carrying the symbolic gateway ref was not detected as needing a gateway")
	}

	plain := &core.OperationPlan{Intent: core.IntentOpen, Steps: []core.PlanStep{{
		ID:        "quick-start",
		Kind:      core.StepStartConnector,
		Technical: core.TechnicalOperation{Parameters: map[string]string{"mode": "quick", "origin_url": "http://127.0.0.1:8080"}},
	}}}
	if planNeedsGateway(plain) {
		t.Fatal("an open plan without the symbolic ref was reported as needing a gateway")
	}

	compensation := &core.OperationPlan{Intent: core.IntentDelete, Steps: []core.PlanStep{{
		ID:           "stop",
		Kind:         core.StepStopConnector,
		Compensation: &core.CompensationStep{Technical: core.TechnicalOperation{Parameters: map[string]string{"origin_url": core.GatewayTargetRef}}},
	}}}
	if !planNeedsGateway(compensation) {
		t.Fatal("a plan whose compensation carries the symbolic ref was not detected")
	}
}
