package tui

import (
	"github.com/B-A-M-N/portico/internal/ipc"
)

// What "access protection" means, per provider.
//
// The Providers screen once rendered the protection line as
// renderCapability(len(ProtectionModes) > 0, "Access protection"), which
// collapsed four different facts into one claim. A local forward (whose
// protection list is [none] because the listener binds loopback) and Tailscale
// (whose list is [private_network] because reachability is tailnet membership)
// both displayed "○ Access protection (account setup required)" — promising
// that configuring an account would produce protection Portico neither holds a
// credential for nor applies. For a beginner, the Providers screen is the
// answer to "what can I use and what do I need to do?", so the line must say
// which of the four things is true:
//
//   - Portico-applied policy (email_otp, identity_provider, service_token):
//     real Access protection, gated on the account that carries the policy.
//   - Network-enforced reachability (private_network): membership and the
//     network's own ACLs decide who reaches the service; Portico stores
//     nothing and applies nothing.
//   - Inherently local-only (only "none", and no public exposure): protection
//     is not applicable — the connection never leaves the machine.
//   - Open to anyone (only "none", with a public address): no protection —
//     whoever has the address can open it.
//
// The classification reads only the structured capability DTO the supervisor
// already carries; no human semantics are derived from a list length.

// porticoPolicyModes are the protection kinds Portico itself configures on the
// provider after an account is set up. Anything else in the list means
// reachability is governed outside Portico.
var porticoPolicyModes = map[string]bool{
	"email_otp":         true,
	"identity_provider": true,
	"service_token":     true,
}

// protectionSemantics is the rendered form of one provider's protection
// capability: a symbol consistent with the screen's other capability lines,
// the honest sentence, and the theme style to draw it in.
type protectionSemantics struct {
	Symbol string
	Text   string
	Style  string
}

// protectionSemanticsFor classifies a provider's declared protection
// capability into what it means for who can reach a connection through it.
func protectionSemanticsFor(p ipc.ProviderDTO) protectionSemantics {
	caps := p.Capabilities
	if caps == nil {
		caps = &ipc.CapabilitySetDTO{}
	}

	// An adapter that never answered declares nothing: ProtectionModes empty
	// alongside no exposure capability is the zero payload an unavailable
	// provider carries, and every branch below would read it as a fact. A
	// public tunnel provider whose client is missing would be described as
	// local-only — the same lie in the opposite direction. Say nothing yet.
	hasDeclaration := len(caps.ProtectionModes) > 0 ||
		caps.TemporaryAddresses || caps.CustomHostnames || caps.PrivateExposure
	if !hasDeclaration {
		return protectionSemantics{
			Symbol: "–", Style: "muted",
			Text: "Access protection: not known until the provider is available",
		}
	}

	hasPolicy := false
	privateNetwork := false
	for _, mode := range caps.ProtectionModes {
		if porticoPolicyModes[mode] {
			hasPolicy = true
		}
		if mode == "private_network" {
			privateNetwork = true
		}
	}

	// The declared connection kinds say what a connection through this
	// provider is. port_forward and client_tunnel both declare
	// PrivateExposure — loopback and a platform tunnel are both non-public —
	// but "local-only" is true of exactly one of them, so the kind, not the
	// exposure flag, is what the last branch reads.
	kindOf := func(want string) bool {
		for _, k := range caps.Kinds {
			if k == want {
				return true
			}
		}
		return false
	}

	switch {
	case hasPolicy:
		if p.Authenticated {
			return protectionSemantics{
				Symbol: "✓", Style: "stable",
				Text: "Access protection — Portico applies the policy you choose",
			}
		}
		return protectionSemantics{
			Symbol: "○", Style: "muted",
			Text: "Access protection (account setup required)",
		}
	case privateNetwork || kindOf("private_network"):
		// Membership is a real, working guarantee — the network refuses
		// everyone else — so it draws as satisfied, with the honest note that
		// the network, not Portico, is what enforces it.
		return protectionSemantics{
			Symbol: "✓", Style: "stable",
			Text: "Access limited to the network — membership decides who reaches it, not Portico",
		}
	case caps.TemporaryAddresses || caps.CustomHostnames:
		return protectionSemantics{
			Symbol: "–", Style: "muted",
			Text: "No access protection — anyone with the address can open it",
		}
	case kindOf("client_tunnel"):
		// A private platform tunnel: reachability is mediated by the platform
		// that carries the tunnel, and Portico applies no policy — the
		// declaration says exactly that, so the line does too.
		return protectionSemantics{
			Symbol: "✓", Style: "stable",
			Text: "Access limited to the private network — the platform mediates it; Portico applies no policy",
		}
	case kindOf("port_forward"):
		return protectionSemantics{
			Symbol: "–", Style: "muted",
			Text: "Access protection: not applicable — this connection is local-only",
		}
	case caps.PrivateExposure:
		// A private-exposure provider whose kind is not one of the three
		// above: reachability is private but the declaration does not say by
		// whom, so the line does not guess.
		return protectionSemantics{
			Symbol: "✓", Style: "stable",
			Text: "Access limited to the private network",
		}
	}
	return protectionSemantics{
		Symbol: "–", Style: "muted",
		Text: "Access protection: not applicable — this connection is local-only",
	}
}
