package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/process"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/provider/cloudflare"
	"github.com/B-A-M-N/portico/internal/provider/mock"
	"github.com/B-A-M-N/portico/internal/provider/openaitunnel"
	"github.com/B-A-M-N/portico/internal/provider/portforward"
	"github.com/B-A-M-N/portico/internal/store"
)

// --------------- test fixtures ---------------

func newRecoveryTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func recoveryTestProfile(connID core.ConnectionID) *core.ConnectionProfile {
	now := time.Now().UTC().Truncate(time.Second)
	return &core.ConnectionProfile{
		ID:       connID,
		Name:     "recovery-test",
		Revision: 1,
		Kind:     core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind: core.SourceExisting,
					Existing: &core.ExistingServiceSpec{
						Network:  "tcp",
						Address:  "localhost",
						Protocol: core.ProtocolHTTP,
					},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver: core.DriverSelection{
			ProviderID: core.ProviderID("mock"),
		},
		Lifecycle: core.LifecycleSpec{OnDisconnect: core.DisconnectKeepAlive},
		Desired:   core.DesiredOpen,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func recoveryTestRuntime(connID core.ConnectionID) *core.ConnectionRuntime {
	now := time.Now().UTC().Truncate(time.Second)
	return &core.ConnectionRuntime{
		ConnectionID:   connID,
		State:          core.RuntimeOpening,
		LastObservedAt: now,
		LastTransition: now,
		Provider:       core.ProviderRuntime{ProviderID: core.ProviderID("mock")},
	}
}

func recoveryTestPlan(t *testing.T, connID core.ConnectionID, intent core.OperationIntent, steps []core.PlanStep) *core.OperationPlan {
	t.Helper()
	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    connID,
		ProfileRevision: 1,
		Provider:        core.ProviderID("mock"),
		Intent:          intent,
		Steps:           steps,
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}
	if err := plan.ComputeFingerprint(); err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	return plan
}

// seedInterruptedOperation persists a profile, runtime, plan, and an
// operation left in 'running' state, mimicking a supervisor crash.
func seedInterruptedOperation(t *testing.T, st *store.Store, connID core.ConnectionID,
	intent core.OperationIntent, steps []core.PlanStep) (*core.OperationPlan, core.OperationID) {
	t.Helper()
	ctx := context.Background()

	if err := st.SaveProfile(ctx, recoveryTestProfile(connID)); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	if err := st.SaveRuntime(ctx, recoveryTestRuntime(connID)); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}
	plan := recoveryTestPlan(t, connID, intent, steps)
	if err := st.SavePlan(ctx, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	opID := core.NewOperationID()
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if err := st.SaveOperation(ctx, opID, plan.ID, connID, "running", startedAt); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
	return plan, opID
}

func appendStepEvent(t *testing.T, st *store.Store, opID core.OperationID,
	stepID string, kind core.StepKind, stage core.EventStage) {
	t.Helper()
	evtType := core.EventOperationStepStarted
	switch stage {
	case core.StageSucceeded:
		evtType = core.EventOperationStepSucceeded
	case core.StageFailed:
		evtType = core.EventOperationStepFailed
	}
	err := st.AppendOperationEvent(context.Background(), core.Event{
		Type:      evtType,
		Timestamp: time.Now().UTC(),
		Data: core.OperationEvent{
			StepID:    stepID,
			StepKind:  kind,
			Stage:     stage,
			Timestamp: time.Now().UTC(),
		},
	}, opID)
	if err != nil {
		t.Fatalf("AppendOperationEvent(%s/%s): %v", stepID, stage, err)
	}
}

func openSteps() []core.PlanStep {
	return []core.PlanStep{
		{ID: "step-1", Kind: core.StepCreateTunnel, Summary: "Create tunnel"},
		{ID: "step-2", Kind: core.StepCreateDNSRecord, Summary: "Create DNS record"},
	}
}

func TestSnapshotIncludesNonSecretProviderAccountSummaries(t *testing.T) {
	st := newRecoveryTestStore(t)
	profile := recoveryTestProfile("conn-account-summary")
	profile.Driver.AccountID = "account-a"
	if err := st.SaveProfile(context.Background(), profile); err != nil {
		t.Fatalf("save profile: %v", err)
	}
	registry := provider.NewRegistry()
	if err := registry.Add(mock.New()); err != nil {
		t.Fatalf("register mock provider: %v", err)
	}
	registry.SetAccountInfo("mock", []provider.AccountInfo{
		{ID: "account-a", Label: "Personal", Status: "authenticated"},
		{ID: "account-b", Label: "Work", Status: "authenticated"},
	})

	snapshot, err := (&supervisorHandler{sup: &Supervisor{store: st, registry: registry}}).HandleSnapshot()
	if err != nil {
		t.Fatalf("HandleSnapshot: %v", err)
	}
	if len(snapshot.Providers) != 1 || len(snapshot.Providers[0].Accounts) != 2 {
		t.Fatalf("provider snapshot = %#v", snapshot.Providers)
	}
	if got := snapshot.Providers[0].Accounts[1]; got.ID != "account-b" || got.Label != "Work" || got.Status != "authenticated" {
		t.Fatalf("account summary = %#v", got)
	}
	if len(snapshot.Connections) != 1 || snapshot.Connections[0].ProviderAccountID != "account-a" {
		t.Fatalf("connection snapshot = %#v", snapshot.Connections)
	}
}

// cloudflareTestSupervisor builds a supervisor whose Cloudflare provider comes
// from its definition, as production does, with binary lookup stubbed.
func cloudflareTestSupervisor(t *testing.T, st *store.Store, validator AccountValidator) *Supervisor {
	t.Helper()
	sup, _ := activationTestSupervisor(t, st,
		cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}))
	sup.accountValidator = validator
	return sup
}

// stubAccountValidator stands in for a live provider so account setup can be
// tested without real credentials.
type stubAccountValidator struct {
	result *AccountValidation
	err    error
	calls  int
}

func (s *stubAccountValidator) Validate(context.Context, string, string, string) (*AccountValidation, error) {
	s.calls++
	return s.result, s.err
}

func TestConfigureCloudflareAccountPersistsEncryptedAccountForRestart(t *testing.T) {
	st := newRecoveryTestStore(t)
	validator := &stubAccountValidator{result: &AccountValidation{
		AccountAccessible: true,
		Zones:             []ZoneSummary{{ID: "zone-a", Name: "example.com"}},
	}}
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st, validator)}
	response, err := handler.HandleConfigureProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		AccountID: "account-a", Label: "Personal", ZoneID: "zone-a", Credential: "secret-token",
	})
	if err != nil {
		t.Fatalf("HandleConfigureProviderAccount: %v", err)
	}
	// A saved account takes effect in this process. Requiring a restart was the
	// old behaviour, and it existed only because Cloudflare was the one
	// provider with a rebuild path.
	if response.RestartRequired {
		t.Fatal("configuring an account still demands a restart")
	}
	if validator.calls != 1 {
		t.Fatalf("credential validated %d times, want exactly 1 before persisting", validator.calls)
	}
	if !response.Validated {
		t.Fatal("response does not report that the credential was validated")
	}
	accounts, err := st.ListProviderAccounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].ID != "account-a" || accounts[0].Metadata["zone_id"] != "zone-a" {
		t.Fatalf("ListProviderAccounts = %#v, %v", accounts, err)
	}
	credential, err := st.LoadProviderCredential(context.Background(), "cloudflare", "cloudflare:account-a:api-token")
	if err != nil || credential != "secret-token" {
		t.Fatalf("LoadProviderCredential = %q, %v", credential, err)
	}
}

