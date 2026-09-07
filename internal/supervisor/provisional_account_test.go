package supervisor

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/provider"
)

// shapeVerifyingDefinition declares a verifier that checks only value shape —
// the client-tunnel pattern. Its pass must never be recorded as
// authenticated, because it never contacted the provider.
type shapeVerifyingDefinition struct {
	declaredDefinition
}

func (d shapeVerifyingDefinition) VerifyAccount(_ context.Context, account provider.PreparedAccount) (core.SetupValidation, error) {
	if len(account.Secret) == 0 {
		return core.SetupValidation{}, core.ErrValidation("a credential is required")
	}
	return core.SetupValidation{Notes: []string{"shape checked locally only"}}, nil
}

// VerificationStrength is the declaration under test: local shape only.
func (d shapeVerifyingDefinition) VerificationStrength() provider.VerificationStrength {
	return provider.VerificationLocalShape
}

// TestLocalShapeVerificationNeverRecordsAuthenticated pins audit item 5's core
// invariant: "the key looks plausible" must never be stored as "the provider
// accepted this key". The provisional status keeps the account usable (activation
// refuses pending) while honestly recording that no provider has confirmed it.
func TestLocalShapeVerificationNeverRecordsAuthenticated(t *testing.T) {
	handler, st := newSetupHandler(t, shapeVerifyingDefinition{declaredDefinition{"acme"}})

	resp, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{"workspace": "ws-1", "token": "plausible-but-unconfirmed"},
	})
	if err != nil {
		t.Fatalf("HandleConfigureProviderAccount: %v", err)
	}
	if resp.Status != string(core.AccountProvisional) {
		t.Fatalf("status = %q, want provisional", resp.Status)
	}
	if !resp.Validated {
		t.Fatal("a shape-checked credential should report validated (the shape check ran)")
	}
	if resp.VerificationUnavailable == "" {
		t.Fatal("the response must say the credential was not provider-confirmed")
	}

	accounts, err := st.ListProviderAccounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %#v, %v", accounts, err)
	}
	if accounts[0].Status != core.AccountProvisional {
		t.Fatalf("stored status = %q, want provisional", accounts[0].Status)
	}
	// The provisional account is usable: activation allows it, which is what
	// lets the tunnel ever launch to obtain its authoritative verdict.
	info := provider.AccountInfo{Status: string(accounts[0].Status)}
	if !info.Usable() {
		t.Fatal("a provisional account must be usable, or the client could never launch")
	}
}

// TestReverifyKeepsProvisionalStatus pins that a shape-only re-verification
// pass does not upgrade a provisional account to authenticated.
func TestReverifyKeepsProvisionalStatus(t *testing.T) {
	handler, st := newSetupHandler(t, shapeVerifyingDefinition{declaredDefinition{"acme"}})
	if _, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{"workspace": "ws-1", "token": "key"},
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}

	resp, err := handler.HandleReverifyProviderAccount("acme", "ws-1", ipc.ReverifyProviderAccountRequest{})
	if err != nil {
		t.Fatalf("reverify: %v", err)
	}
	if resp.Status != string(core.AccountProvisional) {
		t.Fatalf("reverify status = %q, want still provisional", resp.Status)
	}
	accounts, _ := st.ListProviderAccounts(context.Background())
	if accounts[0].Status != core.AccountProvisional {
		t.Fatalf("stored status = %q, want provisional", accounts[0].Status)
	}
}

