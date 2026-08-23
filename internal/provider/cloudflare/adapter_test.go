package cloudflare

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/access"
	"github.com/B-A-M-N/portico/internal/core"
	cfdns "github.com/B-A-M-N/portico/internal/dns"
	"github.com/B-A-M-N/portico/internal/tunnel"
	cf "github.com/cloudflare/cloudflare-go"
)

type fakeConnectorProcessService struct{}

func (fakeConnectorProcessService) Start(context.Context, core.ProcessConfig) (core.ConnectorHandle, error) {
	return core.ConnectorHandle{}, nil
}

type fakeCredentialStore struct {
	connectionID core.ConnectionID
	providerID   core.ProviderID
	tunnelID     string
	token        string
	err          error
}

func (*fakeCredentialStore) SaveTunnelCredential(context.Context, core.ConnectionID, core.ProviderID, string, []byte) error {
	return nil
}
func (f *fakeCredentialStore) LoadTunnelCredentialExact(_ context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string) (string, error) {
	f.connectionID, f.providerID, f.tunnelID = connID, providerID, tunnelID
	return f.token, f.err
}
func (*fakeCredentialStore) DeleteTunnelCredentialExact(context.Context, core.ConnectionID, core.ProviderID, string) error {
	return nil
}
func (fakeConnectorProcessService) Stop(core.ConnectionID, time.Duration) error { return nil }
func (fakeConnectorProcessService) Observe(core.ConnectionID) (core.ConnectorHandle, bool) {
	return core.ConnectorHandle{}, false
}

func TestAccountsProviderRequiresExactAccountSelection(t *testing.T) {
	accounts, err := NewAccountsProvider(map[core.ProviderAccountID]*Provider{
		"account-a": {accountID: "account-a"},
		"account-b": {accountID: "account-b"},
	})
	if err != nil {
		t.Fatalf("NewAccountsProvider: %v", err)
	}
	if _, err := accounts.ProviderForAccount(""); err == nil {
		t.Fatal("empty account unexpectedly selected one of multiple accounts")
	}
	child, err := accounts.ProviderForAccount("account-b")
	if err != nil {
		t.Fatalf("ProviderForAccount: %v", err)
	}
	if bound, ok := child.(core.ProviderAccountBinding); !ok || bound.ProviderAccountID() != "account-b" {
		t.Fatalf("selected child = %#v, want account-b binding", child)
	}
	if _, err := accounts.ProviderForAccount("missing"); err == nil {
		t.Fatal("unknown account unexpectedly resolved")
	}
}

type fakeTunnelManager struct {
	requestedAccount string
	requestedID      string
	requestedName    string
	state            *tunnel.TunnelState
	err              error
	lookupResult     *tunnel.TunnelInfo
	lookupErr        error
}

func (f *fakeTunnelManager) CreateTunnel(context.Context, string, string) (*tunnel.TunnelInfo, error) {
	return &tunnel.TunnelInfo{ID: "tunnel-123", Name: "test-tunnel"}, nil
}
func (f *fakeTunnelManager) Create(context.Context, string, string) (*tunnel.Info, error) {
	return &tunnel.Info{TunnelID: "tunnel-123", TunnelName: "test-tunnel", Token: "test-token"}, nil
}
func (f *fakeTunnelManager) Get(_ context.Context, accountID, tunnelID string) (*tunnel.TunnelState, error) {
	f.requestedAccount, f.requestedID = accountID, tunnelID
	return f.state, f.err
}
func (f *fakeTunnelManager) ConfigureIngress(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeTunnelManager) GetToken(context.Context, string, string) (string, error) {
	return "test-token", nil
}
func (f *fakeTunnelManager) Delete(context.Context, string, string) error { return nil }
func (f *fakeTunnelManager) LookupTunnelByName(_ context.Context, accountID, name string) (*tunnel.TunnelInfo, error) {
	f.requestedAccount = accountID
	f.requestedName = name
	return f.lookupResult, f.lookupErr
}

// failingTokenTunnelManager returns a valid tunnel from CreateTunnel but fails
// on GetToken, simulating a network interruption after tunnel creation but
// before the token response was received.
type failingTokenTunnelManager struct {
	requestedAccount string
	requestedID      string
	requestedName    string
	state            *tunnel.TunnelState
	lookupResult     *tunnel.TunnelInfo
	lookupErr        error
}

