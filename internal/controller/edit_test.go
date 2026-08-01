package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider/mock"
)

func editProfile(name, hostname string, protection core.ProtectionSpec) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID:       "conn-edit",
		Name:     name,
		Revision: 1,
		Kind:     core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "127.0.0.1:3000", Protocol: core.ProtocolHTTP},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposurePermanent, RequestedAddress: hostname},
				Protection: protection,
			},
		},
		Driver:  core.DriverSelection{ProviderID: "mock"},
		Desired: core.DesiredOpen,
	}
}

// editController returns a controller holding the given profile, with the
// listed managed resources and an open runtime.
func editController(t *testing.T, profile *core.ConnectionProfile, open bool, resources []core.ProviderResource) *Controller {
	t.Helper()
	c := New(newTestRegistry(mock.New()), newTestJournal())
	c.RestoreProfile(profile)
	rt := &core.ConnectionRuntime{ConnectionID: profile.ID, State: core.RuntimeClosed}
	if open {
		rt.State = core.RuntimeOpen
		rt.Connector.Status = core.ConnectorStatusRunning
	}
	rt.Provider.Resources = resources
	c.RestoreRuntime(rt)
	return c
}

func managedResources() []core.ProviderResource {
	return []core.ProviderResource{
		{Type: core.ResourceTunnel, ExternalID: "tun-1", Ownership: core.OwnershipManaged, Lifecycle: core.LifecyclePresent},
		{Type: core.ResourceDNSRecord, ExternalID: "dns-1", Ownership: core.OwnershipManaged, Lifecycle: core.LifecyclePresent},
		{Type: core.ResourceAccessApp, ExternalID: "app-1", Ownership: core.OwnershipManaged, Lifecycle: core.LifecyclePresent},
	}
}

func stepKinds(plan *core.OperationPlan) []core.StepKind {
	kinds := make([]core.StepKind, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		kinds = append(kinds, s.Kind)
	}
	return kinds
}

func hasKind(plan *core.OperationPlan, kind core.StepKind) bool {
	for _, s := range plan.Steps {
		if s.Kind == kind {
			return true
		}
	}
	return false
}

// TestNoOpEditProducesNoSteps ensures editing nothing does not close and reopen
// a working connection.
func TestNoOpEditProducesNoSteps(t *testing.T) {
	profile := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := editController(t, profile, true, managedResources())

	plan, delta, err := c.PlanEdit(context.Background(), profile.ID, profile.DeepCopy())
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	if !delta.Empty() {
		t.Fatalf("delta = %#v, want empty", delta.Changes)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("a no-op edit produced steps: %v", stepKinds(plan))
	}
}

// TestHostnameChangeReplacesDNSButKeepsTheTunnel pins the resource impact
// classification. Deleting the tunnel would be needless churn; keeping the DNS
// record would leak it.
func TestHostnameChangeReplacesDNSButKeepsTheTunnel(t *testing.T) {
	profile := editProfile("demo", "old.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := editController(t, profile, true, managedResources())

	proposed := profile.DeepCopy()
	proposed.Spec.ServiceExposure.Exposure.RequestedAddress = "new.example.com"

	plan, delta, err := c.PlanEdit(context.Background(), profile.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	if !delta.InvalidatedResources[core.ResourceDNSRecord] {
		t.Fatal("a hostname change did not invalidate the DNS record")
	}
	if delta.InvalidatedResources[core.ResourceTunnel] {
		t.Fatal("a hostname change needlessly invalidated the tunnel")
	}
	if !hasKind(plan, core.StepDeleteDNSRecord) {
		t.Fatalf("plan does not delete the stale DNS record: %v", stepKinds(plan))
	}
	if hasKind(plan, core.StepDeleteTunnel) {
		t.Fatalf("plan deletes the tunnel unnecessarily: %v", stepKinds(plan))
	}
}

// TestProviderChangeReplacesEveryManagedResource ensures resources at the old
// provider are not left behind.
func TestProviderChangeReplacesEveryManagedResource(t *testing.T) {
	current := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})

	proposed := current.DeepCopy()
	proposed.Driver.AccountID = "different-account"

	delta, err := DiffProfiles(current, proposed)
	if err != nil {
		t.Fatalf("DiffProfiles: %v", err)
	}
	for _, kind := range []core.ResourceType{
		core.ResourceTunnel, core.ResourceDNSRecord, core.ResourceAccessApp,
	} {
		if !delta.InvalidatedResources[kind] {
			t.Fatalf("an account change did not invalidate %s", kind)
		}
	}
}