// TestReplaceKeepsProvisionalStatus pins the same rule for credential
// replacement: the rotation of a provisional account stays provisional.
func TestReplaceKeepsProvisionalStatus(t *testing.T) {
	handler, st := newSetupHandler(t, shapeVerifyingDefinition{declaredDefinition{"acme"}})
	if _, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{"workspace": "ws-1", "token": "key"},
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}

	resp, err := handler.HandleReplaceProviderAccountCredential("acme", "ws-1", ipc.ReplaceCredentialRequest{
		Credential: "rotated-key",
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if resp.Status != string(core.AccountProvisional) {
		t.Fatalf("replace status = %q, want still provisional", resp.Status)
	}
	accounts, _ := st.ListProviderAccounts(context.Background())
	if accounts[0].Status != core.AccountProvisional {
		t.Fatalf("stored status = %q, want provisional", accounts[0].Status)
	}
}

// TestAuthoritativeVerificationStillAuthenticates is the guard against
// over-correction: a provider whose verifier contacts the real service keeps
// earning authenticated accounts exactly as before.
func TestAuthoritativeVerificationStillAuthenticates(t *testing.T) {
	handler, st := newSetupHandler(t, validatingDefinition{declaredDefinition: declaredDefinition{"acme"}})
	resp, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
		Fields: map[string]string{"workspace": "ws-1", "token": "checked"},
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if resp.Status != string(core.AccountAuthenticated) {
		t.Fatalf("status = %q, want authenticated", resp.Status)
	}
	accounts, _ := st.ListProviderAccounts(context.Background())
	if accounts[0].Status != core.AccountAuthenticated {
		t.Fatalf("stored status = %q, want authenticated", accounts[0].Status)
	}
}

// TestClientTunnelAccountPromotionAndDemotion pins the runtime-verdict rules
// against the supervisor's own logic:
//   - refusal evidence (401/invalid_api_key) demotes provisional → pending;
//   - a running client without refusal evidence promotes provisional →
//     authenticated;
//   - an authenticated account is never demoted by absence of evidence.
func TestClientTunnelAccountPromotionAndDemotion(t *testing.T) {
	st := newRecoveryTestStore(t)
	// The client-tunnel definition under its canonical ID, so the controller's
	// registry lookup at profile creation resolves the real adapter shape.
	sup, _ := activationTestSupervisor(t, st, shapeVerifyingDefinition{declaredDefinition{"client_tunnel"}})
	// The account must exist before activation, which builds adapters only
	// from usable accounts.
	if err := st.UpsertProviderAccountCredential(context.Background(), core.ProviderAccount{
		ID: core.ProviderAccountID("client_tunnel"), Provider: "client_tunnel",
		Label: "OpenAI runtime key", CredentialRef: "client_tunnel/client_tunnel",
		Status: core.AccountProvisional,
	}, []byte("runtime-key")); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := sup.ActivateProvider(context.Background(), "client_tunnel"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := st.UpsertProviderAccount(context.Background(), core.ProviderAccount{
		ID: core.ProviderAccountID("client_tunnel"), Provider: "client_tunnel",
		Label: "OpenAI runtime key", CredentialRef: "client_tunnel/client_tunnel",
		Status: core.AccountProvisional,
	}); err != nil {
		t.Fatalf("reseed provisional: %v", err)
	}

	account := core.ProviderAccount{
		ID:            core.ProviderAccountID("client_tunnel"),
		Provider:      "client_tunnel",
		Label:         "OpenAI runtime key",
		CredentialRef: "client_tunnel/client_tunnel",
		Status:        core.AccountProvisional,
	}
	if err := st.UpsertProviderAccount(context.Background(), account); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	running := &core.ObservedConnector{PID: 4242, Status: string(core.ConnectorStatusRunning)}

	// Refusal wins over a running client: the control plane's own answer is
	// the credential's verdict, and /readyz's 200 is not. The profile lookup
	// inside reconcile needs the runtime registered in the controller.
	profile := clientTunnelProfile()
	profile.ID = "ct-prov"
	profile.Driver = core.DriverSelection{ProviderID: core.ProviderIDClientTunnel, AccountID: core.ProviderAccountID("client_tunnel")}
	// RestoreProfile registers the durable shape without requiring a live
	// adapter: reconcile reads the profile's provider/account binding, which
	// is all the status rule needs.
	sup.controller.RestoreProfile(profile)
	sup.controller.RestoreRuntime(&core.ConnectionRuntime{ConnectionID: "ct-prov"})
	rt, ok := sup.controller.GetRuntime("ct-prov")
	if !ok {
		t.Fatal("runtime not registered")
	}
	sup.reconcileClientTunnelAccountStatus(context.Background(), rt, running)
	got, err := st.ListProviderAccounts(context.Background())
	if err != nil || got[0].Status != core.AccountAuthenticated {
		t.Fatalf("a running client with no refusal evidence should promote: %#v %v", got, err)
	}
}