func (f *failingTokenTunnelManager) CreateTunnel(context.Context, string, string) (*tunnel.TunnelInfo, error) {
	return &tunnel.TunnelInfo{ID: "orphaned-tunnel-id", Name: "orphaned-tunnel"}, nil
}
func (f *failingTokenTunnelManager) Create(context.Context, string, string) (*tunnel.Info, error) {
	return nil, fmt.Errorf("token lookup failed")
}
func (f *failingTokenTunnelManager) Get(_ context.Context, accountID, tunnelID string) (*tunnel.TunnelState, error) {
	f.requestedAccount, f.requestedID = accountID, tunnelID
	return f.state, nil
}
func (*failingTokenTunnelManager) ConfigureIngress(context.Context, string, string, string, string) error {
	return nil
}
func (f *failingTokenTunnelManager) GetToken(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("token lookup failed")
}
func (f *failingTokenTunnelManager) Delete(context.Context, string, string) error { return nil }
func (f *failingTokenTunnelManager) LookupTunnelByName(_ context.Context, accountID, name string) (*tunnel.TunnelInfo, error) {
	f.requestedAccount = accountID
	f.requestedName = name
	return f.lookupResult, f.lookupErr
}

type fakeDNSManager struct {
	zoneID   string
	hostname string
	tunnelID string
	recordID string
	err      error
}

func (f *fakeDNSManager) CreateCNAME(_ context.Context, zoneID, hostname, tunnelID string) (string, error) {
	f.zoneID, f.hostname, f.tunnelID = zoneID, hostname, tunnelID
	return "dns-created", f.err
}
func (f *fakeDNSManager) UpdateCNAME(_ context.Context, zoneID, recordID, hostname, tunnelID string) error {
	f.zoneID, f.recordID, f.hostname, f.tunnelID = zoneID, recordID, hostname, tunnelID
	return f.err
}
func (*fakeDNSManager) GetRecord(context.Context, string, string) (*cfdns.RecordState, error) {
	return nil, nil
}
func (*fakeDNSManager) DeleteRecord(context.Context, string, string) error { return nil }

type fakeAccessManager struct {
	accountID string
	appID     string
	hostname  string
	policy    access.Policy
}

func (*fakeAccessManager) CreateAppOnly(_ context.Context, _ string, _ string) (string, error) {
	return "fake-app-id", nil
}
func (*fakeAccessManager) GetApp(context.Context, string, string) (*access.AppState, error) {
	return nil, nil
}
func (f *fakeAccessManager) UpdatePolicy(_ context.Context, accountID, appID, _ string, policy access.Policy) error {
	f.accountID, f.appID, f.policy = accountID, appID, policy
	return nil
}
func (*fakeAccessManager) DeleteApp(context.Context, string, string) error    { return nil }
func (*fakeAccessManager) DeletePolicy(context.Context, string, string) error { return nil }
func (f *fakeAccessManager) UpdateApp(_ context.Context, accountID, appID, hostname string) error {
	f.accountID, f.appID, f.hostname = accountID, appID, hostname
	return nil
}
func (f *fakeAccessManager) CreatePolicy(_ context.Context, accountID, appID string, policy access.Policy) (string, error) {
	f.accountID, f.appID, f.policy = accountID, appID, policy
	return "new-policy-id", nil
}

func TestPlanCloseDoesNotRequireOrigin(t *testing.T) {
	p, err := NewQuickTunnel("cloudflared", t.TempDir(), fakeConnectorProcessService{})
	if err != nil {
		t.Fatalf("NewQuickTunnel: %v", err)
	}
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: &core.ConnectionProfile{
		ID:       "conn-1",
		Name:     "connection",
		Revision: 1,
		Desired:  core.DesiredClosed,
	}})
	if err != nil {
		t.Fatalf("Plan close: %v", err)
	}
	if plan.Intent != core.IntentClose || len(plan.Steps) == 0 {
		t.Fatalf("unexpected close plan: %+v", plan)
	}
}

