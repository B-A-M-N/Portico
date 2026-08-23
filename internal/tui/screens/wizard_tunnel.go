package screens

import (
	"fmt"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Managing an existing OpenAI Secure MCP Tunnel.
//
// The adapter for this has a full lifecycle — capabilities, a setup flow, a plan,
// step execution, observation — and refuses to plan without a tunnel ID, because
// Portico cannot create the tunnel: creation happens in the OpenAI platform's own
// organization settings. The wizard had no way to collect one, so buildRequest
// returned "client tunnel connections are not yet supported" and the whole adapter
// was unreachable.
//
// So this is framed as what it is: adopting a tunnel that already exists. The
// questions are the ID, the local MCP server the tunnel should reach, and the
// client profile — and the review says plainly that Portico did not create the
// tunnel and will not delete it.

// tunnelIDPattern is the client's own requirement: tunnel_ then 32 lowercase
// alphanumerics. Checking it here means a typo is refused on the question that
// asked for it rather than by the adapter at open time.
var tunnelIDPattern = regexp.MustCompile(`^tunnel_[a-z0-9]{32}$`)

// handleTunnelIDKey collects the identifier of a tunnel that already exists.
func (m *WizardModel) handleTunnelIDKey(key string) tea.Cmd {
	switch key {
	case "esc":
		m.goBack()
	case "enter":
		id := strings.TrimSpace(m.inputValue())
		if id == "" {
			m.err = fmt.Errorf(
				"a tunnel ID is required: Portico manages a tunnel you have already created " +
					"in the OpenAI platform, and cannot create one for you")
			return nil
		}
		if !tunnelIDPattern.MatchString(id) {
			m.err = fmt.Errorf(
				"that does not look like a tunnel ID: the client expects tunnel_ followed by " +
					"32 lowercase letters or digits")
			return nil
		}
		m.err = nil
		m.state.TunnelID = id
		m.state.Step = WizardStepTunnelMCP
		m.setInput(m.state.SourceAddress)
	default:
		m.setInput(editInput(m.inputValue(), key))
	}
	return nil
}

// handleTunnelMCPKey collects the local MCP server the tunnel reaches.
func (m *WizardModel) handleTunnelMCPKey(key string) tea.Cmd {
	switch key {
	case "esc":
		m.goBack()
	case "enter":
		endpoint := strings.TrimSpace(m.inputValue())
		if endpoint == "" {
			m.err = fmt.Errorf(
				"the tunnel needs somewhere to forward to: give the address your MCP server " +
					"listens on, such as http://127.0.0.1:8000")
			return nil
		}
		m.err = nil
		m.state.SourceAddress = endpoint
		m.state.Step = WizardStepTunnelProfile
		m.setInput(m.state.TunnelProfile)
	default:
		m.setInput(editInput(m.inputValue(), key))
	}
	return nil
}

// handleTunnelProfileKey collects the client profile, which is optional.
func (m *WizardModel) handleTunnelProfileKey(key string) tea.Cmd {
	switch key {
	case "esc":
		m.goBack()
	case "enter":
		// Optional: the client has a default profile, and requiring a value here
		// would demand a choice most users do not need to make.
		m.err = nil
		m.state.TunnelProfile = strings.TrimSpace(m.inputValue())
		m.state.Provider = "openai_tunnel"
		m.state.Step = WizardStepReview
		m.selected = 0
	default:
		m.setInput(editInput(m.inputValue(), key))
	}
	return nil
}

// clientTunnelReview describes a tunnel Portico manages rather than one it made.
func (m *WizardModel) clientTunnelReview() []reviewSection {
	return []reviewSection{
		{title: "What will be reachable", lines: []string{
			"Your MCP server at " + m.state.SourceAddress + ", from ChatGPT.",
		}},
		{title: "How it will be reached", lines: []string{
			"Through the tunnel " + m.state.TunnelID + ", which already exists in your",
			"OpenAI organization. The client opens an outbound connection to OpenAI —",
			"nothing on this machine listens for the internet, and there is no public address.",
		}},
		{title: "Who can reach it", lines: []string{
			"Only the tunnel's owner in the OpenAI platform. Access is controlled there,",
			"not by Portico.",
		}},
		{title: "What Portico will create and manage", lines: m.tunnelManagedLines()},
		{title: "When you close it", lines: []string{
			"The tunnel client stops and your MCP server stops being reachable.",
			"The tunnel itself continues to exist in the OpenAI platform.",
		}},
		{title: "Startup and quitting", lines: m.lifecycleLines()},
	}
}

// tunnelManagedLines states exactly what Portico is responsible for, and what it
// is not.
//
// This is the honesty item 13 asks for: Portico runs a client against a tunnel
// somebody else created. Saying it "creates a tunnel" would claim a step it never
// performed, and deleting the connection would then look as though it should
// delete the tunnel.
func (m *WizardModel) tunnelManagedLines() []string {
	lines := []string{
		"The tunnel client process: Portico starts it, watches it, and stops it.",
	}
	if m.state.TunnelProfile != "" {
		lines = append(lines, "It runs with the client profile "+m.state.TunnelProfile+".")
	}
	lines = append(lines,
		"Portico did not create the tunnel and will not delete it. Deleting this",
		"connection stops the client and leaves the tunnel in place.")
	return lines
}

// tunnelStepActions describes one of the tunnel questions.
func (m *WizardModel) tunnelStepActions(help string) WizardActions {
	_, canGoBack := m.previousStep()
	return WizardActions{
		{
			Keys: []string{"enter"}, Label: "Continue", Enabled: true, Primary: true,
			Help: help,
		},
		{
			Keys: []string{"esc"}, Label: "Back", Enabled: canGoBack, Primary: true,
			DisabledReason: "this is the first question",
			Help:           "Return to the previous question. Your answers are kept.",
		},
	}
}

// recipeUnavailable says why an outcome cannot be delivered on this machine, or
// returns empty when it can.
//
// A recipe naming a required provider is answered from the snapshot: the provider
// is present, or it is not, and its own definition decides that. Writing the
// reason into the recipe made it a claim that had to be maintained by hand, and it
// was wrong the moment the provider became available.
func (m *WizardModel) recipeUnavailable(recipe wizardRecipe) string {
	if recipe.Unavailable != "" {
		return recipe.Unavailable
	}
	if recipe.RequiresProvider == "" {
		return ""
	}
	for _, provider := range m.caps.providers {
		if provider.ID != recipe.RequiresProvider {
			continue
		}
		if provider.Selectable {
			return ""
		}
		// The provider is known and not usable. Its own setup actions are the
		// specific answer — a missing binary, a missing credential — and are
		// better than anything this could say generically.
		if len(provider.SetupActions) > 0 {
			return provider.DisplayName + " needs setting up first: " +
				strings.Join(provider.SetupActions, "; ")
		}
		if provider.LastError != "" {
			return provider.DisplayName + " cannot be used: " + provider.LastError
		}
		return provider.DisplayName + " is installed but cannot be used yet. " +
			"The Setup screen says what it needs."
	}
	return "This needs the " + recipe.RequiresProvider + " provider, which is not installed. " +
		"The Setup screen lists what each provider needs."
}
