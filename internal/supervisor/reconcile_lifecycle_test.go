package supervisor

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// Reconciling private-network and client-tunnel connections.
//
// These prove durable desired state is re-established after the supervisor's in-memory
// state disappears — the central promise. The "survives restart" tests only prove the
// profile can be loaded from SQLite; these prove the connection is actually moved back to
// where it should be.

// testSupervisor builds a supervisor with a stub provider that returns fixed
// observations, so reconciliation decisions that need plan generation can complete.
func testSupervisor(t *testing.T, profile *core.ConnectionProfile, obs *core.ObservedConnection) *Supervisor {
	t.Helper()
	st := newRecoveryTestStore(t)
	registry := provider.NewRegistry()
	if err := registry.Add(&stubReconcile{observed: obs}); err != nil {
		t.Fatalf("registering the provider: %v", err)
	}
	ctrl := controller.New(registry, st)
	ctrl.SetConnectionStorer(st)
	// Register the profile so the controller can plan against it.
	_, _, err := ctrl.CreateProfile(context.Background(), profile)
	if err != nil {
		t.Fatalf("registering the profile: %v", err)
	}
	return &Supervisor{store: st, registry: registry, controller: ctrl, mutating: true}
}

// stubReconcile returns a fixed observation.
type stubReconcile struct{ observed *core.ObservedConnection }

func (*stubReconcile) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "tailscale", Name: "tailscale", DisplayName: "Tailscale"}
}
func (*stubReconcile) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		Kinds:           []core.ConnectionKind{core.ConnectionPrivateNetwork, core.ConnectionClientTunnel},
		PrivateExposure: core.CapabilitySupport{Supported: true},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Private: true},
		},
		Redundancy: core.RedundancyCapability{Supported: false, MaxConnectors: 1},
	}, nil
}
func (*stubReconcile) Authenticate(context.Context, core.AuthRequest) error { return nil }
func (*stubReconcile) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	plan := &core.OperationPlan{
		ID: core.NewPlanID(), ConnectionID: desired.Profile.ID,
		ProfileRevision: desired.Profile.Revision, Provider: "stub_reconcile",
	}
	if desired.Profile.Desired == core.DesiredClosed {
		plan.Intent = core.IntentClose
		plan.Expected.State = core.RuntimeClosed
	} else {
		plan.Intent = core.IntentOpen
		plan.Expected.State = core.RuntimeOpen
	}
	plan.Steps = []core.PlanStep{{ID: "stub-step", Kind: core.StepStartConnector, Summary: "stub"}}
	if err := plan.ComputeFingerprint(); err != nil {
		return nil, err
	}
	return plan, nil
}
func (*stubReconcile) ExecuteStep(context.Context, core.ConnectionID,
	core.PlanStep) (core.StepResult, error) {
	return core.StepResult{Succeeded: true}, nil
}
func (s *stubReconcile) Observe(_ context.Context,
	id core.ConnectionID) (*core.ObservedConnection, error) {
	if s.observed == nil || s.observed.ConnectionID != id {
		return &core.ObservedConnection{ConnectionID: id, ProviderID: "stub_reconcile"}, nil
	}
	return s.observed, nil
}

// TestAHealthyJoinNeedsNoReconciliation pins the common case.
func TestAHealthyJoinNeedsNoReconciliation(t *testing.T) {
	profile := networkProfile("conn-join", core.PrivateNetworkJoin, "")
	obs := &core.ObservedConnection{
		ConnectionID: profile.ID, ProviderID: "stub_reconcile",
		ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
				Status: core.ObservationPresent},
		},
	}

	decision, err := testSupervisor(t, profile, obs).computeReconcileDecision(context.Background(),
		ReconcileInput{Profile: profile, Observed: obs})
	if err != nil {
		t.Fatalf("computeReconcileDecision: %v", err)
	}
	if decision.Action != "none" {
		t.Fatalf("a healthy join produced action %q", decision.Action)
	}
}

