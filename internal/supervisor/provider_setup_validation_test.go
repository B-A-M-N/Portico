package supervisor

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

type discoveringAccountValidator struct {
	*stubAccountValidator
	accounts []AccountSummary
}

func (v *discoveringAccountValidator) ListAccounts(context.Context, string) ([]AccountSummary, error) {
	return v.accounts, nil
}

func TestCloudflareSetupDiscoversAndAutoSelectsFriendlyAccounts(t *testing.T) {
	st := newRecoveryTestStore(t)
	base := &stubAccountValidator{result: &AccountValidation{AccountAccessible: true}}
	validator := &discoveringAccountValidator{
		stubAccountValidator: base,
		accounts: []AccountSummary{
			{ID: "account-personal", Name: "Personal"},
			{ID: "account-work", Name: "Work"},
		},
	}
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st, validator)}

	choices, err := handler.HandleValidateProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{Credential: "token"})
	if err != nil {
		t.Fatalf("discover accounts: %v", err)
	}
	if !choices.AccountSelectionRequired || len(choices.AccountChoices) != 2 {
		t.Fatalf("account choices = %#v, want two selectable accounts", choices)
	}
	if choices.AccountChoices[0].Label != "Personal" || choices.AccountChoices[1].Label != "Work" {
		t.Fatalf("account labels = %#v, want provider names", choices.AccountChoices)
	}
	if base.calls != 0 {
		t.Fatalf("ambiguous account discovery validated an account before selection: %d calls", base.calls)
	}

	validator.accounts = validator.accounts[:1]
	auto, err := handler.HandleValidateProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{Credential: "token"})
	if err != nil {
		t.Fatalf("auto-select account: %v", err)
	}
	if auto.AccountID != "account-personal" || auto.AccountLabel != "Personal" || !auto.Validated {
		t.Fatalf("auto-selected account = %#v, want Personal/account-personal and validated", auto)
	}
}

func TestCloudflareValidationDoesNotPersistBeforeExplicitConfigure(t *testing.T) {
	st := newRecoveryTestStore(t)
	validator := &stubAccountValidator{result: &AccountValidation{
		AccountAccessible: true,
		Zones:             []ZoneSummary{{ID: "zone-a", Name: "example.com"}},
	}}
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st, validator)}

	resp, err := handler.HandleValidateProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		AccountID: "account-a", Credential: "token",
	})
	if err != nil {
		t.Fatalf("HandleValidateProviderAccount: %v", err)
	}
	if resp == nil || !resp.Validated || resp.Committed {
		t.Fatalf("validation response = %#v, want validated and uncommitted", resp)
	}
	if len(resp.Zones) != 1 || resp.Zones[0].Name != "example.com" {
		t.Fatalf("available zones = %#v", resp.Zones)
	}
	accounts, err := st.ListProviderAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListProviderAccounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("validation persisted %d account(s)", len(accounts))
	}

	// The explicit configure operation remains the only path that writes.
	configured, err := handler.HandleConfigureProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		AccountID: "account-a", Credential: "token",
	})
	if err != nil {
		t.Fatalf("HandleConfigureProviderAccount: %v", err)
	}
	if configured.Status != string(core.AccountAuthenticated) {
		t.Fatalf("configured status = %q, want authenticated", configured.Status)
	}
	accounts, err = st.ListProviderAccounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts after configure = %#v, %v", accounts, err)
	}
}