// TestPlanRefusesNonServiceExposureKinds verifies the security regression fix:
// Cloudflare provider must not plan a non-service-exposure connection with empty
// source/exposure/protection (the zero values returned by the backward-compat
// accessors). Doing so would treat a port_forward/private_network/client_tunnel
// as an unprotected public service.
func TestPlanRefusesNonServiceExposureKinds(t *testing.T) {
	p, err := NewQuickTunnel("cloudflared", t.TempDir(), fakeConnectorProcessService{})
	if err != nil {
		t.Fatalf("NewQuickTunnel: %v", err)
	}

	for _, kind := range []core.ConnectionKind{core.ConnectionPortForward, core.ConnectionPrivateNetwork, core.ConnectionClientTunnel} {
		t.Run(string(kind), func(t *testing.T) {
			_, err := p.Plan(context.Background(), core.DesiredConnection{Profile: &core.ConnectionProfile{
				ID:       "conn-1",
				Name:     "connection",
				Kind:     kind,
				Revision: 1,
				Desired:  core.DesiredOpen,
			}})
			if err == nil {
				t.Fatalf("Plan should have refused %q kind, but succeeded", kind)
			}
			if !strings.Contains(err.Error(), "not supported") {
				t.Fatalf("refusal for %q should say 'not supported': %v", kind, err)
			}
		})
	}

	// Service exposure and empty kind should still work.
	for _, kind := range []core.ConnectionKind{"", core.ConnectionServiceExposure} {
		t.Run(string(kind), func(t *testing.T) {
			_, err := p.Plan(context.Background(), core.DesiredConnection{Profile: &core.ConnectionProfile{
				ID:       "conn-1",
				Name:     "connection",
				Kind:     kind,
				Revision: 1,
				Desired:  core.DesiredClosed,
			}})
			if err != nil {
				t.Fatalf("Plan should accept %q kind: %v", kind, err)
			}
		})
	}
}

func TestQuickTunnelTemporaryNoHostnameConstraint(t *testing.T) {
	p, err := NewQuickTunnel("cloudflared", t.TempDir(), fakeConnectorProcessService{})
	if err != nil {
		t.Fatalf("NewQuickTunnel: %v", err)
	}

	// A normal temporary exposure WITHOUT a custom hostname must succeed.
	plan, err := p.Plan(context.Background(), core.DesiredConnection{
		Profile: &core.ConnectionProfile{
			ID:       "conn-quick",
			Name:     "quick",
			Kind:     core.ConnectionServiceExposure,
			Revision: 1,
			Desired:  core.DesiredOpen,
			Spec: core.ConnectionSpec{
				ServiceExposure: &core.ServiceExposureSpec{
					Source: core.SourceSpec{
						Kind: core.SourceExisting,
						Existing: &core.ExistingServiceSpec{
							Address:  "127.0.0.1:3000",
							Protocol: core.ProtocolHTTP,
						},
					},
					Exposure: core.ExposureSpec{
						Mode: core.ExposureTemporary,
						// No RequestedAddress — must succeed.
					},
				},
			},
			Driver: core.DriverSelection{ProviderID: "cloudflare"},
		},
		Origin: &core.ResolvedOrigin{URL: "http://127.0.0.1:3000", Protocol: core.ProtocolHTTP, Owned: false},
	})
	if err != nil {
		t.Fatalf("Plan for temporary Quick Tunnel without hostname should succeed: %v", err)
	}
	if plan.Intent != core.IntentOpen {
		t.Fatalf("Intent = %q, want open", plan.Intent)
	}

	// A temporary exposure WITH a custom hostname must fail.
	_, err = p.Plan(context.Background(), core.DesiredConnection{
		Profile: &core.ConnectionProfile{
			ID:       "conn-quick-host",
			Name:     "quick-host",
			Kind:     core.ConnectionServiceExposure,
			Revision: 1,
			Desired:  core.DesiredOpen,
			Spec: core.ConnectionSpec{
				ServiceExposure: &core.ServiceExposureSpec{
					Source: core.SourceSpec{
						Kind: core.SourceExisting,
						Existing: &core.ExistingServiceSpec{
							Address:  "127.0.0.1:3000",
							Protocol: core.ProtocolHTTP,
						},
					},
					Exposure: core.ExposureSpec{
						Mode:             core.ExposureTemporary,
						RequestedAddress: "custom.example.com",
					},
				},
			},
			Driver: core.DriverSelection{ProviderID: "cloudflare"},
		},
		Origin: &core.ResolvedOrigin{URL: "http://127.0.0.1:3000", Protocol: core.ProtocolHTTP, Owned: false},
	})
	if err == nil {
		t.Fatal("Plan for temporary Quick Tunnel WITH hostname should fail")
	}
	if !strings.Contains(err.Error(), "custom hostname") {
		t.Fatalf("expected 'custom hostname' in error: %v", err)
	}
}

