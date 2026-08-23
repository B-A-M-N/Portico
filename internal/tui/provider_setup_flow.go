package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Provider setup, reached from wherever the user hit the need for it.
//
// The same setup form is opened from the Providers screen, from the Setup
// readiness screen, and from inside the wizard's provider question. It is one
// form: the provider's own declared setup flow. What differs is only where the
// user came from and where they should be returned to.

// beginProviderSetupForScreen starts provider setup for whatever the current
// screen has selected.
func (m Model) beginProviderSetupForScreen() (Model, tea.Cmd, bool) {
	switch m.screen {
	case ScreenProviders:
		next, cmd := m.beginProviderSetup(m.selectedProviderID())
		return next, cmd, true
	case ScreenSetup:
		if m.setup == nil {
			return m, nil, true
		}
		selected := m.setup.Selected()
		if selected == nil {
			return m, nil, true
		}
		next, cmd := m.beginProviderSetup(selected.ID)
		// The form is shown over the providers screen, so leaving it returns
		// somewhere that lists what was just configured.
		next.pushScreen(ScreenProviders)
		return next, cmd, true
	case ScreenNewConnection:
		// Handled by the wizard, which records the request; see
		// wizardProviderSetupCmd.
		return m, nil, true
	}
	return m, nil, true
}

// wizardProviderSetupCmd starts provider setup for a provider the wizard asked
// to configure, remembering to return to the wizard afterwards.
//
// This is the whole of item 9's interaction: the wizard is not torn down, its
// answers are untouched, and the provider question is re-asked against the new
// provider landscape when setup finishes.
func (m Model) wizardProviderSetupCmd() (Model, tea.Cmd, bool) {
	if m.wizard == nil {
		return m, nil, false
	}
	providerID, ok := m.wizard.PendingProviderSetup()
	if !ok {
		return m, nil, false
	}
	next, cmd := m.beginProviderSetup(providerID)
	// The wizard stays on the stack. Setup is a child of it, not a replacement
	// for it, which is what stops the answers being lost.
	next.resumeWizardAfterSetup = providerID
	return next, cmd, true
}

// finishProviderSetup returns the user wherever they should go after a
// successful setup, and recomputes anything that depended on the old provider
// landscape.
func (m *Model) finishProviderSetup() tea.Cmd {
	providerID := m.resumeWizardAfterSetup
	m.resumeWizardAfterSetup = ""
	m.providerSetupStep = 0
	m.clearProviderSetupSecret()

	// The snapshot is refreshed either way: the provider's availability and its
	// accounts have changed, and every screen reads that from the snapshot
	// rather than keeping its own copy.
	cmds := []tea.Cmd{m.requestSnapshot()}

	if providerID != "" && m.wizard != nil {
		// Back to the wizard, on the provider question, with the recommendation
		// recomputed. ResumeAfterSetup reuses ProvidersChanged, so which provider
		// now suits the stated requirements remains the recommendation engine's
		// answer rather than a second copy of it here.
		m.transitionTo(ScreenNewConnection)
		if cmd := m.wizard.ResumeAfterSetup(providerID, m.providerSnapshot()); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// startManualAddress lets the user type an address instead of choosing from the
// discovered list.
//
// Discovery finding nothing was previously a dead end: the screen said nothing
// was found and offered no way forward, so a user with a service on a port the
// scan could not see had to leave and start again from the wizard.
func (m Model) startManualAddress() (Model, tea.Cmd, bool) {
	m.wizard = screens.NewWizardForManualAddress(m.client, m.providerSnapshot()).
		WithContext(m.rootCtx).WithDefaults(m.lifecycleDefaults())
	m.status = ""
	m.pushScreen(ScreenNewConnection)
	return m, nil, true
}
