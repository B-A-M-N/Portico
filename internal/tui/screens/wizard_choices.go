package screens

import (
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
	Reason  string
	Section string // group label for multi-section menus
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
func (m *WizardModel) renderChoices(title string, choices []wizardChoice, selected int) string {
	width := m.contentWidth()
	var b strings.Builder
	b.WriteString(wizardOneLine(title, width))
	b.WriteString("\n\n")

	for i, choice := range choices {
		prefix := "  "
		if i == selected {
			prefix = "\u25b8 "
			if m.useASCII {
				prefix = "> "
			}
		}
		b.WriteString(wizardOneLine(prefix+choice.Label, width) + "\n")
		// The reason is explanatory prose, not part of the label: appending it
		// to a truncated line amputated the "why" — exactly the information an
		// unavailable option exists to carry. It wraps on its own line, hanging
		// under the label.
		if !choice.Available && choice.Reason != "" {
			for _, row := range WrapProse("— "+choice.Reason, m.width, 4) {
				b.WriteString(row + "\n")
			}
		}
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
				// Detail prose hangs four cells in and wraps to the width
				// that remains: hand-broken source strings amputated their
				// tails at narrow terminals exactly where a beginner needed
				// them. Labels stay single lines truncated with an ellipsis,
				// because this expanded detail carries the full text.
				for _, row := range WrapProse(strings.Join(strings.Fields(line), " "), m.width, wizardDetailIndent) {
					b.WriteString(row + "\n")
				}
			}
		}
		if len(choice.Providers) > 0 {
			provided := "Provided by: " + strings.Join(choice.Providers, ", ")
			for _, row := range WrapProse(provided, m.width, wizardDetailIndent) {
				b.WriteString(row + "\n")
			}
		}
	}

	b.WriteString("\n" + wizardOneLine(choiceFooter(choices), width))
	return b.String()
}

// wizardDetailIndent is how far an expanded choice's detail hangs in.
const wizardDetailIndent = 4

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

// usableProvider reports whether a provider can be planned against right now.
//
// The answer is the supervisor's, carried on the snapshot, rather than this
// package's reading of an availability string. Two readings existed and
// disagreed: the recommendation engine refused experimental and degraded
// providers while this list refused only missing clients, so a failed
// recommendation offered providers the engine considers unusable and their
// capabilities made options look deliverable.
func usableProvider(p ipc.ProviderDTO) bool {
	return p.Selectable
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
// providers whose potential includes it. The predicate is the potential
// filter, not a readiness guess: advice must name only providers that could
// deliver the capability after setup, never every provider that happens to be
// unready.
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
// rather than its identity. The supports* family answers what a provider can
// do right now; the could* family answers what its definition declares it
// could do after setup. They are different facts about different moments and
// neither may be derived from the other.

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

// couldSupportCustomHostname reports whether a provider's definition declares
// custom hostnames as achievable after setup, whatever its adapter can do now.
func couldSupportCustomHostname(p ipc.ProviderDTO) bool {
	return p.Capabilities != nil && p.Capabilities.PotentialCustomHostnames
}

// couldSupportProtection is the potential-capability analogue of
// supportsProtection.
func couldSupportProtection(kind string) func(ipc.ProviderDTO) bool {
	return func(p ipc.ProviderDTO) bool {
		if p.Capabilities == nil {
			return false
		}
		for _, mode := range p.Capabilities.PotentialProtectionModes {
			if mode == kind {
				return true
			}
		}
		return false
	}
}

// setupAdvice appends the "one of these providers needs setting up" block for
// the providers the potential predicate accepts, and returns whether there was
// anything to advise.
func (c providerCapabilities) setupAdvice(detail []string, pred func(ipc.ProviderDTO) bool) ([]string, bool) {
	actions := c.setupActionsFor(pred)
	if len(actions) == 0 {
		return detail, false
	}
	detail = append(detail, "",
		"To use this, one of these providers needs setting up:")
	for _, action := range actions {
		detail = append(detail, "  • "+action)
	}
	return detail, true
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
		// Advice is filtered by potential, not by unreadiness: naming every
		// unready provider here sent users to configure providers that could
		// never own a hostname, however well they were set up.
		if detail, advised := c.setupAdvice(permanent.Detail, couldSupportCustomHostname); advised {
			permanent.Reason = "needs a provider account"
			permanent.Detail = detail
		} else {
			permanent.Reason = "no installed provider offers permanent hostnames"
			permanent.Detail = append(permanent.Detail, "",
				"No installed provider can own a hostname. Set one up from the",
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
		// Same potential filter as the exposure choices: only providers whose
		// definition declares email OTP after setup belong in the advice.
		if detail, advised := c.setupAdvice(otp.Detail, couldSupportProtection("email_otp")); advised {
			otp.Reason = "needs a provider account"
			otp.Detail = detail
		} else {
			otp.Reason = "no installed provider offers sign-in protection"
		}
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