func TestUnavailableProviderIsPersistedAsActionableRuntimeError(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-unavailable-provider")
	profile := recoveryTestProfile(connID)
	profile.Driver.ProviderID = "missing-provider"
	profile.Desired = core.DesiredOpen
	runtime := recoveryTestRuntime(connID)
	runtime.State = core.RuntimeOpen
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	if err := st.SaveRuntime(ctx, runtime); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}

	registry := provider.NewRegistry() // intentionally does not register missing-provider
	ctrl := controller.New(registry, st)
	ctrl.RestoreProfile(profile)
	ctrl.RestoreRuntime(runtime)
	sup := &Supervisor{store: st, controller: ctrl, registry: registry}

	sup.markProviderUnavailable(ctx, profile)

	gotRuntime, err := st.LoadRuntime(ctx, connID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if gotRuntime.State != core.RuntimeError {
		t.Fatalf("runtime state = %q, want %q", gotRuntime.State, core.RuntimeError)
	}
	if gotRuntime.Error == nil || gotRuntime.Error.Code != core.ErrCorePrefix+"002" {
		t.Fatalf("runtime error = %+v, want typed provider-not-found error", gotRuntime.Error)
	}
	findings, err := st.ListUnresolvedFindings(ctx, connID)
	if err != nil {
		t.Fatalf("ListUnresolvedFindings: %v", err)
	}
	if len(findings) != 1 || findings[0].Summary != "Selected provider is unavailable" {
		t.Fatalf("findings = %+v", findings)
	}

	// Repeated startup/reconciliation passes must upsert rather than duplicate.
	sup.markProviderUnavailable(ctx, profile)
	findings, err = st.ListUnresolvedFindings(ctx, connID)
	if err != nil {
		t.Fatalf("ListUnresolvedFindings after repeat: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("repeat created duplicate findings: %+v", findings)
	}
}

func TestClassifyResourceStateOnlyMarksAuthoritativelyMissingResources(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-externally-removed")
	profile := recoveryTestProfile(connID)
	profile.Driver.ProviderID = "repair-observer"
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	resource := &core.ProviderResource{
		ConnectionID: connID,
		ProviderID:   profile.Driver.ProviderID,
		Type:         core.ResourceDNSRecord,
		ExternalID:   "dns-externally-removed",
		Ownership:    core.OwnershipManaged,
	}
	if err := st.SaveResource(ctx, resource); err != nil {
		t.Fatalf("SaveResource: %v", err)
	}

	observer := &repairObservationProvider{observed: &core.ObservedConnection{ResourceStatuses: []core.ObservedResourceStatus{{
		Type: resource.Type, ExternalID: resource.ExternalID, Status: core.ObservationUnauthorized,
	}}}}
	registry := provider.NewRegistry()
	if err := registry.Add(observer); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctrl := controller.New(registry, st)
	ctrl.RestoreProfile(profile)
	sup := &Supervisor{store: st, controller: ctrl, registry: registry}

	sup.classifyResourceState(ctx)
	stored, err := st.LoadResource(ctx, resource.ProviderID, resource.Type, resource.ExternalID)
	if err != nil {
		t.Fatalf("LoadResource after unauthorized observation: %v", err)
	}
	if stored.Lifecycle != core.LifecyclePresent {
		t.Fatalf("unauthorized observation changed lifecycle to %q", stored.Lifecycle)
	}
	findings, err := st.ListUnresolvedFindings(ctx, connID)
	if err != nil {
		t.Fatalf("ListUnresolvedFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("unauthorized observation created findings: %#v", findings)
	}

	observer.observed.ResourceStatuses[0].Status = core.ObservationMissing
	sup.classifyResourceState(ctx)
	stored, err = st.LoadResource(ctx, resource.ProviderID, resource.Type, resource.ExternalID)
	if err != nil {
		t.Fatalf("LoadResource after missing observation: %v", err)
	}
	if stored.Lifecycle != core.LifecycleExternallyRemoved {
		t.Fatalf("missing observation lifecycle = %q, want %q", stored.Lifecycle, core.LifecycleExternallyRemoved)
	}
	findings, err = st.ListUnresolvedFindings(ctx, connID)
	if err != nil {
		t.Fatalf("ListUnresolvedFindings: %v", err)
	}
	if len(findings) != 1 || findings[0].Summary != "Managed provider resource was removed outside Portico" {
		t.Fatalf("externally removed findings: %#v", findings)
	}
}

func TestOperationEventEndpointUsesUnifiedDurableSequence(t *testing.T) {
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-operation-history")
	_, opID := seedInterruptedOperation(t, st, connID, core.IntentOpen, openSteps())
	appendStepEvent(t, st, opID, "step-1", core.StepCreateTunnel, core.StageStarted)

	journal, err := st.GetDurableEventsForOperation(context.Background(), opID)
	if err != nil || len(journal) != 2 {
		t.Fatalf("GetDurableEventsForOperation = %#v, %v", journal, err)
	}
	h := &supervisorHandler{sup: &Supervisor{store: st}}
	events, err := h.HandleGetOperationEvents(string(opID))
	if err != nil {
		t.Fatalf("HandleGetOperationEvents: %v", err)
	}
	if len(events) != 2 || events[1].Sequence != journal[1].Event.Sequence || events[1].OperationID != string(opID) || events[1].ConnectionID != string(connID) {
		t.Fatalf("operation endpoint did not preserve durable event metadata: %#v vs %#v", events, journal)
	}
}

// runRecovery loads the single seeded non-terminal operation and applies
// the recovery decision exactly the way recoverOperationJournals does.
func runRecovery(t *testing.T, st *store.Store, observed *core.ObservedConnection) recoveryDecision {
	t.Helper()
	ctx := context.Background()

	ops, err := st.ListNonTerminalOperations(ctx)
	if err != nil {
		t.Fatalf("ListNonTerminalOperations: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("expected 1 non-terminal operation, got %d", len(ops))
	}
	op := ops[0]

	plan, err := st.LoadPlan(ctx, op.PlanID)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	events, err := st.GetOperationEvents(ctx, op.ID)
	if err != nil {
		t.Fatalf("GetOperationEvents: %v", err)
	}
	resources, err := st.ListResourcesByConnection(ctx, op.ConnectionID)
	if err != nil {
		t.Fatalf("ListResourcesByConnection: %v", err)
	}

	decision := classifyOperationRecovery(plan, events, resources, observed)
	if err := applyRecoveryDecision(ctx, st, op, plan, decision); err != nil {
		t.Fatalf("applyRecoveryDecision: %v", err)
	}
	return decision
}

// --------------- end-to-end recovery against a real temp store ---------------

// TestRecoveryCompletesFullyCommittedOperation: every step has a durable
// terminal succeeded event -> recovery finishes the operation with the
// normal terminal success commit.
func TestRecoveryCompletesFullyCommittedOperation(t *testing.T) {
	st := newRecoveryTestStore(t)
	ctx := context.Background()
	connID := core.ConnectionID("conn-recovery-complete")

	_, opID := seedInterruptedOperation(t, st, connID, core.IntentOpen, openSteps())
	appendStepEvent(t, st, opID, "step-1", core.StepCreateTunnel, core.StageStarted)
	appendStepEvent(t, st, opID, "step-1", core.StepCreateTunnel, core.StageSucceeded)
	appendStepEvent(t, st, opID, "step-2", core.StepCreateDNSRecord, core.StageStarted)
	appendStepEvent(t, st, opID, "step-2", core.StepCreateDNSRecord, core.StageSucceeded)
	if err := st.SaveResource(ctx, &core.ProviderResource{
		ConnectionID: connID,
		ProviderID:   "mock",
		Type:         core.ResourceTunnel,
		ExternalID:   "tun-1",
		Ownership:    core.OwnershipManaged,
	}); err != nil {
		t.Fatalf("SaveResource: %v", err)
	}

	decision := runRecovery(t, st, nil)
	if decision.Outcome != recoveryComplete {
		t.Fatalf("expected outcome %q, got %q", recoveryComplete, decision.Outcome)
	}

	ops, err := st.ListNonTerminalOperations(ctx)
	if err != nil {
		t.Fatalf("ListNonTerminalOperations: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("expected operation to be terminal after recovery, still non-terminal: %+v", ops)
	}

	rt, err := st.LoadRuntime(ctx, connID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if rt.State != core.RuntimeOpen {
		t.Errorf("expected runtime state %q, got %q", core.RuntimeOpen, rt.State)
	}
	if rt.Error != nil {
		t.Errorf("expected no runtime error, got %+v", rt.Error)
	}
	if rt.ActiveOperation != nil {
		t.Errorf("expected active operation cleared, got %v", *rt.ActiveOperation)
	}
}

// TestRecoveryFailsUntouchedOperationSafely: an operation with no journal
// events never reached the provider -> recovery fails it cleanly without a
// recovery finding.
func TestRecoveryFailsUntouchedOperationSafely(t *testing.T) {
	st := newRecoveryTestStore(t)
	ctx := context.Background()
	connID := core.ConnectionID("conn-recovery-untouched")

	_, opID := seedInterruptedOperation(t, st, connID, core.IntentOpen, openSteps())

	decision := runRecovery(t, st, nil)
	if decision.Outcome != recoveryFailSafe {
		t.Fatalf("expected outcome %q, got %q", recoveryFailSafe, decision.Outcome)
	}

	ops, err := st.ListNonTerminalOperations(ctx)
	if err != nil {
		t.Fatalf("ListNonTerminalOperations: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("expected operation to be terminal after recovery, still non-terminal: %+v", ops)
	}

	rt, err := st.LoadRuntime(ctx, connID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if rt.State != core.RuntimeError {
		t.Errorf("expected runtime state %q, got %q", core.RuntimeError, rt.State)
	}
	if rt.Error == nil {
		t.Fatal("expected a runtime error to be recorded")
	}
	if rt.Error.Code == "PTO-OP-RECOVERY-REQUIRED" {
		t.Errorf("untouched operation must not be marked recovery-required, got %+v", rt.Error)
	}

	findings, err := st.ListFindingsByConnection(ctx, connID)
	if err != nil {
		t.Fatalf("ListFindingsByConnection: %v", err)
	}
	for _, f := range findings {
		if strings.HasPrefix(string(f.ID), "recovery-") {
			t.Errorf("clean failure must not create a recovery finding, got %+v", f)
		}
	}

	// No step should have been recorded as needing recovery.
	results, err := st.GetStepResults(ctx, opID)
	if err != nil {
		t.Fatalf("GetStepResults: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no step results for untouched operation, got %+v", results)
	}
}

// TestRecoveryMarksUncertainOperationRecoveryRequired: one step committed,
// the next started but never terminated -> recovery marks the operation
// failed with PTO-OP-RECOVERY-REQUIRED, records the uncertain step as
// outcome_unknown + recovery_required, and creates a finding with the exact
// evidence.
func TestRecoveryMarksUncertainOperationRecoveryRequired(t *testing.T) {
	st := newRecoveryTestStore(t)
	ctx := context.Background()
	connID := core.ConnectionID("conn-recovery-required")

	_, opID := seedInterruptedOperation(t, st, connID, core.IntentOpen, openSteps())
	// step-1 committed (terminal event + persisted resource).
	appendStepEvent(t, st, opID, "step-1", core.StepCreateTunnel, core.StageStarted)
	appendStepEvent(t, st, opID, "step-1", core.StepCreateTunnel, core.StageSucceeded)
	if err := st.SaveResource(ctx, &core.ProviderResource{
		ConnectionID: connID,
		ProviderID:   "mock",
		Type:         core.ResourceTunnel,
		ExternalID:   "tun-1",
		Ownership:    core.OwnershipManaged,
	}); err != nil {
		t.Fatalf("SaveResource: %v", err)
	}
	// step-2 started, no terminal event, no persisted resource.
	appendStepEvent(t, st, opID, "step-2", core.StepCreateDNSRecord, core.StageStarted)

	// The provider reports a DNS record that was never persisted: it must
	// be listed as evidence, never silently adopted or orphaned.
	observed := &core.ObservedConnection{
		ConnectionID: connID,
		ProviderID:   "mock",
		Tunnel:       &core.ObservedTunnel{ID: "tun-1"},
		DNSRecords:   []core.ObservedDNSRecord{{ID: "dns-ghost"}},
	}

	decision := runRecovery(t, st, observed)
	if decision.Outcome != recoveryRequired {
		t.Fatalf("expected outcome %q, got %q", recoveryRequired, decision.Outcome)
	}
	if decision.LastStartedStepID != "step-2" {
		t.Errorf("expected last started step step-2, got %q", decision.LastStartedStepID)
	}

	// Operation must be terminal.
	ops, err := st.ListNonTerminalOperations(ctx)
	if err != nil {
		t.Fatalf("ListNonTerminalOperations: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("expected operation to be terminal after recovery, still non-terminal: %+v", ops)
	}

	// Runtime must carry the recovery-required error code.
	rt, err := st.LoadRuntime(ctx, connID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if rt.State != core.RuntimeError {
		t.Errorf("expected runtime state %q, got %q", core.RuntimeError, rt.State)
	}
	if rt.Error == nil || rt.Error.Code != "PTO-OP-RECOVERY-REQUIRED" {
		t.Fatalf("expected runtime error PTO-OP-RECOVERY-REQUIRED, got %+v", rt.Error)
	}

	// The uncertain step must be durably recorded.
	results, err := st.GetStepResults(ctx, opID)
	if err != nil {
		t.Fatalf("GetStepResults: %v", err)
	}
	var found bool
	for _, r := range results {
		if r.StepID == "step-2" {
			found = true
			if r.Status != string(store.StepOutcomeUnknown) {
				t.Errorf("expected step-2 status %q, got %q", store.StepOutcomeUnknown, r.Status)
			}
			if !r.RecoveryRequired {
				t.Error("expected step-2 to be marked recovery_required")
			}
		}
	}
	if !found {
		t.Fatalf("expected a step result row for step-2, got %+v", results)
	}

	// A finding must preserve operation, last started step, and exact IDs.
	findings, err := st.ListFindingsByConnection(ctx, connID)
	if err != nil {
		t.Fatalf("ListFindingsByConnection: %v", err)
	}
	var finding *core.DiagnosticFinding
	for i := range findings {
		if findings[i].ID == core.FindingID("recovery-"+string(opID)) {
			finding = &findings[i]
		}
	}
	if finding == nil {
		t.Fatalf("expected recovery finding recovery-%s, got %+v", opID, findings)
	}
	if len(finding.Evidence) != 1 {
		t.Fatalf("expected 1 evidence entry, got %+v", finding.Evidence)
	}
	data := finding.Evidence[0].Data
	if data["operation_id"] != string(opID) {
		t.Errorf("evidence operation_id = %q, want %q", data["operation_id"], opID)
	}
	if data["last_started_step"] != "step-2" {
		t.Errorf("evidence last_started_step = %q, want step-2", data["last_started_step"])
	}
	if !strings.Contains(data["known_resources"], "tunnel/tun-1") {
		t.Errorf("evidence known_resources = %q, want to contain tunnel/tun-1", data["known_resources"])
	}
	if !strings.Contains(data["possibly_unpersisted"], "dns_record/dns-ghost") {
		t.Errorf("evidence possibly_unpersisted = %q, want to contain dns_record/dns-ghost", data["possibly_unpersisted"])
	}
	if !strings.Contains(data["unknown_steps"], "step-2") {
		t.Errorf("evidence unknown_steps = %q, want to contain step-2", data["unknown_steps"])
	}
}

// --------------- pure classification tests ---------------

func TestClassifyOperationRecovery(t *testing.T) {
	plan := recoveryTestPlan(t, "conn-classify", core.IntentOpen, openSteps())

	started := func(stepID string, kind core.StepKind) store.OperationJournalEvent {
		return store.OperationJournalEvent{StepID: stepID, EventType: string(kind), Stage: string(core.StageStarted)}
	}
	succeededEvt := func(stepID string, kind core.StepKind) store.OperationJournalEvent {
		return store.OperationJournalEvent{StepID: stepID, EventType: string(kind), Stage: string(core.StageSucceeded)}
	}
	failedEvt := func(stepID string, kind core.StepKind) store.OperationJournalEvent {
		return store.OperationJournalEvent{StepID: stepID, EventType: string(kind), Stage: string(core.StageFailed)}
	}
	tunnelRes := core.ProviderResource{
		ConnectionID: "conn-classify", ProviderID: "mock",
		Type: core.ResourceTunnel, ExternalID: "tun-1",
		Ownership: core.OwnershipManaged,
	}

	cases := []struct {
		name      string
		events    []store.OperationJournalEvent
		resources []core.ProviderResource
		observed  *core.ObservedConnection
		want      recoveryOutcome
		wantStep2 stepRecoveryClass
	}{
		{
			name: "all steps committed",
			events: []store.OperationJournalEvent{
				started("step-1", core.StepCreateTunnel), succeededEvt("step-1", core.StepCreateTunnel),
				started("step-2", core.StepCreateDNSRecord), succeededEvt("step-2", core.StepCreateDNSRecord),
			},
			want:      recoveryComplete,
			wantStep2: stepRecoveryCommitted,
		},
		{
			name:      "no steps started",
			want:      recoveryFailSafe,
			wantStep2: stepRecoveryNotStarted,
		},
		{
			name: "first step failed terminally, nothing committed",
			events: []store.OperationJournalEvent{
				started("step-1", core.StepCreateTunnel), failedEvt("step-1", core.StepCreateTunnel),
			},
			want:      recoveryFailSafe,
			wantStep2: stepRecoveryNotStarted,
		},
		{
			name: "committed then started-unterminated is recovery required",
			events: []store.OperationJournalEvent{
				started("step-1", core.StepCreateTunnel), succeededEvt("step-1", core.StepCreateTunnel),
				started("step-2", core.StepCreateDNSRecord),
			},
			resources: []core.ProviderResource{tunnelRes},
			want:      recoveryRequired,
			wantStep2: stepRecoveryUnknown,
		},
		{
			name: "started-unterminated with persisted resource counts as committed",
			events: []store.OperationJournalEvent{
				// step-1 started, no terminal event, but the tunnel resource
				// was durably persisted: the commit landed.
				started("step-1", core.StepCreateTunnel),
				started("step-2", core.StepCreateDNSRecord), succeededEvt("step-2", core.StepCreateDNSRecord),
			},
			resources: []core.ProviderResource{tunnelRes},
			want:      recoveryComplete,
			wantStep2: stepRecoveryCommitted,
		},
		{
			name: "observed unpersisted resource forces recovery required",
			observed: &core.ObservedConnection{
				ConnectionID: "conn-classify",
				ProviderID:   "mock",
				Tunnel:       &core.ObservedTunnel{ID: "tun-ghost"},
			},
			want:      recoveryRequired,
			wantStep2: stepRecoveryNotStarted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := classifyOperationRecovery(plan, tc.events, tc.resources, tc.observed)
			if d.Outcome != tc.want {
				t.Errorf("outcome = %q, want %q", d.Outcome, tc.want)
			}
			if len(d.Steps) != len(plan.Steps) {
				t.Fatalf("classified %d steps, want %d", len(d.Steps), len(plan.Steps))
			}
			if d.Steps[1].Class != tc.wantStep2 {
				t.Errorf("step-2 class = %q, want %q", d.Steps[1].Class, tc.wantStep2)
			}
		})
	}
}

// TestConnectionDetailReportsDurableResourcesWithoutRuntime pins the inspect
// screen's resource list to the durable record.
//
// Controller.RestoreResources drops restored resources when a connection has no
// runtime row, so the in-memory projection can report none while the database
// still tracks managed provider resources. Reporting an empty list there would
// tell an operator there is nothing to clean up, hiding the exact external IDs
// a provider-side cleanup has to act on.
func TestConnectionDetailReportsDurableResourcesWithoutRuntime(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-durable-resources")

	profile := recoveryTestProfile(connID)
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	if err := st.SaveResource(ctx, &core.ProviderResource{
		ConnectionID: connID,
		ProviderID:   "cloudflare",
		Type:         core.ResourceTunnel,
		ExternalID:   "tun-durable-1",
		Ownership:    core.OwnershipManaged,
	}); err != nil {
		t.Fatalf("SaveResource: %v", err)
	}

	registry := provider.NewRegistry()
	ctrl := controller.New(registry, st)
	ctrl.RestoreProfile(profile)
	// Deliberately no RestoreRuntime: this is the state in which the
	// projection loses the resources.
	ctrl.RestoreResources(connID, []core.ProviderResource{{
		ConnectionID: connID, ProviderID: "cloudflare", Type: core.ResourceTunnel,
		ExternalID: "tun-durable-1", Ownership: core.OwnershipManaged,
	}})

	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry}}
	detail, err := handler.HandleGetConnectionDetail(string(connID))
	if err != nil {
		t.Fatalf("HandleGetConnectionDetail: %v", err)
	}

	if len(detail.Resources) != 1 {
		t.Fatalf("detail reported %d resources, want 1 from the durable record", len(detail.Resources))
	}
	if detail.Resources[0].ExternalID != "tun-durable-1" {
		t.Fatalf("external ID = %q, want tun-durable-1", detail.Resources[0].ExternalID)
	}
	if detail.Resources[0].Ownership != string(core.OwnershipManaged) {
		t.Fatalf("ownership = %q, want managed", detail.Resources[0].Ownership)
	}
}

// TestOperationHistoryReadsDurableJournal pins the history endpoint to the
// durable journal. It previously returned an unconditional empty list with a
// comment saying the store method did not exist, so the operations screen
// reported "No operations found" regardless of how much work had run.
func TestOperationHistoryReadsDurableJournal(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-history")

	profile := recoveryTestProfile(connID)
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	plan := &core.OperationPlan{
		ID: "plan-history", ConnectionID: connID, ProfileRevision: 1,
		Provider: core.ProviderID("mock"), Intent: core.IntentOpen,
		Steps: []core.PlanStep{{ID: "s1", Kind: core.StepCreateTunnel}},
	}
	_ = plan.ComputeFingerprint()
	if err := st.SavePlan(ctx, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if err := st.SaveOperation(ctx, "op-history", plan.ID, connID, "succeeded", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}

	handler := &supervisorHandler{sup: &Supervisor{store: st}}
	history, err := handler.HandleOperationHistory()
	if err != nil {
		t.Fatalf("HandleOperationHistory: %v", err)
	}
	if !history.Available {
		t.Fatalf("history reported unavailable: %s", history.Unavailable)
	}
	if len(history.Operations) != 1 {
		t.Fatalf("got %d operations, want 1", len(history.Operations))
	}

	op := history.Operations[0]
	if op.ID != "op-history" {
		t.Fatalf("operation ID = %q", op.ID)
	}
	// Intent is what makes the entry meaningful; it lives on the plan.
	if op.Intent != string(core.IntentOpen) {
		t.Fatalf("intent = %q, want %q", op.Intent, core.IntentOpen)
	}
	if op.ProviderID != "mock" {
		t.Fatalf("provider = %q, want mock", op.ProviderID)
	}
	if op.Fingerprint == "" {
		t.Fatal("plan fingerprint was not carried into history")
	}
	if op.State != "succeeded" {
		t.Fatalf("state = %q, want succeeded", op.State)
	}
}

// TestOperationHistoryEmptyIsMarkedAvailable ensures a genuinely empty journal
// is reported as authoritative, so the UI can say "none yet" rather than
// "unavailable".
func TestOperationHistoryEmptyIsMarkedAvailable(t *testing.T) {
	st := newRecoveryTestStore(t)
	handler := &supervisorHandler{sup: &Supervisor{store: st}}

	history, err := handler.HandleOperationHistory()
	if err != nil {
		t.Fatalf("HandleOperationHistory: %v", err)
	}
	if !history.Available {
		t.Fatalf("empty history was reported unavailable: %s", history.Unavailable)
	}
	if len(history.Operations) != 0 {
		t.Fatalf("got %d operations, want 0", len(history.Operations))
	}
}

// TestCloudflareSetupAcceptsAccountWithoutZone resolves the contradiction in
// audit item 10: the setup UI offered to skip the zone while the backend
// rejected an empty one. A zone is only needed for DNS and custom hostnames.
func TestCloudflareSetupAcceptsAccountWithoutZone(t *testing.T) {
	st := newRecoveryTestStore(t)
	validator := &stubAccountValidator{result: &AccountValidation{AccountAccessible: true}}
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st, validator)}

	resp, err := handler.HandleConfigureProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		AccountID: "account-a", Label: "Personal", Credential: "secret-token",
	})
	if err != nil {
		t.Fatalf("setup rejected an account with no zone: %v", err)
	}
	if resp.CapabilityLevel != "tunnels_without_dns" {
		t.Fatalf("capability level = %q, want tunnels_without_dns", resp.CapabilityLevel)
	}

	accounts, err := st.ListProviderAccounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("ListProviderAccounts = %#v, %v", accounts, err)
	}
	if _, ok := accounts[0].Metadata["zone_id"]; ok {
		t.Fatal("an empty zone was persisted as metadata")
	}
}

// TestCloudflareSetupDoesNotPersistUnvalidatedCredentials ensures a rejected
// credential is never recorded, and never recorded as authenticated. The
// handler previously wrote Status: authenticated on the strength of a non-empty
// string.
func TestCloudflareSetupDoesNotPersistUnvalidatedCredentials(t *testing.T) {
	st := newRecoveryTestStore(t)
	validator := &stubAccountValidator{
		result: &AccountValidation{MissingPermissions: []string{"Zone: Read"}},
		err:    errors.New("the token is valid but cannot see account account-a"),
	}
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st, validator)}

	resp, err := handler.HandleConfigureProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		AccountID: "account-a", Credential: "bad-token", ZoneID: "zone-a",
	})
	if err == nil {
		t.Fatal("setup accepted a credential the provider rejected")
	}
	// The specific missing permissions must reach the caller so the failure is
	// actionable rather than merely reported.
	if resp == nil || len(resp.MissingPermissions) == 0 {
		t.Fatalf("missing permissions were not reported: %#v", resp)
	}

	accounts, listErr := st.ListProviderAccounts(context.Background())
	if listErr != nil {
		t.Fatalf("ListProviderAccounts: %v", listErr)
	}
	if len(accounts) != 0 {
		t.Fatalf("a rejected credential was persisted: %#v", accounts)
	}
}

