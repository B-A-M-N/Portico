package screens

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Making something reachable on a private network.
//
// The kind has two modes and they answer different questions, so the wizard asks which
// before asking anything else:
//
//   - joining records that this machine is reachable by the other devices on the network.
//     Nothing local is published, and there is nothing for Portico to create.
//   - publishing makes one service on this machine reachable to those devices, and only
//     to those devices.
//
// Neither produces a public address. That is the whole point of the kind, and the review
// says so rather than leaving the user to infer it from the absence of one.

// handlePrivateNetworkModeKey asks which of the two things the user wants.
func (m *WizardModel) handlePrivateNetworkModeKey(key string) tea.Cmd {
	choices := privateNetworkModeChoices()
	switch key {
	case "esc":
		m.goBack()
	case "up", "k":
		if m.selected > 0 {
			m.selected--
		}
	case "down", "j":
		if m.selected < len(choices)-1 {
			m.selected++
		}
	case "enter":
		if m.selected < 0 || m.selected >= len(choices) {
			return nil
		}
		m.err = nil
		m.state.PrivateNetworkMode = choices[m.selected].Value
		if m.state.PrivateNetworkMode == "expose" {
			m.state.Step = WizardStepPrivateNetworkAddress
			m.setInput(m.state.SourceAddress)
			return nil
		}
		// A join publishes nothing, so there is no address to ask for.
		m.state.Provider = "tailscale"
		m.state.Step = WizardStepReview
		m.selected = 0
	}
	return nil
}

// handlePrivateNetworkAddressKey collects the address to publish.
func (m *WizardModel) handlePrivateNetworkAddressKey(key string) tea.Cmd {
	switch key {
	case "esc":
		m.goBack()
	case "enter":
		address := strings.TrimSpace(m.inputValue())
		if address == "" {
			m.err = fmt.Errorf(
				"publishing a service needs the address it is listening on, such as " +
					"127.0.0.1:3000")
			return nil
		}
		if !strings.Contains(address, ":") {
			m.err = fmt.Errorf(
				"%q has no port; the network needs to know which port to reach", address)
			return nil
		}
		m.err = nil
		m.state.SourceAddress = address
		m.state.Provider = "tailscale"
		m.state.Step = WizardStepReview
		m.selected = 0
	default:
		m.setInput(editInput(m.inputValue(), key))
	}
	return nil
}

// privateNetworkModeChoices are the two things Portico can do with a private network.
//
// Nothing else Tailscale offers is listed. Exit nodes and subnet routes are provider
// features rather than answers to a question Portico's model asks, and a funnel would
// publish to the internet — which is a public exposure, and belongs to a different kind.
func privateNetworkModeChoices() []wizardChoice {
	return []wizardChoice{
		{
			Value: "join", Label: "Let other devices reach this machine", Available: true,
			Detail: []string{
				"This machine is already on your private network. Portico records the",
				"connection and watches that it stays reachable.",
				"Nothing on this machine is published, and nothing is created.",
			},
		},
		{
			Value: "expose", Label: "Publish one service to the network", Available: true,
			Detail: []string{
				"A service running here becomes reachable by the other devices on your",
				"network, and only by them. There is no public address.",
			},
		},
	}
}

// renderPrivateNetworkMode draws the mode question.
func (m *WizardModel) renderPrivateNetworkMode() string {
	return renderChoices("What should the private network do?",
		privateNetworkModeChoices(), m.selected)
}

// privateNetworkReview describes what a private-network connection will do.
func (m *WizardModel) privateNetworkReview() []reviewSection {
	if m.state.PrivateNetworkMode == "expose" {
		return []reviewSection{
			{title: "What will be reachable", lines: []string{
				"The service at " + m.state.SourceAddress + " on this machine.",
			}},
			{title: "How it will be reached", lines: []string{
				"Through your private network. Portico asks the network's client to publish",
				"that address to it. There is no public address and nothing on this machine",
				"listens for the internet.",
			}},
			{title: "Who can reach it", lines: []string{
				"Only devices on your private network. Membership is managed by the network,",
				"not by Portico.",
			}},
			{title: "What Portico will create and manage", lines: []string{
				"The published address, and nothing else. Portico did not put this machine on",
				"the network and will not take it off.",
			}},
			{title: "When you close it", lines: []string{
				"The service stops being published. This machine stays on the network, and",
				"anything else you published by hand is left alone.",
			}},
			{title: "Startup and quitting", lines: m.lifecycleLines()},
		}
	}

	return []reviewSection{
		{title: "What will be reachable", lines: []string{
			"This machine, to the other devices on your private network.",
		}},
		{title: "How it will be reached", lines: []string{
			"It is already on the network. Portico records the connection and watches that",
			"it stays reachable; it does not sign the machine in.",
		}},
		{title: "Who can reach it", lines: []string{
			"Only devices on your private network. There is no public address, and nothing",
			"on this machine becomes reachable from the internet.",
		}},
		{title: "What Portico will create and manage", lines: []string{
			"Nothing. Being on the network is a setting for the whole machine, and it was",
			"there before this connection.",
		}},
		{title: "When you close it", lines: []string{
			"Portico stops tracking the connection. The machine stays on the network —",
			"Portico did not sign it in and will not sign it out.",
		}},
		{title: "Startup and quitting", lines: m.lifecycleLines()},
	}
}

// privateNetworkModeIndex is the cursor position for a mode already chosen, so returning
// to the question shows what was picked rather than resetting to the first option.
func privateNetworkModeIndex(mode string) int {
	for i, choice := range privateNetworkModeChoices() {
		if choice.Value == mode {
			return i
		}
	}
	return 0
}

// privateNetworkModeActions describes the mode question.
func (m *WizardModel) privateNetworkModeActions() WizardActions {
	_, canGoBack := m.previousStep()
	return WizardActions{
		{
			Keys: []string{"up", "k"}, Label: "Up", Enabled: true,
			Help: "Move between letting the network reach this machine and publishing one " +
				"service to it.",
		},
		{
			Keys: []string{"down", "j"}, Label: "Down", Enabled: true,
			Help: "Move between letting the network reach this machine and publishing one " +
				"service to it.",
		},
		{
			Keys: []string{"enter"}, Label: "Choose", Enabled: true, Primary: true,
			Help: "Take the highlighted option. Neither one creates a public address.",
		},
		{
			Keys: []string{"esc"}, Label: "Back", Enabled: canGoBack, Primary: true,
			DisabledReason: "this is the first question",
			Help:           "Return to the previous question. Your answers are kept.",
		},
	}
}