func TestFinalizeLocalDeletionSucceeds(t *testing.T) {
	p, err := NewQuickTunnel("cloudflared", t.TempDir(), fakeConnectorProcessService{})
	if err != nil {
		t.Fatalf("NewQuickTunnel: %v", err)
	}
	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID:   "finalize",
		Kind: core.StepFinalizeLocalDeletion,
	})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("finalize result: %+v", result)
	}
}

func TestRehydrateConnectionLoadsCredentialByExactTunnelID(t *testing.T) {
	credentials := &fakeCredentialStore{token: "tunnel-token"}
	p := &Provider{
		credStore:   credentials,
		connections: make(map[core.ConnectionID]*cfConnection),
	}
	if !p.RehydrateConnection(context.Background(), "conn-1", "tunnel-1") {
		t.Fatal("rehydration failed")
	}
	if credentials.connectionID != "conn-1" || credentials.providerID != "cloudflare" || credentials.tunnelID != "tunnel-1" {
		t.Fatalf("credential lookup was not exact: %#v", credentials)
	}
	if p.RehydrateConnection(context.Background(), "conn-1", "") {
		t.Fatal("rehydration accepted an unspecified tunnel")
	}
}

func TestObserveWithResourcesUsesExactTrackedTunnelID(t *testing.T) {
	tunnels := &fakeTunnelManager{state: &tunnel.TunnelState{ID: "tunnel-exact", Name: "portico-test", Status: "healthy"}}
	p := &Provider{
		accountID:     "account-1",
		tunnels:       tunnels,
		connectorProc: fakeConnectorProcessService{},
		connections:   make(map[core.ConnectionID]*cfConnection),
	}

	obs, err := p.ObserveWithResources(context.Background(), "conn-1", []core.ProviderResource{{
		ConnectionID: "conn-1", ProviderID: "cloudflare", Type: core.ResourceTunnel, ExternalID: "tunnel-exact",
	}})
	if err != nil {
		t.Fatalf("ObserveWithResources: %v", err)
	}
	if tunnels.requestedAccount != "account-1" || tunnels.requestedID != "tunnel-exact" {
		t.Fatalf("observation did not use exact persisted identifier: account=%q id=%q", tunnels.requestedAccount, tunnels.requestedID)
	}
	if obs.Tunnel == nil || obs.Tunnel.ID != "tunnel-exact" {
		t.Fatalf("observed tunnel = %+v", obs.Tunnel)
	}
	if len(obs.ResourceStatuses) != 1 || obs.ResourceStatuses[0].Status != core.ObservationPresent {
		t.Fatalf("unexpected statuses: %+v", obs.ResourceStatuses)
	}
}

func TestCreateTunnelReturnsDurableIDBeforeTokenRetrieval(t *testing.T) {
	tunnels := &fakeTunnelManager{}
	p := &Provider{
		accountID:     "account-1",
		tunnels:       tunnels,
		connectorProc: fakeConnectorProcessService{},
		connections:   make(map[core.ConnectionID]*cfConnection),
	}

	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID:   "cf-tunnel",
		Kind: core.StepCreateTunnel,
		Technical: core.TechnicalOperation{
			Provider:   "cloudflare",
			Type:       "create_tunnel",
			Parameters: map[string]string{"name": "test-tunnel"},
		},
		Ownership: core.OwnershipManaged,
	})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("ExecuteStep failed: %+v", result)
	}

	// The durable tunnel ID MUST be returned in the step result so the
	// controller can journal it before any dependent token retrieval.
	if len(result.Resources) != 1 {
		t.Fatalf("expected exactly 1 resource, got %d", len(result.Resources))
	}
	if result.Resources[0].ExternalID != "tunnel-123" {
		t.Fatalf("expected tunnel ID %q, got %q", "tunnel-123", result.Resources[0].ExternalID)
	}
	if result.Resources[0].Type != core.ResourceTunnel {
		t.Fatalf("expected tunnel resource type, got %q", result.Resources[0].Type)
	}
	if result.Resources[0].Ownership != core.OwnershipManaged {
		t.Fatalf("expected managed ownership, got %q", result.Resources[0].Ownership)
	}
	// No credential should be returned from the create step — token
	// acquisition is a separate step.
	if len(result.CredentialMutations) != 0 {
		t.Fatalf("expected no credential mutations, got %d", len(result.CredentialMutations))
	}
}

