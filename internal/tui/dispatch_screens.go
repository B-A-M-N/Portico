package tui

import (
	"strings"

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
//
// Every branch ends by following the selection into view. Moving a selection
// without moving the scroll left the highlighted row below the clipped
// viewport: the user was steering something they could not see.
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
	default:
		count := m.selectionCount()
		if count == 0 {
			return
		}
		m.setSelectionIndex(clampIndex(m.selectionIndex()+delta, count))
		m.followSelectionIntoView()
	}
}

// moveSelectionByPage moves the selection the way the page and jump keys
// describe: a screenful, or to an end of the list.
//
// On a screen with a selection these keys moved only the free scroll, so End
// jumped the viewport away from a cursor that stayed at row zero — the user
// was suddenly steering something they could no longer see. Where a selection
// exists it is the source of truth, and the view follows it.
func (m *Model) moveSelectionByPage(id ActionID) {
	count := m.selectionCount()
	if count == 0 {
		return
	}
	next := m.selectionIndex()
	switch id {
	case ActionPageUp:
		next -= m.scroll.page()
	case ActionPageDown:
		next += m.scroll.page()
	case ActionTop:
		next = 0
	case ActionBottom:
		next = count - 1
	default:
		return
	}
	m.setSelectionIndex(clampIndex(next, count))
	m.followSelectionIntoView()
}

// screenHasSelection reports whether the current screen steers a cursor, as
// opposed to scrolling free-form content. The page and jump keys mean
// different things on the two kinds of screen.
func (m *Model) screenHasSelection() bool {
	switch m.screen {
	case ScreenDiscovery, ScreenOperations, ScreenRepair, ScreenSettings, ScreenProviders, ScreenSetup:
		return true
	default:
		return false
	}
}

// selectionCount is how many rows the current screen can select.
func (m *Model) selectionCount() int {
	switch m.screen {
	case ScreenDiscovery:
		return len(m.discovery)
	case ScreenOperations:
		return len(m.visibleOperations())
	case ScreenRepair:
		return len(m.diagnostics)
	case ScreenSettings:
		if m.settings != nil {
			return len(m.settingsRows())
		}
	case ScreenSetup:
		if m.setup != nil {
			return m.setup.Rows()
		}
	case ScreenProviders:
		return len(m.buildScreenRows())
	}
	return 0
}

// selectionIndex is which row the current screen has selected.
func (m *Model) selectionIndex() int {
	switch m.screen {
	case ScreenDiscovery:
		return m.discoverySelected
	case ScreenOperations:
		return m.opsSelectedIdx
	case ScreenRepair:
		return m.diagnosticSelected
	case ScreenSettings:
		if m.settings != nil {
			return m.settings.cursor
		}
	case ScreenSetup:
		if m.setup != nil {
			return m.setup.SelectedRow()
		}
	case ScreenProviders:
		return m.cursorIndex
	}
	return 0
}

// setSelectionIndex names the selected row. The caller clamps; the index
// given is within the list the screen renders.
func (m *Model) setSelectionIndex(index int) {
	switch m.screen {
	case ScreenDiscovery:
		m.discoverySelected = index
	case ScreenOperations:
		m.opsSelectedIdx = index
	case ScreenRepair:
		m.diagnosticSelected = index
	case ScreenSettings:
		if m.settings != nil {
			m.settings.cursor = index
		}
	case ScreenSetup:
		if m.setup != nil {
			m.setup.SetSelectedRow(index)
		}
	case ScreenProviders:
		m.cursorIndex = index
	}
}

// followSelectionIntoView scrolls the free viewport so the screen's selected
// row is physically visible.
//
// The scroll offset and the selection were two independent pieces of state, and
// nothing connected them: a list longer than the screen let Down move the
// cursor past the last rendered row, where the selection still existed but was
// drawn nowhere. This measures the rendered screen and walks to the row that
// carries the selection marker, which keeps one mechanism (scroll.go) owning
// the offset while the selection drives where it must land.
func (m *Model) followSelectionIntoView() {
	if m.height <= 0 {
		return
	}
	content := m.renderScreen()
	lines := strings.Split(content, "\n")
	m.scroll.setContentLines(len(lines))

	// The providers screen draws its selected ACCOUNT row with the same
	// marker family as the provider row ("> "), so one scan finds either.
	// Account rows previously drew only "▸", which this scan never matched —
	// so moving the cursor onto an account left the follow-scroll blind and
	// the selection could sit below the visible window while the screen
	// showed it nowhere. The LAST marked line wins: a screen renders at most
	// one selection marker, so the scan tolerates provider rows that quote
	// "> " inside help text above the selection.
	marker := "> "
	row := -1
	for i, line := range lines {
		if strings.Contains(line, marker) {
			row = i
		}
	}
	if row < 0 {
		return
	}
	visible := m.scroll.visibleLines()
	if visible <= 0 {
		return
	} // Back at the first row, show the screen from its top: the header the
	// list sits under belongs above the selection, and a viewport parked
	// partway down hides it.
	if m.selectionIndex() == 0 {
		m.scroll.offset = 0
		m.scroll.clamp()
		return
	}
	// Keep the selected row inside [offset, offset+visible). A selection
	// above the window pulls the offset up to it; below, down to it. That
	// is the whole contract: after any move, the row the selection names
	// is physically on screen.
	if row < m.scroll.offset {
		m.scroll.offset = row
	} else if row >= m.scroll.offset+visible {
		m.scroll.offset = row - visible + 1
	}
	m.scroll.clamp()
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
