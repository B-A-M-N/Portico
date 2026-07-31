package screens

import (
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// wizardChoice is one option on a wizard menu, including options that cannot be
// picked right now.
//
// Unavailable options are listed rather than hidden. Hiding them meant a menu
// could contain a single item — while still advertising "↑↓ Navigate" — and
// gave the user no way to learn that the missing capability exists, what it
// would do, or what it needs. A person cannot ask for a permanent address they
// have never been told about.
type wizardChoice struct {
	// Value is the stored answer.
	Value string
	// Label is the one-line summary shown in the list.
	Label string
	// Available reports whether this can be picked now.
	Available bool
	// Reason says why it cannot be picked, in the user's terms.
	Reason string
	// Detail expands under the highlighted choice: what it does, and for an
	// unavailable one, what would make it work.
	Detail []string
	// Providers names the providers that can deliver this, so a choice is
	// attributed rather than appearing to be a property of Portico itself.
	Providers []string
}

// availableValues returns the values that can currently be picked.
func availableValues(choices []wizardChoice) []string {
	values := make([]string, 0, len(choices))
	for _, c := range choices {
		if c.Available {
			values = append(values, c.Value)
		}
	}
	return values
}

// choiceAt returns the choice at an index, if any.
func choiceAt(choices []wizardChoice, index int) (wizardChoice, bool) {
	if index < 0 || index >= len(choices) {
		return wizardChoice{}, false
	}
	return choices[index], true
}

// firstAvailable returns the index of the first pickable choice, so a menu does
// not open with an unavailable item highlighted.
func firstAvailable(choices []wizardChoice) int {
	for i, c := range choices {
		if c.Available {
			return i
		}
	}
	return 0
}

// renderChoices draws a menu that shows every option, marks the ones that
// cannot be picked, and expands the highlighted one.
//
// The footer describes only the keys that do something here: a one-item menu
// used to advertise navigation that had nowhere to go.
func renderChoices(title string, choices []wizardChoice, selected int) string {
	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n\n")

	for i, choice := range choices {
		prefix := "  "
		if i == selected {
			prefix = "▸ "
		}
		mark := ""
		if !choice.Available {
			mark = "  —  " + choice.Reason
		}
		b.WriteString(prefix + choice.Label + mark + "\n")
	}

	// Only the highlighted choice expands, so the list stays readable.
	if choice, ok := choiceAt(choices, selected); ok {
		if len(choice.Detail) > 0 {
			b.WriteString("\n")
			for _, line := range choice.Detail {
				if line == "" {
					b.WriteString("\n")
					continue
				}
				b.WriteString("    " + line + "\n")
			}
		}
		if len(choice.Providers) > 0 {
			b.WriteString(fmt.Sprintf("\n    Provided by: %s\n", strings.Join(choice.Providers, ", ")))
		}
	}

	b.WriteString("\n" + choiceFooter(choices))
	return b.String()
}

// choiceFooter describes the keys that actually do something on this menu.
func choiceFooter(choices []wizardChoice) string {
	keys := []string{}
	if len(choices) > 1 {
		keys = append(keys, "↑↓ Navigate")
	}
	keys = append(keys, "Enter Select", "Esc Back")
	return strings.Join(keys, "  ")
}

// providerCapabilities is the snapshot the wizard derives its options from.
//
// It replaces a single "is Cloudflare fully configured" boolean, which decided
// what every menu offered — so a second provider could be installed, configured
// and working without changing a single thing the user was shown.
type providerCapabilities struct {
	providers []ipc.ProviderDTO
}

// usable reports whether a provider can be planned against right now.
func usableProvider(p ipc.ProviderDTO) bool {
	switch p.Availability {
	case "not_implemented", "client_missing":
		return false
	}
	return true
}

// supporting returns the display names of usable providers satisfying a
// predicate, so an option can say who delivers it.
func (c providerCapabilities) supporting(pred func(ipc.ProviderDTO) bool) []string {
	var names []string
	for _, p := range c.providers {
		if !usableProvider(p) || !pred(p) {
			continue
		}
		name := p.DisplayName
		if name == "" {
			name = p.ID
		}
		names = append(names, name)
	}
	return names
}

// setupActionsFor collects what would make a capability available, from the
// providers that declare it but are not ready.
func (c providerCapabilities) setupActionsFor(pred func(ipc.ProviderDTO) bool) []string {
	var actions []string
	seen := map[string]bool{}
	for _, p := range c.providers {
		if !pred(p) {
			continue
		}
		for _, action := range p.SetupActions {
			if !seen[action] {
				seen[action] = true
				actions = append(actions, action)
			}
		}
	}
	return actions
}

// hasCapability reports whether any usable provider satisfies a predicate.
func (c providerCapabilities) hasCapability(pred func(ipc.ProviderDTO) bool) bool {
	return len(c.supporting(pred)) > 0
}

// Capability predicates. Each reads the provider's declared capability set
// rather than its identity.

func supportsTemporary(p ipc.ProviderDTO) bool {
	return p.Capabilities != nil && p.Capabilities.TemporaryAddresses
}

func supportsCustomHostname(p ipc.ProviderDTO) bool {
	return p.Capabilities != nil && p.Capabilities.CustomHostnames
}

func supportsProtection(kind string) func(ipc.ProviderDTO) bool {
	return func(p ipc.ProviderDTO) bool {
		if p.Capabilities == nil {
			return false
		}
		for _, mode := range p.Capabilities.ProtectionModes {
			if mode == kind {
				return true
			}
		}
		return false
	}
}

// exposureChoices lists every way a connection can be reachable.
//
// Both are always listed. Offering only the one that happens to work today left
// a single-item menu and no way to discover that a permanent address exists —
// so a user with no account could not find out that Portico supports the thing
// they actually wanted, or what it would take.
func (c providerCapabilities) exposureChoices(sourceKind, mcpTransport string) []wizardChoice {
	temporary := wizardChoice{
		Value: "temporary_public",
		Label: "Temporarily, with a generated address",
		Detail: []string{
			"Portico generates the address. It changes each time the connection opens,",
			"and anyone with the link can reach it while it is open.",
		},
		Providers: c.supporting(supportsTemporary),
	}
	temporary.Available = len(temporary.Providers) > 0
	if !temporary.Available {
		temporary.Reason = "no installed provider offers generated addresses"
	}

	permanent := wizardChoice{
		Value: "permanent_public",
		Label: "Permanently, at a hostname you choose",
		Detail: []string{
			"The address stays the same across restarts, so links keep working and",
			"access rules can be attached to it.",
		},
		Providers: c.supporting(supportsCustomHostname),
	}
	permanent.Available = len(permanent.Providers) > 0
	if !permanent.Available {
		permanent.Reason = "needs a provider account"
		actions := c.setupActionsFor(func(p ipc.ProviderDTO) bool {
			return !usableProvider(p) || !supportsCustomHostname(p)
		})
		if len(actions) > 0 {
			permanent.Detail = append(permanent.Detail, "",
				"To use this, one of these providers needs setting up:")
			for _, action := range actions {
				permanent.Detail = append(permanent.Detail, "  • "+action)
			}
		} else {
			permanent.Detail = append(permanent.Detail, "",
				"No installed provider can own a hostname yet. Set one up from the",
				"provider screen, then come back.")
		}
	}

	// An SSE client cannot follow an address that changes, so a temporary
	// address genuinely cannot carry it.
	if sourceKind == "mcp_server" && mcpTransport == "sse" {
		temporary.Available = false
		temporary.Reason = "SSE cannot follow an address that changes"
		temporary.Detail = []string{
			"An SSE client holds one long-lived connection to a fixed address.",
			"A generated address changes, so the client cannot reconnect to it.",
		}
	}

	return []wizardChoice{temporary, permanent}
}

// protectionChoices lists who may reach the connection.
//
// Access protection is bound to an address that does not move, which core
// validation now enforces for every path. Listing it against a temporary
// address, with that reason, is how a user learns the two decisions are linked.
func (c providerCapabilities) protectionChoices(exposureMode string) []wizardChoice {
	open := wizardChoice{
		Value: "none", Label: "Anyone with the address", Available: true,
		Detail: []string{"No sign-in. Anyone holding the link can reach it."},
	}

	otp := wizardChoice{
		Value: "email_otp",
		Label: "Only people you name, by email passcode",
		Detail: []string{
			"Visitors enter their email and receive a one-time code.",
			"Only the addresses and domains you list can sign in.",
		},
		Providers: c.supporting(supportsProtection("email_otp")),
	}
	switch {
	case len(otp.Providers) == 0:
		otp.Reason = "no installed provider offers sign-in protection"
	case exposureMode != "permanent_public":
		// Not a provider limitation, so it is not phrased as one.
		otp.Reason = "needs a permanent address"
		otp.Detail = append(otp.Detail, "",
			"A generated address changes when the connection reopens, so the rule",
			"would stop applying without anything reporting it. Choose a permanent",
			"address to use this.")
	default:
		otp.Available = true
	}

	return []wizardChoice{open, otp}
}
