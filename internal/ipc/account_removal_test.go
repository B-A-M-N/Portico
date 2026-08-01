package ipc

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/store"
)

// removalHandler records what the route asked for and answers as told.
type removalHandler struct {
	nullHandler
	gotProvider    string
	gotAccount     string
	response       *RemoveProviderAccountResponse
	err            error
	gotFingerprint string
}

func (h *removalHandler) HandleAccountRemovalPreview(providerID, accountID string) (
	*AccountRemovalPreviewDTO, error) {
	return &AccountRemovalPreviewDTO{
		ProviderID: providerID, AccountID: accountID,
		Removable: true, Fingerprint: "fp-test",
	}, nil
}

func (h *removalHandler) HandleRemoveProviderAccount(providerID, accountID string,
	req RemoveProviderAccountRequest) (*RemoveProviderAccountResponse, error) {
	h.gotFingerprint = req.Fingerprint
	h.gotProvider, h.gotAccount = providerID, accountID
	return h.response, h.err
}

func serverWith(t *testing.T, h RequestHandler) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := NewServer(filepath.Join(t.TempDir(), "test.sock"), h, st)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// TestAnAccountCanBeRemoved pins audit finding 10.
//
// Removal was implemented on the supervisor and never routed or exposed on the
// client, so an account could be added and never taken away. A revoked token
// stayed listed as a working account with no way to say otherwise.
func TestAnAccountCanBeRemoved(t *testing.T) {
	h := &removalHandler{response: &RemoveProviderAccountResponse{Removed: true}}
	s := serverWith(t, h)

	req := httptest.NewRequest(http.MethodDelete, "/v1/providers/cloudflare/accounts/acct-1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if h.gotProvider != "cloudflare" || h.gotAccount != "acct-1" {
		t.Fatalf("handler received provider %q account %q", h.gotProvider, h.gotAccount)
	}
	var resp RemoveProviderAccountResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Removed {
		t.Fatal("the response does not report the removal")
	}
}

// TestARefusalNamesTheConnectionsThatDependOnTheAccount pins that the reason
// survives the transport.
//
// The supervisor refuses while connections still select the account, and knows
// exactly which. Reporting only a count would leave the user to find them.
func TestARefusalNamesTheConnectionsThatDependOnTheAccount(t *testing.T) {
	h := &removalHandler{
		response: &RemoveProviderAccountResponse{
			Removed:              false,
			DependentConnections: []string{"conn-a", "conn-b"},
			Dependencies: []AccountDependencyDTO{
				{Kind: "connection", ID: "conn-a", Name: "api-staging",
					Explanation: "this connection uses the account"},
				{Kind: "connection", ID: "conn-b", Name: "docs-site",
					Explanation: "this connection uses the account"},
			},
		},
		err: errors.New("2 connection(s) still use this account"),
	}
	s := serverWith(t, h)

	req := httptest.NewRequest(http.MethodDelete, "/v1/providers/cloudflare/accounts/acct-1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	body := rec.Body.String()
	for _, id := range []string{"conn-a", "conn-b", "api-staging", "docs-site"} {
		if !strings.Contains(body, id) {
			t.Errorf("the refusal does not name %s:\n%s", id, body)
		}
	}

	// The dependencies must arrive typed, not only as prose in an action label.
	var apiErr APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatal(err)
	}
	if len(apiErr.AccountDependencies) != 2 {
		t.Fatalf("the refusal carries %d typed dependencies, want 2", len(apiErr.AccountDependencies))
	}
	if apiErr.AccountDependencies[0].Name != "api-staging" {
		t.Fatalf("the dependency is unnamed: %#v", apiErr.AccountDependencies[0])
	}
}

// TestRemovalRequiresDelete pins that the account path is not removable by a
// method that means something else.
func TestRemovalRequiresDelete(t *testing.T) {
	h := &removalHandler{response: &RemoveProviderAccountResponse{Removed: true}}
	s := serverWith(t, h)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(method, "/v1/providers/cloudflare/accounts/acct-1", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s returned %d, want 405", method, rec.Code)
		}
		if h.gotAccount != "" {
			t.Fatalf("%s reached the handler", method)
		}
	}
}

// TestAMalformedAccountPathIsRefused pins that the widened path parser does not
// accept paths it should not.
func TestAMalformedAccountPathIsRefused(t *testing.T) {
	h := &removalHandler{response: &RemoveProviderAccountResponse{Removed: true}}
	s := serverWith(t, h)

	for _, path := range []string{
		"/v1/providers/cloudflare/accounts/",
		"/v1/providers//accounts/acct-1",
		"/v1/providers/cloudflare/authenticate/acct-1",
		"/v1/providers/cloudflare/accounts/acct-1/extra",
	} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s was accepted", path)
		}
		if h.gotAccount != "" {
			t.Fatalf("%s reached the removal handler", path)
		}
	}
}

// TestConfiguringAnAccountStillWorks guards the widened parser against breaking
// the two-segment routes it already served.
func TestConfiguringAnAccountStillWorks(t *testing.T) {
	s := serverWith(t, nullHandler{})

	body := strings.NewReader(`{"account_id":"acct-1"}`)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/providers/cloudflare/accounts", body))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("configuring an account returned %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTheRemovalPreviewIsReachable pins the one widened path.
func TestTheRemovalPreviewIsReachable(t *testing.T) {
	h := &removalHandler{response: &RemoveProviderAccountResponse{Removed: true}}
	s := serverWith(t, h)

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/providers/cloudflare/accounts/acct-1/removal-preview", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var preview AccountRemovalPreviewDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Fingerprint == "" {
		t.Fatal("the preview carries no fingerprint to confirm against")
	}
}

// TestOnlyTheRemovalPreviewIsAcceptedAsAFourthSegment pins that widening the
// parser by one literal did not open it generally. The existing refusal of
// other four-segment paths is unchanged.
func TestOnlyTheRemovalPreviewIsAcceptedAsAFourthSegment(t *testing.T) {
	h := &removalHandler{response: &RemoveProviderAccountResponse{Removed: true}}
	s := serverWith(t, h)

	for _, path := range []string{
		"/v1/providers/cloudflare/accounts/acct-1/extra",
		"/v1/providers/cloudflare/accounts/acct-1/removal-preview/more",
		"/v1/providers/cloudflare/authenticate/acct-1/removal-preview",
	} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s was accepted", path)
		}
	}
}

// TestTheFingerprintReachesTheHandler pins that what the caller confirmed
// travels with the removal.
func TestTheFingerprintReachesTheHandler(t *testing.T) {
	h := &removalHandler{response: &RemoveProviderAccountResponse{Removed: true}}
	s := serverWith(t, h)

	body := strings.NewReader(`{"fingerprint":"fp-abc123"}`)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete,
		"/v1/providers/cloudflare/accounts/acct-1", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.gotFingerprint != "fp-abc123" {
		t.Fatalf("the handler received fingerprint %q", h.gotFingerprint)
	}
}