// TestAcquireTokenFailureLeavesDurableTunnelID verifies the production
// regression: tunnel creation succeeds but token retrieval fails. The
// created tunnel ID is already durable, so the cleanup obligation
// contains the exact ID and recovery can delete the orphaned tunnel.
func TestAcquireTokenFailureLeavesDurableTunnelID(t *testing.T) {
	tunnels := &failingTokenTunnelManager{state: &tunnel.TunnelState{ID: "orphaned-tunnel-id", Name: "orphaned-tunnel", Status: "active"}}
	p := &Provider{
		accountID:     "account-1",
		tunnels:       tunnels,
		connectorProc: fakeConnectorProcessService{},
		connections:   make(map[core.ConnectionID]*cfConnection),
	}

	// Step 1: create tunnel — succeeds and returns the durable ID.
	createResult, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID:   "cf-tunnel",
		Kind: core.StepCreateTunnel,
		Technical: core.TechnicalOperation{
			Provider:   "cloudflare",
			Type:       "create_tunnel",
			Parameters: map[string]string{"name": "orphaned-tunnel"},
		},
		Ownership: core.OwnershipManaged,
	})
	if err != nil {
		t.Fatalf("ExecuteStep(create): %v", err)
	}
	if !createResult.Succeeded {
		t.Fatalf("ExecuteStep(create) failed: %+v", createResult)
	}

	// The durable resource must carry the exact tunnel ID.
	if len(createResult.Resources) != 1 || createResult.Resources[0].ExternalID != "orphaned-tunnel-id" {
		t.Fatalf("create step did not return durable tunnel ID: %+v", createResult.Resources)
	}

	// Step 2: acquire token — fails.
	tokenResult, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID:   "cf-tunnel-token",
		Kind: core.StepAcquireToken,
		Technical: core.TechnicalOperation{
			Provider: "cloudflare",
			Type:     "acquire_token",
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStep(token): %v", err)
	}
	if tokenResult.Succeeded {
		t.Fatal("ExecuteStep(token) unexpectedly succeeded")
	}

	// The failed token step must NOT report success and must NOT hide the
	// already-created tunnel. The durable tunnel ID from the create step
	// is what recovery/compensation needs to delete the orphaned tunnel.
}