// TestADepartedMachineIsBlockedNotRepaired pins the honest refusal.
func TestADepartedMachineIsBlockedNotRepaired(t *testing.T) {
	profile := networkProfile("conn-join", core.PrivateNetworkJoin, "")
	obs := &core.ObservedConnection{
		ConnectionID: profile.ID, ProviderID: "stub_reconcile",
		ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
				Status: core.ObservationMissing},
		},
	}

	decision, err := testSupervisor(t, profile, obs).computeReconcileDecision(context.Background(),
		ReconcileInput{Profile: profile, Observed: obs})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "none" {
		t.Fatalf("action = %q, want none (blocked)", decision.Action)
	}
	if decision.Blocked == nil {
		t.Fatal("a departed machine is not reported as blocked")
	}
}

// TestAMissingServeIsRestored pins the drift Portico owns.
func TestAMissingServeIsRestored(t *testing.T) {
	profile := networkProfile("conn-serve", core.PrivateNetworkExpose, "127.0.0.1:3000")
	obs := &core.ObservedConnection{
		ConnectionID: profile.ID, ProviderID: "stub_reconcile",
		ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
				Status: core.ObservationPresent},
			{Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
				Status: core.ObservationMissing},
		},
	}

	decision, err := testSupervisor(t, profile, obs).computeReconcileDecision(context.Background(),
		ReconcileInput{Profile: profile, Observed: obs})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "repair" {
		t.Fatalf("a missing serve produced action %q, want repair", decision.Action)
	}
}

// TestAHealthyExposeNeedsNoReconciliation pins the healthy expose case.
func TestAHealthyExposeNeedsNoReconciliation(t *testing.T) {
	profile := networkProfile("conn-serve", core.PrivateNetworkExpose, "127.0.0.1:3000")
	obs := &core.ObservedConnection{
		ConnectionID: profile.ID, ProviderID: "stub_reconcile",
		ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
				Status: core.ObservationPresent},
			{Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
				Status: core.ObservationPresent},
		},
	}

	decision, err := testSupervisor(t, profile, obs).computeReconcileDecision(context.Background(),
		ReconcileInput{Profile: profile, Observed: obs})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "none" {
		t.Fatalf("a healthy expose produced action %q", decision.Action)
	}
}

// TestAClosedExposeWithdrawsTheServe pins the close path.
func TestAClosedExposeWithdrawsTheServe(t *testing.T) {
	profile := networkProfile("conn-serve", core.PrivateNetworkExpose, "127.0.0.1:3000")
	profile.Desired = core.DesiredClosed
	obs := &core.ObservedConnection{
		ConnectionID: profile.ID, ProviderID: "stub_reconcile",
		ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
				Status: core.ObservationPresent},
			{Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
				Status: core.ObservationPresent},
		},
	}

	decision, err := testSupervisor(t, profile, obs).computeReconcileDecision(context.Background(),
		ReconcileInput{Profile: profile, Observed: obs})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "close" {
		t.Fatalf("a closed expose still serving produced action %q, want close", decision.Action)
	}
}

// TestAClosedJoinReleasesTrackingOnly pins that closing a join touches nothing.
func TestAClosedJoinReleasesTrackingOnly(t *testing.T) {
	profile := networkProfile("conn-join", core.PrivateNetworkJoin, "")
	profile.Desired = core.DesiredClosed
	obs := &core.ObservedConnection{
		ConnectionID: profile.ID, ProviderID: "stub_reconcile",
		ResourceStatuses: []core.ObservedResourceStatus{
			{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
				Status: core.ObservationPresent},
		},
	}

	decision, err := testSupervisor(t, profile, obs).computeReconcileDecision(context.Background(),
		ReconcileInput{Profile: profile, Observed: obs})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "none" {
		t.Fatalf("a closed join produced action %q", decision.Action)
	}
}

// Client-tunnel reconciliation.

