package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Selection movement, and the screen-specific half of dispatch.

// moveSelection moves whatever the current screen selects.
//
// Each screen selects something different — a connection, a provider row, a
// discovered service, an operation, a setting — and the up/down cases each
// carried their own copy of that branch. One function keeps the movement in one
// place; the screens differ only in what they are moving through.
func (m *Model) moveSelection(delta int) {
	switch m.screen {
	case ScreenHome:
		conns := m.ConnectionList()
		if len(conns) == 0 {
			return
		}
		idx := m.selectedConnectionIndex()
		if idx < 0 {
			m.selectedID = conns[0].ID
			return
		}
		next := idx + delta
		if next >= 0 && next < len(conns) {
			m.selectedID = conns[next].ID
		}
	case ScreenProviders:
		m.moveCursor(delta)
	case ScreenDiscovery:
		m.discoverySelected = clampIndex(m.discoverySelected+delta, len(m.discovery))
	case ScreenOperations:
		m.opsSelectedIdx = clampIndex(m.opsSelectedIdx+delta, len(m.visibleOperations()))
	case ScreenRepair:
		m.diagnosticSelected = clampIndex(m.diagnosticSelected+delta, len(m.diagnostics))
	case ScreenSettings:
		if m.settings != nil {
			m.settings.cursor = clampIndex(m.settings.cursor+delta, len(m.settingsRows()))
		}
	case ScreenSetup:
		if m.setup != nil {
			if delta < 0 {
				m.setup.HandleKey("up")
			} else {
				m.setup.HandleKey("down")
			}
		}
	case ScreenInspect:
		// Inspect scrolls its content rather than selecting within it.
		m.scroll.scrollBy(delta)
	}
}

// clampIndex keeps an index inside a list, and returns zero for an empty one.
func clampIndex(index, length int) int {
	if length == 0 {
		return 0
	}
	if index < 0 {
		return 0
	}
	if index >= length {
		return length - 1
	}
	return index
}

// selectionChangedCmd is the work a new selection implies.
//
// Only the operations screen has any: the event journal it shows belongs to the
// selected operation, so moving the selection has to fetch a different one.
func (m *Model) selectionChangedCmd() tea.Cmd {
	if m.screen == ScreenOperations {
		return m.operationEventsForSelection()
	}
	return nil
}

// openInspect opens the detail screen for the selected connection.
func (m Model) openInspect() (Model, tea.Cmd, bool) {
	conn := m.SelectedConnection()
	if conn == nil {
		return m, nil, true
	}
	m.inspect = screens.NewInspect(conn)
	m.syncInspectModel()
	// Detail from the previous connection must not be shown against this one
	// while the fetch is in flight.
	m.connectionDetail = nil
	m.connectionLogs = nil
	m.telemetry = nil
	m.telemetryFor = ""
	m.telemetryUnavailable = ""
	m.pushScreen(ScreenInspect)
	return m, tea.Batch(
		m.connectionDetailCmd(conn.ID),
		m.connectionLogsCmd(conn.ID),
		m.telemetryCmd(conn.ID),
	), true
}

// openDiagnosis runs the diagnostic engine for the selected connection.
//
// Whether a kind can be diagnosed is decided by the action, which carries the
// reason. Reaching here means the action was enabled.
func (m Model) openDiagnosis() (Model, tea.Cmd, bool) {
	conn := m.SelectedConnection()
	if conn == nil {
		return m, nil, true
	}
	if m.screen == ScreenRepair {
		// Already here: this is a re-run, so previous findings must not be
		// compared against a diagnosis they did not produce.
		m.preRepairDiagnostics = nil
		m.repairConnectionID = ""
		m.diagnostics = nil
		m.diagnosticSelected = 0
		return m, m.diagnosticsCmd(conn.ID), true
	}
	m.pushScreen(ScreenRepair)
	m.diagnostics = nil
	m.diagnosticSelected = 0
	return m, m.diagnosticsCmd(conn.ID), true
}

// handleScreenAction runs the actions that mean something on one screen only.
func (m Model) handleScreenAction(id ActionID, act Action) (Model, tea.Cmd, bool) {
	switch id {
	case ActionConfirm:
		return m.confirmOnScreen()

	case ActionApply:
		return m.applyPreviewedPlan()

	case ActionPreview:
		if m.screen == ScreenRepair {
			conn := m.SelectedConnection()
			if conn == nil {
				return m, nil, true
			}
			// The findings the repair is judged against must be the ones it was
			// planned from.
			m.preRepairDiagnostics = append([]ipc.DiagnosticDTO(nil), m.diagnostics...)
			m.repairConnectionID = conn.ID
			return m, m.planRepairCmd(conn.ID), true
		}
		if m.screen == ScreenEdit {
			return m, m.planEditCmd(), true
		}
		return m, nil, true

	case ActionRefresh:
		return m.refreshOnScreen()

	case ActionLeft, ActionRight:
		if m.screen == ScreenInspect && m.inspect != nil {
			if id == ActionLeft {
				m.inspect.HandleKey("left")
			} else {
				m.inspect.HandleKey("right")
			}
			m.scroll.toTop()
			return m, m.inspectTabCmd(), true
		}
		return m, nil, true

	case ActionSupportExport:
		return m, m.supportExportCmd(), true

	case ActionLaunchMode:
		return m, m.setLaunchModeCmd(oppositeLaunchMode(m.currentLaunchMode())), true

	case ActionConfigureProvider:
		return m.beginProviderSetupForScreen()

	case ActionRemoveAccount:
		if row, ok := m.selectedAccount(); ok {
			m.accountRemovalTarget = row
			m.accountRemovalError = ""
			m.accountRemovalDependents = nil
			m.accountRemovalPreview = nil
			m.pushScreen(ScreenAccountRemoval)
			return m, m.previewAccountRemovalCmd(*row), true
		}
		return m, nil, true

	case ActionVerifyAccount:
		if row, ok := m.selectedAccount(); ok {
			return m, m.reverifyAccountCmd(*row), true
		}
		return m, nil, true

	case ActionReplaceCredential:
		if row, ok := m.selectedAccount(); ok {
			return m.beginCredentialReplacement(*row)
		}
		return m, nil, true

	case ActionShowMore:
		next := m.operationsLimit * 2
		if next <= 0 {
			next = 100
		}
		return m, m.loadOperationsLimitCmd(next), true

	case ActionFilter:
		return m.cycleFilter()

	case ActionFollowLogs:
		m.logsFollow = !m.logsFollow
		if m.logsFollow {
			if conn := m.SelectedConnection(); conn != nil {
				return m, m.connectionLogsCmd(conn.ID), true
			}
		}
		return m, nil, true

	case ActionEvidence:
		m.discoveryEvidence = !m.discoveryEvidence
		return m, nil, true

	case ActionManualEntry:
		return m.startManualAddress()

	case ActionDiscard:
		if m.screen == ScreenEdit {
			m.edit = nil
			m.abandonScreenWork()
			if !m.popScreen() {
				m.transitionTo(ScreenHome)
			}
		}
		return m, nil, true

	case ActionKeepEditing:
		if m.edit != nil {
			m.edit.confirmingDiscard = false
		}
		return m, nil, true
	}
	_ = act
	return m, nil, false
}