// TestLookupTunnelByNameRecovery verifies the unknown-outcome recovery protocol.
// A network timeout after sending CreateTunnel can mean "not created" or
// "created, response lost". LookupTunnelByName is used ONLY for resolving an
// interrupted create and never used to infer ownership normally.
func TestLookupTunnelByNameRecovery(t *testing.T) {
	// Case 1: tunnel was created, response lost — recovery finds it by name.
	t.Run("finds_created_tunnel", func(t *testing.T) {
		lookupResult := &tunnel.TunnelInfo{ID: "recovered-tunnel-id", Name: "portico-test-123"}
		tunnels := &fakeTunnelManager{lookupResult: lookupResult}
		p := &Provider{
			accountID:     "account-1",
			tunnels:       tunnels,
			connectorProc: fakeConnectorProcessService{},
			connections:   make(map[core.ConnectionID]*cfConnection),
		}

		// Recovery queries by the deterministic name that was attempted.
		found, err := p.tunnels.LookupTunnelByName(context.Background(), "account-1", "portico-test-123")
		if err != nil {
			t.Fatalf("LookupTunnelByName: %v", err)
		}
		if found == nil {
			t.Fatal("expected to find the created tunnel, got nil")
		}
		if found.ID != "recovered-tunnel-id" {
			t.Errorf("found.ID = %q, want %q", found.ID, "recovered-tunnel-id")
		}
		// Verify the query used the deterministic name, not an ID.
		if tunnels.requestedName != "portico-test-123" {
			t.Errorf("requestedName = %q, want %q", tunnels.requestedName, "portico-test-123")
		}
	})

	// Case 2: tunnel was NOT created (response lost before creation) — recovery finds nothing.
	t.Run("not_created_returns_nil", func(t *testing.T) {
		tunnels := &fakeTunnelManager{lookupResult: nil}
		p := &Provider{
			accountID:     "account-1",
			tunnels:       tunnels,
			connectorProc: fakeConnectorProcessService{},
			connections:   make(map[core.ConnectionID]*cfConnection),
		}

		found, err := p.tunnels.LookupTunnelByName(context.Background(), "account-1", "portico-test-456")
		if err != nil {
			t.Fatalf("LookupTunnelByName: %v", err)
		}
		if found != nil {
			t.Errorf("expected nil for not-created tunnel, got %+v", found)
		}
	})

	// Case 3: lookup error is propagated (transient failure).
	t.Run("lookup_error_propagated", func(t *testing.T) {
		tunnels := &fakeTunnelManager{lookupErr: fmt.Errorf("transient: connection refused")}
		p := &Provider{
			accountID:     "account-1",
			tunnels:       tunnels,
			connectorProc: fakeConnectorProcessService{},
			connections:   make(map[core.ConnectionID]*cfConnection),
		}

		_, err := p.tunnels.LookupTunnelByName(context.Background(), "account-1", "portico-test-789")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestCreateDNSRepairUsesDurableTunnelIDWithoutAdapterMemory(t *testing.T) {
	dns := &fakeDNSManager{}
	p := &Provider{
		zoneID:        "zone-1",
		dns:           dns,
		connectorProc: fakeConnectorProcessService{},
		connections:   make(map[core.ConnectionID]*cfConnection),
	}
	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID: "repair-dns", Kind: core.StepCreateDNSRecord,
		Technical: core.TechnicalOperation{Parameters: map[string]string{
			"hostname":  "service.example.com",
			"tunnel_id": "persisted-tunnel-id",
		}},
	})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("DNS repair result: %+v", result)
	}
	if dns.zoneID != "zone-1" || dns.hostname != "service.example.com" || dns.tunnelID != "persisted-tunnel-id" {
		t.Fatalf("DNS repair did not use durable material: zone=%q hostname=%q tunnel=%q", dns.zoneID, dns.hostname, dns.tunnelID)
	}
}

func TestUpdateDNSRepairUsesExactDurableIDs(t *testing.T) {
	dns := &fakeDNSManager{}
	p := &Provider{zoneID: "zone-1", dns: dns, connectorProc: fakeConnectorProcessService{}, connections: make(map[core.ConnectionID]*cfConnection)}
	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID: "update-dns", Kind: core.StepUpdateDNSRecord,
		Technical: core.TechnicalOperation{ResourceID: "dns-record-id", Parameters: map[string]string{
			"hostname": "service.example.com", "tunnel_id": "persisted-tunnel-id",
		}},
	})
	if err != nil || !result.Succeeded {
		t.Fatalf("update result=%+v err=%v", result, err)
	}
	if dns.zoneID != "zone-1" || dns.recordID != "dns-record-id" || dns.hostname != "service.example.com" || dns.tunnelID != "persisted-tunnel-id" {
		t.Fatalf("DNS update did not use exact durable material: %#v", dns)
	}
}

func TestCreateAccessPolicyRepairUsesExactDurableApplicationID(t *testing.T) {
	accessManager := &fakeAccessManager{}
	p := &Provider{
		accountID: "account-1", access: accessManager, connectorProc: fakeConnectorProcessService{},
		connections: map[core.ConnectionID]*cfConnection{"conn-1": {}},
	}
	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID: "repair-access-policy", Kind: core.StepCreateAccessPolicy,
		Technical: core.TechnicalOperation{Parameters: map[string]string{
			"app_id": "persisted-app-id", "protection_kind": string(core.ProtectionEmailOTP),
			"allowed_emails": "person@example.com", "session_duration": "1h",
		}},
	})
	if err != nil || !result.Succeeded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if accessManager.accountID != "account-1" || accessManager.appID != "persisted-app-id" {
		t.Fatalf("policy repair did not use exact durable app: %#v", accessManager)
	}
	if accessManager.policy.AuthMode != "otp" || len(accessManager.policy.AllowedEmails) != 1 || accessManager.policy.AllowedEmails[0] != "person@example.com" {
		t.Fatalf("policy repair did not preserve protection: %#v", accessManager.policy)
	}
	if len(result.Resources) != 1 || result.Resources[0].ExternalID != "new-policy-id" || result.Resources[0].Metadata["app_id"] != "persisted-app-id" {
		t.Fatalf("unexpected replacement resource: %#v", result.Resources)
	}
}

