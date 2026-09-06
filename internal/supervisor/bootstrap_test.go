package supervisor

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/config"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/spf13/viper"
)

func TestLegacyEnvironmentBootstrapAcceptsAccountTokenWithoutZoneID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CLOUDFLARE_API_TOKEN", "legacy-token")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "legacy-account")
	t.Setenv("CLOUDFLARE_ZONE_ID", "")
	viper.Reset()
	t.Cleanup(viper.Reset)
	if err := config.Init(); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	st := newRecoveryTestStore(t)
	migrateLegacyEnvironmentAccounts(context.Background(), st, acceptingVerifier)

	accounts, err := st.ListProviderAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListProviderAccounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "legacy-account" {
		t.Fatalf("account-only environment was not imported: %#v", accounts)
	}
	if _, ok := accounts[0].Metadata["zone_id"]; ok {
		t.Fatalf("account-only import unexpectedly stored a zone: %#v", accounts[0].Metadata)
	}
	secret, err := st.LoadProviderCredential(context.Background(), "cloudflare",
		providerCredentialRef("cloudflare", "legacy-account"))
	if err != nil || secret != "legacy-token" {
		t.Fatalf("legacy credential was not imported: %q err=%v", secret, err)
	}
}

func TestBootstrapImportIsOneTimeAndDoesNotOverwriteAccountState(t *testing.T) {
	ctx := context.Background()
	st := newRecoveryTestStore(t)
	calls := 0
	verify := func(context.Context, core.ProviderID, string, string) error {
		calls++
		return nil
	}

	importLegacyEnvironmentAccount(ctx, st, "cloudflare", "acct-env", "token-first",
		map[string]string{"zone_id": "zone-first"}, verify)
	importLegacyEnvironmentAccount(ctx, st, "cloudflare", "acct-env", "token-stale",
		map[string]string{"zone_id": "zone-stale"}, verify)

	if calls != 1 {
		t.Fatalf("bootstrap verifier called %d times, want once", calls)
	}
	accounts, err := st.ListProviderAccounts(ctx)
	if err != nil {
		t.Fatalf("ListProviderAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("bootstrap created %d accounts, want one", len(accounts))
	}
	account := accounts[0]
	if account.Label != "acct-env" || account.Status != core.AccountAuthenticated ||
		account.Metadata["zone_id"] != "zone-first" {
		t.Fatalf("second import overwrote account state: %#v", account)
	}
	secret, err := st.LoadProviderCredential(ctx, "cloudflare", account.CredentialRef)
	if err != nil || secret != "token-first" {
		t.Fatalf("second import overwrote credential: %q err=%v", secret, err)
	}
}
