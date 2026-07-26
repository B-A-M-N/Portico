package supervisor

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

type repairObservationProvider struct{ observed *core.ObservedConnection }

func (*repairObservationProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "repair-observer", DisplayName: "repair observer"}
}
func (*repairObservationProvider) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{}, nil
}
func (*repairObservationProvider) Authenticate(context.Context, core.AuthRequest) error { return nil }
func (*repairObservationProvider) Plan(context.Context, core.DesiredConnection) (*core.OperationPlan, error) {
	return nil, nil
}
func (*repairObservationProvider) ExecuteStep(context.Context, core.ConnectionID, core.PlanStep) (core.StepResult, error) {
	return core.StepResult{}, nil
}
func (p *repairObservationProvider) Observe(context.Context, core.ConnectionID) (*core.ObservedConnection, error) {
	return p.observed, nil
}
func (*repairObservationProvider) Apply(context.Context, core.OperationPlan) (<-chan core.Event, error) {
	return nil, nil
}
func (*repairObservationProvider) Repair(context.Context, core.RepairPlan) (<-chan core.Event, error) {
	return nil, nil
}
func (*repairObservationProvider) Remove(context.Context, core.RemovePlan) (<-chan core.Event, error) {
	return nil, nil
}

func TestBuildDNSCreatePlanMaterializesTrackedTunnelID(t *testing.T) {
	profile := &core.ConnectionProfile{
		ID:       "conn-1",
		Revision: 1,
		Provider: core.ProviderSelection{ProviderID: "cloudflare"},
		Exposure: core.ExposureSpec{RequestedAddress: "service.example.com"},
	}
	plan := buildDNSCreatePlan(profile, "missing-dns-id", "persisted-tunnel-id")
	if len(plan.Steps) != 1 {
		t.Fatalf("plan steps = %d, want 1", len(plan.Steps))
	}
	step := plan.Steps[0]
	if step.Technical.Parameters["tunnel_id"] != "persisted-tunnel-id" {
		t.Fatalf("repair plan tunnel_id = %q", step.Technical.Parameters["tunnel_id"])
	}
	if step.Technical.Parameters["hostname"] != "service.example.com" {
		t.Fatalf("repair plan hostname = %q", step.Technical.Parameters["hostname"])
	}
}

func TestReconcileOpenConnectionRepairsOnlyMissingDNS(t *testing.T) {
	profile := &core.ConnectionProfile{
		ID:       "conn-1",
		Revision: 1,
		Provider: core.ProviderSelection{ProviderID: "cloudflare"},
		Exposure: core.ExposureSpec{RequestedAddress: "service.example.com"},
		Desired:  core.DesiredOpen,
	}
	decision, err := (&Supervisor{}).computeReconcileDecision(context.Background(), ReconcileInput{
		Profile: profile,
		Runtime: &core.ConnectionRuntime{
			ConnectionID: profile.ID,
			State:        core.RuntimeOpen,
			Connector:    core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
		},
		Resources: []core.ProviderResource{
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceTunnel, ExternalID: "tunnel-1", Ownership: core.OwnershipManaged},
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceDNSRecord, ExternalID: "dns-1", Ownership: core.OwnershipManaged},
		},
		Observed: &core.ObservedConnection{ResourceStatuses: []core.ObservedResourceStatus{{
			Type: core.ResourceDNSRecord, ExternalID: "dns-1", Status: core.ObservationMissing,
		}}},
	})
	if err != nil {
		t.Fatalf("computeReconcileDecision: %v", err)
	}
	if decision.Action != "repair" || decision.Plan == nil || len(decision.Plan.Steps) != 1 {
		t.Fatalf("unexpected decision: %#v", decision)
	}
	step := decision.Plan.Steps[0]
	if step.Kind != core.StepCreateDNSRecord || step.Technical.Parameters["tunnel_id"] != "tunnel-1" {
		t.Fatalf("unexpected DNS-only repair step: %#v", step)
	}
}

