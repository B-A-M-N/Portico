package cloudflare

import (
	"context"
	"net/http"
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
	state            *tunnel.TunnelState
	err              error
}

func (f *fakeTunnelManager) Create(context.Context, string, string) (*tunnel.Info, error) {
	return nil, nil
}
func (f *fakeTunnelManager) Get(_ context.Context, accountID, tunnelID string) (*tunnel.TunnelState, error) {
	f.requestedAccount, f.requestedID = accountID, tunnelID
	return f.state, f.err
}
func (f *fakeTunnelManager) ConfigureIngress(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeTunnelManager) GetToken(context.Context, string, string) (string, error) { return "", nil }
func (f *fakeTunnelManager) Delete(context.Context, string, string) error             { return nil }

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

func (*fakeAccessManager) CreateApp(context.Context, string, string, access.Policy) (*access.AppInfo, error) {
	return nil, nil
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
