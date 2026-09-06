package supervisor

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// Audit P0-4: an authenticated gateway must cross IPC reporting
// auth_enabled=true with only its opaque credential reference — the plaintext
// token must never appear in runtime state.
func TestGatewayRuntimeProjectsAuthMetadata(t *testing.T) {
	sup := &Supervisor{gatewayMgr: newGatewayManager()}
	profile := gatewayVerticalProfile() // openai_compatible → AuthRequired

	spec, needed := sup.gatewaySpecFor(profile)
	if !needed {
		t.Fatal("an openai_compatible exposure produced no gateway requirement")
	}
	const secretToken = "super-secret-gateway-token-value"
	spec.AuthTokens = []string{secretToken}
	spec.CredentialRef = GatewayCredentialRef(profile.ID)

	endpoint, err := sup.gatewayMgr.StartGateway(context.Background(), profile.ID, spec)
	if err != nil {
		t.Fatalf("StartGateway: %v", err)
	}

	rt, ok := sup.gatewayMgr.Runtime(profile.ID)
	if !ok {
		t.Fatal("the running gateway produced no runtime projection")
	}
	if rt.Endpoint != endpoint {
		t.Fatalf("runtime endpoint = %q, want %q", rt.Endpoint, endpoint)
	}
	if !rt.AuthEnabled {
		t.Fatal("an authenticated gateway crossed IPC looking unauthenticated")
	}
	wantRef := GatewayCredentialRef(profile.ID)
	if rt.CredentialRef != wantRef {
		t.Fatalf("credential ref = %q, want %q", rt.CredentialRef, wantRef)
	}
	// The plaintext must not ride along anywhere in the projection.
	blob := string([]byte(rt.Endpoint + rt.Upstream + rt.CredentialRef + rt.StartedAt.String()))
	if strings.Contains(blob, secretToken) {
		t.Fatal("the plaintext gateway token leaked into the runtime projection")
	}
}

// A non-authenticated gateway (client-mediated MCP transport) projects
// auth_enabled=false and no credential reference.
func TestUnauthenticatedGatewayRuntimeProjection(t *testing.T) {
	sup := &Supervisor{gatewayMgr: newGatewayManager()}
	spec := core.GatewayStartSpec{
		Upstream:     "http://127.0.0.1:8081",
		AuthRequired: false,
		AllowSSE:     true,
	}
	if _, err := sup.gatewayMgr.StartGateway(context.Background(), "conn-open", spec); err != nil {
		t.Fatalf("StartGateway: %v", err)
	}
	rt, ok := sup.gatewayMgr.Runtime("conn-open")
	if !ok {
		t.Fatal("no runtime projection for a running gateway")
	}
	if rt.AuthEnabled {
		t.Fatal("an unauthenticated gateway projected auth_enabled=true")
	}
	if rt.CredentialRef != "" {
		t.Fatalf("an unauthenticated gateway projected credential ref %q", rt.CredentialRef)
	}
}
