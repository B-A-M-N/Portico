package screens

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// ServiceDiscoverer is the discovery half of the wizard's client.
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

// HandleDiscovery applies a scan result.
func (m *WizardModel) HandleDiscovery(msg WizardDiscoveryMsg) {
	if msg.WizardID != m.id || msg.Generation != m.generation {
		return
	}
	m.discoverPending = false
	if msg.Err != nil {
		m.discoverErr = msg.Err
		// The list is cleared but nil, not empty: nil plus a set discoverErr
		// is "the scan failed", whereas an empty list means "scanned, found
		// nothing". Only the second may produce an empty-machine claim.
		m.discovered = nil
		return
	}
	m.discoverErr = nil
	m.discovered = msg.Services
	m.selected = 0
	m.menuViewport.Cursor = 0
	m.menuViewport.Offset = 0
	m.menuViewport.ensureVisible(m.menuChoiceCount())
}

// discoveryChoiceList returns filtered choices for the service question.
//
// It is discoveryChoices with the wizard's filter and show-all state applied —
// the established contract, not a second implementation. Labels are the
// canonical one-liners; the evidence already identifies protocol and process,
// so a label that repeated them said everything twice.
func (m *WizardModel) discoveryChoiceList() []wizardChoice {
	services := m.discovered
	if services == nil {
		services = []ipc.DiscoveredServiceDTO{}
	}
	if !m.discoveryShowAll {
		services = VisibleDiscoveryServices(services, false)
	}
	if m.discoveryFilter != "" {
		filtered := make([]ipc.DiscoveredServiceDTO, 0, len(services))
		f := strings.ToLower(m.discoveryFilter)
		for _, svc := range services {
			label := DiscoveryChoiceLabel(svc)
			if strings.Contains(strings.ToLower(label), f) {
				filtered = append(filtered, svc)
			}
		}
		services = filtered
	}
	return discoveryChoices(services)
}

// renderDiscovery draws the service question through the one menu renderer.
func (m *WizardModel) renderDiscovery() string {
	if m.discoverPending {
		width := m.contentWidth()
		return strings.Join(WrapText("Looking for services running on this machine...", width), "\n")
	}
	return m.renderDiscoveryReady()
}

// renderDiscoveryReady renders the scan result.
//
// A failed scan says the scan failed and stops there. Basing "did not find
// anything listening" on an empty list made a timeout claim the machine was
// idle — something the scan never established — while the error line below
// contradicted it.
func (m *WizardModel) renderDiscoveryReady() string {
	width := m.contentWidth()
	choices := m.discoveryChoiceList()
	title := "Which service should be reachable?"
	if m.discoverErr != nil {
		// Branch on the failure first: an error must never share a screen
		// with, let alone be overridden by, an empty-machine claim.
		title = "The scan itself failed, so Portico does not know what is listening. " +
			"It did not establish that nothing is running."
	} else if len(m.discovered) == 0 {
		title = "Portico did not find anything listening."
	}

	// One renderer owns the menu: clipping, selection visibility, section
	// headers, the above/below indicator and detail budgeting all live in
	// renderChoiceWindow. Discovery carried a second, hand-rolled copy of
	// that logic, and the copies had already drifted.
	body := m.renderChoiceWindow(title, choices, m.selected)

	var trailer []string
	if m.discoverErr != nil {
		trailer = []string{
			"",
			wizardOneLine("Scan failed: "+m.discoverErr.Error(), width),
			wizardOneLine("Scan again with r, or enter the address yourself.", width),
		}
	} else if len(m.discovered) == 0 {
		trailer = []string{"", wizardOneLine("Scan again with r, or enter the address yourself.", width)}
	}
	trailer = append(trailer, wizardOneLine(m.discoveryFooter(choices), width))
	return body + "\n" + strings.Join(trailer, "\n")
}