func TestReconcileWithoutRuntimeRepairsMissingDNSBeforeStartingConnector(t *testing.T) {
	profile := &core.ConnectionProfile{
		ID:       "conn-without-runtime",
		Revision: 1,
		Provider: core.ProviderSelection{ProviderID: "cloudflare"},
		Exposure: core.ExposureSpec{RequestedAddress: "service.example.com"},
		Desired:  core.DesiredOpen,
	}
	decision, err := (&Supervisor{}).computeReconcileDecision(context.Background(), ReconcileInput{
		Profile: profile,
		Resources: []core.ProviderResource{
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceTunnel, ExternalID: "tunnel-1", Ownership: core.OwnershipManaged},
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceDNSRecord, ExternalID: "dns-1", Ownership: core.OwnershipManaged},
		},
		Observed: &core.ObservedConnection{ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceTunnel, ExternalID: "tunnel-1", Status: core.ObservationPresent},
			{Type: core.ResourceDNSRecord, ExternalID: "dns-1", Status: core.ObservationMissing},
		}},
	})
	if err != nil {
		t.Fatalf("computeReconcileDecision: %v", err)
	}
	if decision.Action != "repair" || decision.Plan == nil || len(decision.Plan.Steps) != 1 || decision.Plan.Steps[0].Kind != core.StepCreateDNSRecord {
		t.Fatalf("expected DNS-only repair without runtime, got %#v", decision)
	}
}

func TestReconcileOpenConnectionUpdatesOnlyDriftedDNSTarget(t *testing.T) {
	profile := &core.ConnectionProfile{
		ID:       "conn-1",
		Revision: 1,
		Provider: core.ProviderSelection{ProviderID: "cloudflare"},
		Exposure: core.ExposureSpec{RequestedAddress: "service.example.com"},
		Desired:  core.DesiredOpen,
	}
	decision, err := (&Supervisor{}).computeReconcileDecision(context.Background(), ReconcileInput{
		Profile: profile,
		Runtime: &core.ConnectionRuntime{
			ConnectionID: profile.ID, State: core.RuntimeOpen,
			Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
		},
		Resources: []core.ProviderResource{
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceTunnel, ExternalID: "tunnel-1", Ownership: core.OwnershipManaged},
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceDNSRecord, ExternalID: "dns-1", Ownership: core.OwnershipManaged},
		},
		Observed: &core.ObservedConnection{
			ResourceStatuses: []core.ObservedResourceStatus{
				{Type: core.ResourceTunnel, ExternalID: "tunnel-1", Status: core.ObservationPresent},
				{Type: core.ResourceDNSRecord, ExternalID: "dns-1", Status: core.ObservationPresent},
			},
			DNSRecords: []core.ObservedDNSRecord{{ID: "dns-1", Name: "service.example.com", Type: "CNAME", Target: "wrong-tunnel.cfargotunnel.com."}},
		},
	})
	if err != nil {
		t.Fatalf("computeReconcileDecision: %v", err)
	}
	if decision.Action != "repair" || decision.Plan == nil || len(decision.Plan.Steps) != 1 {
		t.Fatalf("unexpected decision: %#v", decision)
	}
	step := decision.Plan.Steps[0]
	if step.Kind != core.StepUpdateDNSRecord || step.Technical.ResourceID != "dns-1" || step.Technical.Parameters["tunnel_id"] != "tunnel-1" {
		t.Fatalf("unexpected DNS drift repair step: %#v", step)
	}
}

