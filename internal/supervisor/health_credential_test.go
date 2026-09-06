package supervisor

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/origin"
	"github.com/B-A-M-N/portico/internal/store"
)

// Audit P1-12: the supervisor's credential-health check must detect
// undecryptable credentials and report only metadata — never plaintext.

func newHealthCheckSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Supervisor{store: st, gatewayMgr: newGatewayManager(), origins: origin.NewManager(), mutating: true}
}

func TestCredentialHealthCheckReportsHealthyStore(t *testing.T) {
	sup := newHealthCheckSupervisor(t)
	ctx := context.Background()
	if err := sup.store.SaveProviderCredential(ctx, "cloudflare", "ref-ok", []byte("secret")); err != nil {
		t.Fatalf("SaveProviderCredential: %v", err)
	}
	h := &supervisorHandler{sup: sup}
	check := h.credentialHealthCheck(ctx)
	if check.State != string(checkOK) {
		t.Fatalf("healthy store reported %q: %s", check.State, check.Summary)
	}
	if strings.Contains(check.Summary+check.Detail, "secret") {
		t.Fatal("plaintext leaked into the health check output")
	}
}

func TestCredentialHealthCheckDetectsUndecryptableRecord(t *testing.T) {
	sup := newHealthCheckSupervisor(t)
	ctx := context.Background()
	if err := sup.store.SaveProviderCredential(ctx, "cloudflare", "ref-bad", []byte("plaintext-secret")); err != nil {
		t.Fatalf("SaveProviderCredential: %v", err)
	}
	// Fixture: corrupt the stored blob so decryption fails (via the test-only
	// DB() handle).
	if _, err := sup.store.DB().ExecContext(ctx,
		`UPDATE provider_credentials SET secret_encrypted = ? WHERE credential_ref = ?`,
		[]byte("{corrupted payload}"), "ref-bad"); err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}

	h := &supervisorHandler{sup: sup}
	check := h.credentialHealthCheck(ctx)
	if check.State != string(checkProblem) {
		t.Fatalf("undecryptable record reported state %q (want problem): %s", check.State, check.Summary)
	}
	if !strings.Contains(check.Summary, "cannot be decrypted") {
		t.Fatalf("summary does not explain the failure: %s", check.Summary)
	}
	if strings.Contains(check.Summary+check.Detail, "plaintext-secret") {
		t.Fatal("plaintext leaked into the health check output")
	}
}

var _ = ipc.HealthCheckDTO{}
