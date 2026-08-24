package controller

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// Deleting and editing a private-network Serve connection.
//
// These pin the two leaks the Serve rewrite closed:
//
//   - PlanDelete used to skip ResourceTailnetServe entirely, so deleting a
//     connection left its Serve route published on the tailnet.
//   - PlanEdit could not build a deletion step for ResourceTailnetServe, so an
//     edit that changed the address left the old route active alongside the new.
//
// Both are asserted on the plan the controller actually produces, carrying the
// exact persisted route metadata — not on a plausible route derived from the
// current profile.

// servedRoute is the metadata a successful serve persisted.
func servedRouteMetadata() map[string]string {
	return map[string]string{
		"frontend_protocol": "http",
		"frontend_port":     "3000",
		"frontend_path":     "/",
		"backend_protocol":  "http",
		"backend_host":      "127.0.0.1",
		"backend_port":      "3000",
		"address":           "machine.example",
	}
}

// serveConnectionController registers a private-network expose profile whose
// runtime carries a live managed Serve resource.
func serveConnectionController(t *testing.T, connectorStatus core.ConnectorStatus) (*Controller, *core.ConnectionProfile) {
	t.Helper()
	profile := networkProfile("conn-serve", core.PrivateNetworkExpose, "127.0.0.1:3000")
	c := networkController(t, profile, []core.ObservedResourceStatus{
		{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
			Status: core.ObservationPresent},
		{Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
			Status: core.ObservationPresent},
	})
	c.runtimes[profile.ID] = &core.ConnectionRuntime{
		ConnectionID: profile.ID,
		State:        core.RuntimeOpen,
		Connector:    core.ConnectorRuntime{Status: connectorStatus},
		Provider: core.ProviderRuntime{
			Resources: []core.ProviderResource{
				{
					ConnectionID: profile.ID, ProviderID: "tailscale",
					Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
					Ownership: core.OwnershipAdopted, Lifecycle: core.LifecyclePresent,
				},
				{
					ConnectionID: profile.ID, ProviderID: "tailscale",
					Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
					Ownership: core.OwnershipManaged, Lifecycle: core.LifecyclePresent,
					Metadata: servedRouteMetadata(),
				},
			},
		},
	}
	return c, profile
}

// stepWithKind finds a step by kind.
func stepWithKind(plan *core.OperationPlan, kind core.StepKind) (core.PlanStep, bool) {
	for _, step := range plan.Steps {
		if step.Kind == kind {
			return step, true
		}
	}
	return core.PlanStep{}, false
}

// TestDeletingAServeConnectionWithdrawsTheManagedRoute pins that a delete cannot
// leave the managed Serve active.
//
// PlanDelete previously skipped ResourceTailnetServe, so the route stayed
// published after the connection was gone — a leak nothing would ever clean up.
func TestDeletingAServeConnectionWithdrawsTheManagedRoute(t *testing.T) {
	c, profile := serveConnectionController(t, core.ConnectorStatusRunning)

	plan, err := c.PlanDelete(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}

	step, ok := stepWithKind(plan, core.StepDeleteTailnetServe)
	if !ok {
		var kinds []core.StepKind
		for _, s := range plan.Steps {
			kinds = append(kinds, s.Kind)
		}
		t.Fatalf("delete plan has no Serve withdrawal step; steps = %v", kinds)
	}

	// The step addresses the exact persisted frontend identity, not a route
	// derived from the current profile.
	if step.Technical.ResourceID != "http:3000:/" {
		t.Errorf("the delete step targets %q, want the persisted frontend identity",
			step.Technical.ResourceID)
	}
	// And it carries the exact persisted route so the provider can reconstruct it.
	if step.Technical.Parameters["backend_host"] != "127.0.0.1" ||
		step.Technical.Parameters["frontend_protocol"] != "http" {
		t.Errorf("the delete step does not carry the persisted route: %v", step.Technical.Parameters)
	}
	// The provider's technical operation is the Tailscale withdrawal.
	if step.Technical.Type != "unserve" {
		t.Errorf("the delete step operation = %q, want unserve", step.Technical.Type)
	}
	// The Serve withdrawal must precede local finalization, or the local record
	// would be gone while the route was still published.
	serveIdx, finalizeIdx := -1, -1
	for i, s := range plan.Steps {
		switch s.Kind {
		case core.StepDeleteTailnetServe:
			serveIdx = i
		case core.StepFinalizeLocalDeletion:
			finalizeIdx = i
		}
	}
	if finalizeIdx >= 0 && serveIdx > finalizeIdx {
		t.Errorf("the route is withdrawn (%d) after local finalization (%d)", serveIdx, finalizeIdx)
	}
}

