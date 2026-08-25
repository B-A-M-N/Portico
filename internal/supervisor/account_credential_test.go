package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// Re-verifying and rotating an account's credential.
//
// Re-verification answered "not yet implemented", so an account whose token had
// been revoked could only be removed and re-added — and removal is refused while
// a connection depends on it. The recovery path for the commonest credential
// problem therefore ran through deleting the connections that needed the account.
//
// Both operations preserve the identity (provider_id, account_id), because every
// connection stores it. Neither returns a credential.

// stubValidator answers a credential check without a live provider.
type stubValidator struct {
	accessible bool
	err        error
	// seen records the credential length only. A test double that keeps a
	// credential is one that can print it in a failure message.
	seenLen int
	calls   int
}

func (v *stubValidator) Validate(_ context.Context, _, _, credential string) (*AccountValidation, error) {
	v.calls++
	v.seenLen = len(credential)
	if v.err != nil {
		return &AccountValidation{}, v.err
	}
	return &AccountValidation{AccountAccessible: v.accessible}, nil
}

// VerifyZone satisfies the widened AccountValidator interface: tests using the
// stub do not exercise zone verification, so it verifies nothing.
func (v *stubValidator) VerifyZone(_ context.Context, _, _ string) ([]ZoneSummary, error) {
	return nil, nil
}

// storedAccount puts one account and its credential in the store.
func storedAccount(t *testing.T, h *supervisorHandler, status core.ProviderAccountStatus) core.ProviderAccount {
	t.Helper()
	account := core.ProviderAccount{
		Provider:      "cloudflare",
		ID:            "acct-1",
		Label:         "Work account",
		CredentialRef: providerCredentialRef("cloudflare", "acct-1"),
		Status:        status,
	}
	secret := []byte("token-" + strings.Repeat("x", 20))
	if err := h.sup.store.UpsertProviderAccountCredential(
		context.Background(), account, secret); err != nil {
		t.Fatalf("seed the account: %v", err)
	}
	return account
}

// reverifyHandler builds a handler with a store and a stub validator.
func reverifyHandler(t *testing.T, validator AccountValidator) *supervisorHandler {
	t.Helper()
	return &supervisorHandler{sup: &Supervisor{
		store:            newRecoveryTestStore(t),
		accountValidator: validator,
	}}
}

// TestReverifyConfirmsAWorkingCredential pins the passing case, including that
// the stored credential is the one checked and that it is not returned.
func TestReverifyConfirmsAWorkingCredential(t *testing.T) {
	validator := &stubValidator{accessible: true}
	h := reverifyHandler(t, validator)
	storedAccount(t, h, core.AccountPending)

	resp, err := h.HandleReverifyProviderAccount("cloudflare", "acct-1",
		ipc.ReverifyProviderAccountRequest{AccountID: "acct-1"})
	if err != nil {
		t.Fatalf("HandleReverifyProviderAccount: %v", err)
	}
	if !resp.Validated {
		t.Fatalf("a working credential was not confirmed: %+v", resp)
	}
	if validator.calls != 1 {
		t.Fatalf("the provider was checked %d times, want once", validator.calls)
	}
	// The credential was fetched and used. It is not exposed: the response has
	// no field that could carry one, which is checked here as a property of the
	// type rather than of this call.
	if validator.seenLen == 0 {
		t.Fatal("the stored credential was not passed to the provider check")
	}

	// An account that works again becomes usable without being re-entered.
	accounts, err := h.sup.store.ListProviderAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(accounts))
	}
	if accounts[0].Status != core.AccountAuthenticated {
		t.Fatalf("status = %q, want authenticated", accounts[0].Status)
	}
	// The identity is untouched: every connection stores it.
	if accounts[0].ID != "acct-1" || accounts[0].Provider != "cloudflare" {
		t.Fatalf("re-verification changed the account identity: %+v", accounts[0])
	}
}

// TestReverifyMarksARevokedCredentialUnusable pins the failing case.
//
// This is the whole point of the feature: an account that has stopped working
// must stop being selectable, so a connection is refused at creation rather than
// failing at open.
func TestReverifyMarksARevokedCredentialUnusable(t *testing.T) {
	h := reverifyHandler(t, &stubValidator{err: errors.New("the token was revoked")})
	storedAccount(t, h, core.AccountAuthenticated)

	resp, err := h.HandleReverifyProviderAccount("cloudflare", "acct-1",
		ipc.ReverifyProviderAccountRequest{})
	if err != nil {
		t.Fatalf("a failed verification is an answer, not a request error: %v", err)
	}
	if resp.Validated {
		t.Fatal("a revoked credential was reported as valid")
	}
	if resp.VerificationUnavailable == "" {
		t.Fatal("the refusal gives no reason")
	}

	accounts, _ := h.sup.store.ListProviderAccounts(context.Background())
	if accounts[0].Status == core.AccountAuthenticated {
		t.Fatal("a revoked credential is still recorded as authenticated")
	}
}

