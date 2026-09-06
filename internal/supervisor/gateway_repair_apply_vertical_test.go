package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/origin"
)

// Apply-time gateway verticals for the repair path.
//
// Planning the right steps is half the invariant; these prove the apply side:
// a repair plan carrying the symbolic gateway reference gets a gateway started
// for it, the reference resolves to that newly started gateway, and a failed
// repair does not leave the gateway behind.

// TestRepairWithDeadGatewayStartsGatewayBeforeConnector drives the full apply
// route with a repair plan whose connector step carries the symbolic
// reference. The provider captures the resolved origin_url at execution time;
// it must be the live gateway endpoint, never the raw origin and never the
// unresolved symbol.
func TestRepairWithDeadGatewayStartsGatewayBeforeConnector(t *testing.T) {
	provider := &targetCapturingProvider{}
	sup := testSupervisorWithController(t, provider)
	sup.controller.SetOriginManager(origin.NewManager())
	// Resolve against whatever the supervisor's gateway manager runs, exactly
	// as production wires it.
	sup.controller.SetGatewayEndpointResolver(func(connID core.ConnectionID) string {
		if rt, ok := sup.gatewayMgr.Runtime(connID); ok {
			return rt.Endpoint
		}
		return ""
	})

	profile := gatewayVerticalProfile()
	profile.Driver.ProviderID = provider.Identity().ID

	ctx := context.Background()
	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	// Degraded runtime: connector down, gateway projection gone. Exactly the
	// state repair restores from.
	rt := &core.ConnectionRuntime{ConnectionID: profile.ID, State: core.RuntimeDegraded}
	rt.Connector.Status = core.ConnectorStatusStopped
	sup.controller.RestoreRuntime(rt)

	repairPlan, err := sup.controller.PlanRepair(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanRepair: %v", err)
	}
	// Persist durably the way production does: operations reference
	// operation_plans by foreign key, so a plan known only to the controller
	// makes operation journalling fail.
	canonical, err := sup.store.SaveOrGetPlan(ctx, repairPlan)
	if err != nil {
		t.Fatalf("SaveOrGetPlan: %v", err)
	}
	if err := sup.controller.SavePlan(canonical); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	assertNoRawOriginInSteps(t, profile.ID, canonical.Steps)

	handler := &supervisorHandler{sup: sup}
	op, err := handler.HandleApplyPlan(string(canonical.ID), "")
	if err != nil {
		t.Fatalf("HandleApplyPlan (repair): %v", err)
	}
	// Execution is asynchronous: wait for a terminal state before inspecting
	// what the transport was told.
	opID := core.OperationID(op.ID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok := sup.controller.GetOperation(opID)
		if ok && (snap.State == controller.OperationStateCompleted || snap.State == controller.OperationStateFailed) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The connector must have been started against the live gateway endpoint,
	// resolved from the symbolic reference — not the raw origin, not the
	// symbol itself.
	var resolvedTargets []string
	for _, target := range provider.capturedTargets() {
		if strings.HasPrefix(target, "http://127.0.0.1:") &&
			!strings.HasSuffix(target, ":8080") {
			resolvedTargets = append(resolvedTargets, target)
		}
	}
	if len(resolvedTargets) == 0 {
		t.Fatalf("no step executed against a live gateway endpoint; captured targets: %v",
			provider.capturedTargets())
	}
	for _, target := range provider.capturedTargets() {
		if target == core.GatewayTargetRef {
			t.Fatalf("the symbolic reference reached the provider unresolved: %v",
				provider.capturedTargets())
		}
		if target == "http://127.0.0.1:8080" {
			t.Fatalf("the raw origin reached the provider during repair: %v",
				provider.capturedTargets())
		}
	}

	// Cleanup: stop what the test started so it does not leak a listener.
	sup.gatewayMgr.StopGateway(profile.ID)
}

// TestFailedRepairStopsTheGateway pins cleanup symmetry on the repair path:
// the gateway started to serve a repair must not outlive a failed repair any
// more than one started for an open would.
func TestFailedRepairStopsTheGateway(t *testing.T) {
	failing := &failingApplyProvider{}
	sup := testSupervisorWithController(t, failing)
	sup.controller.SetOriginManager(origin.NewManager())
	// Resolve against whatever the supervisor's gateway manager runs, exactly
	// as production wires it.
	sup.controller.SetGatewayEndpointResolver(func(connID core.ConnectionID) string {
		if rt, ok := sup.gatewayMgr.Runtime(connID); ok {
			return rt.Endpoint
		}
		return ""
	})

	profile := gatewayVerticalProfile()
	profile.Driver.ProviderID = failing.Identity().ID

	ctx := context.Background()
	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	rt := &core.ConnectionRuntime{ConnectionID: profile.ID, State: core.RuntimeDegraded}
	rt.Connector.Status = core.ConnectorStatusStopped
	sup.controller.RestoreRuntime(rt)

	repairPlan, err := sup.controller.PlanRepair(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanRepair: %v", err)
	}
	canonical, err := sup.store.SaveOrGetPlan(ctx, repairPlan)
	if err != nil {
		t.Fatalf("SaveOrGetPlan: %v", err)
	}
	if err := sup.controller.SavePlan(canonical); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	handler := &supervisorHandler{sup: sup}
	op, err := handler.HandleApplyPlan(string(canonical.ID), "")
	if err != nil {
		t.Fatalf("HandleApplyPlan (repair): %v", err)
	}
	// Execution is asynchronous; wait for the terminal state.
	opID := core.OperationID(op.ID)
	deadline := time.Now().Add(10 * time.Second)
	var final *controller.Operation
	for time.Now().Before(deadline) {
		snap, ok := sup.controller.GetOperation(opID)
		if ok && (snap.State == controller.OperationStateCompleted || snap.State == controller.OperationStateFailed) {
			final = snap
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if final == nil || final.State != controller.OperationStateFailed {
		state := "never reached terminal"
		if final != nil {
			state = string(final.State)
		}
		t.Fatalf("the deliberately failing repair ended as %q, want failed", state)
	}

	// The gateway stop is asynchronous (a watcher reacts to the terminal
	// operation state); give it a bounded window rather than asserting in the
	// same instant the failure lands.
	goneBy := time.Now().Add(5 * time.Second)
	for {
		if _, exists := sup.gatewayMgr.Runtime(profile.ID); !exists {
			break
		}
		if time.Now().After(goneBy) {
			t.Fatal("a supervisor-owned gateway outlived its failed repair")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