// TestAStoppedClientTunnelIsRestarted pins the graceful-shutdown recovery.
func TestAStoppedClientTunnelIsRestarted(t *testing.T) {
	profile := reconcileClientTunnelProfile()
	obs := &core.ObservedConnection{
		ConnectionID: profile.ID, ProviderID: "stub_reconcile",
		Connector: &core.ObservedConnector{Status: string(core.ConnectorStatusStopped)},
	}

	decision, err := testSupervisor(t, profile, obs).computeReconcileDecision(context.Background(),
		ReconcileInput{Profile: profile, Observed: obs})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "open" {
		t.Fatalf("a stopped open tunnel produced action %q, want open", decision.Action)
	}
}

// TestAHealthyClientTunnelNeedsNoReconciliation pins the common case.
func TestAHealthyClientTunnelNeedsNoReconciliation(t *testing.T) {
	profile := reconcileClientTunnelProfile()
	obs := &core.ObservedConnection{
		ConnectionID: profile.ID, ProviderID: "stub_reconcile",
		Connector: &core.ObservedConnector{Status: string(core.ConnectorStatusRunning)},
	}

	decision, err := testSupervisor(t, profile, obs).computeReconcileDecision(context.Background(),
		ReconcileInput{Profile: profile, Observed: obs})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "none" {
		t.Fatalf("a running open tunnel produced action %q", decision.Action)
	}
}

// TestAnAliveButUnreadyClientTunnelIsRepaired pins the truthfulness rule at
// the reconciliation layer: a client that answers /healthz while /readyz fails
// is observed unstable, and reconciliation must act on it rather than accept
// PID liveness as a working tunnel. This is exactly the state a control-plane
// outage or a refused credential produces.
func TestAnAliveButUnreadyClientTunnelIsRepaired(t *testing.T) {
	profile := reconcileClientTunnelProfile()
	obs := &core.ObservedConnection{
		ConnectionID: profile.ID, ProviderID: "stub_reconcile",
		Connector: &core.ObservedConnector{
			PID:       4242,
			Status:    string(core.ConnectorStatusUnstable),
			LastError: "the tunnel client is running but not ready (control plane unreachable or credential rejected)",
		},
	}

	decision, err := testSupervisor(t, profile, obs).computeReconcileDecision(context.Background(),
		ReconcileInput{Profile: profile, Observed: obs})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "repair" {
		t.Fatalf("an alive-but-unready tunnel produced action %q, want repair", decision.Action)
	}
	if decision.Plan == nil || len(decision.Plan.Steps) == 0 {
		t.Fatal("the repair decision carries no steps")
	}
}

// TestAClosedClientTunnelIsStopped pins the close path.
func TestAClosedClientTunnelIsStopped(t *testing.T) {
	profile := reconcileClientTunnelProfile()
	profile.Desired = core.DesiredClosed
	obs := &core.ObservedConnection{
		ConnectionID: profile.ID, ProviderID: "stub_reconcile",
		Connector: &core.ObservedConnector{Status: string(core.ConnectorStatusRunning)},
	}

	decision, err := testSupervisor(t, profile, obs).computeReconcileDecision(context.Background(),
		ReconcileInput{Profile: profile, Observed: obs})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "close" {
		t.Fatalf("a closed running tunnel produced action %q, want close", decision.Action)
	}
}

// reconcileClientTunnelProfile has the driver the shared fixture lacks, so the controller
// can look up the provider.
func reconcileClientTunnelProfile() *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID: "ct-1", Name: "mcp", Kind: core.ConnectionClientTunnel,
		Desired: core.DesiredOpen, Revision: 1,
		Driver: core.DriverSelection{ProviderID: "tailscale"},
		Spec: core.ConnectionSpec{ClientTunnel: &core.ClientTunnelSpec{
			Client:   core.ClientOpenAISecureMCPTunnel,
			TunnelID: "tun-abc",
			MCP: core.MCPServiceSpec{
				Transport: core.MCPTransportStreamable, Endpoint: "http://127.0.0.1:3000/mcp",
			},
		}},
	}
}