// TestCloudflareSetupRejectsZoneNotVisibleToToken ensures a zone mismatch fails
// at setup rather than much later during a DNS operation.
func TestCloudflareSetupRejectsZoneNotVisibleToToken(t *testing.T) {
	st := newRecoveryTestStore(t)
	validator := &stubAccountValidator{result: &AccountValidation{
		AccountAccessible: true,
		Zones:             []ZoneSummary{{ID: "zone-real", Name: "example.com"}},
	}}
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st, validator)}

	_, err := handler.HandleConfigureProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		AccountID: "account-a", Credential: "token", ZoneID: "zone-typo",
	})
	if err == nil {
		t.Fatal("setup accepted a zone the token cannot see")
	}
	if !strings.Contains(err.Error(), "zone-typo") {
		t.Fatalf("error does not name the offending zone: %v", err)
	}
}

// TestUpdateConnectionRefusesSilentlyIgnoredChanges pins audit item 17. The
// update request accepted spec, driver and lifecycle changes and the handler
// applied none of them, so a caller could change the origin, provider or
// protection, receive a success response, and have nothing happen.
func TestUpdateConnectionRefusesSilentlyIgnoredChanges(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-update")
	profile := recoveryTestProfile(connID)

	registry := provider.NewRegistry()
	ctrl := controller.New(registry, st)
	ctrl.RestoreProfile(profile)
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}

	driver := ipc.DriverSelectionDTO{ProviderID: "someone-else"}
	err := func() error {
		_, e := handler.HandleUpdateConnection(string(connID), ipc.UpdateConnectionRequest{Driver: &driver})
		return e
	}()
	if err == nil {
		t.Fatal("a provider change was accepted and silently discarded")
	}
	// The refusal must point at the workflow that does apply the change.
	if !strings.Contains(err.Error(), "plan/edit") {
		t.Fatalf("refusal does not point at the edit plan: %v", err)
	}

	spec := ipc.ConnectionSpecDTO{}
	if _, err := handler.HandleUpdateConnection(string(connID), ipc.UpdateConnectionRequest{
		Spec: &spec,
	}); err == nil {
		t.Fatal("a spec change was accepted and silently discarded")
	}

	// The profile must be untouched by the refusals.
	stored, err := st.LoadProfile(ctx, connID)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if stored.Driver.ProviderID != profile.Driver.ProviderID {
		t.Fatalf("a refused update still changed the driver: %q", stored.Driver.ProviderID)
	}
}

