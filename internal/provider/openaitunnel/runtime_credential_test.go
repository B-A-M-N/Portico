package openaitunnel

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
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
		{"leading space trimmed ok", " sk-runtime-key-1 ", true}, // trimmed by PrepareAccount first
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

// TestStoredCredentialTakesPrecedenceOverEnvironment pins rotation semantics:
// once a stored key is activated it wins, so rotating through Portico affects
// the next launch without touching the supervisor's environment.
func TestStoredCredentialTakesPrecedenceOverEnvironment(t *testing.T) {
	t.Setenv(CredentialEnvVar, "env-key")
	p, _ := testProvider(t)
	p.SetCredential("stored-key")
	if got := p.credential(); got != "stored-key" {
		t.Fatalf("credential = %q, want the stored key", got)
	}

	// Without a stored key, the environment still works.
	p2, _ := testProvider(t)
	if got := p2.credential(); got != "env-key" {
		t.Fatalf("fallback credential = %q, want the environment value", got)
	}
}

// TestCredentialNeverEntersPlan pins that the stored secret stays out of plans:
// only the env: reference travels in step parameters.
func TestCredentialNeverEntersPlan(t *testing.T) {
	const secret = "sk-super-secret-value"
	t.Setenv(CredentialEnvVar, "")
	p, _ := testProvider(t)
	p.SetCredential(secret)

	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: tunnelProfile(core.DesiredOpen)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, step := range plan.Steps {
		blob := step.Technical.Parameters["mcp_server_url"] +
			step.Technical.Parameters["mcp_command"] + step.Technical.Parameters["tunnel_id"]
		if strings.Contains(blob, secret) {
			t.Fatal("the credential leaked into plan parameters")
		}
	}
}

var _ provider.SetupDefinition = (*Definition)(nil)