// TestOpenConnectionPausesFirstAndReopensLast pins the pause/resume shape.
func TestOpenConnectionPausesFirstAndReopensLast(t *testing.T) {
	profile := editProfile("demo", "old.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := editController(t, profile, true, managedResources())

	proposed := profile.DeepCopy()
	proposed.Spec.ServiceExposure.Exposure.RequestedAddress = "new.example.com"

	plan, _, err := c.PlanEdit(context.Background(), profile.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	kinds := stepKinds(plan)
	if len(kinds) == 0 || kinds[0] != core.StepStopConnector {
		t.Fatalf("plan does not pause first: %v", kinds)
	}

	applyIdx, deleteIdx := -1, -1
	for i, k := range kinds {
		if k == core.StepApplyProfile {
			applyIdx = i
		}
		if k == core.StepDeleteDNSRecord {
			deleteIdx = i
		}
	}
	if applyIdx < 0 {
		t.Fatalf("plan has no commit boundary: %v", kinds)
	}
	// Resource cleanup happens under the old profile, before the commit.
	if deleteIdx > applyIdx {
		t.Fatalf("stale resources are deleted after the profile commits: %v", kinds)
	}
	// The reopen follows the commit.
	if applyIdx == len(kinds)-1 {
		t.Fatalf("an open connection was not reopened: %v", kinds)
	}
}

// TestClosedConnectionIsNotOpenedByAnEdit ensures an edit never starts a
// connection the user had closed.
func TestClosedConnectionIsNotOpenedByAnEdit(t *testing.T) {
	profile := editProfile("demo", "old.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	profile.Desired = core.DesiredClosed
	c := editController(t, profile, false, managedResources())

	proposed := profile.DeepCopy()
	proposed.Spec.ServiceExposure.Exposure.RequestedAddress = "new.example.com"

	plan, _, err := c.PlanEdit(context.Background(), profile.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	if hasKind(plan, core.StepStartConnector) {
		t.Fatalf("an edit opened a closed connection: %v", stepKinds(plan))
	}
	if hasKind(plan, core.StepStopConnector) {
		t.Fatalf("an edit paused an already closed connection: %v", stepKinds(plan))
	}
	if plan.Expected.State != core.RuntimeClosed {
		t.Fatalf("expected state = %q, want closed", plan.Expected.State)
	}
}

// TestPreviousProfileSurvivesUntilTheCommitStep pins the retain-previous
// property: planning an edit must not change what a reader sees.
func TestPreviousProfileSurvivesUntilTheCommitStep(t *testing.T) {
	profile := editProfile("original", "old.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := editController(t, profile, true, managedResources())

	proposed := profile.DeepCopy()
	proposed.Name = "renamed"
	proposed.Spec.ServiceExposure.Exposure.RequestedAddress = "new.example.com"

	if _, _, err := c.PlanEdit(context.Background(), profile.ID, proposed); err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}

	stored, ok := c.GetProfile(profile.ID)
	if !ok {
		t.Fatal("profile disappeared")
	}
	if stored.Name != "original" {
		t.Fatalf("planning an edit already changed the name to %q", stored.Name)
	}
	if stored.Spec.ServiceExposure.Exposure.RequestedAddress != "old.example.com" {
		t.Fatalf("planning an edit already changed the hostname to %q",
			stored.Spec.ServiceExposure.Exposure.RequestedAddress)
	}
}

// TestAdoptedResourcesAreNeverDeletedByAnEdit ensures Portico does not remove
// infrastructure it did not create.
func TestAdoptedResourcesAreNeverDeletedByAnEdit(t *testing.T) {
	profile := editProfile("demo", "old.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	adopted := []core.ProviderResource{
		{Type: core.ResourceDNSRecord, ExternalID: "dns-adopted", Ownership: core.OwnershipAdopted, Lifecycle: core.LifecyclePresent},
	}
	c := editController(t, profile, true, adopted)

	proposed := profile.DeepCopy()
	proposed.Spec.ServiceExposure.Exposure.RequestedAddress = "new.example.com"

	plan, _, err := c.PlanEdit(context.Background(), profile.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	if hasKind(plan, core.StepDeleteDNSRecord) {
		t.Fatalf("an adopted resource was planned for deletion: %v", stepKinds(plan))
	}
	// The user must still be told it is now unmanaged.
	if len(plan.Warnings) == 0 {
		t.Fatal("no warning was raised about the orphaned adopted resource")
	}
	if !strings.Contains(plan.Warnings[0].Message, "dns-adopted") {
		t.Fatalf("warning does not name the resource: %q", plan.Warnings[0].Message)
	}
}

// TestChangingConnectionKindIsRefused keeps migrations out of the edit path.
func TestChangingConnectionKindIsRefused(t *testing.T) {
	profile := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := editController(t, profile, false, nil)

	proposed := profile.DeepCopy()
	proposed.Kind = core.ConnectionClientTunnel
	proposed.Spec.ServiceExposure = nil
	proposed.Spec.ClientTunnel = &core.ClientTunnelSpec{
		Client: core.ClientOpenAISecureMCPTunnel,
		MCP:    core.MCPServiceSpec{Endpoint: "http://127.0.0.1:8787/mcp"},
	}

	_, _, err := c.PlanEdit(context.Background(), profile.ID, proposed)
	if err == nil {
		t.Fatal("changing the connection kind was accepted as an edit")
	}
	if !strings.Contains(err.Error(), "clone") {
		t.Fatalf("refusal does not point at the safe alternative: %v", err)
	}
}

// TestRenameIsRecognisedAsNotNeedingAPlan keeps the common case on the fast
// path.
func TestRenameIsRecognisedAsNotNeedingAPlan(t *testing.T) {
	current := editProfile("before", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	proposed := current.DeepCopy()
	proposed.Name = "after"

	delta, err := DiffProfiles(current, proposed)
	if err != nil {
		t.Fatalf("DiffProfiles: %v", err)
	}
	if !delta.NameOnly() {
		t.Fatalf("a rename was not recognised as name-only: %#v", delta)
	}
	if len(delta.InvalidatedResources) != 0 {
		t.Fatalf("a rename invalidated resources: %#v", delta.InvalidatedResources)
	}
}

// TestProtectionChangeReplacesAccessOnly ensures tightening access does not
// churn the tunnel or DNS record.
func TestProtectionChangeReplacesAccessOnly(t *testing.T) {
	current := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	proposed := current.DeepCopy()
	proposed.Spec.ServiceExposure.Protection = core.ProtectionSpec{
		Kind: core.ProtectionEmailOTP, AllowedEmails: []string{"alice@example.com"},
	}

	delta, err := DiffProfiles(current, proposed)
	if err != nil {
		t.Fatalf("DiffProfiles: %v", err)
	}
	if !delta.InvalidatedResources[core.ResourceAccessApp] {
		t.Fatal("a protection change did not invalidate the access application")
	}
	if delta.InvalidatedResources[core.ResourceTunnel] || delta.InvalidatedResources[core.ResourceDNSRecord] {
		t.Fatalf("a protection change churned unrelated resources: %#v", delta.InvalidatedResources)
	}
}

// TestAnEditPlanCanActuallyBeApplied pins the boundary the edit tests never
// crossed.
//
// PlanEdit built plans with core.IntentEdit, and ApplyPlan's intent dispatch
// listed open, close, repair and delete — so every edit plan fell to the
// default branch and returned "unknown plan intent: edit". The whole edit
// feature could be previewed and never applied.
//
// Every existing edit test asserted on the plan. None applied one, which is why
// a feature that could not work looked covered.
func TestAnEditPlanCanActuallyBeApplied(t *testing.T) {
	ctx := context.Background()
	current := editProfile("original", "old.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := editController(t, current, false, nil)

	proposed := current.DeepCopy()
	proposed.Name = "renamed"

	plan, _, err := c.PlanEdit(ctx, current.ID, proposed)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("the fixture no longer produces an edit with steps")
	}
	if err := c.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	op, err := c.ApplyPlan(ctx, plan.ID)
	if err != nil {
		if strings.Contains(err.Error(), "unknown plan intent") {
			t.Fatalf("an edit plan cannot be applied at all: %v", err)
		}
		t.Fatalf("ApplyPlan: %v", err)
	}

	// Execution is asynchronous, so wait for the operation to finish before
	// asking whether the edit took effect.
	deadline := time.Now().Add(10 * time.Second)
	var final *Operation
	for time.Now().Before(deadline) {
		snap, ok := c.GetOperation(op.ID)
		if ok && (snap.State == OperationStateCompleted || snap.State == OperationStateFailed) {
			final = snap
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if final == nil {
		t.Fatal("the edit operation never reached a terminal state")
	}
	if final.State != OperationStateCompleted {
		t.Fatalf("the edit operation ended in %q: %v", final.State, final.Error)
	}

	// And the edit actually took effect.
	applied, ok := c.GetProfile(current.ID)
	if !ok {
		t.Fatal("the connection is gone after applying an edit")
	}
	if applied.Name != "renamed" {
		t.Fatalf("the applied profile is still named %q", applied.Name)
	}
}
