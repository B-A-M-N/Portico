package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/store"
)

// activationFailureDefinition gives lifecycle handlers a provider whose
// construction can fail or panic without involving an external client.
type activationFailureDefinition struct {
	declaredDefinition
	activateErr error
	panicValue  any
}

type activationProviderIdentity struct {
	core.Provider
	identity core.ProviderIdentity
}

func (p activationProviderIdentity) Identity() core.ProviderIdentity { return p.identity }

func (d *activationFailureDefinition) Activate(ctx context.Context, req provider.ActivationRequest) (inst provider.Installation, err error) {
	if d.panicValue != nil {
		panic(d.panicValue)
	}
	if d.activateErr != nil {
		return provider.Installation{}, d.activateErr
	}
	inst, err = d.declaredDefinition.Activate(ctx, req)
	if err == nil && inst.Provider != nil {
		// declaredDefinition uses the mock adapter, whose identity is "mock";
		// this wrapper keeps the test fixture faithful to Definition's contract.
		inst.Provider = activationProviderIdentity{
			Provider: inst.Provider,
			identity: d.Identity(),
		}
	}
	return inst, err
}

func (*activationFailureDefinition) VerifyAccount(context.Context, provider.PreparedAccount) (core.SetupValidation, error) {
	return core.SetupValidation{}, nil
}

func newActivationFailureHandler(t *testing.T, def *activationFailureDefinition) (*supervisorHandler, *store.Store) {
	t.Helper()
	st := newRecoveryTestStore(t)
	sup, _ := activationTestSupervisor(t, st, def)
	sup.accountValidator = &stubValidator{accessible: true}
	return &supervisorHandler{sup: sup}, st
}

func seedActivationFailureAccount(t *testing.T, st *store.Store, secret string) {
	t.Helper()
	account := core.ProviderAccount{
		Provider:      "cloudflare",
		ID:            "acct-1",
		Label:         "Work account",
		CredentialRef: providerCredentialRef("cloudflare", "acct-1"),
		Status:        core.AccountAuthenticated,
	}
	if err := st.UpsertProviderAccountCredential(context.Background(), account, []byte(secret)); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

func TestProviderActivationErrorsPropagateThroughSetup(t *testing.T) {
	const secret = "setup-activation-secret"
	sentinel := errors.New("setup activation failed")
	tests := []struct {
		name        string
		activateErr error
		panicValue  any
	}{
		{name: "error", activateErr: sentinel},
		{name: "panic", panicValue: secret},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := &activationFailureDefinition{
				declaredDefinition: declaredDefinition{id: "acme"},
				activateErr:        tc.activateErr,
				panicValue:         tc.panicValue,
			}
			handler, st := newSetupHandler(t, def)
			response, err := handler.HandleConfigureProviderAccount("acme", ipc.ConfigureProviderAccountRequest{
				Fields: map[string]string{"workspace": "ws-1", "token": secret},
			})
			if err == nil {
				t.Fatal("setup hid the provider activation failure")
			}
			if tc.activateErr != nil && !errors.Is(err, tc.activateErr) {
				t.Fatalf("setup error = %v, want %v", err, tc.activateErr)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("setup error exposed the credential: %v", err)
			}
			if response == nil {
				t.Fatal("setup discarded its durable response while returning activation failure")
			}
			accounts, listErr := st.ListProviderAccounts(context.Background())
			if listErr != nil || len(accounts) != 1 {
				t.Fatalf("setup did not preserve the stored account: %#v, %v", accounts, listErr)
			}
		})
	}
}

