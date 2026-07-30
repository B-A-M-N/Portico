package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/provider/mock"
)

// testJournal is a no-op Journal for tests that don't need persistence verification.
type testJournal struct {
	mu     sync.Mutex
	ops    map[core.OperationID]string // opID → state
	events []core.Event
}

func newTestJournal() *testJournal {
	return &testJournal{
		ops:    make(map[core.OperationID]string),
		events: nil,
	}
}

func (j *testJournal) SaveOperation(ctx context.Context, opID core.OperationID, planID core.PlanID, connID core.ConnectionID, state string, startedAt string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ops[opID] = state
	return nil
}

func (j *testJournal) CompleteOperation(ctx context.Context, opID core.OperationID, state string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ops[opID] = state
	return nil
}

func (j *testJournal) AppendOperationEvent(ctx context.Context, event core.Event, opID core.OperationID) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, event)
	return nil
}

func (j *testJournal) AppendFinding(ctx context.Context, finding core.DiagnosticFinding) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return nil
}

// registry is a minimal registry for tests that only stores a single provider.
type testRegistry struct {
	providers map[core.ProviderID]core.Provider
	accounts  map[core.ProviderID][]core.ProviderAccountID
}

func newTestRegistry(providers ...core.Provider) *testRegistry {
	r := &testRegistry{providers: make(map[core.ProviderID]core.Provider), accounts: make(map[core.ProviderID][]core.ProviderAccountID)}
	for _, p := range providers {
		id := p.Identity().ID
		r.providers[id] = p
	}
	return r
}

func (r *testRegistry) Add(p core.Provider) error             { r.providers[p.Identity().ID] = p; return nil }
func (r *testRegistry) Get(id core.ProviderID) core.Provider  { return r.providers[id] }
func (r *testRegistry) List() []provider.ProviderSnapshot     { return nil }
func (r *testRegistry) Snapshot() []provider.ProviderSnapshot { return nil }
func (r *testRegistry) SetAccounts(id core.ProviderID, accounts []core.ProviderAccountID) {
	r.accounts[id] = append([]core.ProviderAccountID(nil), accounts...)
}
func (r *testRegistry) SetAccountInfo(id core.ProviderID, accounts []provider.AccountInfo) {
	ids := make([]core.ProviderAccountID, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	r.SetAccounts(id, ids)
}
func (r *testRegistry) GetAccounts(id core.ProviderID) []core.ProviderAccountID {
	return append([]core.ProviderAccountID(nil), r.accounts[id]...)
}
func (r *testRegistry) DiscoverIdentities(ctx context.Context) []provider.ProviderSnapshot {
	return nil
}

type accountBoundMockProvider struct {
	*mock.Provider
	accountID core.ProviderAccountID
}

type multiAccountMockProvider struct {
	*mock.Provider
	children map[core.ProviderAccountID]core.Provider
}

func (p *multiAccountMockProvider) ProviderForAccount(id core.ProviderAccountID) (core.Provider, error) {
	child := p.children[id]
	if child == nil {
		return nil, errors.New("account unavailable")
	}
	return child, nil
}

func (p *accountBoundMockProvider) ProviderAccountID() core.ProviderAccountID {
	return p.accountID
}

func TestCreateProfileDefaultsProtectedSessionTTL(t *testing.T) {
	ctrl := New(newTestRegistry(mock.New()), newTestJournal())
	profile := newFailureTestProfile()
	if profile.Spec.ServiceExposure != nil {
		profile.Spec.ServiceExposure.Protection = core.ProtectionSpec{
			Kind:          core.ProtectionEmailOTP,
			AllowedEmails: []string{"person@example.com"},
		}
	}

	stored, _, err := ctrl.CreateProfile(context.Background(), profile)
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if stored.GetProtection().SessionTTL != core.DefaultProtectedSessionTTL {
		t.Fatalf("session TTL = %s, want %s", stored.GetProtection().SessionTTL, core.DefaultProtectedSessionTTL)
	}
}

func TestCreateProfilePersistsTheOnlyConfiguredAccount(t *testing.T) {
	child := &accountBoundMockProvider{Provider: mock.New(), accountID: "account-a"}
	parent := &multiAccountMockProvider{Provider: mock.New(), children: map[core.ProviderAccountID]core.Provider{"account-a": child}}
	registry := newTestRegistry(parent)
	registry.SetAccounts("mock", []core.ProviderAccountID{"account-a"})
	ctrl := New(registry, newTestJournal())

	profile := newFailureTestProfile()
	stored, _, err := ctrl.CreateProfile(context.Background(), profile)
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if stored.Driver.AccountID != "account-a" {
		t.Fatalf("stored account = %q, want account-a", stored.Driver.AccountID)
	}
}

func TestControllerRejectsMismatchedProviderAccountBeforePlanningOrMutation(t *testing.T) {
	prov := &accountBoundMockProvider{Provider: mock.New(), accountID: "account-a"}
	ctrl := New(newTestRegistry(prov), newTestJournal())
	ctx := context.Background()

	profile := newFailureTestProfile()
	profile.Driver.AccountID = "account-b"
	if _, _, err := ctrl.CreateProfile(ctx, profile); err == nil {
		t.Fatal("CreateProfile accepted a provider account bound to a different adapter")
	} else {
		var porticoErr *core.PorticoError
		if !errors.As(err, &porticoErr) || porticoErr.Code != core.ErrCorePrefix+"010" {
			t.Fatalf("CreateProfile error = %v, want typed provider-account error", err)
		}
	}

	profile = newFailureTestProfile()
	profile.Driver.AccountID = "account-a"
	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile with bound account: %v", err)
	}
	plan, err := ctrl.PlanOpen(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen with bound account: %v", err)
	}
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	// Simulate a pre-existing profile loaded after its selected account was
	// changed outside a current controller update. Apply must repeat the check
	// so an already-previewed plan cannot mutate the wrong remote account.
	loaded, ok := ctrl.GetProfile(profile.ID)
	if !ok {
		t.Fatal("profile missing")
	}
	loaded.Driver.AccountID = "account-b"
	ctrl.RestoreProfile(loaded)
	if _, err := ctrl.ApplyPlan(ctx, plan.ID); err == nil {
		t.Fatal("ApplyPlan accepted a stale account binding")
	} else {
		var porticoErr *core.PorticoError
		if !errors.As(err, &porticoErr) || porticoErr.Code != core.ErrCorePrefix+"010" {
			t.Fatalf("ApplyPlan error = %v, want typed provider-account error", err)
		}
	}
}