func TestReconcileOpenConnectionRepairsOnlyMissingAccessApplication(t *testing.T) {
	profile := &core.ConnectionProfile{
		ID:       "conn-access-app",
		Revision: 1,
		Provider: core.ProviderSelection{ProviderID: "cloudflare"},
		Exposure: core.ExposureSpec{RequestedAddress: "service.example.com"},
		Protection: core.ProtectionSpec{
			Kind: core.ProtectionEmailOTP, AllowedEmails: []string{"person@example.com"},
		},
		Desired: core.DesiredOpen,
	}
	decision, err := (&Supervisor{}).computeReconcileDecision(context.Background(), ReconcileInput{
		Profile: profile,
		Runtime: &core.ConnectionRuntime{
			ConnectionID: profile.ID, State: core.RuntimeOpen,
			Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
		},
		Resources: []core.ProviderResource{
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceTunnel, ExternalID: "tunnel-1", Ownership: core.OwnershipManaged},
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceAccessApp, ExternalID: "access-app-1", Ownership: core.OwnershipManaged},
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceAccessPolicy, ExternalID: "access-policy-1", Ownership: core.OwnershipManaged, Metadata: map[string]string{"app_id": "access-app-1"}},
		},
		Observed: &core.ObservedConnection{ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceTunnel, ExternalID: "tunnel-1", Status: core.ObservationPresent},
			{Type: core.ResourceAccessApp, ExternalID: "access-app-1", Status: core.ObservationMissing},
			{Type: core.ResourceAccessPolicy, ExternalID: "access-policy-1", Status: core.ObservationMissing},
		}},
	})
	if err != nil {
		t.Fatalf("computeReconcileDecision: %v", err)
	}
	if decision.Action != "repair" || decision.Plan == nil || len(decision.Plan.Steps) != 1 {
		t.Fatalf("unexpected decision: %#v", decision)
	}
	step := decision.Plan.Steps[0]
	if step.Kind != core.StepCreateAccessApp || step.Technical.Parameters["replaces_access_app_id"] != "access-app-1" || step.Technical.Parameters["replaces_access_policy_id"] != "access-policy-1" {
		t.Fatalf("unexpected Access-app repair step: %#v", step)
	}
	if step.Technical.Parameters["protection_kind"] != string(core.ProtectionEmailOTP) || step.Technical.Parameters["allowed_emails"] != "person@example.com" {
		t.Fatalf("repair did not preserve desired protection: %#v", step.Technical.Parameters)
	}
}

func TestReconcileOpenConnectionRepairsOnlyMissingAccessPolicy(t *testing.T) {
	profile := &core.ConnectionProfile{
		ID:       "conn-access-policy",
		Revision: 1,
		Provider: core.ProviderSelection{ProviderID: "cloudflare"},
		Exposure: core.ExposureSpec{RequestedAddress: "service.example.com"},
		Protection: core.ProtectionSpec{
			Kind: core.ProtectionEmailOTP, AllowedDomains: []string{"example.com"},
		},
		Desired: core.DesiredOpen,
	}
	decision, err := (&Supervisor{}).computeReconcileDecision(context.Background(), ReconcileInput{
		Profile: profile,
		Runtime: &core.ConnectionRuntime{
			ConnectionID: profile.ID, State: core.RuntimeOpen,
			Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
		},
		Resources: []core.ProviderResource{
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceAccessApp, ExternalID: "access-app-1", Ownership: core.OwnershipManaged},
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceAccessPolicy, ExternalID: "access-policy-1", Ownership: core.OwnershipManaged, Metadata: map[string]string{"app_id": "access-app-1"}},
		},
		Observed: &core.ObservedConnection{ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceAccessApp, ExternalID: "access-app-1", Status: core.ObservationPresent},
			{Type: core.ResourceAccessPolicy, ExternalID: "access-policy-1", Status: core.ObservationMissing},
		}},
	})
	if err != nil {
		t.Fatalf("computeReconcileDecision: %v", err)
	}
	if decision.Action != "repair" || decision.Plan == nil || len(decision.Plan.Steps) != 1 {
		t.Fatalf("unexpected decision: %#v", decision)
	}
	step := decision.Plan.Steps[0]
	if step.Kind != core.StepCreateAccessPolicy || step.Technical.Parameters["app_id"] != "access-app-1" || step.Technical.Parameters["replaces_access_policy_id"] != "access-policy-1" {
		t.Fatalf("unexpected Access-policy repair step: %#v", step)
	}
}