func TestProviderActivationErrorsPropagateThroughReplace(t *testing.T) {
	const secret = "replace-activation-secret"
	sentinel := errors.New("replace activation failed")
	tests := []struct {
		name        string
		activateErr error
		panicValue  any
	}{
		{name: "error", activateErr: sentinel},
		{name: "panic", panicValue: secret},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := &activationFailureDefinition{
				declaredDefinition: declaredDefinition{id: "cloudflare"},
				activateErr:        tc.activateErr,
				panicValue:         tc.panicValue,
			}
			handler, st := newActivationFailureHandler(t, def)
			seedActivationFailureAccount(t, st, "old-secret")

			response, err := handler.HandleReplaceProviderAccountCredential("cloudflare", "acct-1",
				ipc.ReplaceCredentialRequest{Credential: secret})
			if err == nil {
				t.Fatal("credential replacement hid the provider activation failure")
			}
			if tc.activateErr != nil && !errors.Is(err, tc.activateErr) {
				t.Fatalf("replacement error = %v, want %v", err, tc.activateErr)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("replacement error exposed the credential: %v", err)
			}
			if response == nil || !response.Validated {
				t.Fatalf("replacement discarded its durable response: %#v", response)
			}
		})
	}
}

func TestProviderActivationErrorsPropagateThroughReverify(t *testing.T) {
	const secret = "reverify-activation-secret"
	sentinel := errors.New("reverify activation failed")
	tests := []struct {
		name        string
		activateErr error
		panicValue  any
	}{
		{name: "error", activateErr: sentinel},
		{name: "panic", panicValue: secret},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := &activationFailureDefinition{
				declaredDefinition: declaredDefinition{id: "cloudflare"},
				activateErr:        tc.activateErr,
				panicValue:         tc.panicValue,
			}
			handler, st := newActivationFailureHandler(t, def)
			seedActivationFailureAccount(t, st, secret)

			response, err := handler.HandleReverifyProviderAccount("cloudflare", "acct-1",
				ipc.ReverifyProviderAccountRequest{})
			if err == nil {
				t.Fatal("re-verification hid the provider activation failure")
			}
			if tc.activateErr != nil && !errors.Is(err, tc.activateErr) {
				t.Fatalf("re-verification error = %v, want %v", err, tc.activateErr)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("re-verification error exposed the credential: %v", err)
			}
			if response == nil || !response.Validated {
				t.Fatalf("re-verification discarded its durable response: %#v", response)
			}
		})
	}
}

func TestProviderActivationErrorsPropagateThroughRemoval(t *testing.T) {
	sentinel := errors.New("removal activation failed")
	tests := []struct {
		name        string
		activateErr error
		panicValue  any
	}{
		{name: "error", activateErr: sentinel},
		{name: "panic", panicValue: "removal activation panicked"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := &activationFailureDefinition{
				declaredDefinition: declaredDefinition{id: "cloudflare"},
				activateErr:        tc.activateErr,
				panicValue:         tc.panicValue,
			}
			handler, st := newActivationFailureHandler(t, def)
			seedActivationFailureAccount(t, st, "removal-secret")

			preview, err := handler.HandleAccountRemovalPreview("cloudflare", "acct-1")
			if err != nil {
				t.Fatalf("HandleAccountRemovalPreview: %v", err)
			}
			response, err := handler.HandleRemoveProviderAccount("cloudflare", "acct-1",
				ipc.RemoveProviderAccountRequest{Fingerprint: preview.Fingerprint})
			if err == nil {
				t.Fatal("removal hid the provider activation failure")
			}
			if tc.activateErr != nil && !errors.Is(err, tc.activateErr) {
				t.Fatalf("removal error = %v, want %v", err, tc.activateErr)
			}
			if response == nil || !response.Removed {
				t.Fatalf("removal discarded its durable response: %#v", response)
			}
			accounts, listErr := st.ListProviderAccounts(context.Background())
			if listErr != nil || len(accounts) != 0 {
				t.Fatalf("removal did not commit before activation failed: %#v, %v", accounts, listErr)
			}
		})
	}
}

