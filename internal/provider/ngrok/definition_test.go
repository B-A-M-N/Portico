package ngrok

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/provider"
)

// TestNgrokActivatesFromTheEnvironmentToken pins that a machine configured the
// way ngrok's own setup actions describe is actually usable.
//
// The agent reads NGROK_AUTHTOKEN itself, so no account needs to be stored.
// Requiring one made ngrok unconfigurable by any path: Portico cannot check an
// ngrok token, so importing it as an account would have claimed a verification
// that never happened, and refusing the import left the provider with nothing.
func TestNgrokActivatesFromTheEnvironmentToken(t *testing.T) {
	def := NewDefinition(DefinitionConfig{Bin: "ngrok", Enabled: true})

	withToken := provider.ActivationRequest{Services: provider.RuntimeServices{
		Getenv: func(key string) string {
			if key == AuthTokenEnvVar {
				return "a-real-looking-token"
			}
			return ""
		},
	}}
	inst, err := def.Activate(context.Background(), withToken)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if inst.Provider == nil {
		t.Fatal("an environment token produced no adapter, so ngrok cannot be used at all")
	}
	if inst.Catalog.Reason == "" {
		t.Fatal("nothing says where the credential came from")
	}

	withoutToken := provider.ActivationRequest{Services: provider.RuntimeServices{
		Getenv: func(string) string { return "" },
	}}
	inst, err = def.Activate(context.Background(), withoutToken)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if inst.Provider != nil {
		t.Fatal("ngrok produced an adapter with no credential available")
	}
	if inst.Catalog.Availability != provider.AvailabilityUnconfigured {
		t.Fatalf("availability = %q, want unconfigured", inst.Catalog.Availability)
	}
}