func TestReconcileOpenConnectionUpdatesOnlyDriftedAccessApplicationDomain(t *testing.T) {
	profile := &core.ConnectionProfile{
		ID:       "conn-access-domain",
		Revision: 1,
		Provider: core.ProviderSelection{ProviderID: "cloudflare"},
		Exposure: core.ExposureSpec{RequestedAddress: "service.example.com"},
		Protection: core.ProtectionSpec{
			Kind: core.ProtectionEmailOTP, AllowedEmails: []string{"person@example.com"},
		},
		Desired: core.DesiredOpen,
	}
	decision, err := (&Supervisor{}).computeReconcileDecision(context.Background(), ReconcileInput{
		Profile: profile,
		Runtime: &core.ConnectionRuntime{
			ConnectionID: profile.ID, State: core.RuntimeOpen,
			Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
		},
		Resources: []core.ProviderResource{{
			ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceAccessApp,
			ExternalID: "access-app-1", Ownership: core.OwnershipManaged,
		}},
		Observed: &core.ObservedConnection{
			ResourceStatuses: []core.ObservedResourceStatus{{Type: core.ResourceAccessApp, ExternalID: "access-app-1", Status: core.ObservationPresent}},
			AccessApps:       []core.ObservedAccessApp{{ID: "access-app-1", Domain: "old.example.com"}},
		},
	})
	if err != nil {
		t.Fatalf("computeReconcileDecision: %v", err)
	}
	if decision.Action != "repair" || decision.Plan == nil || len(decision.Plan.Steps) != 1 {
		t.Fatalf("unexpected decision: %#v", decision)
	}
	step := decision.Plan.Steps[0]
	if step.Kind != core.StepUpdateAccessApp || step.Technical.ResourceID != "access-app-1" || step.Technical.Parameters["hostname"] != "service.example.com" {
		t.Fatalf("unexpected Access-app domain repair step: %#v", step)
	}
}

func TestReconcileErrorConnectionStillRepairsAuthoritativelyMissingAccessPolicy(t *testing.T) {
	profile := &core.ConnectionProfile{
		ID:       "conn-error-access-policy",
		Revision: 1,
		Provider: core.ProviderSelection{ProviderID: "cloudflare"},
		Exposure: core.ExposureSpec{RequestedAddress: "service.example.com"},
		Protection: core.ProtectionSpec{
			Kind: core.ProtectionEmailOTP, AllowedEmails: []string{"person@example.com"},
		},
		Desired: core.DesiredOpen,
	}
	decision, err := (&Supervisor{}).computeReconcileDecision(context.Background(), ReconcileInput{
		Profile: profile,
		Runtime: &core.ConnectionRuntime{
			ConnectionID: profile.ID, State: core.RuntimeError,
			Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
		},
		Resources: []core.ProviderResource{
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceAccessApp, ExternalID: "access-app-1", Ownership: core.OwnershipManaged},
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceAccessPolicy, ExternalID: "access-policy-1", Ownership: core.OwnershipManaged, Metadata: map[string]string{"app_id": "access-app-1"}},
		},
		Observed: &core.ObservedConnection{ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceAccessApp, ExternalID: "access-app-1", Status: core.ObservationPresent},
			{Type: core.ResourceAccessPolicy, ExternalID: "access-policy-1", Status: core.ObservationMissing},
		}},
	})
	if err != nil {
		t.Fatalf("computeReconcileDecision: %v", err)
	}
	if decision.Action != "repair" || decision.Plan == nil || len(decision.Plan.Steps) != 1 || decision.Plan.Steps[0].Kind != core.StepCreateAccessPolicy {
		t.Fatalf("expected policy-only repair from error runtime, got %#v", decision)
	}
}