func TestActivationEnumerationFailurePreservesPriorAdapter(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	def := &activationFailureDefinition{declaredDefinition: declaredDefinition{id: "acme"}}
	sup, registry := activationTestSupervisor(t, st, def)
	if err := st.UpsertProviderAccountCredential(ctx, core.ProviderAccount{
		Provider:      "acme",
		ID:            "acct-1",
		Label:         "Acme account",
		CredentialRef: providerCredentialRef("acme", "acct-1"),
		Status:        core.AccountAuthenticated,
	}, []byte("enumeration-secret")); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	if err := sup.ActivateProvider(ctx, "acme"); err != nil {
		t.Fatalf("initial ActivateProvider: %v", err)
	}
	prior := registry.Get("acme")
	if prior == nil {
		t.Fatal("initial activation did not install an adapter")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	err := sup.ActivateProvider(ctx, "acme")
	if err == nil {
		t.Fatal("account enumeration failure was hidden")
	}
	if !strings.Contains(err.Error(), "list provider accounts") {
		t.Fatalf("enumeration error = %v", err)
	}
	if registry.Get("acme") != prior {
		t.Fatal("enumeration failure discarded the prior adapter")
	}

	snapshots := registry.Snapshot()
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	if snapshots[0].Availability != provider.AvailabilityDegraded {
		t.Fatalf("availability = %q, want degraded", snapshots[0].Availability)
	}
	if snapshots[0].Reason != "provider activation failed" {
		t.Fatalf("failure reason = %q, want sanitized reason", snapshots[0].Reason)
	}
}

func TestActivationCredentialLoadFailurePreservesPriorAdapter(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	secret := "credential-load-secret"
	seedActivationFailureAccount(t, st, secret)
	def := &activationFailureDefinition{declaredDefinition: declaredDefinition{id: "cloudflare"}}
	sup, registry := activationTestSupervisor(t, st, def)

	if err := sup.ActivateProvider(ctx, "cloudflare"); err != nil {
		t.Fatalf("initial ActivateProvider: %v", err)
	}
	prior := registry.Get("cloudflare")
	if _, err := st.DB().ExecContext(ctx,
		"UPDATE provider_credentials SET secret_encrypted = ? WHERE credential_ref = ?",
		[]byte("not-valid-ciphertext"), providerCredentialRef("cloudflare", "acct-1")); err != nil {
		t.Fatalf("corrupt credential: %v", err)
	}

	err := sup.ActivateProvider(ctx, "cloudflare")
	if err == nil {
		t.Fatal("credential loading failure was hidden")
	}
	if !strings.Contains(err.Error(), "load credential") {
		t.Fatalf("credential load error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("credential load error exposed the credential: %v", err)
	}
	if registry.Get("cloudflare") != prior {
		t.Fatal("credential loading failure discarded the prior adapter")
	}

	snapshots := registry.Snapshot()
	if len(snapshots) != 1 || snapshots[0].Availability != provider.AvailabilityDegraded {
		t.Fatalf("snapshots = %#v, want one degraded provider", snapshots)
	}
	if len(snapshots[0].PendingAccounts) != 1 ||
		snapshots[0].PendingAccounts[0].UnusableReason == "" {
		t.Fatalf("failed account was not retained as sanitized pending state: %#v", snapshots[0].PendingAccounts)
	}
}

func TestActivationFailureReasonDoesNotEchoProviderError(t *testing.T) {
	secret := "provider-error-secret"
	def := &activationFailureDefinition{
		declaredDefinition: declaredDefinition{id: "acme"},
		activateErr:        fmt.Errorf("client rejected credential %s", secret),
	}
	sup, registry := activationTestSupervisor(t, newRecoveryTestStore(t), def)

	// The definition has no stored account, so this test only verifies that a
	// provider error is not copied into the user-visible degraded catalog.
	if err := sup.ActivateProvider(context.Background(), "acme"); err == nil {
		t.Fatal("provider activation unexpectedly succeeded")
	}
	snapshots := registry.Snapshot()
	if len(snapshots) != 1 || strings.Contains(snapshots[0].Reason, secret) {
		t.Fatalf("catalog leaked provider error: %#v", snapshots)
	}
}
