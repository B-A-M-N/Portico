package supervisor

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/origin"
	"github.com/B-A-M-N/portico/internal/store"
)

// Audit P0-3: the gateway credential is provisioned once through the
// credential store and reloaded — never silently regenerated — on subsequent
// starts, which is what a supervisor restart exercises.
func TestGatewayCredentialSurvivesRestart(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sup := &Supervisor{
		store: st, gatewayMgr: newGatewayManager(),
		origins: origin.NewManager(), mutating: true,
	}
	profile := gatewayVerticalProfile()

	ctx := context.Background()

	// First "boot": provision via the production path.
	tokens1, ref1, err := sup.provisionGatewayCredential(ctx, profile.ID)
	if err != nil {
		t.Fatalf("provision (first boot): %v", err)
	}
	if len(tokens1) != 1 || tokens1[0] == "" {
		t.Fatalf("first provisioning produced no token: %v", tokens1)
	}
	if ref1 != GatewayCredentialRef(profile.ID) {
		t.Fatalf("ref = %q, want the connection-scoped reference", ref1)
	}

	// Second "boot": a fresh in-memory supervisor over the SAME store. The
	// credential must be reloaded from the encrypted row, not regenerated.
	sup2 := &Supervisor{
		store: st, gatewayMgr: newGatewayManager(),
		origins: origin.NewManager(), mutating: true,
	}
	tokens2, ref2, err := sup2.provisionGatewayCredential(ctx, profile.ID)
	if err != nil {
		t.Fatalf("provision (second boot): %v", err)
	}
	if len(tokens2) != 1 || tokens2[0] != tokens1[0] {
		t.Fatal("the gateway credential was regenerated instead of reloaded across a restart")
	}
	if ref2 != ref1 {
		t.Fatalf("ref changed across restart: %q -> %q", ref1, ref2)
	}

	// The stored value must decrypt to the same plaintext.
	loaded, err := st.LoadProviderCredential(ctx, "gateway", ref1)
	if err != nil {
		t.Fatalf("LoadProviderCredential: %v", err)
	}
	if loaded != tokens1[0] {
		t.Fatal("the stored ciphertext does not round-trip to the provisioned token")
	}
}