// TestController_OpenCloseFullPath tests the full open → observe → close cycle
// through the controller using the mock provider.
func TestController_OpenCloseFullPath(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	journal := newTestJournal()
	ctrl := New(reg, journal)

	ctx := context.Background()

	// Step 1: Create a profile
	profile := &core.ConnectionProfile{
		Name: "test-connection",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind: core.SourceExisting,
					Existing: &core.ExistingServiceSpec{
						Address:  "localhost:8080",
						Protocol: core.ProtocolHTTP,
					},
				},
				Exposure: core.ExposureSpec{
					Mode: core.ExposureTemporary,
				},
				Protection: core.ProtectionSpec{
					Kind: core.ProtectionNone,
				},
			},
		},
		Driver: core.DriverSelection{
			ProviderID: "mock",
		},
		Lifecycle: core.LifecycleSpec{
			AutoStart:    true,
			OnDisconnect: core.DisconnectKeepAlive,
		},
		Desired: core.DesiredOpen,
	}

	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	connID := profile.ID
	if connID == "" {
		t.Fatal("CreateProfile did not assign ID")
	}

	// Verify initial runtime state is closed
	rt, ok := ctrl.GetRuntime(connID)
	if !ok {
		t.Fatal("runtime not created")
	}
	if rt.State != core.RuntimeClosed {
		t.Fatalf("expected closed, got %s", rt.State)
	}

	// Step 2: Plan open
	plan, err := ctrl.PlanOpen(ctx, connID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	if plan.Intent != core.IntentOpen {
		t.Fatalf("expected open intent, got %s", plan.Intent)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("plan has no steps")
	}
	if plan.Fingerprint == "" {
		t.Fatal("plan has no fingerprint")
	}

	// Step 3: Save the plan and apply it
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := ctrl.ApplyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if op.State != OperationStateRunning {
		t.Fatalf("expected running, got %s", op.State)
	}

	// Step 4: Verify runtime transitioned to opening
	rt, ok = ctrl.GetRuntime(connID)
	if !ok {
		t.Fatal("runtime missing after apply")
	}
	if rt.State != core.RuntimeOpening {
		t.Fatalf("expected opening, got %s", rt.State)
	}
	if rt.ActiveOperation == nil {
		t.Fatal("expected active operation")
	}

	// Step 5: Wait for the operation to complete via drainEvents
	// drainEvents runs in a goroutine; poll for completion
	deadline := time.Now().Add(5 * time.Second)
	var finalOp *Operation
	for time.Now().Before(deadline) {
		snap, ok := ctrl.GetOperation(op.ID)
		if !ok {
			t.Fatal("operation disappeared")
		}
		if snap.State == OperationStateCompleted {
			finalOp = snap
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finalOp == nil {
		t.Fatal("operation did not complete within deadline")
	}

	// Step 6: Verify runtime state is open
	rt, ok = ctrl.GetRuntime(connID)
	if !ok {
		t.Fatal("runtime missing after completion")
	}
	if rt.State != core.RuntimeOpen {
		t.Fatalf("expected open, got %s", rt.State)
	}
	if rt.ActiveOperation != nil {
		t.Fatal("active operation should be nil after completion")
	}
	if len(finalOp.StepEvents) == 0 {
		t.Fatal("expected step events")
	}

	// Step 7: Observe from the provider
	if _, err := ctrl.Observe(ctx, connID); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	rt, ok = ctrl.GetRuntime(connID)
	if !ok {
		t.Fatal("runtime missing after observe")
	}
	if rt.ObservedRevision == 0 {
		t.Fatal("expected observed revision to be incremented")
	}

	// Step 8: Reconcile — desired is open, runtime is open → no action
	action, err := ctrl.Reconcile(ctx, connID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if action != "" {
		t.Fatalf("expected no action for open+open, got %q", action)
	}

	// Step 9: Plan close
	planClose, err := ctrl.PlanClose(ctx, connID)
	if err != nil {
		t.Fatalf("PlanClose: %v", err)
	}
	if planClose.Intent != core.IntentClose {
		t.Fatalf("expected close intent, got %s", planClose.Intent)
	}

	// Step 10: Save close plan and apply
	if err := ctrl.SavePlan(planClose); err != nil {
		t.Fatalf("SavePlan (close): %v", err)
	}
	opClose, err := ctrl.ApplyPlan(ctx, planClose.ID)
	if err != nil {
		t.Fatalf("ApplyPlan (close): %v", err)
	}

	// Step 11: Wait for close completion
	deadline = time.Now().Add(5 * time.Second)
	var finalCloseOp *Operation
	for time.Now().Before(deadline) {
		snap, ok := ctrl.GetOperation(opClose.ID)
		if !ok {
			t.Fatal("close operation disappeared")
		}
		if snap.State == OperationStateCompleted {
			finalCloseOp = snap
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finalCloseOp == nil {
		t.Fatal("close operation did not complete within deadline")
	}

	// Step 12: Verify runtime state is closed
	rt, ok = ctrl.GetRuntime(connID)
	if !ok {
		t.Fatal("runtime missing after close")
	}
	if rt.State != core.RuntimeClosed {
		t.Fatalf("expected closed, got %s", rt.State)
	}

	// Step 13: Reconcile — desired is closed, runtime is closed → no action
	action, err = ctrl.Reconcile(ctx, connID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if action != "" {
		t.Fatalf("expected no action for closed+closed, got %q", action)
	}
}

// TestController_RepairFullPath tests the repair path through the controller.
func TestController_RepairFullPath(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	ctrl := New(reg, newTestJournal())

	ctx := context.Background()

	// Create profile and open the connection first
	profile := &core.ConnectionProfile{
		Name: "repair-test",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "localhost:8080", Protocol: core.ProtocolHTTP},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver: core.DriverSelection{ProviderID: "mock"},
		Desired:    core.DesiredOpen,
	}
	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	connID := profile.ID

	// Open the connection
	plan, _ := ctrl.PlanOpen(ctx, connID)
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := ctrl.ApplyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok := ctrl.GetOperation(op.ID)
		if !ok {
			t.Fatal("operation disappeared")
		}
		if snap.State == OperationStateCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Inject connector failure in the mock provider
	prov.InjectConnectorFailure(connID)

	// Observe — should now show degraded state
	if _, err := ctrl.Observe(ctx, connID); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	// Reconcile should detect the issue and suggest repair
	action, err := ctrl.Reconcile(ctx, connID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if action != "repair" {
		t.Fatalf("expected repair, got %q", action)
	}

	// Plan the repair
	repairPlan, err := ctrl.PlanRepair(ctx, connID)
	if err != nil {
		t.Fatalf("PlanRepair: %v", err)
	}
	if repairPlan.Intent != core.IntentRepair {
		t.Fatalf("expected repair intent, got %s", repairPlan.Intent)
	}

	// Apply the repair
	if err := ctrl.SavePlan(repairPlan); err != nil {
		t.Fatalf("SavePlan (repair): %v", err)
	}
	opRepair, err := ctrl.ApplyPlan(ctx, repairPlan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan (repair): %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	var repairCompleted bool
	for time.Now().Before(deadline) {
		snap, ok := ctrl.GetOperation(opRepair.ID)
		if !ok {
			break
		}
		if snap.State == OperationStateCompleted {
			repairCompleted = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !repairCompleted {
		t.Fatal("repair operation did not complete")
	}

	// Verify provider reports running connector
	status := prov.GetConnectorStatus(connID)
	if status != core.ConnectorStatusRunning {
		t.Fatalf("expected connector running after repair, got %s", status)
	}

	// Verify controller runtime state is open after repair (audit #20)
	rt, ok := ctrl.GetRuntime(connID)
	if !ok {
		t.Fatal("runtime missing after repair")
	}
	if rt.State != core.RuntimeOpen {
		t.Fatalf("expected runtime open after repair, got %s", rt.State)
	}
}

func TestPlanRepairHandlesUnknownAndUnstableConnector(t *testing.T) {
	for _, status := range []core.ConnectorStatus{core.ConnectorStatusUnknown, core.ConnectorStatusUnstable} {
		t.Run(string(status), func(t *testing.T) {
			ctrl := New(newTestRegistry(mock.New()), newTestJournal())
			profile := &core.ConnectionProfile{
				ID:   core.ConnectionID("repair-" + string(status)),
				Name: "repair " + string(status),
				Kind: core.ConnectionServiceExposure,
				Spec: core.ConnectionSpec{
					ServiceExposure: &core.ServiceExposureSpec{
						Source: core.SourceSpec{Kind: core.SourceExisting, Existing: &core.ExistingServiceSpec{Address: "127.0.0.1:8080", Protocol: core.ProtocolHTTP}},
						Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
						Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
					},
				},
				Driver: core.DriverSelection{ProviderID: "mock"},
				Desired:    core.DesiredOpen,
			}
			if _, _, err := ctrl.CreateProfile(context.Background(), profile); err != nil {
				t.Fatalf("create profile: %v", err)
			}
			rt, ok := ctrl.GetRuntime(profile.ID)
			if !ok {
				t.Fatal("missing runtime")
			}
			rt.State = core.RuntimeDegraded
			rt.Connector.Status = status
			ctrl.RestoreRuntime(rt)
			plan, err := ctrl.PlanRepair(context.Background(), profile.ID)
			if err != nil {
				t.Fatalf("plan repair: %v", err)
			}
			if len(plan.Steps) == 0 || plan.Steps[len(plan.Steps)-1].Kind != core.StepStartConnector {
				t.Fatalf("expected connector restart plan, got %+v", plan.Steps)
			}
		})
	}
}

// TestController_DeleteFullPath tests the delete path.
func TestController_DeleteFullPath(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	ctrl := New(reg, newTestJournal())

	ctx := context.Background()

	// Create profile and open
	profile := &core.ConnectionProfile{
		Name: "delete-test",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "localhost:8080", Protocol: core.ProtocolHTTP},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver: core.DriverSelection{ProviderID: "mock"},
		Desired:    core.DesiredOpen,
	}
	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	connID := profile.ID

	plan, _ := ctrl.PlanOpen(ctx, connID)
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := ctrl.ApplyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok := ctrl.GetOperation(op.ID)
		if !ok {
			t.Fatal("operation disappeared")
		}
		if snap.State == OperationStateCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Verify the provider has recorded the connection
	status := prov.GetConnectorStatus(connID)
	if status != core.ConnectorStatusRunning {
		t.Fatalf("expected connector running before delete, got %s", status)
	}

	// Plan delete
	deletePlan, err := ctrl.PlanDelete(ctx, connID)
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if deletePlan.Intent != core.IntentDelete {
		t.Fatalf("expected delete intent, got %s", deletePlan.Intent)
	}

	// Apply delete
	if err := ctrl.SavePlan(deletePlan); err != nil {
		t.Fatalf("SavePlan (delete): %v", err)
	}
	opDelete, err := ctrl.ApplyPlan(ctx, deletePlan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan (delete): %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	var deleteCompleted bool
	for time.Now().Before(deadline) {
		snap, ok := ctrl.GetOperation(opDelete.ID)
		if !ok {
			break
		}
		if snap.State == OperationStateCompleted {
			deleteCompleted = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !deleteCompleted {
		t.Fatal("delete operation did not complete")
	}

	// After successful delete, profile and runtime should be removed
	// by the atomic store transaction (SPEC P0 #2-3).
	_, ok := ctrl.GetProfile(connID)
	if ok {
		t.Fatal("profile should be deleted after successful delete operation")
	}
	_, ok = ctrl.GetRuntime(connID)
	if ok {
		t.Fatal("runtime should be deleted after successful delete operation")
	}

	// Verify provider has removed the connection (connector stopped).
	status = prov.GetConnectorStatus(connID)
	if status != core.ConnectorStatusStopped {
		t.Fatalf("expected connector stopped after delete, got %s", status)
	}
}

func TestController_LocalOnlyDeleteDoesNotRequireUnavailableProvider(t *testing.T) {
	ctrl := New(newTestRegistry(), newTestJournal())
	committer := &fakeRuntimeCommitter{}
	ctrl.SetRuntimeCommitter(committer)

	profile := &core.ConnectionProfile{
		ID:       "connection-unavailable-provider",
		Name:     "legacy development connection",
		Revision: 1,
		Kind:     core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "127.0.0.1:8080", Protocol: core.ProtocolHTTP},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver: core.DriverSelection{ProviderID: "removed-provider"},
		Desired:    core.DesiredClosed,
	}
	ctrl.RestoreProfile(profile)
	ctrl.RestoreRuntime(&core.ConnectionRuntime{
		ConnectionID: profile.ID,
		State:        core.RuntimeError,
		Connector:    core.ConnectorRuntime{Status: core.ConnectorStatusStopped},
	})

	plan, err := ctrl.PlanDelete(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Kind != core.StepFinalizeLocalDeletion {
		t.Fatalf("expected one local finalizer step, got %#v", plan.Steps)
	}
	if planRequiresProvider(plan) {
		t.Fatal("local-only delete should not require an unavailable provider")
	}
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	op, err := ctrl.ApplyPlan(context.Background(), plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if result := awaitOperationTerminal(t, ctrl, op.ID); result.State != OperationStateCompleted {
		t.Fatalf("operation state = %s, error = %v", result.State, result.Error)
	}
	if _, ok := ctrl.GetProfile(profile.ID); ok {
		t.Fatal("profile should be removed after local-only deletion")
	}
}

func TestPlanDeleteSkipsResourcesAlreadyConfirmedExternallyRemoved(t *testing.T) {
	prov := mock.New()
	ctrl := New(newTestRegistry(prov), newTestJournal())
	profile := newFailureTestProfile()
	if _, _, err := ctrl.CreateProfile(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	ctrl.RestoreRuntime(&core.ConnectionRuntime{
		ConnectionID: profile.ID,
		Connector:    core.ConnectorRuntime{Status: core.ConnectorStatusStopped},
		Provider: core.ProviderRuntime{Resources: []core.ProviderResource{
			{ProviderID: "mock", Type: core.ResourceDNSRecord, ExternalID: "dead-dns", Ownership: core.OwnershipManaged, Lifecycle: core.LifecycleExternallyRemoved},
			{ProviderID: "mock", Type: core.ResourceTunnel, ExternalID: "live-tunnel", Ownership: core.OwnershipManaged, Lifecycle: core.LifecyclePresent},
		}},
	})
	plan, err := ctrl.PlanDelete(context.Background(), profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range plan.Steps {
		if step.Technical.ResourceID == "dead-dns" {
			t.Fatalf("delete plan must not retry known-absent resource: %#v", plan.Steps)
		}
	}
	if len(plan.Steps) != 2 || plan.Steps[0].Technical.ResourceID != "live-tunnel" {
		t.Fatalf("unexpected deletion plan: %#v", plan.Steps)
	}
}

// TestController_ApplyStalePlan tests that a plan with a stale revision is rejected.
func TestController_ApplyStalePlan(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	ctrl := New(reg, newTestJournal())

	ctx := context.Background()

	profile := &core.ConnectionProfile{
		Name: "stale-test",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "localhost:8080", Protocol: core.ProtocolHTTP},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver: core.DriverSelection{ProviderID: "mock"},
		Desired:    core.DesiredOpen,
	}
	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	connID := profile.ID

	// Plan open
	plan, _ := ctrl.PlanOpen(ctx, connID)
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	// Update the profile to bump revision (simulating concurrent modification)
	profile.Desired = core.DesiredClosed
	ctrl.UpdateProfile(ctx, profile, 1)

	// Trying to apply the old plan should fail (revision mismatch)
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	_, err := ctrl.ApplyPlan(ctx, plan.ID)
	if err == nil {
		t.Fatal("expected error applying plan with stale revision")
	}
}

// TestController_PlanValidationError tests validation of an unregistered provider.
func TestController_PlanValidationError(t *testing.T) {
	reg := newTestRegistry()
	ctrl := New(reg, newTestJournal())

	ctx := context.Background()

	// Profile with a provider that doesn't exist in registry
	profile := &core.ConnectionProfile{
		Name: "no-provider",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "localhost:8080", Protocol: core.ProtocolHTTP},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver: core.DriverSelection{ProviderID: "nonexistent"},
		Desired:    core.DesiredOpen,
	}

	// CreateProfile should fail because provider not found
	_, _, err := ctrl.CreateProfile(ctx, profile)
	if err == nil {
		t.Fatal("expected error creating profile with unregistered provider")
	}
}

// TestController_ConcurrentOperationLimit tests that concurrent operation limit is enforced.
func TestController_ConcurrentOperationLimit(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	ctrl := New(reg, newTestJournal())

	ctx := context.Background()

	// Create multiple profiles
	var connIDs []core.ConnectionID
	for i := 0; i < GlobalOpLimit+2; i++ {
		profile := &core.ConnectionProfile{
			Name: "test-connection",
			Kind: core.ConnectionServiceExposure,
			Spec: core.ConnectionSpec{
				ServiceExposure: &core.ServiceExposureSpec{
					Source: core.SourceSpec{
						Kind:     core.SourceExisting,
						Existing: &core.ExistingServiceSpec{Address: "localhost:8080", Protocol: core.ProtocolHTTP},
					},
					Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
					Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
				},
			},
			Driver: core.DriverSelection{ProviderID: "mock"},
			Desired:    core.DesiredOpen,
		}
		if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
			t.Fatalf("CreateProfile %d: %v", i, err)
		}
		connIDs = append(connIDs, profile.ID)
	}

	// Start operations up to the limit — these should succeed
	var ops []Operation
	for i := 0; i < GlobalOpLimit; i++ {
		plan, err := ctrl.PlanOpen(ctx, connIDs[i])
		if err != nil {
			t.Fatalf("PlanOpen %d: %v", i, err)
		}
		if err := ctrl.SavePlan(plan); err != nil {
			t.Fatalf("SavePlan %d: %v", i, err)
		}
		op, err := ctrl.ApplyPlan(ctx, plan.ID)
		if err != nil {
			t.Fatalf("ApplyPlan %d: %v", i, err)
		}
		ops = append(ops, op)
	}

	// Next operation should be rejected due to concurrency limit
	// Wait a moment to ensure previous operations are still running
	time.Sleep(10 * time.Millisecond)

	// Verify at least one operation is still running
	runningCount := 0
	for _, op := range ops {
		if snap, ok := ctrl.GetOperation(op.ID); ok && snap.State == OperationStateRunning {
			runningCount++
		}
	}
	if runningCount == 0 {
		t.Skip("all operations completed too quickly to test concurrency limit")
	}

	plan, err := ctrl.PlanOpen(ctx, connIDs[GlobalOpLimit])
	if err != nil {
		t.Fatalf("PlanOpen beyond limit: %v", err)
	}
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	_, err = ctrl.ApplyPlan(ctx, plan.ID)
	if err == nil {
		t.Fatal("expected error applying plan beyond concurrency limit")
	}

	// Wait for all operations to complete
	deadline := time.Now().Add(10 * time.Second)
	for _, op := range ops {
		for time.Now().Before(deadline) {
			snap, ok := ctrl.GetOperation(op.ID)
			if !ok {
				break
			}
			if snap.State == OperationStateCompleted {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Now we should be able to start a new operation
	plan, err = ctrl.PlanOpen(ctx, connIDs[GlobalOpLimit+1])
	if err != nil {
		t.Fatalf("PlanOpen after limit freed: %v", err)
	}
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	_, err = ctrl.ApplyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan after completion: %v", err)
	}
}

// TestController_ExpiredPlanRejection tests that expired plans are rejected.
func TestController_ExpiredPlanRejection(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	ctrl := New(reg, newTestJournal())

	ctx := context.Background()

	profile := &core.ConnectionProfile{
		Name: "expired-test",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "localhost:8080", Protocol: core.ProtocolHTTP},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver: core.DriverSelection{ProviderID: "mock"},
		Desired:    core.DesiredOpen,
	}
	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	plan, err := ctrl.PlanOpen(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}

	// Set expiration in the past — SavePlan should reject expired plans.
	plan.ExpiresAt = time.Now().UTC().Add(-1 * time.Minute)
	plan.ComputeFingerprint()

	if err := ctrl.SavePlan(plan); err == nil {
		t.Fatal("expected error saving plan with past expiration")
	}
}

// TestController_ObservedFingerprintStalePlan tests that a plan whose
// ObservedFingerprint does not match the current provider state is
// rejected as stale.
func TestController_ObservedFingerprintStalePlan(t *testing.T) {
	prov := mock.New()
	reg := newTestRegistry(prov)
	ctrl := New(reg, newTestJournal())

	ctx := context.Background()

	profile := &core.ConnectionProfile{
		Name: "fingerprint-test",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "localhost:8080", Protocol: core.ProtocolHTTP},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver: core.DriverSelection{ProviderID: "mock"},
		Desired:    core.DesiredOpen,
	}
	if _, _, err := ctrl.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	connID := profile.ID

	// Plan open
	plan, err := ctrl.PlanOpen(ctx, connID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}

	// Observe to get a fresh observed fingerprint
	observed, obsErr := prov.Observe(ctx, connID)
	if obsErr != nil {
		t.Fatalf("Observe: %v", obsErr)
	}
	fp, fpErr := observed.ComputeFingerprint()
	if fpErr != nil {
		t.Fatalf("ComputeFingerprint: %v", fpErr)
	}
	plan.ObservedFingerprint = fp

	// Recompute fingerprint now that ObservedFingerprint is set.
	if err := plan.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}

	// Save and apply — should succeed because fingerprint matches.
	if err := ctrl.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	_, err = ctrl.ApplyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan with matching fingerprint: %v", err)
	}

	// Now create a second plan with a wrong ObservedFingerprint.
	plan2, err := ctrl.PlanOpen(ctx, connID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	plan2.ObservedFingerprint = "deadbeef-0000-0000-0000-000000000000"
	if err := plan2.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}

	// SavePlan verifies the fingerprint — since nothing changed
	// between ComputeFingerprint and this SavePlan call, VerifyFingerprint
	// should pass.
	if saveErr := ctrl.SavePlan(plan2); saveErr != nil {
		t.Fatalf("SavePlan: %v", saveErr)
	}
	_, err = ctrl.ApplyPlan(ctx, plan2.ID)
	if err == nil {
		t.Fatal("expected error applying plan with stale observed fingerprint")
	}
}
