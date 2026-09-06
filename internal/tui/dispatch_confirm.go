package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// The confirm and refresh actions, which mean something different per screen.

// confirmOnScreen takes the current screen's primary confirmation.
//
// Enter is one action with one identity, and its meaning is the screen's rather
// than the key's: inspecting a connection, approving a removal, previewing a
// repair, using a discovered service. Each of those was a separate branch of a
// key switch, and each had to re-derive whether it was allowed.
func (m Model) confirmOnScreen() (Model, tea.Cmd, bool) {
	switch m.screen {
	case ScreenHome:
		return m.openInspect()

	case ScreenAccountRemoval:
		// Nothing is removed that was not previewed, and nothing the preview
		// says cannot be removed. The action is disabled in both cases, so
		// reaching here means the preview permitted it.
		if m.accountRemovalTarget == nil || m.accountRemovalPreview == nil {
			return m, nil, true
		}
		return m, m.removeAccountCmd(*m.accountRemovalTarget, m.accountRemovalPreview.Fingerprint), true

	case ScreenPlanPreview:
		return m.applyPreviewedPlan()

	case ScreenRepair:
		conn := m.SelectedConnection()
		if conn == nil {
			return m, nil, true
		}
		m.preRepairDiagnostics = append([]ipc.DiagnosticDTO(nil), m.diagnostics...)
		m.repairConnectionID = conn.ID
		return m, m.planRepairCmd(conn.ID), true

	case ScreenDiscovery:
		return m.useDiscoveredService()

	case ScreenSettings:
		// While a rotation is awaiting confirmation, enter is its answer, not
		// the highlighted setting's. The rotation command itself re-checks the
		// flag, so the two cannot disagree.
		if m.settings != nil && m.settings.confirmingRotate {
			return m, m.rotateSecretKeyCmd(), true
		}
		return m.changeSelectedSetting()
	}
	return m, nil, true
}

// applyPreviewedPlan carries out the plan the user approved.
func (m Model) applyPreviewedPlan() (Model, tea.Cmd, bool) {
	if m.plan == nil || m.applying {
		return m, nil, true
	}
	// What is applied must be what was previewed. This fails closed: an
	// identity that cannot be verified is not an identity that matches.
	if !m.planMatchesPreview() {
		m.err = &UserFacingError{
			Summary:     "Portico could not confirm this plan is the one you approved",
			Explanation: "The plan's identity did not match the connection it was previewed for, so it was not applied.",
			NextActions: []string{"Press esc and ask for the plan again."},
			Retryable:   true,
		}
		return m, nil, true
	}
	m.applying = true
	m.status = "Applying plan..."
	if m.plan.Intent == "repair" {
		m.awaitingRepairVerification = true
	}
	return m, m.applyPlanCmd(m.plan.ID), true
}

// useDiscoveredService starts a connection from a service found listening.
func (m Model) useDiscoveredService() (Model, tea.Cmd, bool) {
	if m.discoverySelected < 0 || m.discoverySelected >= len(m.discovery) {
		return m, nil, true
	}
	svc := m.discovery[m.discoverySelected]
	m.wizard = screens.NewWizardForService(m.client, m.providerSnapshot(), svc.Address, svc.Protocol).
		WithContext(m.rootCtx).WithASCII(m.useASCII).WithDefaults(m.lifecycleDefaults())
	m.status = ""
	m.pushScreen(ScreenNewConnection)
	return m, nil, true
}

// refreshOnScreen re-reads whatever the current screen is showing.
func (m Model) refreshOnScreen() (Model, tea.Cmd, bool) {
	switch m.screen {
	case ScreenSetup:
		return m, m.readinessCmd(), true
	case ScreenOperations:
		return m, m.loadOperationsCmd(), true
	case ScreenOperationProgress:
		if m.operation != nil {
			return m, m.getOperationCmd(m.operation.ID), true
		}
		return m, nil, true
	case ScreenRepair:
		return m.openDiagnosis()
	case ScreenDiscovery:
		m.status = "Scanning for local services..."
		return m, m.discoveryCmd(), true
	case ScreenSettings:
		return m, m.loadSettingsCmd(), true
	case ScreenInspect:
		if conn := m.SelectedConnection(); conn != nil {
			return m, m.connectionLogsCmd(conn.ID), true
		}
		return m, nil, true
	case ScreenHome:
		return m, m.requestSnapshot(), true
	}
	return m, nil, true
}

// lifecycleDefaults are the persistent defaults a new connection starts from.
//
// The wizard hardcoded AutoStart true and OnDisconnect keep_alive, so every
// connection anyone ever created was armed to open at supervisor startup and to
// outlive the interface, without being asked and without being told. These are
// the supervisor's stored answers, which the user can change in Settings and
// per-connection in the wizard's review.
func (m Model) lifecycleDefaults() screens.LifecycleDefaults {
	defaults := screens.LifecycleDefaults{
		AutoStart:    false,
		OnDisconnect: "keep_alive",
	}
	if m.settings != nil && m.settings.settings != nil {
		defaults.AutoStart = m.settings.settings.DefaultAutoStart
		if policy := m.settings.settings.DefaultOnDisconnect; policy != "" {
			defaults.OnDisconnect = policy
		}
	}
	return defaults
}
