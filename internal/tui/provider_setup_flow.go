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
//
// Setup is a real screen: ScreenProviderSetup is pushed onto the navigation
// stack, so rendering, key handling and Help/Escape semantics are the same
// stack-driven rules every other screen follows. There is no global flag that
// pre-empts the keyboard while a different screen is rendered — that design is
// what let the wizard advertise "[s] Set up this provider" and then keep
// sending the user's keys to the wizard, and what let one Escape from the Help
// opened during setup tear the form down behind Help's back.

// providerSetupReturn records where setup was entered from, so every exit
// path — successful, cancelled, or reached through Help — knows the way back.
// The wizard needs more than a ScreenID: its answers live in the wizard model,
// which is preserved on the model, and finishing must resume it on the provider
// question with the recommendation recomputed.
type providerSetupReturn struct {
	Screen ScreenID
}

// beginProviderSetupForScreen starts provider setup for whatever the current
// screen has selected. The navigation stack records the caller: leaving setup
// — finished or cancelled — pops back to the screen it was opened over. From
// Setup that is the readiness screen itself, whose row for the provider now
// reads configured; from Providers it is the provider list.
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
// This is the whole of the setup handoff: the wizard is not torn down, its
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
	// The wizard stays intact. Setup is a child of it rather than a replacement
	// for it, which is what stops the answers being lost; resumeWizardAfterSetup
	// is what distinguishes "return to the provider question" from "return to
	// the caller screen" on every exit path.
	next.resumeWizardAfterSetup = providerID
	return next, cmd, true
}

// finishProviderSetup returns the user wherever they should go after a
// successful setup, and recomputes anything that depended on the old provider
// landscape.
func (m *Model) finishProviderSetup() tea.Cmd {
	providerID := m.resumeWizardAfterSetup
	m.resumeWizardAfterSetup = ""
	m.clearProviderSetup()
	m.clearProviderSetupNav()

	// The snapshot is refreshed either way: the provider's availability and its
	// accounts have changed, and every screen reads that from the snapshot
	// rather than keeping its own copy.
	cmds := []tea.Cmd{m.requestSnapshot()}

	if providerID != "" && m.wizard != nil {
		// Back to the wizard, on the provider question, with the recommendation
		// recomputed. ResumeAfterSetup reuses ProvidersChanged, so which provider
		// now suits the stated requirements remains the recommendation engine's
		// answer rather than a second copy of it here. Popping the stack is the
		// same return the non-wizard path takes: setup was pushed over the
		// wizard, so the entry below it is the wizard screen itself.
		m.clearProviderSetupNav()
		m.transitionTo(ScreenNewConnection)
		if cmd := m.wizard.ResumeAfterSetup(providerID, m.providerSnapshot()); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...)
	}

	// No wizard is waiting: return to wherever setup was entered from.
	m.returnFromProviderSetup()
	return tea.Batch(cmds...)
}

// cancelProviderSetup abandons the form: nothing collected persists, the
// provider simply remains unconfigured. The user is returned to the screen
// they came from — or, when the wizard sent them here, to the wizard's
// provider question with every answer intact. The resume path is consumed
// either way: a stale one would reroute a later, unrelated exit.
func (m *Model) cancelProviderSetup() tea.Cmd {
	resume := m.resumeWizardAfterSetup
	m.resumeWizardAfterSetup = ""
	if resume != "" && m.wizard != nil {
		m.clearProviderSetup()
		m.clearProviderSetupNav()
		m.transitionTo(ScreenNewConnection)
		return m.wizard.ResumeAfterSetup(resume, m.providerSnapshot())
	}
	m.clearProviderSetup()
	m.returnFromProviderSetup()
	return nil
}

// clearProviderSetupNav removes the pushed setup screen from the navigation
// stack without changing the current screen. The wizard return paths then
// transition to ScreenNewConnection explicitly, because the wizard resume —
// not the stack — decides what the user sees next.
func (m *Model) clearProviderSetupNav() {
	for i := len(m.navStack) - 1; i >= 0; i-- {
		if m.navStack[i] == ScreenProviderSetup {
			m.navStack = append(m.navStack[:i], m.navStack[i+1:]...)
		}
	}
	m.providerSetupCaller = providerSetupReturn{}
}

// returnFromProviderSetup navigates back to the caller screen. Setup sits on
// the navigation stack like any other screen, so the ordinary pop handles it;
// the recorded caller is the fallback for a stack that has already been
// unwound beneath the form.
func (m *Model) returnFromProviderSetup() {
	if m.screen == ScreenProviderSetup && !m.popScreen() {
		m.transitionTo(m.providerSetupCaller.Screen)
	}
	m.providerSetupCaller = providerSetupReturn{}
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
