package tui

import (
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Per-screen action sets.
//
// This is the single answer to "what can I do here?". Dispatch consults it,
// the footer draws it, and Help explains it, so the three cannot disagree.
//
// Each set is computed from current model state, which is what makes an
// unavailable action say why rather than silently doing nothing: the Repair
// action on a port forward carries its own reason, instead of the key being
// accepted and a status line appearing afterwards.

// actionsFor returns everything the current screen can do right now.
func (m Model) actionsFor(screen ScreenID) ActionSet {
	var set ActionSet
	switch screen {
	case ScreenHome:
		set = m.homeActions()
	case ScreenInspect:
		set = m.inspectActions()
	case ScreenProviders:
		set = m.providerActions()
	case ScreenSetup:
		set = m.setupActions()
	case ScreenSettings:
		set = m.settingsActions()
	case ScreenDiscovery:
		set = m.discoveryActions()
	case ScreenOperations:
		set = m.operationsActions()
	case ScreenPlanPreview:
		set = m.planPreviewActions()
	case ScreenOperationProgress:
		set = m.operationProgressActions()
	case ScreenRepair:
		set = m.repairActions()
	case ScreenAccountRemoval:
		set = m.accountRemovalActions()
	case ScreenEdit:
		set = m.editActions()
	case ScreenClone:
		set = m.cloneActions()
	case ScreenNewConnection:
		set = m.wizardActions()
	case ScreenRecovery:
		set = m.recoveryActions()
	case ScreenHelp:
		// Esc closes help; ? is deliberately absorbed here rather than
		// reopening it, which used to record help as the screen to return to
		// and stranded the user. It is not advertised as a second binding for
		// closing, because it does not close: it does nothing.
		set = ActionSet{{
			ID: ActionBack, Keys: []string{"esc"}, Label: "Close help", Enabled: true,
			Primary: true, Help: "Return to the screen you came from.",
		}}
	}
	return append(set, navigationActions(screen, scrollsFreely(screen))...)
}

// homeActions is the connection list. When there are no connections the
// lifecycle actions are disabled with the reason, rather than being absent —
// so the shape of the screen does not change as the first connection appears.
func (m Model) homeActions() ActionSet {
	conn := m.SelectedConnection()
	has := conn != nil
	noSelection := "no connection is selected"

	toggleLabel := "Open"
	toggleHelp := "Open this connection: Portico previews the plan first, then applies it."
	if has && conn.DesiredState == "open" {
		toggleLabel = "Close"
		toggleHelp = "Close this connection. The saved configuration is kept."
	}

	repairEnabled, repairReason := m.repairAvailability(conn)

	return ActionSet{
		{
			ID: ActionUp, Keys: []string{"up", "k"}, Label: "Up", Enabled: len(m.ConnectionList()) > 1,
			Help: "Move the selection up the list.",
		},
		{
			ID: ActionDown, Keys: []string{"down", "j"}, Label: "Down", Enabled: len(m.ConnectionList()) > 1,
			Help: "Move the selection down the list.",
		},
		{
			ID: ActionInspect, Keys: []string{"enter"}, Label: "Inspect", Enabled: has,
			DisabledReason: noSelection, Primary: true,
			Help: "Open this connection: its route, activity, technical detail and connector logs.",
		},
		{
			ID: ActionToggleOpen, Keys: []string{"space", " "}, Label: toggleLabel, Enabled: has,
			DisabledReason: noSelection, Primary: true, Help: toggleHelp,
		},
		{
			ID: ActionNew, Keys: []string{"n"}, Label: "New connection", Enabled: true, Primary: true,
			Help: "Describe something you want reachable. Portico chooses the provider and the steps.",
		},
		{
			ID: ActionDiscover, Keys: []string{"a"}, Label: "Discover services", Enabled: true, Primary: true,
			Help: "Look for services already listening on this machine and make a connection from one.",
		},
		{
			ID: ActionEdit, Keys: []string{"e"}, Label: "Edit", Enabled: has,
			DisabledReason: noSelection,
			Help:           "Change this connection. Every change is previewed as a plan before it is applied.",
		},
		{
			ID: ActionCopy, Keys: []string{"c"}, Label: "Copy", Enabled: has,
			DisabledReason: noSelection,
			Help:           "Make another connection like this one. The copy is created closed.",
		},
		{
			ID: ActionRepair, Keys: []string{"r"}, Label: "Diagnose", Enabled: repairEnabled,
			DisabledReason: repairReason,
			Help:           "Check each hop of the route and offer the smallest repair that fixes what is broken.",
		},
		{
			ID: ActionDelete, Keys: []string{"d"}, Label: "Delete", Enabled: has,
			DisabledReason: noSelection,
			Help: "Remove this connection and the provider resources Portico created for it. " +
				"Previewed before anything is deleted.",
		},
		{
			ID: ActionOperations, Keys: []string{"o"}, Label: "History", Enabled: true,
			Help: "Everything Portico has done, most recent first.",
		},
		{
			ID: ActionProviders, Keys: []string{"p"}, Label: "Providers", Enabled: true,
			Help: "The services Portico can publish through, and the accounts you have configured.",
		},
		{
			ID: ActionSetup, Keys: []string{"s"}, Label: "Setup", Enabled: true,
			Help: "What Portico needs before it can work, and what is already satisfied.",
		},
		{
			ID: ActionSettings, Keys: []string{"S"}, Label: "Settings", Enabled: true,
			Help: "How Portico behaves: whether connections open at startup, and what new ones default to.",
		},
	}
}

// repairAvailability reports whether diagnosis can run for a connection, and
// why not when it cannot.
//
// The reason is carried by the action rather than produced as a status line
// after the key is pressed, which is what let Inspect accept `r` on a kind the
// backend refuses.
func (m Model) repairAvailability(conn *ipc.ConnectionDTO) (bool, string) {
	if conn == nil {
		return false, "no connection is selected"
	}
	if !kindSupportsRepair(conn.Kind) {
		return false, "Portico cannot yet diagnose a " + screens.ConnectionKindLabel(conn.Kind)
	}
	return true, ""
}