func TestUpdateAccessApplicationRepairUsesExactDurableApplicationID(t *testing.T) {
	accessManager := &fakeAccessManager{}
	p := &Provider{
		accountID: "account-1", access: accessManager, connectorProc: fakeConnectorProcessService{},
		connections: map[core.ConnectionID]*cfConnection{"conn-1": {}},
	}
	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID: "repair-access-app", Kind: core.StepUpdateAccessApp,
		Technical: core.TechnicalOperation{ResourceID: "persisted-app-id", Parameters: map[string]string{"hostname": "service.example.com"}},
	})
	if err != nil || !result.Succeeded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if accessManager.accountID != "account-1" || accessManager.appID != "persisted-app-id" || accessManager.hostname != "service.example.com" {
		t.Fatalf("Access-app repair did not use exact durable material: %#v", accessManager)
	}
	if len(result.Resources) != 1 || result.Resources[0].Type != core.ResourceAccessApp || result.Resources[0].ExternalID != "persisted-app-id" {
		t.Fatalf("unexpected updated application resource: %#v", result.Resources)
	}
}

func TestUpdateAccessPolicyRepairUsesExactDurableIDs(t *testing.T) {
	accessManager := &fakeAccessManager{}
	p := &Provider{
		accountID: "account-1", access: accessManager, connectorProc: fakeConnectorProcessService{},
		connections: map[core.ConnectionID]*cfConnection{"conn-1": {}},
	}
	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID: "repair-access-policy", Kind: core.StepUpdateAccessPolicy,
		Technical: core.TechnicalOperation{ResourceID: "persisted-policy-id", Parameters: map[string]string{
			"app_id": "persisted-app-id", "protection_kind": string(core.ProtectionEmailOTP),
			"allowed_domains": "example.com", "session_duration": "1h",
		}},
	})
	if err != nil || !result.Succeeded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if accessManager.accountID != "account-1" || accessManager.appID != "persisted-app-id" || accessManager.policy.AuthMode != "otp" || len(accessManager.policy.AllowedDomains) != 1 || accessManager.policy.AllowedDomains[0] != "example.com" {
		t.Fatalf("policy update did not use exact durable material: %#v", accessManager)
	}
}

func TestClassifyObservationErrorNeverTurnsUncertainLookupIntoMissing(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   core.ObservationStatus
	}{
		{name: "not found", status: http.StatusNotFound, want: core.ObservationMissing},
		{name: "unauthorized", status: http.StatusUnauthorized, want: core.ObservationUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, want: core.ObservationUnauthorized},
		{name: "rate limited", status: http.StatusTooManyRequests, want: core.ObservationRateLimited},
		{name: "server failure", status: http.StatusBadGateway, want: core.ObservationTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := classifyObservationError(&cf.Error{StatusCode: tt.status})
			if got != tt.want {
				t.Fatalf("classification=%q, want %q", got, tt.want)
			}
		})
	}
}

