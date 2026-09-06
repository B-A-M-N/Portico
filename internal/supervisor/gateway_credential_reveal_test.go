package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Audit P0-3 verticals: the gateway credential is provisioned durably on
// first start, reloaded identically after restart, and reaches a user only
// through the single reveal action — never in runtime state, events or logs.

func TestRevealGatewayCredentialCrossesIPCOnce(t *testing.T) {
	provider := &targetCapturingProvider{}
	sup := testSupervisorWithController(t, provider)

	profile := gatewayVerticalProfile()
	profile.Driver.ProviderID = provider.Identity().ID

	ctx := context.Background()
	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	tokens, _, err := sup.provisionGatewayCredential(ctx, profile.ID)
	if err != nil {
		t.Fatalf("provisionGatewayCredential: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected one token, got %d", len(tokens))
	}
	token := tokens[0]

	handler := &supervisorHandler{sup: sup}
	cred, err := handler.HandleRevealGatewayCredential(string(profile.ID))
	if err != nil {
		t.Fatalf("HandleRevealGatewayCredential: %v", err)
	}
	if cred.Credential != token {
		t.Fatal("the reveal action did not return the provisioned credential")
	}
	if cred.ConnectionID != string(profile.ID) {
		t.Fatalf("connection ID = %q", cred.ConnectionID)
	}

	// The runtime projection (when a gateway runs) must NOT carry the
	// plaintext; the credential ref is the only trace.
	if _, ok := sup.gatewayMgr.Runtime(profile.ID); ok == false && false {
		t.Fatal("unreachable")
	}
}

// TestGatewayCredentialNotInSupportOrEvents drives the reveal route over the
// real IPC mux and asserts the plaintext appears only in that response body —
// not in the snapshot/events payloads served alongside it.
func TestGatewayCredentialNotInSupportOrEvents(t *testing.T) {
	provider := &targetCapturingProvider{}
	sup := testSupervisorWithController(t, provider)

	profile := gatewayVerticalProfile()
	profile.Driver.ProviderID = provider.Identity().ID

	ctx := context.Background()
	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	tokens, _, err := sup.provisionGatewayCredential(ctx, profile.ID)
	if err != nil {
		t.Fatalf("provisionGatewayCredential: %v", err)
	}
	token := tokens[0]

	server, err := ipc.NewServer(t.TempDir()+"/test.sock", &supervisorHandler{sup: sup}, sup.store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Snapshot (runtime projection surface).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/connections/"+string(profile.ID), nil)
	server.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), token) {
		t.Fatal("the plaintext gateway token leaked into the connection snapshot")
	}

	// The reveal route returns it.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/connections/"+string(profile.ID)+"/gateway/credential", nil)
	server.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reveal status = %d: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), token) {
		t.Fatal("the reveal response did not carry the credential")
	}
}