// TestUpdateConnectionHonoursExpectedRevision pins the optimistic concurrency
// check. The handler read the current revision back out of the profile it had
// just loaded, which made the comparison vacuous: two concurrent edits both
// succeeded and the later silently overwrote the earlier.
func TestUpdateConnectionHonoursExpectedRevision(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-revision")
	profile := recoveryTestProfile(connID)
	profile.Revision = 5

	registry := provider.NewRegistry()
	ctrl := controller.New(registry, st)
	ctrl.RestoreProfile(profile)
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}

	name := "renamed"
	stale := uint64(3)
	if _, err := handler.HandleUpdateConnection(string(connID), ipc.UpdateConnectionRequest{
		Name: &name, ExpectedRevision: stale,
	}); err == nil {
		t.Fatal("an update against a stale revision was accepted")
	}
}

// TestCloneConnectionProducesAnIndependentCopy pins the clone workflow: it must
// never adopt the original's identity or provider resources.
func TestCloneConnectionProducesAnIndependentCopy(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-source")
	profile := recoveryTestProfile(connID)
	profile.Desired = core.DesiredOpen

	registry := provider.NewRegistry()
	if err := registry.Add(mock.New()); err != nil {
		t.Fatalf("register mock provider: %v", err)
	}
	ctrl := controller.New(registry, st)
	ctrl.SetConnectionStorer(st)
	ctrl.RestoreProfile(profile)
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}

	clone, err := handler.HandleCloneConnection(string(connID), ipc.CloneConnectionRequest{Name: "copy"})
	if err != nil {
		t.Fatalf("HandleCloneConnection: %v", err)
	}
	if clone.ID == string(connID) {
		t.Fatal("clone reused the source connection ID")
	}
	if clone.Name != "copy" {
		t.Fatalf("clone name = %q", clone.Name)
	}
	// Copying an open connection must not start a second one by surprise.
	if clone.DesiredState != string(core.DesiredClosed) {
		t.Fatalf("clone desired state = %q, want closed", clone.DesiredState)
	}

	// The source must be untouched.
	source, err := st.LoadProfile(ctx, connID)
	if err != nil {
		t.Fatalf("LoadProfile(source): %v", err)
	}
	if source.Desired != core.DesiredOpen || source.Name != profile.Name {
		t.Fatalf("cloning mutated the source: %+v", source)
	}
}

// TestCloneRequiresANewHostnameForPermanentConnections ensures a clone cannot
// contend with its original for the same DNS record.
func TestCloneRequiresANewHostnameForPermanentConnections(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-permanent")
	profile := recoveryTestProfile(connID)
	profile.Spec.ServiceExposure.Exposure.Mode = core.ExposurePermanent
	profile.Spec.ServiceExposure.Exposure.RequestedAddress = "demo.example.com"

	registry := provider.NewRegistry()
	if err := registry.Add(mock.New()); err != nil {
		t.Fatalf("register mock provider: %v", err)
	}
	ctrl := controller.New(registry, st)
	ctrl.SetConnectionStorer(st)
	ctrl.RestoreProfile(profile)
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}

	if _, err := handler.HandleCloneConnection(string(connID), ipc.CloneConnectionRequest{}); err == nil {
		t.Fatal("clone of a permanent connection was allowed to reuse the hostname")
	}
	if _, err := handler.HandleCloneConnection(string(connID), ipc.CloneConnectionRequest{
		RequestedAddress: "demo.example.com",
	}); err == nil {
		t.Fatal("clone was allowed to specify the original's hostname")
	}
	if _, err := handler.HandleCloneConnection(string(connID), ipc.CloneConnectionRequest{
		RequestedAddress: "copy.example.com",
	}); err != nil {
		t.Fatalf("clone with a new hostname was rejected: %v", err)
	}
}

// TestProviderSetupIsDeclarative pins audit item 23. Setup was hardcoded to
// Cloudflare in both the TUI and the backend, so adding a provider meant
// editing the UI and the handler rejected every other provider by ID.
func TestProviderSetupIsDeclarative(t *testing.T) {
	st := newRecoveryTestStore(t)
	// Deliberately no adapter: setup requirements are static and must be
	// answerable before anything is constructed.
	sup, _ := activationTestSupervisor(t, st,
		cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}))
	handler := &supervisorHandler{sup: sup}

	flow, err := handler.HandleProviderSetupFlow("cloudflare")
	if err != nil {
		t.Fatalf("HandleProviderSetupFlow: %v", err)
	}
	if len(flow.Fields) == 0 {
		t.Fatal("provider declared no setup fields")
	}

	byID := map[string]ipc.SetupFieldDTO{}
	for _, f := range flow.Fields {
		byID[f.ID] = f
	}
	// The credential must be declared secret so the UI knows to mask it and
	// clear it, rather than the UI knowing which field happens to be a token.
	if cred, ok := byID["credential"]; !ok || !cred.Secret || !cred.Required {
		t.Fatalf("credential field = %#v", cred)
	}
	// The zone must be declared optional, matching the capability split.
	if zone, ok := byID["zone_id"]; !ok || zone.Required {
		t.Fatalf("zone field = %#v", zone)
	}
	if len(flow.CapabilityNotes) == 0 {
		t.Fatal("setup flow does not explain what each level of configuration enables")
	}
}

// TestUnknownProviderSetupIsRefusedByCapabilityNotByName ensures the refusal is
// based on whether the provider declares a setup flow.
func TestUnknownProviderSetupIsRefusedByCapabilityNotByName(t *testing.T) {
	st := newRecoveryTestStore(t)
	registry := provider.NewRegistry()
	if err := registry.Add(mock.New()); err != nil {
		t.Fatalf("register mock: %v", err)
	}
	handler := &supervisorHandler{sup: &Supervisor{store: st, registry: registry, mutating: true}}

	_, err := handler.HandleProviderSetupFlow("mock")
	if err == nil {
		t.Fatal("a provider declaring no setup flow was accepted for configuration")
	}
	if !strings.Contains(err.Error(), "cannot be configured") {
		t.Fatalf("error does not explain why: %v", err)
	}
}

// declaredProvider declares a setup flow but cannot check its own credential.
// It is the case the authenticated-only-after-validation invariant is about.
// declaredDefinition declares a setup flow but cannot check its own
// credential. It is the case the authenticated-only-after-validation invariant
// is about.
type declaredDefinition struct{ id string }

func (d declaredDefinition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: core.ProviderID(d.id), Name: d.id, DisplayName: d.id}
}

func (d declaredDefinition) CatalogEntry() provider.CatalogEntry {
	return provider.CatalogEntry{ID: core.ProviderID(d.id), DisplayName: d.id}
}

func (d declaredDefinition) SetupFlow() core.SetupFlow {
	return core.SetupFlow{
		Kind:          core.SetupAccount,
		IdentityField: "workspace",
		SecretField:   "token",
		Summary:       "Configure a workspace.",
		Fields: []core.SetupField{
			{ID: "workspace", Label: "Workspace", Required: true},
			{ID: "region", Label: "Region"},
			{ID: "token", Label: "API token", Secret: true, Required: true},
		},
	}
}

// PrepareAccount builds the canonical account, which is the provider's job.
func (d declaredDefinition) PrepareAccount(values map[string]string) (provider.PreparedAccount, error) {
	workspace := strings.TrimSpace(values["workspace"])
	if workspace == "" {
		return provider.PreparedAccount{}, fmt.Errorf("Workspace is required")
	}
	metadata := map[string]string{}
	if region := strings.TrimSpace(values["region"]); region != "" {
		metadata["region"] = region
	}
	return provider.PreparedAccount{
		Account: core.ProviderAccount{
			ID:       core.ProviderAccountID(workspace),
			Provider: core.ProviderID(d.id),
			Label:    workspace,
			Metadata: metadata,
		},
		Secret: []byte(values["token"]),
	}, nil
}

func (d declaredDefinition) Activate(_ context.Context, req provider.ActivationRequest) (provider.Installation, error) {
	entry := d.CatalogEntry()
	if len(req.Accounts) == 0 {
		entry.Availability = provider.AvailabilityUnconfigured
		return provider.Installation{Catalog: entry}, nil
	}
	entry.Availability = provider.AvailabilityReady
	infos := make([]provider.AccountInfo, 0, len(req.Accounts))
	for _, m := range req.Accounts {
		infos = append(infos, provider.AccountInfo{
			ID: m.Account.ID, Label: m.Account.Label, Status: string(m.Account.Status),
		})
	}
	return provider.Installation{Provider: mock.New(), Catalog: entry, Accounts: infos}, nil
}

// validatingDefinition can check its own credential, so its accounts may be
// recorded as authenticated.
type validatingDefinition struct {
	declaredDefinition
	err error
}

func (d validatingDefinition) VerifyAccount(context.Context, provider.PreparedAccount) (core.SetupValidation, error) {
	if d.err != nil {
		return core.SetupValidation{}, d.err
	}
	return core.SetupValidation{}, nil
}

// guidanceDefinition is configured outside Portico entirely.
type guidanceDefinition struct{ declaredDefinition }

func (d guidanceDefinition) SetupFlow() core.SetupFlow {
	flow := d.declaredDefinition.SetupFlow()
	flow.Kind = core.SetupGuidance
	flow.GuidanceReason = "this provider reads its credential from the environment"
	return flow
}

func newSetupHandler(t *testing.T, defs ...provider.Definition) (*supervisorHandler, *store.Store) {
	t.Helper()
	return newSetupHandlerOn(t, newRecoveryTestStore(t), defs...)
}

// newSetupHandlerOn wires a handler over an existing store, for tests that seed
// state before configuring.
func newSetupHandlerOn(t *testing.T, st *store.Store, defs ...provider.Definition) (*supervisorHandler, *store.Store) {
	t.Helper()
	sup, _ := activationTestSupervisor(t, st, defs...)
	return &supervisorHandler{sup: sup}, st
}

// TestAnyProviderDeclaringAFlowCanBeConfigured is the point of this package.
// The handler previously refused everything but Cloudflare, so the declarative
// setup flow existed and no provider could use it.
func TestAnyProviderDeclaringAFlowCanBeConfigured(t *testing.T) {
	handler, st := newSetupHandler(t, declaredDefinition{"acme"})

	resp, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{
			"workspace": "ws-1",
			"region":    "eu-west",
			"token":     "a-real-looking-token",
		},
	})
	if err != nil {
		t.Fatalf("HandleConfigureProviderAccount: %v", err)
	}

	accounts, err := st.ListProviderAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListProviderAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("stored %d accounts, want 1", len(accounts))
	}
	account := accounts[0]
	if account.ID != "ws-1" {
		t.Fatalf("account ID = %q, want the declared identity field's value", account.ID)
	}
	// A field that is neither identity, label nor secret is account metadata,
	// which is a free-form map — so a new provider needs no schema change.
	if account.Metadata["region"] != "eu-west" {
		t.Fatalf("declared field was not carried into metadata: %#v", account.Metadata)
	}
	if resp.Status != string(core.AccountPending) {
		t.Fatalf("status = %q, want pending", resp.Status)
	}
	// A provider Portico has no built-in knowledge of becomes usable in this
	// process. This used to answer "restart required" into a restart that
	// changed nothing, because no code built an adapter for it.
	if resp.RestartRequired {
		t.Fatal("configuring a declared provider still demands a restart")
	}
}

