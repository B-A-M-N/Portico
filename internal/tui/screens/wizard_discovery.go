package screens

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Finding the service, inside the wizard.
//
// Publishing something already running required knowing that Home had a separate
// discovery shortcut, using it first, and choosing from there — otherwise the
// wizard asked for an address and the user had to go and find one. The wizard is
// where someone says what they want reachable, so that is where the question
// belongs.
//
// This calls the same discovery the Home screen calls, through the same DTOs.
// There is no second implementation: the supervisor scans, and both surfaces
// render what it found.

// ServiceDiscoverer is the discovery half of the wizard's client.
//
// It is separate from ConnectionCreator so a test can supply one without the
// other, and so the dependency the wizard has on discovery is visible in its
// type rather than hidden inside a method.
type ServiceDiscoverer interface {
	Discovery(ctx context.Context) (*ipc.DiscoveryDTO, error)
	RefreshDiscovery(ctx context.Context) (*ipc.DiscoveryDTO, error)
}

// WizardDiscoveryMsg carries the result of a scan started by the wizard.
type WizardDiscoveryMsg struct {
	WizardID   WizardID
	Generation WizardGeneration
	Services   []ipc.DiscoveredServiceDTO
	Err        error
}

// discoverCmd scans for local services.
//
// RefreshDiscovery is the explicit, user-initiated scan: a service started since
// the last cached scan must be found, because the user is looking at the wizard
// precisely because they just started something.
func (m *WizardModel) discoverCmd() tea.Cmd {
	discoverer, ok := m.client.(ServiceDiscoverer)
	if !ok {
		return nil
	}
	ctx := m.ctx
	wizID, gen := m.id, m.generation
	m.discoverPending = true
	m.discoverErr = nil
	return func() tea.Msg {
		reply := WizardDiscoveryMsg{WizardID: wizID, Generation: gen}
		scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		result, err := discoverer.RefreshDiscovery(scanCtx)
		if err != nil {
			reply.Err = err
			return reply
		}
		if result != nil {
			reply.Services = result.Services
		}
		return reply
	}
}

// HandleDiscovery applies a scan result, discarding a reply for a wizard the user
// has abandoned or an answer to a question they have moved past.
func (m *WizardModel) HandleDiscovery(msg WizardDiscoveryMsg) {
	if msg.WizardID != m.id || msg.Generation != m.generation {
		return
	}
	m.discoverPending = false
	if msg.Err != nil {
		// A failed scan is a failed scan. Rendering it as an empty list would
		// tell the user nothing is running, which Portico did not establish.
		m.discoverErr = msg.Err
		m.discovered = nil
		return
	}
	m.discoverErr = nil
	m.discovered = msg.Services
	m.selected = 0
}

// discoveryChoiceList is the current options for the service question.
func (m *WizardModel) discoveryChoiceList() []wizardChoice {
	return discoveryChoices(m.discovered)
}

// renderDiscovery draws the service question.
func (m *WizardModel) renderDiscovery() string {
	if m.discoverPending {
		return "Looking for services running on this machine..."
	}

	title := "Which service should be reachable?"
	if len(m.discovered) == 0 {
		title = "Portico did not find anything listening."
	}

	out := renderChoices(title, m.discoveryChoiceList(), m.selected)
	if m.discoverErr != nil {
		out += "\n" + "The scan itself failed, so this is not a statement that nothing is running: " +
			m.discoverErr.Error() + "\n"
	} else if len(m.discovered) == 0 {
		out += "\nA service Portico cannot see is still a service — it may be listening only on " +
			"an interface the scan does not cover, or it may not be started yet. " +
			"Scan again with r, or enter the address yourself.\n"
	}
	return out
}

// handleDiscoveryKey answers the service question.
func (m *WizardModel) handleDiscoveryKey(key string) tea.Cmd {
	choices := m.discoveryChoiceList()
	switch key {
	case "esc":
		m.goBack()
	case "r":
		// Scanning again is offered whether or not anything was found: the user
		// may have started the service after the first scan.
		return m.discoverCmd()
	case "up", "k":
		if m.selected > 0 {
			m.selected--
		}
	case "down", "j":
		if m.selected < len(choices)-1 {
			m.selected++
		}
	case "enter":
		choice, ok := choiceAt(choices, m.selected)
		if !ok {
			return nil
		}
		if choice.Value == manualAddressChoice {
			// The address question, with nothing filled in.
			m.err = nil
			m.state.SourceAddress = ""
			m.state.Step = WizardStepSource
			m.setInput("")
			return nil
		}
		// The discovered address and protocol are taken exactly as found, so a
		// scanned host:port is never rewritten.
		m.err = nil
		m.state.SourceAddress = choice.Value
		if svc, found := m.discoveredByAddress(choice.Value); found {
			if svc.Protocol != "" {
				m.state.SourceProtocol = svc.Protocol
			}
		}
		m.advanceFromSource()
	}
	return nil
}

// discoveredByAddress finds the service behind a chosen address.
func (m *WizardModel) discoveredByAddress(address string) (ipc.DiscoveredServiceDTO, bool) {
	for _, svc := range m.discovered {
		if svc.Address == address {
			return svc, true
		}
	}
	return ipc.DiscoveredServiceDTO{}, false
}

// discoveryActions describes the service question.
func (m *WizardModel) discoveryActions() WizardActions {
	has := len(m.discovered) > 0
	return WizardActions{
		{
			Keys: []string{"up", "k"}, Label: "Up", Enabled: has,
			DisabledReason: "nothing was found to choose between",
			Help:           "Move up the list of services.",
		},
		{
			Keys: []string{"down", "j"}, Label: "Down", Enabled: true,
			Help: "Move down the list. Entering an address yourself is the last option.",
		},
		{
			Keys: []string{"enter"}, Label: "Use this one", Enabled: true, Primary: true,
			Help: "Publish the highlighted service, or open the address field if you chose " +
				"to enter one yourself.",
		},
		{
			Keys: []string{"r"}, Label: "Scan again", Enabled: !m.discoverPending,
			DisabledReason: "a scan is already running", Primary: true,
			Help: "Look again. Something started since the last scan will be found now.",
		},
		{
			Keys: []string{"esc"}, Label: "Back", Enabled: true, Primary: true,
			Help: "Return to the previous question. Your answers are kept.",
		},
		{
			Label: "Why this one?", Enabled: true,
			Help: "The highlighted service expands to show what Portico probed and what " +
				"answered, so you can judge the identification yourself.",
		},
	}
}

// ensureDiscoveryStarted asks for a scan the first time the question is reached.
func (m *WizardModel) ensureDiscoveryStarted() tea.Cmd {
	if m.discovered != nil || m.discoverPending || m.discoverErr != nil {
		return nil
	}
	return m.discoverCmd()
}

var _ = fmt.Sprintf
