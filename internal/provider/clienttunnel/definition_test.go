package clienttunnel

import (
	"context"
	"strings"
	"testing"
)

// TestPrepareAccountRejectsMalformedKeys pins the local key check: setup can
// rule out truncated or whitespace-corrupted pastes even though the
// authoritative key check happens at /readyz after launch.
func TestPrepareAccountRejectsMalformedKeys(t *testing.T) {
	d := NewDefinition(DefinitionConfig{})
	cases := []struct {
		name string
		key  string
		ok   bool
	}{
		{"valid", "sk-runtime-abcdef1234567890", true},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"leading space trimmed ok", " sk-runtime-key-1 ", true},
		{"interior space", "sk runtime key", false},
		{"newline", "sk-key\n", false},
		{"too short", "sk", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := d.PrepareAccount(map[string]string{"credential": tc.key})
			if tc.ok && err != nil {
				t.Fatalf("valid key refused: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("malformed key accepted")
			}
			if tc.ok {
				if prepared.Account.Provider != ProviderID || prepared.Account.ID != runtimeCredentialAccountID() {
					t.Fatalf("account identity = %s/%s, want the transport's implicit account",
						prepared.Account.Provider, prepared.Account.ID)
				}
				if prepared.Account.CredentialRef == "" {
					t.Fatal("no credential reference derived")
				}
			}
		})
	}
}

// TestVerifyAccountIsLocalOnlyAndHonest pins that verification never claims an
// OpenAI round trip it did not make.
func TestVerifyAccountIsLocalOnlyAndHonest(t *testing.T) {
	d := NewDefinition(DefinitionConfig{})
	prepared, err := d.PrepareAccount(map[string]string{"credential": "sk-runtime-abcdef1234567890"})
	if err != nil {
		t.Fatal(err)
	}
	validation, err := d.VerifyAccount(context.Background(), prepared)
	if err != nil {
		t.Fatalf("a well-shaped key was refused: %v", err)
	}
	found := false
	for _, note := range validation.Notes {
		if strings.Contains(note, "checked locally only") {
			found = true
		}
	}
	if !found {
		t.Fatalf("verification notes do not disclose the local-only scope: %#v", validation.Notes)
	}
}
