package core

import (
	"strings"
	"testing"
)

// TestValidateTunnelID pins the control plane's identifier format at the
// single authority both the adapter and the wizard call. The format is
// tunnel_ + exactly 32 lowercase hexadecimal characters; anything else —
// including letters g-z that a looser [a-z0-9] class once accepted — is
// malformed.
func TestValidateTunnelID(t *testing.T) {
	valid := "tunnel_" + strings.Repeat("a0f1", 8)
	cases := []struct {
		name string
		id   string
		want bool // true = accepted
	}{
		{"valid hexadecimal", valid, true},
		{"all digits", "tunnel_00000000000000000000000000000000", true},
		{"all letters", "tunnel_" + strings.Repeat("abcdef", 5) + "ab", true},
		{"contains g", "tunnel_g000000000000000000000000000000", false},
		{"contains z", "tunnel_z000000000000000000000000000000", false},
		{"uppercase hex", strings.ToUpper(valid), false},
		{"too short", "tunnel_" + strings.Repeat("a", 31), false},
		{"too long", "tunnel_" + strings.Repeat("a", 33), false},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"surrounding whitespace trimmed", "  " + valid + "\t", true},
		{"interior whitespace", "tunnel_a aaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{"missing prefix", strings.Repeat("a", 32), false},
		{"wrong prefix", "tunnels_" + strings.Repeat("a", 32), false},
		{"prefix only", "tunnel_", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTunnelID(tc.id)
			if got := err == nil; got != tc.want {
				t.Fatalf("ValidateTunnelID(%q) = %v, want acceptance=%v", tc.id, err, tc.want)
			}
			if tc.want && !ValidTunnelID(strings.TrimSpace(tc.id)) {
				t.Fatalf("ValidTunnelID disagrees with ValidateTunnelID for %q", tc.id)
			}
		})
	}
}

// TestNormalizeProviderID pins the read-time alias: the legacy workload
// spelling resolves to the transport identity and never the reverse.
func TestNormalizeProviderID(t *testing.T) {
	if got := NormalizeProviderID(ProviderIDOpenAITunnel); got != ProviderIDClientTunnel {
		t.Fatalf("legacy ID normalized to %q, want %q", got, ProviderIDClientTunnel)
	}
	if got := NormalizeProviderID(ProviderIDClientTunnel); got != ProviderIDClientTunnel {
		t.Fatalf("canonical ID must pass through unchanged, got %q", got)
	}
	if got := NormalizeProviderID("cloudflare"); got != "cloudflare" {
		t.Fatalf("unrelated provider IDs must pass through unchanged, got %q", got)
	}
	if NormalizeProviderID(ProviderIDClientTunnel) == ProviderIDOpenAITunnel {
		t.Fatal("the alias must never map the canonical ID back onto the legacy spelling")
	}
}