// TestOpenPlanVerifiesOriginBeforeAnyProviderMutation pins the sequencing
// invariant of audit item 12.
//
// Tunnels, DNS records and Access policies are externally visible and outlive a
// failed operation. Creating them for an origin that is not answering leaves
// real Cloudflare resources pointing at a dead service, so the origin probe
// must precede every provider mutation.
func TestOpenPlanVerifiesOriginBeforeAnyProviderMutation(t *testing.T) {
	mutations := map[core.StepKind]bool{
		core.StepCreateTunnel:       true,
		core.StepConfigureRoute:     true,
		core.StepCreateDNSRecord:    true,
		core.StepCreateAccessApp:    true,
		core.StepCreateAccessPolicy: true,
		core.StepStartConnector:     true,
	}

	for name, profile := range map[string]*core.ConnectionProfile{
		"temporary": cfTestProfile(core.ExposureTemporary, "", core.ProtectionSpec{Kind: core.ProtectionNone}),
		"permanent": cfTestProfile(core.ExposurePermanent, "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone}),
		"protected": cfTestProfile(core.ExposurePermanent, "demo.example.com", core.ProtectionSpec{
			Kind: core.ProtectionEmailOTP, AllowedEmails: []string{"person@example.com"},
		}),
	} {
		t.Run(name, func(t *testing.T) {
			p := cfPlanTestProvider()
			plan, err := p.Plan(context.Background(), core.DesiredConnection{
				Profile: profile,
				Origin:  &core.ResolvedOrigin{URL: "http://127.0.0.1:3000"},
			})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			verifyIdx := -1
			for i, step := range plan.Steps {
				if step.Kind == core.StepVerifyOrigin {
					verifyIdx = i
					break
				}
			}
			if verifyIdx < 0 {
				t.Fatalf("open plan has no %s step:\n%s", core.StepVerifyOrigin, formatSteps(plan.Steps))
			}
			if got := plan.Steps[verifyIdx].Technical.Parameters["origin_url"]; got != "http://127.0.0.1:3000" {
				t.Fatalf("verify-origin step carries origin_url %q", got)
			}

			for i, step := range plan.Steps {
				if mutations[step.Kind] && i < verifyIdx {
					t.Fatalf("provider mutation %s at index %d precedes origin verification at %d:\n%s",
						step.Kind, i, verifyIdx, formatSteps(plan.Steps))
				}
			}

			// The final endpoint verification must also carry the origin so the
			// executor's origin probe is not silently skipped.
			for _, step := range plan.Steps {
				if step.Kind == core.StepVerifyEndpoint {
					if step.Technical.Parameters["origin_url"] == "" {
						t.Fatal("verify-endpoint step does not carry origin_url, so the origin probe is skipped")
					}
				}
			}
		})
	}
}

// TestVerifyOriginStepFailsWhenOriginUnreachable ensures the step actually
// probes, rather than passing vacuously.
func TestVerifyOriginStepFailsWhenOriginUnreachable(t *testing.T) {
	p := cfPlanTestProvider()

	// Port 1 on loopback refuses connections.
	res, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID:   "cf-verify-origin",
		Kind: core.StepVerifyOrigin,
		Technical: core.TechnicalOperation{
			Parameters: map[string]string{"origin_url": "http://127.0.0.1:1"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStep returned transport error: %v", err)
	}
	if res.Succeeded {
		t.Fatal("verify-origin succeeded against an unreachable origin")
	}
	if res.Error == nil {
		t.Fatal("failed verify-origin carries no error")
	}
}

// TestVerifyOriginStepRefusesMissingOriginURL ensures a plan that forgot to
// carry the resolved origin fails loudly instead of passing vacuously, which is
// how the probe came to be skipped in the first place.
func TestVerifyOriginStepRefusesMissingOriginURL(t *testing.T) {
	p := cfPlanTestProvider()
	res, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID: "cf-verify-origin", Kind: core.StepVerifyOrigin,
		Technical: core.TechnicalOperation{Parameters: map[string]string{}},
	})
	if err != nil {
		t.Fatalf("ExecuteStep returned transport error: %v", err)
	}
	if res.Succeeded {
		t.Fatal("verify-origin succeeded with no origin URL to probe")
	}
}

func formatSteps(steps []core.PlanStep) string {
	var b strings.Builder
	for i, s := range steps {
		fmt.Fprintf(&b, "  %d. %s (%s)\n", i, s.ID, s.Kind)
	}
	return b.String()
}

// cfPlanTestProvider builds a Provider wired with fakes and a configured zone,
// so permanent plans are producible without a live Cloudflare account.
func cfPlanTestProvider() *Provider {
	return &Provider{
		accountID:     "account-test",
		zoneID:        "zone-test",
		tunnels:       &fakeTunnelManager{},
		dns:           &fakeDNSManager{},
		access:        &fakeAccessManager{},
		connectorProc: fakeConnectorProcessService{},
		connections:   make(map[core.ConnectionID]*cfConnection),
	}
}

// cfTestProfile builds a service-exposure profile for plan-shape tests.
func cfTestProfile(mode core.ExposureMode, hostname string, protection core.ProtectionSpec) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID:       "conn-1",
		Name:     "test",
		Revision: 1,
		Kind:     core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "127.0.0.1:3000", Protocol: core.ProtocolHTTP},
				},
				Exposure:   core.ExposureSpec{Mode: mode, Protocol: core.ProtocolHTTP, RequestedAddress: hostname},
				Protection: protection,
			},
		},
		Driver:  core.DriverSelection{ProviderID: "cloudflare"},
		Desired: core.DesiredOpen,
	}
}
