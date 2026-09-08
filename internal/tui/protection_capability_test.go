package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The protection line on the Providers screen must say which of the four
// truths a provider's declaration carries — not collapse them into one
// "account setup required" claim. A local forward was told it needed an
// account for protection that is not applicable (the listener binds loopback),
// and Tailscale was told an account would produce protection Portico neither
// stores nor applies (membership and tailnet ACLs decide). Each case below
// carries the ProtectionModes and exposure facts the real supervisor derives
// from that provider's Capabilities() declaration.
func TestProtectionSemanticsMatchTheDeclaration(t *testing.T) {
	cases := []struct {
		name string
		p    ipc.ProviderDTO
		// wantText is asserted with Contains so stability styling never has to
		// be repeated here; wantAbsent forbids a phrase the truth contradicts.
		wantText   string
		wantAbsent []string
	}{
		{
			name: "local port forward: not applicable, no account lie",
			p: ipc.ProviderDTO{
				DisplayName: "Local port forward",
				Capabilities: &ipc.CapabilitySetDTO{
					// portforward.Capabilities: loopback listener, no policy.
					Kinds:           []string{"port_forward"},
					ProtectionModes: []string{"none"},
					PrivateExposure: true,
				},
			},
			wantText: "not applicable",
			wantAbsent: []string{
				"account setup required",
				"✓",
			},
		},
		{
			name: "tailscale: network membership, no account lie",
			p: ipc.ProviderDTO{
				DisplayName: "Tailscale",
				Capabilities: &ipc.CapabilitySetDTO{
					// tailscale.Capabilities: reachability is tailnet membership.
					Kinds:           []string{"private_network"},
					ProtectionModes: []string{"private_network"},
					PrivateExposure: true,
				},
			},
			wantText: "membership",
			wantAbsent: []string{
				"account setup required",
			},
		},
		{
			name: "cloudflare without an account: open address, honestly",
			p: ipc.ProviderDTO{
				DisplayName: "Cloudflare",
				Capabilities: &ipc.CapabilitySetDTO{
					// Unauthenticated Cloudflare: quick tunnel only, no policy.
					ProtectionModes:    []string{"none"},
					TemporaryAddresses: true,
				},
			},
			wantText: "anyone with the address",
			wantAbsent: []string{
				"account setup required",
			},
		},
		{
			name: "cloudflare with an account: real protection, stated as working",
			p: ipc.ProviderDTO{
				DisplayName:   "Cloudflare",
				Authenticated: true,
				Capabilities: &ipc.CapabilitySetDTO{
					// cloudflare.Capabilities after account setup.
					ProtectionModes: []string{"email_otp", "none"},
				},
			},
			wantText: "Access protection",
			wantAbsent: []string{
				"account setup required",
			},
		},
		{
			name: "cloudflare without an account but policy-capable: names the gate",
			p: ipc.ProviderDTO{
				DisplayName: "Cloudflare",
				Capabilities: &ipc.CapabilitySetDTO{
					ProtectionModes:    []string{"email_otp", "none"},
					TemporaryAddresses: true,
				},
			},
			wantText: "account setup required",
		},
		{
			name: "ngrok with client missing: no empty-payload lie",
			p: ipc.ProviderDTO{
				DisplayName: "ngrok",
				// An unavailable provider carries the zero payload: no modes,
				// no exposure facts. Reading it as a declaration said
				// "local-only" of a public tunnel provider.
				Capabilities: &ipc.CapabilitySetDTO{},
			},
			wantText: "not known until the provider is available",
			wantAbsent: []string{
				"local-only",
				"account setup required",
			},
		},
		{
			name: "private platform tunnel: platform-mediated, honestly",
			p: ipc.ProviderDTO{
				DisplayName: "Client tunnel",
				Capabilities: &ipc.CapabilitySetDTO{
					// clienttunnel.Capabilities: private tunnel, no policy.
					Kinds:           []string{"client_tunnel"},
					ProtectionModes: []string{"none"},
					PrivateExposure: true,
				},
			},
			wantText: "private network",
			wantAbsent: []string{
				"local-only",
				"account setup required",
			},
		},
		{
			name:     "no capability payload at all says nothing yet",
			p:        ipc.ProviderDTO{DisplayName: "Unknown"},
			wantText: "not known until the provider is available",
			wantAbsent: []string{
				"local-only",
				"account setup required",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := protectionSemanticsFor(tc.p)
			line := got.Symbol + " " + got.Text
			if !strings.Contains(got.Text, tc.wantText) {
				t.Fatalf("protection line %q does not contain %q", line, tc.wantText)
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got.Text, absent) {
					t.Fatalf("protection line %q claims %q, which the declaration contradicts", line, absent)
				}
			}
		})
	}
}

// TestProvidersScreenRendersTheProtectionClassification pins the rendering
// path: the screen draws the classified sentence, and the two providers the
// audit caught lying never display the misleading claim again.
func TestProvidersScreenRendersTheProtectionClassification(t *testing.T) {
	snap := ipc.SnapshotDTO{
		Providers: []ipc.ProviderDTO{
			{
				ID: "portforward", DisplayName: "Local port forward",
				Availability: "ready", Readiness: "ready", Selectable: true,
				Capabilities: &ipc.CapabilitySetDTO{ProtectionModes: []string{"none"}},
			},
			{
				ID: "tailscale", DisplayName: "Tailscale",
				Availability: "ready", Readiness: "ready", Selectable: true,
				Capabilities: &ipc.CapabilitySetDTO{ProtectionModes: []string{"private_network"}},
			},
			{
				ID: "cloudflare", DisplayName: "Cloudflare",
				Availability: "ready", Readiness: "ready", Selectable: true,
				SetupKind: "account",
				Capabilities: &ipc.CapabilitySetDTO{
					ProtectionModes:    []string{"email_otp", "none"},
					TemporaryAddresses: true,
				},
			},
		},
	}
	m := readyModel(&fakeClient{}, snap)
	m.screen = ScreenProviders
	view := m.renderProviders()

	if strings.Count(view, "Access protection (account setup required)") != 1 {
		t.Fatalf("exactly the policy-capable accountless provider should gate protection on setup, got:\n%s", view)
	}
	if !strings.Contains(view, "not applicable") {
		t.Fatalf("the local forward's protection line does not say it is not applicable:\n%s", view)
	}
	if !strings.Contains(view, "membership") {
		t.Fatalf("the Tailscale protection line does not say reachability is membership:\n%s", view)
	}
}