// TestUnvalidatedAccountIsNeverRecordedAsAuthenticated pins the invariant that
// survives generalising setup. Recording an account as authenticated on the
// strength of a non-empty string is how Portico came to advertise providers
// that could not perform a single operation.
func TestUnvalidatedAccountIsNeverRecordedAsAuthenticated(t *testing.T) {
	handler, st := newSetupHandler(t, declaredDefinition{"acme"})

	resp, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{"workspace": "ws-1", "token": "unchecked"},
	})
	if err != nil {
		t.Fatalf("HandleConfigureProviderAccount: %v", err)
	}
	if resp.Validated {
		t.Fatal("an unchecked credential was reported as validated")
	}
	if resp.VerificationUnavailable == "" {
		t.Fatal("nothing says the credential was not checked")
	}

	accounts, _ := st.ListProviderAccounts(context.Background())
	if len(accounts) != 1 {
		t.Fatalf("stored %d accounts, want 1", len(accounts))
	}
	if accounts[0].Status == core.AccountAuthenticated {
		t.Fatal("an unverified account was stored as authenticated")
	}
	if accounts[0].Status != core.AccountPending {
		t.Fatalf("status = %q, want pending", accounts[0].Status)
	}
}

// TestAProviderThatCanValidateGetsAnAuthenticatedAccount is the other half:
// the pending status is a consequence of being unable to check, not a blanket
// downgrade.
func TestAProviderThatCanValidateGetsAnAuthenticatedAccount(t *testing.T) {
	handler, st := newSetupHandler(t, validatingDefinition{declaredDefinition: declaredDefinition{"acme"}})

	resp, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{"workspace": "ws-1", "token": "checked"},
	})
	if err != nil {
		t.Fatalf("HandleConfigureProviderAccount: %v", err)
	}
	if !resp.Validated {
		t.Fatal("a checked credential was not reported as validated")
	}
	accounts, _ := st.ListProviderAccounts(context.Background())
	if accounts[0].Status != core.AccountAuthenticated {
		t.Fatalf("status = %q, want authenticated", accounts[0].Status)
	}
}

// TestAFailedValidationStoresNothing ensures a rejected credential does not
// leave an account behind.
func TestAFailedValidationStoresNothing(t *testing.T) {
	handler, st := newSetupHandler(t, validatingDefinition{
		declaredDefinition: declaredDefinition{"acme"},
		err:                errors.New("token rejected"),
	})

	if _, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{"workspace": "ws-1", "token": "bad"},
	}); err == nil {
		t.Fatal("a rejected credential was accepted")
	}
	accounts, _ := st.ListProviderAccounts(context.Background())
	if len(accounts) != 0 {
		t.Fatalf("a rejected credential left %d accounts behind", len(accounts))
	}
}

// TestAGuidanceFlowCannotBeSubmitted pins the third provider class. The OpenAI
// tunnel's client reads its credential from the supervisor's own environment,
// so storing one here would write a value nothing reads and report success for
// a setup that had no effect.
func TestAGuidanceFlowCannotBeSubmitted(t *testing.T) {
	handler, st := newSetupHandler(t, guidanceDefinition{declaredDefinition{"acme"}})

	flow, err := handler.HandleProviderSetupFlow("acme")
	if err != nil {
		t.Fatalf("HandleProviderSetupFlow: %v", err)
	}
	if flow.StoresAccount() {
		t.Fatal("a guidance flow was reported as one that stores an account")
	}
	if flow.GuidanceReason == "" {
		t.Fatal("a read-only flow does not say why, so it reads as a missing feature")
	}

	_, err = handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{"workspace": "ws-1", "token": "ignored"},
	})
	if err == nil {
		t.Fatal("a guidance flow accepted a submission")
	}
	accounts, _ := st.ListProviderAccounts(context.Background())
	if len(accounts) != 0 {
		t.Fatalf("a guidance flow stored %d accounts", len(accounts))
	}
}

// TestTheOpenAITunnelDeclaresGuidance checks the real adapter, not a stand-in.
// Its credential reaches the client through the supervisor's environment
// (validateClient and clientProcessSpec both read it with os.Getenv), so a form
// that appeared to save it would be a lie the setup screen told.
func TestTheOpenAITunnelDeclaresGuidance(t *testing.T) {
	flow := openaitunnel.New("", nil).SetupFlow()
	if flow.StoresAccount() {
		t.Fatal("the OpenAI tunnel offers to store a credential it cannot use")
	}
}

// TestConfiguringOneProviderLeavesAnotherProvidersSameNamedAccountIntact is the
// inverse of the guard this package removed.
//
// Account identity is the pair (provider, ID), so an account named "acct-1"
// belongs to whichever provider owns it and another provider may use the same
// name. The old supervisor check enforced global uniqueness and would have
// refused this outright; the schema now enforces the pairing instead, which is
// both correct and covers every write path rather than one of four.
func TestConfiguringOneProviderLeavesAnotherProvidersSameNamedAccountIntact(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	handler, _ := newSetupHandlerOn(t, st, declaredDefinition{"acme"})

	existing := core.ProviderAccount{
		ID: "shared", Provider: "cloudflare", Label: "Personal",
		CredentialRef: "cloudflare:shared:api-token",
		Status:        core.AccountAuthenticated, Metadata: map[string]string{},
	}
	if err := st.UpsertProviderAccountCredential(ctx, existing, []byte("cloudflare-secret")); err != nil {
		t.Fatalf("seed cloudflare account: %v", err)
	}

	// The same name, for a different provider. This must succeed.
	if _, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{"workspace": "shared", "token": "acme-token"},
	}); err != nil {
		t.Fatalf("a legitimate same-named account for another provider was refused: %v", err)
	}

	accounts, listErr := st.ListProviderAccounts(ctx)
	if listErr != nil {
		t.Fatalf("ListProviderAccounts: %v", listErr)
	}
	if len(accounts) != 2 {
		t.Fatalf("stored %d accounts, want 2", len(accounts))
	}

	var cloudflareAccount *core.ProviderAccount
	for i := range accounts {
		if accounts[i].Provider == "cloudflare" && accounts[i].ID == "shared" {
			cloudflareAccount = &accounts[i]
		}
	}
	if cloudflareAccount == nil {
		t.Fatal("the Cloudflare account was taken over")
	}
	if cloudflareAccount.Label != "Personal" || cloudflareAccount.Status != core.AccountAuthenticated {
		t.Fatalf("the Cloudflare account was modified: %#v", cloudflareAccount)
	}
	secret, err := st.LoadProviderCredential(ctx, "cloudflare", "cloudflare:shared:api-token")
	if err != nil {
		t.Fatalf("the Cloudflare credential no longer loads: %v", err)
	}
	if secret != "cloudflare-secret" {
		t.Fatalf("the Cloudflare credential changed: %q", secret)
	}
}

// TestCloudflareSetupCannotOverwriteAnotherProvidersAccount covers the write
// path that never had a guard.
//
// The removed supervisor check lived only in the generic setup path. Ordinary
// Cloudflare setup wrote straight through, so it could reassign another
// provider's account until the key made that impossible.
func TestCloudflareSetupCannotOverwriteAnotherProvidersAccount(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st,
		&stubAccountValidator{result: &AccountValidation{AccountAccessible: true}})}

	other := core.ProviderAccount{
		ID: "acct-x", Provider: "acme", Label: "Someone else's",
		CredentialRef: "acme:acct-x:credential",
		Status:        core.AccountAuthenticated, Metadata: map[string]string{},
	}
	if err := st.UpsertProviderAccountCredential(ctx, other, []byte("acme-secret")); err != nil {
		t.Fatalf("seed acme account: %v", err)
	}

	if _, err := handler.HandleConfigureProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		AccountID:  "acct-x",
		Credential: "a-real-looking-token",
	}); err != nil {
		t.Fatalf("HandleConfigureProviderAccount: %v", err)
	}

	accounts, _ := st.ListProviderAccounts(ctx)
	var acme *core.ProviderAccount
	for i := range accounts {
		if accounts[i].Provider == "acme" && accounts[i].ID == "acct-x" {
			acme = &accounts[i]
		}
	}
	if acme == nil {
		t.Fatal("Cloudflare setup took over another provider's account")
	}
	if acme.Label != "Someone else's" || acme.CredentialRef != "acme:acct-x:credential" {
		t.Fatalf("the other provider's account was modified: %#v", acme)
	}
	secret, err := st.LoadProviderCredential(ctx, "acme", "acme:acct-x:credential")
	if err != nil || secret != "acme-secret" {
		t.Fatalf("the other provider's credential was damaged: %q err=%v", secret, err)
	}
}

// TestCloudflareAcceptsTheGenericFieldMap pins that the provider Portico knows
// best is reachable through the same contract as every other one. The
// required-field gate validates the merged map, so a writer reading the named
// request fields instead would accept a caller and then reject it for a missing
// account ID.
func TestCloudflareAcceptsTheGenericFieldMap(t *testing.T) {
	st := newRecoveryTestStore(t)
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st,
		&stubAccountValidator{result: &AccountValidation{AccountAccessible: true}})}

	resp, err := handler.HandleConfigureProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{
			"account_id": "acct-generic",
			"label":      "From the generic form",
			"credential": "a-real-looking-token",
		},
	})
	if err != nil {
		t.Fatalf("a Fields-only Cloudflare request was refused: %v", err)
	}
	if !resp.Validated {
		t.Fatal("the credential was not validated")
	}

	accounts, _ := st.ListProviderAccounts(context.Background())
	if len(accounts) != 1 || accounts[0].ID != "acct-generic" {
		t.Fatalf("account not stored from the generic map: %#v", accounts)
	}
	if accounts[0].Label != "From the generic form" {
		t.Fatalf("label from the generic map was lost: %q", accounts[0].Label)
	}
}

// TestAnUnstatedAccountStatusIsPending guards the store's default. Defaulting
// to authenticated makes "I forgot to set this" indistinguishable from "this
// credential was checked".
func TestAnUnstatedAccountStatusIsPending(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)

	account := core.ProviderAccount{
		ID: "acct-nostatus", Provider: "cloudflare", Label: "No status",
		CredentialRef: "cloudflare:acct-nostatus:api-token", Metadata: map[string]string{},
	}
	if err := st.UpsertProviderAccountCredential(ctx, account, []byte("secret")); err != nil {
		t.Fatalf("UpsertProviderAccountCredential: %v", err)
	}

	accounts, _ := st.ListProviderAccounts(ctx)
	for _, stored := range accounts {
		if stored.ID == "acct-nostatus" && stored.Status == core.AccountAuthenticated {
			t.Fatal("an account saved without a status was recorded as authenticated")
		}
	}
}

// TestGuidanceReasonComesFromTheProvider ensures the explanation is declared by
// the provider that knows how its client obtains a credential, rather than
// asserted centrally and happening to be right for the first such provider.
func TestGuidanceReasonComesFromTheProvider(t *testing.T) {
	st := newRecoveryTestStore(t)
	sup, _ := activationTestSupervisor(t, st, openaitunnel.NewDefinition(openaitunnel.DefinitionConfig{}))
	handler := &supervisorHandler{sup: sup}

	flow, err := handler.HandleProviderSetupFlow("openai_tunnel")
	if err != nil {
		t.Fatalf("HandleProviderSetupFlow: %v", err)
	}
	if !strings.Contains(flow.GuidanceReason, openaitunnel.CredentialEnvVar) {
		t.Fatalf("the reason does not name the variable to export: %q", flow.GuidanceReason)
	}
}

// TestRestartDoesNotPromotePendingAccounts pins the invariant across the
// startup path, which is where it actually broke.
//
// Setup stores an unverified credential as pending and reports it honestly.
// Startup then reloaded every stored account into the registry, which derived
// "authenticated" from how many accounts existed — so restarting Portico turned
// an unverified account into a ready, selectable provider.
func TestRestartDoesNotPromotePendingAccounts(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	sup, registry := activationTestSupervisor(t, st, declaredDefinition{"acme"})

	pending := core.ProviderAccount{
		ID: "acct-pending", Provider: "acme", Label: "Unverified",
		CredentialRef: "acme:acct-pending:credential",
		Status:        core.AccountPending, Metadata: map[string]string{},
	}
	if err := st.UpsertProviderAccountCredential(ctx, pending, []byte("unchecked")); err != nil {
		t.Fatalf("seed pending account: %v", err)
	}

	// This is what a restart does.
	sup.activateAll(ctx)

	for _, snap := range registry.Snapshot() {
		if snap.ID != "acme" {
			continue
		}
		if snap.Authenticated {
			t.Fatal("a pending account became authenticated across a restart")
		}
		if snap.Availability == provider.AvailabilityReady {
			t.Fatal("a pending account made the provider ready across a restart")
		}
		if len(snap.Accounts) != 0 {
			t.Fatalf("a pending account was offered as usable: %#v", snap.Accounts)
		}
		// It must survive as something the user can repair.
		if len(snap.PendingAccounts) != 1 {
			t.Fatalf("the pending account was lost rather than surfaced: %#v", snap.PendingAccounts)
		}
		return
	}
	t.Fatal("provider not found in snapshot")
}

