package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Dispatch, from the same description the footer and Help read.
//
// Keys were handled by one long switch on the key, each case then asking which
// screen it was on. The screen's capabilities were therefore spread across a
// dozen unrelated cases, and the footer and Help each restated them from
// memory. That is how Inspect came to execute Space, Repair and Delete without
// advertising any of them, and how `q` came to navigate Home on screens whose
// footer said it would quit.
//
// dispatchAction runs the action a key names on the current screen, and only if
// that action is enabled. An action that is not advertised is not dispatched,
// which makes the footer a description of behaviour rather than a caption.

// handleAction executes one identified action.
//
// It is the only place a screen's action turns into a command, so an action
// advertised on two screens runs the same code on both. Actions the screen's own
// handler owns — the wizard's questions, the edit fields — never reach here:
// those screens take the keyboard first.
func (m Model) handleAction(id ActionID, act Action) (Model, tea.Cmd, bool) {
	switch id {
	case ActionQuit:
		if m.rootCancel != nil {
			m.rootCancel()
		}
		m.clearProviderSetupSecret()
		return m, tea.Quit, true

	case ActionHelp:
		// Pressing ? on the help screen recorded help as the screen to return
		// to, which stranded the user there.
		if m.screen == ScreenHelp {
			return m, nil, true
		}
		m.prevScreen = m.screen
		m.transitionTo(ScreenHelp)
		return m, nil, true

	case ActionBack:
		if m.screen == ScreenSettings && m.settings != nil && m.settings.confirmingRotate {
			// Esc while confirming is the refusal: no key material moves, and
			// the prompt simply goes away.
			m.settings.confirmingRotate = false
			return m, nil, true
		}
		return m.goBack()

	case ActionUp:
		m.moveSelection(-1)
		return m, m.selectionChangedCmd(), true

	case ActionDown:
		m.moveSelection(1)
		return m, m.selectionChangedCmd(), true

	case ActionPageUp, ActionPageDown, ActionTop, ActionBottom:
		// A screen that steers a selection moves the selection with these
		// keys; the view follows it. Free-scrolling the viewport away from the
		// cursor left the user paging content they could no longer act on.
		if m.screenHasSelection() {
			m.moveSelectionByPage(id)
			return m, m.selectionChangedCmd(), true
		}
		switch id {
		case ActionPageUp:
			m.scroll.scrollBy(-m.scroll.page())
		case ActionPageDown:
			m.scroll.scrollBy(m.scroll.page())
		case ActionTop:
			m.scroll.toTop()
		case ActionBottom:
			m.scroll.toBottom()
		}
		return m, nil, true

	case ActionInspect:
		return m.openInspect()

	case ActionToggleOpen:
		conn := m.SelectedConnection()
		if conn == nil {
			return m, nil, true
		}
		if conn.DesiredState == "open" {
			return m, m.planCloseCmd(conn.ID), true
		}
		return m, m.planOpenCmd(conn.ID), true

	case ActionNew:
		m.wizard = screens.NewWizard(m.client, m.providerSnapshot()).
			WithContext(m.rootCtx).WithDefaults(m.lifecycleDefaults()).WithASCII(m.useASCII)
		m.status = ""
		m.pushScreen(ScreenNewConnection)
		return m, nil, true

	case ActionEdit:
		if conn := m.SelectedConnection(); conn != nil {
			next, cmd := m.beginEdit(conn.ID)
			return next, cmd, true
		}
		return m, nil, true

	case ActionCopy:
		if conn := m.SelectedConnection(); conn != nil {
			next, cmd := m.beginClone(conn.ID, conn.Name)
			return next, cmd, true
		}
		return m, nil, true

	case ActionDelete:
		if conn := m.SelectedConnection(); conn != nil {
			return m, m.planDeleteCmd(conn.ID), true
		}
		return m, nil, true

	case ActionRepair:
		return m.openDiagnosis()

	case ActionDiscover:
		m.status = "Discovering local services..."
		return m, m.discoveryCmd(), true

	case ActionOperations:
		m.pushScreen(ScreenOperations)
		m.operationEvents = nil
		m.operationEventsFor = ""
		m.operationEventsFailed = ""
		return m, m.loadOperationsCmd(), true

	case ActionProviders:
		m.pushScreen(ScreenProviders)
		return m, nil, true

	case ActionSetup:
		if m.setup == nil {
			m.setup = screens.NewSetup()
		}
		m.pushScreen(ScreenSetup)
		return m, m.readinessCmd(), true

	case ActionSettings:
		m.pushScreen(ScreenSettings)
		if m.settings == nil {
			m.settings = &settingsState{}
		}
		return m, m.loadSettingsCmd(), true

	case ActionRotateSecretKey:
		if m.settings != nil && !m.settings.confirmingRotate {
			m.settings.confirmingRotate = true
			return m, nil, true
		}
		return m, m.rotateSecretKeyCmd(), true

	case ActionRetry:
		// The whole bring-up sequence, not a bare snapshot retry against an
		// unchanged dead supervisor.
		m.err = nil
		m.status = ""
		m.transitionTo(ScreenBoot)
		return m, m.bootstrapCmd(), true
	}
	return m.handleScreenAction(id, act)
}

// goBack leaves the current screen.
//
// Esc means back or cancel everywhere. A screen with unsaved work asks before
// discarding it rather than losing it to one keystroke.
func (m Model) goBack() (Model, tea.Cmd, bool) {
	if m.screen == ScreenHelp {
		m.screen = m.prevScreen
		m.prevScreen = ""
		return m, nil, true
	}
	if m.screen == ScreenHome {
		return m, nil, true
	}
	if m.screen == ScreenEdit && m.edit != nil && m.edit.dirty() {
		// An edit is an accumulation of decisions. Discarding it silently for
		// one keystroke throws away exactly the work the screen exists to
		// collect.
		m.edit.confirmingDiscard = true
		return m, nil, true
	}
	// Applying a plan clears its preview before entering operation progress.
	// If the completed operation is later closed, do not navigate back into
	// that now-empty preview; remove the stale frame and return to the screen
	// from which the plan was opened.
	if m.screen == ScreenOperationProgress && m.plan == nil &&
		m.operation != nil && ipc.OperationTerminal(m.operation.State) {
		for len(m.navStack) > 0 && m.navStack[len(m.navStack)-1] == ScreenPlanPreview {
			m.navStack = m.navStack[:len(m.navStack)-1]
		}
	}
	m.abandonScreenWork()
	if !m.popScreen() {
		m.transitionTo(ScreenHome)
	}
	return m, nil, true
}
