package cloudflare

import "testing"

func TestCloudflareSetupUsesOneCredentialAndOptionalIdentityFallback(t *testing.T) {
	flow := cloudflareSetupFlow()
	credentialFields := 0
	var accountFieldID string
	for _, field := range flow.Fields {
		if field.Secret {
			credentialFields++
		}
		if field.ID == "account_id" {
			accountFieldID = field.ID
			if field.Required {
				t.Fatal("account identity is required before token discovery")
			}
			if field.InputKind != "provider_identity" {
				t.Fatalf("account identity kind = %q, want provider_identity", field.InputKind)
			}
		}
	}
	if credentialFields != 1 {
		t.Fatalf("credential fields = %d, want exactly one", credentialFields)
	}
	if accountFieldID == "" {
		t.Fatal("Cloudflare setup has no account identity fallback")
	}
}