// handleDiscoveryKey answers the service question with viewport navigation.
// Enter enforces the same availability the display shows: a choice marked
// unavailable is refused with its reason, never silently substituted.
func (m *WizardModel) handleDiscoveryKey(key string) tea.Cmd {
	choices := m.discoveryChoiceList()
	switch key {
	case "esc":
		m.goBack()
	case "r":
		return m.discoverCmd()
	case "up", "k":
		m.menuViewport.Move(-1, len(choices))
		m.selected = m.menuViewport.Cursor
	case "down", "j":
		m.menuViewport.Move(1, len(choices))
		m.selected = m.menuViewport.Cursor
	case "pgup":
		m.menuViewport.Page(-1, len(choices))
		m.selected = m.menuViewport.Cursor
	case "pgdown":
		m.menuViewport.Page(1, len(choices))
		m.selected = m.menuViewport.Cursor
	case "home":
		m.menuViewport.First(len(choices))
		m.selected = m.menuViewport.Cursor
	case "end":
		m.menuViewport.Last(len(choices))
		m.selected = m.menuViewport.Cursor
	case "m":
		m.err = nil
		m.state.SourceAddress = ""
		m.state.Step = WizardStepSource
		m.setInput("")
		return nil
	case "f":
		m.discoveryFilter = ""
		m.discoveryFilterField.Focus()
		m.state.Step = WizardStepDiscoveryFilter
		m.setInput("")
		return nil
	case "a":
		m.discoveryShowAll = !m.discoveryShowAll
		choices = m.discoveryChoiceList()
		m.menuViewport.Cursor = m.selected
		m.menuViewport.ensureVisible(len(choices))
		m.selected = m.menuViewport.Cursor
	case "enter":
		choice, ok := choiceAt(choices, m.selected)
		if !ok {
			return nil
		}
		if !choice.Available {
			// Display state and behaviour agree: the reason the row shows is
			// the reason the press is refused.
			if choice.Reason != "" {
				m.err = errors.New(choice.Reason)
			}
			return nil
		}
		if choice.Value == manualAddressChoice {
			m.err = nil
			m.state.SourceAddress = ""
			m.state.Step = WizardStepSource
			m.setInput("")
			return nil
		}
		m.err = nil
		m.state.SourceAddress = choice.Value
		if svc, found := m.discoveredByAddress(choice.Value); found {
			if svc.Protocol != "" {
				m.state.SourceProtocol = svc.Protocol
			}
			// The whole service object is preserved, not just its address:
			// the process name suggests a connection name, the confidence and
			// evidence explain the choice in review, and the classification
			// records which answers came from discovery rather than typing.
			svc := svc
			m.state.SelectedService = &svc
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

// discoveryFooter renders a concise footer.
func (m *WizardModel) discoveryFooter(choices []wizardChoice) string {
	keys := []string{}
	if len(choices) > 1 {
		keys = append(keys, "↑↓ Navigate")
	}
	keys = append(keys, "Enter Select", "Esc Back", "PgUp/PgDn Page", "Home/End First/Last", "m Manual", "f Filter", "a Show all")
	return strings.Join(keys, "  ")
}

// ensureDiscoveryStarted asks for a scan the first time the question is reached.
func (m *WizardModel) ensureDiscoveryStarted() tea.Cmd {
	if m.discovered != nil || m.discoverPending || m.discoverErr != nil {
		return nil
	}
	return m.discoverCmd()
}

func (m *WizardModel) discoveryActions() WizardActions {
	has := len(m.discovered) > 0
	return WizardActions{
		{Keys: []string{"up", "k"}, Label: "Up", Enabled: has,
			DisabledReason: "nothing was found to choose between",
			Help:           "Move up the list of services."},
		{Keys: []string{"down", "j"}, Label: "Down", Enabled: true,
			Help: "Move down the list."},
		{Keys: []string{"enter"}, Label: "Use this one", Enabled: true, Primary: true,
			Help: "Publish the highlighted service."},
		{Keys: []string{"r"}, Label: "Scan again", Enabled: !m.discoverPending,
			DisabledReason: "a scan is already running", Primary: true,
			Help: "Look again for services."},
		{Keys: []string{"esc"}, Label: "Back", Enabled: true, Primary: true,
			Help: "Return to the previous question. Your answers are kept."},
	}
}