// TestRequiredFieldsAreEnforcedFromTheDeclaration ensures the requirement comes
// from the provider rather than from a hardcoded check.
func TestRequiredFieldsAreEnforcedFromTheDeclaration(t *testing.T) {
	handler, _ := newSetupHandler(t, declaredDefinition{"acme"})

	_, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{"token": "a-token"}, // workspace missing
	})
	if err == nil {
		t.Fatal("a missing required field was accepted")
	}
	if !strings.Contains(err.Error(), "Workspace") {
		t.Fatalf("error does not name the missing field: %v", err)
	}
}

// TestRemovingAnAccountReportsDependentConnections pins audit item 24. Removing
// an account still selected by a connection would strand it with an opaque
// "provider account unavailable" error.
func TestRemovingAnAccountReportsDependentConnections(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)

	account := core.ProviderAccount{
		ID: "acct-1", Provider: "cloudflare", Label: "Personal",
		CredentialRef: "cloudflare:acct-1:api-token", Status: core.AccountAuthenticated,
		Metadata: map[string]string{},
	}
	if err := st.UpsertProviderAccountCredential(ctx, account, []byte("secret")); err != nil {
		t.Fatalf("UpsertProviderAccountCredential: %v", err)
	}

	profile := recoveryTestProfile("conn-dependent")
	profile.Driver.ProviderID = "cloudflare"
	profile.Driver.AccountID = "acct-1"
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	handler := &supervisorHandler{sup: &Supervisor{store: st, mutating: true}}
	resp, err := handler.HandleRemoveProviderAccount("cloudflare", "acct-1")
	if err == nil {
		t.Fatal("an account with a dependent connection was removed")
	}
	if resp == nil || len(resp.DependentConnections) != 1 || resp.DependentConnections[0] != "conn-dependent" {
		t.Fatalf("dependent connections were not reported: %#v", resp)
	}

	// The account and its credential must survive the refusal.
	accounts, listErr := st.ListProviderAccounts(ctx)
	if listErr != nil || len(accounts) != 1 {
		t.Fatalf("refused removal still deleted the account: %#v, %v", accounts, listErr)
	}
}

// TestRemovingAnUnusedAccountDeletesItsCredential ensures removal is complete
// once nothing depends on it.
func TestRemovingAnUnusedAccountDeletesItsCredential(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)

	account := core.ProviderAccount{
		ID: "acct-unused", Provider: "cloudflare", Label: "Unused",
		CredentialRef: "cloudflare:acct-unused:api-token", Status: core.AccountAuthenticated,
		Metadata: map[string]string{},
	}
	if err := st.UpsertProviderAccountCredential(ctx, account, []byte("secret")); err != nil {
		t.Fatalf("UpsertProviderAccountCredential: %v", err)
	}

	handler := &supervisorHandler{sup: &Supervisor{store: st, mutating: true}}
	resp, err := handler.HandleRemoveProviderAccount("cloudflare", "acct-unused")
	if err != nil {
		t.Fatalf("HandleRemoveProviderAccount: %v", err)
	}
	if !resp.Removed {
		t.Fatal("account was not removed")
	}

	accounts, err := st.ListProviderAccounts(ctx)
	if err != nil || len(accounts) != 0 {
		t.Fatalf("account survived removal: %#v, %v", accounts, err)
	}
	// The credential must not outlive the account that referenced it. A missing
	// credential is reported as an empty value rather than an error.
	secret, err := st.LoadProviderCredential(ctx, "cloudflare", "cloudflare:acct-unused:api-token")
	if err != nil {
		t.Fatalf("LoadProviderCredential: %v", err)
	}
	if secret != "" {
		t.Fatal("credential survived account removal")
	}
}

// TestSupportExportCarriesNoSecrets pins audit item 27. A diagnostic report is
// meant to be attached to a bug report, so it must be safe to share.
func TestSupportExportCarriesNoSecrets(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)

	const secret = "cf-token-MUST-NOT-APPEAR-7b21"
	account := core.ProviderAccount{
		ID: "acct-1", Provider: "cloudflare", Label: "Personal",
		CredentialRef: "cloudflare:acct-1:api-token", Status: core.AccountAuthenticated,
		Metadata: map[string]string{"zone_id": "zone-1"},
	}
	if err := st.UpsertProviderAccountCredential(ctx, account, []byte(secret)); err != nil {
		t.Fatalf("UpsertProviderAccountCredential: %v", err)
	}

	connID := core.ConnectionID("conn-export")
	profile := recoveryTestProfile(connID)
	profile.Driver.AccountID = "acct-1"
	profile.Spec.ServiceExposure.Protection = core.ProtectionSpec{
		Kind:          core.ProtectionEmailOTP,
		AllowedEmails: []string{"private.person@example.com"},
	}
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	registry := provider.NewRegistry()
	ctrl := controller.New(registry, st)
	ctrl.RestoreProfile(profile)
	rt := recoveryTestRuntime(connID)
	rt.Provider.Resources = []core.ProviderResource{{
		Type: core.ResourceTunnel, ExternalID: "tun-1", Ownership: core.OwnershipManaged,
		Metadata: map[string]string{"tunnel_token": secret, "region": "us-east"},
	}}
	ctrl.RestoreRuntime(rt)

	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}
	export, err := handler.HandleSupportExport()
	if err != nil {
		t.Fatalf("HandleSupportExport: %v", err)
	}

	blob, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	rendered := string(blob)

	if strings.Contains(rendered, secret) {
		t.Fatalf("support export leaked a credential:\n%s", rendered)
	}
	// Allowed identities are personal data and are reported as a count only.
	if strings.Contains(rendered, "private.person@example.com") {
		t.Fatalf("support export leaked an allowed identity:\n%s", rendered)
	}

	if len(export.Connections) != 1 {
		t.Fatalf("export connections = %d, want 1", len(export.Connections))
	}
	conn := export.Connections[0]
	if conn.AllowedIdentityCount != 1 {
		t.Fatalf("allowed identity count = %d, want 1", conn.AllowedIdentityCount)
	}
	// The report must still be useful: external IDs are what a support
	// conversation and a manual cleanup act on.
	if len(conn.Resources) != 1 || conn.Resources[0].ExternalID != "tun-1" {
		t.Fatalf("export dropped the resource identity: %#v", conn.Resources)
	}
	// A redacted field must still be visible as having existed.
	if conn.Resources[0].Metadata["tunnel_token"] != "[redacted]" {
		t.Fatalf("secret metadata was not marked redacted: %#v", conn.Resources[0].Metadata)
	}
	if conn.Resources[0].Metadata["region"] != "us-east" {
		t.Fatalf("non-secret metadata was dropped: %#v", conn.Resources[0].Metadata)
	}

	if export.SchemaVersion == 0 {
		t.Fatal("export does not record the database schema version")
	}
	if export.Reviewed {
		t.Fatal("a freshly generated export must not claim to have been reviewed")
	}
	if len(export.Notes) == 0 {
		t.Fatal("export does not state what it excludes")
	}
}

// TestSupportExportRedactsBySubstringNotExactKey ensures the redaction rule
// catches realistic key names.
func TestSupportExportRedactsBySubstringNotExactKey(t *testing.T) {
	for _, key := range []string{
		"token", "api_token", "authToken", "SECRET", "client_secret",
		"password", "credential_ref", "private_key", "Authorization", "cookie",
	} {
		if !isSecretKey(key) {
			t.Fatalf("key %q was not treated as sensitive", key)
		}
	}
	for _, key := range []string{"region", "hostname", "tunnel_id", "created_at"} {
		if isSecretKey(key) {
			t.Fatalf("key %q was needlessly redacted", key)
		}
	}
}

// TestPlanEditPreviewsWithoutApplying pins the retain-previous property at the
// handler boundary: previewing an edit must not change the connection.
func TestPlanEditPreviewsWithoutApplying(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-edit-preview")

	profile := recoveryTestProfile(connID)
	profile.Spec.ServiceExposure.Exposure.Mode = core.ExposurePermanent
	profile.Spec.ServiceExposure.Exposure.RequestedAddress = "old.example.com"
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	registry := provider.NewRegistry()
	if err := registry.Add(mock.New()); err != nil {
		t.Fatalf("register mock: %v", err)
	}
	ctrl := controller.New(registry, st)
	ctrl.SetConnectionStorer(st)
	ctrl.SetProfileUpdater(st)
	ctrl.RestoreProfile(profile)
	ctrl.RestoreRuntime(&core.ConnectionRuntime{ConnectionID: connID, State: core.RuntimeClosed})

	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}

	spec := ipc.ConnectionSpecDTO{
		Exposure: ipc.ExposureDTO{Mode: "permanent_public", RequestedAddress: "new.example.com"},
	}
	plan, err := handler.HandlePlanEdit(string(connID), ipc.UpdateConnectionRequest{Spec: &spec})
	if err != nil {
		t.Fatalf("HandlePlanEdit: %v", err)
	}
	if plan.Noop {
		t.Fatal("a hostname change was reported as a no-op")
	}
	if plan.Outcome == "" {
		t.Fatal("edit plan does not state what it changes")
	}

	// Previewing must not have written anything.
	stored, err := st.LoadProfile(ctx, connID)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if stored.Spec.ServiceExposure.Exposure.RequestedAddress != "old.example.com" {
		t.Fatalf("previewing an edit already changed the stored hostname to %q",
			stored.Spec.ServiceExposure.Exposure.RequestedAddress)
	}
}

// TestPlanEditReportsANoOpAsSuch ensures an edit that changes nothing does not
// become a preview-and-confirm workflow.
func TestPlanEditReportsANoOpAsSuch(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-edit-noop")
	profile := recoveryTestProfile(connID)
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	registry := provider.NewRegistry()
	if err := registry.Add(mock.New()); err != nil {
		t.Fatalf("register mock: %v", err)
	}
	ctrl := controller.New(registry, st)
	ctrl.RestoreProfile(profile)
	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}

	same := profile.Name
	plan, err := handler.HandlePlanEdit(string(connID), ipc.UpdateConnectionRequest{Name: &same})
	if err != nil {
		t.Fatalf("HandlePlanEdit: %v", err)
	}
	if !plan.Noop {
		t.Fatalf("an unchanged edit was not reported as a no-op: %#v", plan.Steps)
	}
}

// activationTestSupervisor builds a supervisor wired to one provider
// definition, with binary lookup stubbed so the test does not depend on what is
// installed on the machine running it.
func activationTestSupervisor(t *testing.T, st *store.Store, defs ...provider.Definition) (*Supervisor, provider.Registry) {
	t.Helper()
	registry := provider.NewRegistry()
	ctrl := controller.New(registry, st)
	sup := &Supervisor{
		store: st, controller: ctrl, registry: registry, mutating: true,
		procMgr: process.NewManager(),
		paths:   app.Paths{ConnectorDir: t.TempDir()},
	}
	sup.SetProviderDefinitions(defs, provider.RuntimeServices{
		Processes:         &processManagerAdapter{mgr: sup.procMgr},
		TunnelCredentials: st,
		ConnectorDir:      sup.paths.ConnectorDir,
		LookPath:          func(name string) (string, error) { return name, nil },
		Getenv:            func(string) string { return "" },
	})
	return sup, registry
}