func TestReconcileOpenConnectionUpdatesOnlyDriftedAccessPolicy(t *testing.T) {
	profile := &core.ConnectionProfile{
		ID:       "conn-access-policy-drift",
		Revision: 1,
		Provider: core.ProviderSelection{ProviderID: "cloudflare"},
		Exposure: core.ExposureSpec{RequestedAddress: "service.example.com"},
		Protection: core.ProtectionSpec{
			Kind: core.ProtectionEmailOTP, AllowedEmails: []string{"person@example.com"},
		},
		Desired: core.DesiredOpen,
	}
	decision, err := (&Supervisor{}).computeReconcileDecision(context.Background(), ReconcileInput{
		Profile: profile,
		Runtime: &core.ConnectionRuntime{
			ConnectionID: profile.ID, State: core.RuntimeOpen,
			Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
		},
		Resources: []core.ProviderResource{
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceAccessApp, ExternalID: "access-app-1", Ownership: core.OwnershipManaged},
			{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceAccessPolicy, ExternalID: "access-policy-1", Ownership: core.OwnershipManaged, Metadata: map[string]string{"app_id": "access-app-1"}},
		},
		Observed: &core.ObservedConnection{
			ResourceStatuses: []core.ObservedResourceStatus{
				{Type: core.ResourceAccessApp, ExternalID: "access-app-1", Status: core.ObservationPresent},
				{Type: core.ResourceAccessPolicy, ExternalID: "access-policy-1", Status: core.ObservationPresent},
			},
			AccessPolicies: []core.ObservedAccessPolicy{{
				ID: "access-policy-1", AppID: "access-app-1", Decision: "allow", AllowedEmails: []string{"other@example.com"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("computeReconcileDecision: %v", err)
	}
	if decision.Action != "repair" || decision.Plan == nil || len(decision.Plan.Steps) != 1 {
		t.Fatalf("unexpected decision: %#v", decision)
	}
	step := decision.Plan.Steps[0]
	if step.Kind != core.StepUpdateAccessPolicy || step.Technical.ResourceID != "access-policy-1" || step.Technical.Parameters["app_id"] != "access-app-1" || step.Technical.Parameters["allowed_emails"] != "person@example.com" {
		t.Fatalf("unexpected Access-policy update step: %#v", step)
	}
}

func TestRepairPreviewUsesAuthoritativeDNSDelta(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	connID := core.ConnectionID("conn-repair-preview")
	profile := recoveryTestProfile(connID)
	profile.Provider.ProviderID = "repair-observer"
	profile.Exposure.Mode = core.ExposurePermanent
	profile.Exposure.RequestedAddress = "service.example.com"
	if err := st.SaveProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	runtime := recoveryTestRuntime(connID)
	runtime.State = core.RuntimeOpen
	runtime.Connector.Status = core.ConnectorStatusRunning
	if err := st.SaveRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []*core.ProviderResource{
		{ConnectionID: connID, ProviderID: "repair-observer", Type: core.ResourceTunnel, ExternalID: "tunnel-1", Ownership: core.OwnershipManaged},
		{ConnectionID: connID, ProviderID: "repair-observer", Type: core.ResourceDNSRecord, ExternalID: "dns-1", Ownership: core.OwnershipManaged},
	} {
		if err := st.SaveResource(ctx, resource); err != nil {
			t.Fatal(err)
		}
	}
	observer := &repairObservationProvider{observed: &core.ObservedConnection{
		ConnectionID: connID, ProviderID: "repair-observer",
		ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceTunnel, ExternalID: "tunnel-1", Status: core.ObservationPresent},
			{Type: core.ResourceDNSRecord, ExternalID: "dns-1", Status: core.ObservationMissing},
		},
		Connector: &core.ObservedConnector{Status: "running"},
	}}
	registry := provider.NewRegistry()
	if err := registry.Add(observer); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(registry, st)
	ctrl.RestoreProfile(profile)
	ctrl.RestoreRuntime(runtime)
	handler := supervisorHandler{sup: &Supervisor{store: st, controller: ctrl, registry: registry}}

	plan, err := handler.HandlePlanRepair(string(connID))
	if err != nil {
		t.Fatalf("HandlePlanRepair: %v", err)
	}
	if plan.Noop || plan.Intent != string(core.IntentRepair) || len(plan.Steps) != 1 || plan.Steps[0].ID != "repair-dns-dns-1" {
		t.Fatalf("expected DNS-only repair preview, got %#v", plan)
	}
}