// TestReverifyRefusesAnUnknownAccount pins that the identity must exist.
func TestReverifyRefusesAnUnknownAccount(t *testing.T) {
	h := reverifyHandler(t, &stubValidator{accessible: true})

	if _, err := h.HandleReverifyProviderAccount("cloudflare", "nope",
		ipc.ReverifyProviderAccountRequest{}); err == nil {
		t.Fatal("re-verifying an account that does not exist was accepted")
	}
}

// TestReplaceCredentialValidatesBeforeCommitting pins the ordering that matters.
//
// A rotation that installs a broken token would take working connections down at
// their next open. The new credential is checked first, and nothing is written
// when it fails.
func TestReplaceCredentialValidatesBeforeCommitting(t *testing.T) {
	validator := &stubValidator{err: errors.New("that token cannot see the account")}
	h := reverifyHandler(t, validator)
	original := storedAccount(t, h, core.AccountAuthenticated)

	_, err := h.HandleReplaceProviderAccountCredential("cloudflare", "acct-1",
		ipc.ReplaceCredentialRequest{Credential: "bad-token-value-here"})
	if err == nil {
		t.Fatal("a rejected credential was accepted")
	}
	if !strings.Contains(err.Error(), "unchanged") {
		t.Fatalf("the refusal does not say the old credential still stands: %v", err)
	}

	// The old credential is still in place and still usable, so whatever was
	// working before still works.
	stored, loadErr := h.sup.store.LoadProviderCredential(
		context.Background(), original.Provider, original.CredentialRef)
	if loadErr != nil {
		t.Fatalf("the original credential was lost: %v", loadErr)
	}
	if !strings.HasPrefix(stored, "token-") {
		t.Fatal("the stored credential is not the original one")
	}
	accounts, _ := h.sup.store.ListProviderAccounts(context.Background())
	if accounts[0].Status != core.AccountAuthenticated {
		t.Fatal("a refused rotation changed the account's status")
	}
}

// TestReplaceCredentialKeepsTheAccountIdentity pins the successful rotation.
func TestReplaceCredentialKeepsTheAccountIdentity(t *testing.T) {
	h := reverifyHandler(t, &stubValidator{accessible: true})
	original := storedAccount(t, h, core.AccountAuthenticated)

	replacement := "rotated-" + strings.Repeat("y", 24)
	resp, err := h.HandleReplaceProviderAccountCredential("cloudflare", "acct-1",
		ipc.ReplaceCredentialRequest{Credential: replacement})
	if err != nil {
		t.Fatalf("HandleReplaceProviderAccountCredential: %v", err)
	}
	if !resp.Validated {
		t.Fatalf("the new credential was stored without being confirmed: %+v", resp)
	}

	accounts, _ := h.sup.store.ListProviderAccounts(context.Background())
	if len(accounts) != 1 {
		t.Fatalf("the rotation created a second account: %d accounts", len(accounts))
	}
	got := accounts[0]
	if got.ID != original.ID || got.Provider != original.Provider {
		t.Fatalf("the identity changed: %s/%s -> %s/%s",
			original.Provider, original.ID, got.Provider, got.ID)
	}
	if got.Label != original.Label {
		t.Fatalf("label = %q, want %q", got.Label, original.Label)
	}

	// The new secret is what is stored now.
	stored, err := h.sup.store.LoadProviderCredential(
		context.Background(), got.Provider, got.CredentialRef)
	if err != nil {
		t.Fatal(err)
	}
	if stored != replacement {
		t.Fatal("the rotation did not install the new credential")
	}
}

// TestReplaceCredentialRefusesAnEmptySecret pins that a rotation must carry one.
func TestReplaceCredentialRefusesAnEmptySecret(t *testing.T) {
	h := reverifyHandler(t, &stubValidator{accessible: true})
	storedAccount(t, h, core.AccountAuthenticated)

	if _, err := h.HandleReplaceProviderAccountCredential("cloudflare", "acct-1",
		ipc.ReplaceCredentialRequest{Credential: "   "}); err == nil {
		t.Fatal("an empty credential was accepted as a replacement")
	}
}

// TestCredentialResponsesCarryNoSecret pins the shape of the responses.
//
// This is a property of the types, asserted so that adding a field that could
// carry a credential fails here rather than in review.
func TestCredentialResponsesCarryNoSecret(t *testing.T) {
	h := reverifyHandler(t, &stubValidator{accessible: true})
	storedAccount(t, h, core.AccountPending)

	secret := "unique-secret-" + strings.Repeat("z", 18)
	replaceResp, err := h.HandleReplaceProviderAccountCredential("cloudflare", "acct-1",
		ipc.ReplaceCredentialRequest{Credential: secret})
	if err != nil {
		t.Fatal(err)
	}
	if rendered := renderResponse(t, replaceResp); strings.Contains(rendered, secret) {
		t.Fatalf("the replacement response carries the credential: %s", rendered)
	}

	verifyResp, err := h.HandleReverifyProviderAccount("cloudflare", "acct-1",
		ipc.ReverifyProviderAccountRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if rendered := renderResponse(t, verifyResp); strings.Contains(rendered, secret) {
		t.Fatalf("the verification response carries the credential: %s", rendered)
	}
}

// renderResponse serialises a response the way IPC would, so the check is
// against what actually crosses the socket rather than against the struct's Go
// representation.
func renderResponse(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return string(encoded)
}