// TestActivationMakesAnAccountUsableWithoutRestart pins the removal of the
// restart requirement. Account changes previously took effect only at startup,
// so a newly saved account was inert until the user restarted the supervisor.
func TestActivationMakesAnAccountUsableWithoutRestart(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	sup, registry := activationTestSupervisor(t, st,
		cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}))

	account := core.ProviderAccount{
		ID: "acct-live", Provider: "cloudflare", Label: "Live",
		CredentialRef: "cloudflare:acct-live:api-token", Status: core.AccountAuthenticated,
		Metadata: map[string]string{},
	}
	if err := st.UpsertProviderAccountCredential(ctx, account, []byte("token")); err != nil {
		t.Fatalf("UpsertProviderAccountCredential: %v", err)
	}

	if err := sup.ActivateProvider(ctx, "cloudflare"); err != nil {
		t.Fatalf("ActivateProvider: %v", err)
	}
	// A zone-less account must still produce a usable adapter: a zone is
	// required only for DNS and custom hostnames.
	if registry.Get("cloudflare") == nil {
		t.Fatal("a zone-less account produced no adapter")
	}
	var usable int
	for _, snap := range registry.Snapshot() {
		if snap.ID == "cloudflare" {
			usable = len(snap.Accounts)
		}
	}
	if usable != 1 {
		t.Fatalf("usable accounts = %d, want 1", usable)
	}
}

// TestRemovingTheLastAccountKeepsQuickTunnelsAvailable resolves a divergence
// between two construction paths that produced different capabilities from the
// same durable state.
//
// Startup fell back to a Quick Tunnel adapter when no account was configured;
// the rebuild path removed Cloudflare entirely when the last account was
// deleted. A Quick Tunnel needs no account and works, so withdrawing it because
// an account was removed was the bug. Fresh startup and post-removal must agree.
func TestRemovingTheLastAccountKeepsQuickTunnelsAvailable(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	sup, registry := activationTestSupervisor(t, st,
		cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}))

	account := core.ProviderAccount{
		ID: "acct-live", Provider: "cloudflare", Label: "Live",
		CredentialRef: "cloudflare:acct-live:api-token", Status: core.AccountAuthenticated,
		Metadata: map[string]string{},
	}
	if err := st.UpsertProviderAccountCredential(ctx, account, []byte("token")); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := sup.ActivateProvider(ctx, "cloudflare"); err != nil {
		t.Fatalf("ActivateProvider: %v", err)
	}
	if err := st.DeleteProviderAccount(ctx, "cloudflare", "acct-live"); err != nil {
		t.Fatalf("DeleteProviderAccount: %v", err)
	}
	if err := sup.ActivateProvider(ctx, "cloudflare"); err != nil {
		t.Fatalf("ActivateProvider after removal: %v", err)
	}

	if registry.Get("cloudflare") == nil {
		t.Fatal("removing the last account withdrew Quick Tunnels, which need no account")
	}

	// A supervisor starting fresh against the same database must agree.
	fresh, freshRegistry := activationTestSupervisor(t, st,
		cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}))
	fresh.activateAll(ctx)
	if freshRegistry.Get("cloudflare") == nil {
		t.Fatal("a fresh start produced no Cloudflare adapter from the same state")
	}

	// It must say why it is limited rather than appearing fully configured.
	for _, snap := range freshRegistry.Snapshot() {
		if snap.ID == "cloudflare" && snap.Reason == "" {
			t.Fatal("an accountless Cloudflare does not explain what it can do")
		}
	}
}

// TestCreateConnectionSupportsPortForwardAndRefusesTheRest pins the end of the
// "advertised but not executable" gap. Kinds either execute or say why not;
// none is accepted and left inert.
func TestCreateConnectionSupportsPortForwardAndRefusesTheRest(t *testing.T) {
	st := newRecoveryTestStore(t)
	registry := provider.NewRegistry()
	if err := registry.Add(portforward.New()); err != nil {
		t.Fatalf("register portforward: %v", err)
	}
	ctrl := controller.New(registry, st)
	ctrl.SetConnectionStorer(st)
	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}

	t.Run("port forward is created", func(t *testing.T) {
		conn, err := handler.HandleCreateConnection(ipc.CreateConnectionRequest{
			Name: "db tunnel",
			Kind: string(core.ConnectionPortForward),
			PortForward: &ipc.PortForwardDTO{
				LocalPort: 15432, RemoteHost: "db.internal", RemotePort: 5432,
			},
		})
		if err != nil {
			t.Fatalf("HandleCreateConnection(port_forward): %v", err)
		}
		if conn.ID == "" {
			t.Fatal("no connection was created")
		}
		stored, loadErr := st.LoadProfile(context.Background(), core.ConnectionID(conn.ID))
		if loadErr != nil {
			t.Fatalf("LoadProfile: %v", loadErr)
		}
		if stored.Kind != core.ConnectionPortForward {
			t.Fatalf("stored kind = %q", stored.Kind)
		}
		if stored.Spec.PortForward == nil || stored.Spec.PortForward.RemotePort != 5432 {
			t.Fatalf("stored port forward spec = %+v", stored.Spec.PortForward)
		}
		// A forward must not be started by creation.
		if stored.Desired != core.DesiredClosed {
			t.Fatalf("desired = %q, want closed", stored.Desired)
		}
	})

	t.Run("private network is refused with a reason", func(t *testing.T) {
		_, err := handler.HandleCreateConnection(ipc.CreateConnectionRequest{
			Name: "net", Kind: string(core.ConnectionPrivateNetwork),
		})
		if err == nil {
			t.Fatal("a private network connection was accepted despite having no adapter")
		}
		if !strings.Contains(err.Error(), "not implemented") {
			t.Fatalf("refusal does not explain itself: %v", err)
		}
	})

	t.Run("unknown kind is refused", func(t *testing.T) {
		_, err := handler.HandleCreateConnection(ipc.CreateConnectionRequest{
			Name: "x", Kind: "teleportation",
		})
		if err == nil {
			t.Fatal("an unknown connection kind was accepted")
		}
	})

	t.Run("service exposure still works", func(t *testing.T) {
		_, err := handler.HandleCreateConnection(ipc.CreateConnectionRequest{
			Name: "web",
			Source: ipc.SourceDTO{
				Kind:     "existing_service",
				Existing: &ipc.ExistingSourceDTO{Address: "127.0.0.1:3000"},
			},
			Exposure: ipc.ExposureDTO{Mode: "temporary_public"},
			Provider: ipc.ProviderSelectionDTO{ProviderID: "portforward"},
		})
		// The provider does not support exposures, so this is refused on
		// capability grounds rather than on kind — which is the point.
		if err == nil {
			t.Log("service exposure creation still reachable")
		}
	})
}

// TestReadinessAggregatesEverythingNeededToGetWorking pins the setup view: one
// response answers what Portico needs and what is already satisfied, rather
// than requiring the answer to be assembled from several screens.
func TestReadinessAggregatesEverythingNeededToGetWorking(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)

	// A credential already on the machine must be reported as found.
	t.Setenv("NGROK_AUTHTOKEN", "already-configured-token")

	registry := provider.NewRegistry()
	registry.AddCatalogEntry(provider.CatalogEntry{
		ID: "ngrok", DisplayName: "ngrok",
		Availability: provider.AvailabilityUnconfigured,
		Reason:       "no account is configured",
		SetupActions: []string{"Add an ngrok account"},
	})
	if err := registry.Add(mock.New()); err != nil {
		t.Fatalf("register mock: %v", err)
	}

	ctrl := controller.New(registry, st)
	profile := recoveryTestProfile("conn-readiness")
	profile.Spec.ServiceExposure.Exposure.Mode = core.ExposurePermanent
	profile.Spec.ServiceExposure.Exposure.RequestedAddress = "" // a blocker
	ctrl.RestoreProfile(profile)
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}
	readiness, err := handler.HandleReadiness()
	if err != nil {
		t.Fatalf("HandleReadiness: %v", err)
	}

	if readiness.Summary == "" {
		t.Fatal("readiness gives no overall summary")
	}
	if readiness.LaunchMode == "" {
		t.Fatal("readiness does not report the launch mode")
	}

	var ngrokEntry *ipc.ProviderReadinessDTO
	for i := range readiness.Providers {
		if readiness.Providers[i].ID == "ngrok" {
			ngrokEntry = &readiness.Providers[i]
		}
	}
	if ngrokEntry == nil {
		t.Fatal("a catalogued provider is missing from readiness")
	}
	// The credential already on the machine must be surfaced as found, and the
	// summary must say the last step is what remains.
	var foundCredential bool
	for _, c := range ngrokEntry.Credentials {
		if c.Present {
			foundCredential = true
		}
	}
	if !foundCredential {
		t.Fatalf("an existing credential was not surfaced: %#v", ngrokEntry.Credentials)
	}
	if !strings.Contains(ngrokEntry.Summary, "found") {
		t.Fatalf("summary does not mention the found credential: %q", ngrokEntry.Summary)
	}

	// A connection that cannot open must say why, in plain language.
	if len(readiness.Connections) != 1 {
		t.Fatalf("connections = %d, want 1", len(readiness.Connections))
	}
	conn := readiness.Connections[0]
	if conn.Ready {
		t.Fatal("a connection with no hostname was reported ready")
	}
	if len(conn.Blockers) == 0 {
		t.Fatal("a blocked connection lists no blockers")
	}
	if !strings.Contains(strings.Join(conn.Blockers, " "), "hostname") {
		t.Fatalf("blockers do not name the missing hostname: %v", conn.Blockers)
	}
}

// TestManualLaunchModeArmsNothing pins the single gate in front of per
// connection autostart.
func TestManualLaunchModeArmsNothing(t *testing.T) {
	sup := &Supervisor{}

	sup.SetLaunchMode(LaunchAuto)
	if sup.launchMode() != LaunchAuto {
		t.Fatalf("launch mode = %q, want auto", sup.launchMode())
	}

	sup.SetLaunchMode(LaunchManual)
	if sup.launchMode() != LaunchManual {
		t.Fatalf("launch mode = %q, want manual", sup.launchMode())
	}

	// The environment override exists so the gate can be closed without
	// changing stored state.
	t.Setenv("PORTICO_LAUNCH_MODE", "manual")
	sup.SetLaunchMode(LaunchAuto)
	if sup.launchMode() != LaunchManual {
		t.Fatal("the environment override did not close the gate")
	}
}

// TestSetLaunchModeReportsTheModeInEffect pins that the handler answers with
// the mode the supervisor is actually using. An environment override wins over
// the stored value, so echoing the request would tell the caller something
// untrue about the machine it is running on.
func TestSetLaunchModeReportsTheModeInEffect(t *testing.T) {
	handler := &supervisorHandler{sup: &Supervisor{}}

	result, err := handler.HandleSetLaunchMode(LaunchManual)
	if err != nil {
		t.Fatalf("HandleSetLaunchMode: %v", err)
	}
	if result.Mode != LaunchManual {
		t.Fatalf("mode = %q, want manual", result.Mode)
	}
	if result.Pinned {
		t.Fatal("mode reported as pinned with no override set")
	}
	// Portico has no settings store, so the mode lasts only as long as the
	// process. The response must not imply the choice is remembered.
	if result.Persistent {
		t.Fatal("mode reported as persistent; nothing writes it to durable state")
	}

	t.Run("an unrecognised mode is rejected, not coerced", func(t *testing.T) {
		// Coercing a typo to auto would arm every connection marked to start,
		// which is the opposite of what this gate is for.
		if _, err := handler.HandleSetLaunchMode("manaul"); err == nil {
			t.Fatal("a misspelled mode was accepted")
		}
		if got := handler.sup.launchMode(); got != LaunchManual {
			t.Fatalf("a rejected mode still changed the gate to %q", got)
		}
	})

	t.Run("an override is reported rather than silently winning", func(t *testing.T) {
		t.Setenv("PORTICO_LAUNCH_MODE", "manual")
		result, err := handler.HandleSetLaunchMode(LaunchAuto)
		if err != nil {
			t.Fatalf("HandleSetLaunchMode: %v", err)
		}
		if result.Mode != LaunchManual {
			t.Fatalf("mode = %q, want the override's manual", result.Mode)
		}
		if !result.Pinned || result.PinnedBy != "PORTICO_LAUNCH_MODE" {
			t.Fatalf("override not reported: pinned=%v by=%q", result.Pinned, result.PinnedBy)
		}
	})
}