// TestDeletingAServeConnectionEmitsNoStopConnector pins that a private network
// does not get a connector-stop step it has no process for.
//
// The normalized connector status reads running whenever the route is healthy, so
// the generic rule would emit stop_connector — an operation the Tailscale adapter
// does not implement, which would fail the delete before the route was withdrawn.
func TestDeletingAServeConnectionEmitsNoStopConnector(t *testing.T) {
	c, profile := serveConnectionController(t, core.ConnectorStatusRunning)

	plan, err := c.PlanDelete(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if step, ok := stepWithKind(plan, core.StepStopConnector); ok {
		t.Fatalf("a private network delete emitted %q; there is no connector subprocess to stop",
			step.Technical.Type)
	}
}

// TestEditingThePublishedAddressWithdrawsTheOldRoute pins that an edit cannot
// leave the superseded Serve active.
//
// DiffProfiles already invalidated ResourceTailnetServe on an address change, but
// deleteStepForResource could not build a step for it — so the invalidation was
// recorded and then silently dropped, leaving the previous address reachable.
func TestEditingThePublishedAddressWithdrawsTheOldRoute(t *testing.T) {
	c, profile := serveConnectionController(t, core.ConnectorStatusRunning)

	proposed := profile.DeepCopy()
	proposed.Spec.PrivateNetwork.LocalAddress = "127.0.0.1:4000"

	plan, delta, err := c.PlanEdit(context.Background(), profile.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	if !delta.InvalidatedResources[core.ResourceTailnetServe] {
		t.Fatal("changing the published address did not invalidate the Serve route")
	}

	step, ok := stepWithKind(plan, core.StepDeleteTailnetServe)
	if !ok {
		var kinds []core.StepKind
		for _, s := range plan.Steps {
			kinds = append(kinds, s.Kind)
		}
		t.Fatalf("edit plan has no Serve withdrawal step; steps = %v", kinds)
	}

	// The withdrawal targets the OLD route — what Portico owns remotely — not the
	// new address the user just asked for.
	if step.Technical.ResourceID != "http:3000:/" {
		t.Errorf("the edit withdraws %q, want the superseded route", step.Technical.ResourceID)
	}
	if step.Technical.Parameters["backend_port"] != "3000" {
		t.Errorf("the withdrawal does not carry the old route: %v", step.Technical.Parameters)
	}

	// The old route must be withdrawn before the profile is committed, or the
	// commit boundary would pass with two routes live.
	withdrawIdx, applyIdx := -1, -1
	for i, s := range plan.Steps {
		switch s.Kind {
		case core.StepDeleteTailnetServe:
			withdrawIdx = i
		case core.StepApplyProfile:
			applyIdx = i
		}
	}
	if applyIdx < 0 {
		t.Fatal("the edit plan has no apply-profile commit boundary")
	}
	if withdrawIdx > applyIdx {
		t.Errorf("the old route is withdrawn (%d) after the profile commit (%d)", withdrawIdx, applyIdx)
	}

	// Compensation must be able to restore the old route if a later step fails.
	if step.Compensation == nil {
		t.Error("the withdrawal has no compensation, so a failed edit cannot restore the old route")
	}
}

// TestEditingTheProtocolWithdrawsTheOldRoute pins that a protocol change is a
// different frontend identity and therefore replaces the route.
func TestEditingTheProtocolWithdrawsTheOldRoute(t *testing.T) {
	c, profile := serveConnectionController(t, core.ConnectorStatusRunning)

	proposed := profile.DeepCopy()
	proposed.Spec.PrivateNetwork.LocalProtocol = core.ProtocolTCP

	plan, delta, err := c.PlanEdit(context.Background(), profile.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	if !delta.InvalidatedResources[core.ResourceTailnetServe] {
		t.Fatal("changing the protocol did not invalidate the Serve route")
	}
	if _, ok := stepWithKind(plan, core.StepDeleteTailnetServe); !ok {
		t.Fatal("a protocol change did not withdraw the old route")
	}
}

// TestEditingAnAdoptedRouteLeavesItAlone pins that an edit does not delete a
// route Portico did not create.
func TestEditingAnAdoptedRouteLeavesItAlone(t *testing.T) {
	c, profile := serveConnectionController(t, core.ConnectorStatusRunning)
	// Downgrade the Serve resource to adopted: Portico did not create it.
	for i := range c.runtimes[profile.ID].Provider.Resources {
		if c.runtimes[profile.ID].Provider.Resources[i].Type == core.ResourceTailnetServe {
			c.runtimes[profile.ID].Provider.Resources[i].Ownership = core.OwnershipAdopted
		}
	}

	proposed := profile.DeepCopy()
	proposed.Spec.PrivateNetwork.LocalAddress = "127.0.0.1:4000"

	plan, _, err := c.PlanEdit(context.Background(), profile.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	if _, ok := stepWithKind(plan, core.StepDeleteTailnetServe); ok {
		t.Fatal("an adopted route was withdrawn; Portico did not create it")
	}
	if len(plan.Warnings) == 0 {
		t.Error("the adopted route was left in place without telling the user")
	}
}