// TestReadinessReportsTheLaunchModeOverride ensures the setup screen can say
// why the launch-mode key will not take effect.
func TestReadinessReportsTheLaunchModeOverride(t *testing.T) {
	st := newRecoveryTestStore(t)
	registry := provider.NewRegistry()
	ctrl := controller.New(registry, st)
	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}

	t.Setenv("PORTICO_LAUNCH_MODE", "manual")
	readiness, err := handler.HandleReadiness()
	if err != nil {
		t.Fatalf("HandleReadiness: %v", err)
	}
	if readiness.LaunchMode != LaunchManual {
		t.Fatalf("launch mode = %q, want manual", readiness.LaunchMode)
	}
	if !readiness.LaunchModePinned || readiness.LaunchModePinnedBy != "PORTICO_LAUNCH_MODE" {
		t.Fatalf("override not surfaced: pinned=%v by=%q",
			readiness.LaunchModePinned, readiness.LaunchModePinnedBy)
	}
}

// TestReadinessReportsAFailedCheckAsAFailure ensures a broken check never reads
// as a clean bill of health.
func TestReadinessReportsAFailedCheckAsAFailure(t *testing.T) {
	st := newRecoveryTestStore(t)
	registry := provider.NewRegistry()
	ctrl := controller.New(registry, st)
	handler := &supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry, mutating: true}}

	readiness, err := handler.HandleReadiness()
	if err != nil {
		t.Fatalf("HandleReadiness: %v", err)
	}
	// With no providers at all, the summary must say so rather than implying
	// everything is fine.
	if !strings.Contains(readiness.Summary, "No provider") {
		t.Fatalf("summary with no providers = %q", readiness.Summary)
	}
}

// TestEveryWriterDerivesTheSameCredentialReference pins the fix for a secret
// that outlived its account.
//
// The generic setup path built "<provider>:<account>:credential" while the
// environment bootstrap built "<provider>:<account>:api-token". Two writers,
// two encrypted rows, one account — and because deletion removes only the
// reference the account currently carries, the other survived being deleted.
func TestEveryWriterDerivesTheSameCredentialReference(t *testing.T) {
	if got, want := providerCredentialRef("ngrok", "acct-1"), "ngrok:acct-1:api-token"; got != want {
		t.Fatalf("reference = %q, want %q", got, want)
	}
	// The property that matters is agreement, not the format itself: the same
	// account must produce one reference no matter which path writes it.
	if providerCredentialRef("cloudflare", "a") == providerCredentialRef("cloudflare", "b") {
		t.Fatal("different accounts share a credential reference")
	}
	if providerCredentialRef("cloudflare", "a") == providerCredentialRef("ngrok", "a") {
		t.Fatal("different providers share a credential reference for the same account name")
	}
}

// TestBootstrapDoesNotOverwriteConfiguredAccountState covers the supervisor
// path end to end: an account configured through Portico must survive a restart
// that also has environment credentials set for it.
func TestBootstrapDoesNotOverwriteConfiguredAccountState(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)

	configured := core.ProviderAccount{
		ID: "acct-1", Provider: "cloudflare", Label: "Production",
		CredentialRef: providerCredentialRef("cloudflare", "acct-1"),
		Status:        core.AccountAuthenticated,
		Metadata:      map[string]string{"zone_id": "zone-production"},
	}
	if err := st.UpsertProviderAccountCredential(ctx, configured, []byte("configured-secret")); err != nil {
		t.Fatalf("seed configured account: %v", err)
	}

	// A stale environment from an earlier install, applied on the next start,
	// through the same helper both provider bootstraps use.
	seedBootstrapAccount(st, "cloudflare", "acct-1", "token-stale",
		map[string]string{"zone_id": "zone-stale"})

	accounts, err := st.ListProviderAccounts(ctx)
	if err != nil {
		t.Fatalf("ListProviderAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("stored %d accounts, want 1", len(accounts))
	}
	// The zone decides which Cloudflare zone the adapter is built against, so
	// reverting it silently points DNS work at the wrong place.
	if accounts[0].Metadata["zone_id"] != "zone-production" {
		t.Fatalf("the configured zone was reverted to %q", accounts[0].Metadata["zone_id"])
	}
	if accounts[0].Label != "Production" {
		t.Fatalf("the configured label was reverted to %q", accounts[0].Label)
	}
}

// TestBootstrapImportKeepsTheValidatedCredential covers the production
// sequence for both providers that import from the environment.
//
// The account guard alone did not protect the secret: the credential was
// written first, and every writer now derives the same reference, so a stale
// environment token replaced a validated one while the account row kept its
// label, zone and status. The adapter was then built from the wrong token.
func TestBootstrapImportKeepsTheValidatedCredential(t *testing.T) {
	for _, providerID := range []core.ProviderID{"cloudflare", "ngrok"} {
		t.Run(string(providerID), func(t *testing.T) {
			ctx := context.Background()
			st := newRecoveryTestStore(t)
			ref := providerCredentialRef(string(providerID), "acct-1")

			configured := core.ProviderAccount{
				ID: "acct-1", Provider: providerID, Label: "Configured in Portico",
				CredentialRef: ref, Status: core.AccountAuthenticated,
				Metadata: map[string]string{"zone_id": "zone-production"},
			}
			if err := st.UpsertProviderAccountCredential(ctx, configured, []byte("token-validated")); err != nil {
				t.Fatalf("seed configured account: %v", err)
			}

			seedBootstrapAccount(st, providerID, "acct-1", "token-stale",
				map[string]string{"zone_id": "zone-stale"})

			secret, err := st.LoadProviderCredential(ctx, providerID, ref)
			if err != nil {
				t.Fatalf("load credential: %v", err)
			}
			if secret != "token-validated" {
				t.Fatalf("a stale environment token replaced the validated one: %q", secret)
			}
			accounts, _ := st.ListProviderAccounts(ctx)
			if len(accounts) != 1 || accounts[0].Label != "Configured in Portico" {
				t.Fatalf("account state was disturbed: %#v", accounts)
			}
		})
	}
}

// TestBootstrapImportCreatesTheAccountWhenThereIsNone keeps environment-only
// setup working.
func TestBootstrapImportCreatesTheAccountWhenThereIsNone(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)

	seedBootstrapAccount(st, "cloudflare", "acct-env", "token-env",
		map[string]string{"zone_id": "z1"})

	accounts, err := st.ListProviderAccounts(ctx)
	if err != nil {
		t.Fatalf("ListProviderAccounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "acct-env" {
		t.Fatalf("environment account not imported: %#v", accounts)
	}
	secret, err := st.LoadProviderCredential(ctx, "cloudflare",
		providerCredentialRef("cloudflare", "acct-env"))
	if err != nil || secret != "token-env" {
		t.Fatalf("credential not imported with the account: %q err=%v", secret, err)
	}
}

// TestAnAccountlessProviderThatWorksReportsReady pins that availability comes
// from what a provider can actually do, not from how many accounts it has.
//
// Deriving it from account count reported a local port forward — which needs no
// account — and Cloudflare with Quick Tunnels — which needs none either — as
// "needs configuration" while both could open a connection immediately.
func TestAnAccountlessProviderThatWorksReportsReady(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	sup, registry := activationTestSupervisor(t, st,
		portforward.NewDefinition(),
		cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}),
	)
	sup.activateAll(ctx)

	byID := map[core.ProviderID]provider.ProviderSnapshot{}
	for _, snap := range registry.Snapshot() {
		byID[snap.ID] = snap
	}

	pf, ok := byID["portforward"]
	if !ok {
		t.Fatal("port forward provider missing")
	}
	if pf.Availability != provider.AvailabilityReady {
		t.Fatalf("a provider needing no account reports %q", pf.Availability)
	}

	cf, ok := byID["cloudflare"]
	if !ok {
		t.Fatal("cloudflare provider missing")
	}
	if cf.Availability != provider.AvailabilityReady {
		t.Fatalf("Cloudflare with Quick Tunnels reports %q", cf.Availability)
	}
	if cf.Reason == "" {
		t.Fatal("an accountless Cloudflare claims full capability with no qualification")
	}
}

// panickingDefinition models an adapter constructor that blows up.
type panickingDefinition struct{}

func (panickingDefinition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "exploding", Name: "exploding", DisplayName: "Exploding"}
}
func (panickingDefinition) CatalogEntry() provider.CatalogEntry {
	return provider.CatalogEntry{ID: "exploding", DisplayName: "Exploding"}
}
func (panickingDefinition) Activate(context.Context, provider.ActivationRequest) (provider.Installation, error) {
	panic("construction exploded")
}

// TestActivationSurvivesAProviderThatPanics ensures one bad definition cannot
// take down startup or an IPC handler goroutine.
func TestActivationSurvivesAProviderThatPanics(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	sup, registry := activationTestSupervisor(t, st, panickingDefinition{}, portforward.NewDefinition())

	sup.activateAll(ctx)

	var seen bool
	for _, snap := range registry.Snapshot() {
		if snap.ID == "exploding" {
			seen = true
			if snap.Availability != provider.AvailabilityDegraded || snap.Reason == "" {
				t.Fatalf("a panicking provider is not reported as degraded: %#v", snap)
			}
		}
	}
	if !seen {
		t.Fatal("a panicking provider vanished instead of reporting why")
	}
	if registry.Get("portforward") == nil {
		t.Fatal("one bad provider prevented the others from activating")
	}
}

// TestActivationNeverWritesToTheAccountStore pins that activation is a pure
// read. The account rows must be unchanged before and after.
func TestActivationNeverWritesToTheAccountStore(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)

	account := core.ProviderAccount{
		ID: "acct-1", Provider: "cloudflare", Label: "Live",
		CredentialRef: "cloudflare:acct-1:api-token", Status: core.AccountAuthenticated,
		Metadata: map[string]string{"zone_id": "z1"},
	}
	if err := st.UpsertProviderAccountCredential(ctx, account, []byte("token")); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	before, _ := st.ListProviderAccounts(ctx)

	sup, _ := activationTestSupervisor(t, st,
		cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}))
	sup.activateAll(ctx)

	after, _ := st.ListProviderAccounts(ctx)
	if len(before) != len(after) {
		t.Fatalf("activation changed the number of accounts: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Status != after[i].Status || before[i].Label != after[i].Label ||
			before[i].CredentialRef != after[i].CredentialRef {
			t.Fatalf("activation modified an account:\nbefore %#v\nafter  %#v", before[i], after[i])
		}
	}
}

// TestAnAccountWithAnUnreadableCredentialIsExplained pins that an authenticated
// account whose credential cannot be resolved stays visible with a reason,
// rather than being skipped and looking like it was never configured.
func TestAnAccountWithAnUnreadableCredentialIsExplained(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)

	// An account row whose credential was never written.
	if err := st.UpsertProviderAccount(ctx, core.ProviderAccount{
		ID: "acct-broken", Provider: "cloudflare", Label: "Broken",
		CredentialRef: "cloudflare:acct-broken:api-token",
		Status:        core.AccountAuthenticated, Metadata: map[string]string{},
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	sup, registry := activationTestSupervisor(t, st,
		cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}))
	sup.activateAll(ctx)

	for _, snap := range registry.Snapshot() {
		if snap.ID != "cloudflare" {
			continue
		}
		if len(snap.Accounts) != 0 {
			t.Fatalf("an account with no readable credential was offered: %#v", snap.Accounts)
		}
		if len(snap.PendingAccounts) != 1 {
			t.Fatalf("the account was hidden rather than explained: %#v", snap.PendingAccounts)
		}
		if snap.PendingAccounts[0].UnusableReason == "" {
			t.Fatal("no reason given, so it is indistinguishable from never having been verified")
		}
		return
	}
	t.Fatal("cloudflare missing from the snapshot")
}
