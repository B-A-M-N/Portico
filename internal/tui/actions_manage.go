package tui

import (
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Action sets for the management, preview and form screens.

// inspectActions is the operational screen for one connection.
//
// Space, Repair and Delete were all executed here and none of them was
// advertised, which is the drift this set removes: every action below is both
// dispatched and drawn from this one description.
func (m Model) inspectActions() ActionSet {
	conn := m.SelectedConnection()
	has := conn != nil
	noSelection := "no connection is selected"

	toggleLabel := "Open"
	if has && conn.DesiredState == "open" {
		toggleLabel = "Close"
	}
	repairEnabled, repairReason := m.repairAvailability(conn)

	// Log actions apply on the Logs tab, and say so rather than being silently
	// inert elsewhere.
	onLogs := m.inspect != nil && m.inspect.SelectedTab() == screens.InspectTabLogs

	return ActionSet{
		{
			ID: ActionLeft, Keys: []string{"left", "h"}, Label: "Previous tab", Enabled: true,
			Help: "Move to the previous tab: Overview, Route, Activity, Technical, Logs.",
		},
		{
			ID: ActionRight, Keys: []string{"right", "l"}, Label: "Next tab", Enabled: true, Primary: true,
			Help: "Move to the next tab: Overview, Route, Activity, Technical, Logs.",
		},
		{
			ID: ActionToggleOpen, Keys: []string{"space", " "}, Label: toggleLabel, Enabled: has,
			DisabledReason: noSelection, Primary: true,
			Help: "Open or close this connection. The change is previewed as a plan first.",
		},
		{
			ID: ActionRepair, Keys: []string{"r"}, Label: "Diagnose", Enabled: repairEnabled,
			DisabledReason: repairReason,
			Help:           "Check each hop of the route and offer the smallest repair.",
		},
		{
			ID: ActionEdit, Keys: []string{"e"}, Label: "Edit", Enabled: has,
			DisabledReason: noSelection,
			Help:           "Change this connection, previewed as a plan before it is applied.",
		},
		{
			ID: ActionCopy, Keys: []string{"c"}, Label: "Copy", Enabled: has,
			DisabledReason: noSelection,
			Help:           "Make another connection like this one.",
		},
		{
			ID: ActionDelete, Keys: []string{"d"}, Label: "Delete", Enabled: has,
			DisabledReason: noSelection,
			Help:           "Remove this connection and the resources Portico created for it.",
		},
		{
			ID: ActionRefresh, Keys: []string{"R"}, Label: "Refresh logs", Enabled: onLogs,
			DisabledReason: "the Logs tab is not open",
			Help:           "Re-read the connector's recent output.",
		},
		{
			ID: ActionFollowLogs, Keys: []string{"f"}, Label: "Follow logs", Enabled: onLogs,
			DisabledReason: "the Logs tab is not open",
			Help:           "Keep re-reading the log while the connection is active.",
		},
		{
			ID: ActionFilter, Keys: []string{"F"}, Label: "Filter stream", Enabled: onLogs,
			DisabledReason: "the Logs tab is not open",
			Help:           "Show all output, or only what the connector wrote to stdout or stderr.",
		},
	}
}

// providerActions is the provider and account list.
//
// Removal was implemented and omitted from the help; verification and
// credential replacement had no action at all.
//
// The configure action is gated by the provider's declared setup kind, not by
// whether a provider row exists: every catalogued provider once showed a live
// "Add account" whose submission the supervisor then refused, because
// enabling on presence alone cannot distinguish a provider that stores
// credentials from one that holds none.
func (m Model) providerActions() ActionSet {
	_, hasAccount := m.selectedAccount()
	provider, hasProvider := m.selectedProvider()

	setupKind := ""
	if hasProvider {
		setupKind = provider.SetupKind
	}

	configureLabel, configureHelp, configureReason := describeConfigureAction(setupKind)

	return ActionSet{
		{
			ID: ActionUp, Keys: []string{"up", "k"}, Label: "Up", Enabled: true,
			Help: "Move up the list of providers and their accounts.",
		},
		{
			ID: ActionDown, Keys: []string{"down", "j"}, Label: "Down", Enabled: true, Primary: true,
			Help: "Move down the list of providers and their accounts.",
		},
		{
			ID: ActionConfigureProvider, Keys: []string{"a"}, Label: configureLabel, Enabled: setupKind != "",
			DisabledReason: configureReason, Primary: true,
			Help: configureHelp,
		},
		{
			ID: ActionVerifyAccount, Keys: []string{"v"}, Label: "Verify", Enabled: hasAccount,
			DisabledReason: "no account is selected", Primary: true,
			Help: "Check this account's stored credential against the provider. " +
				"The credential is not shown and is not changed.",
		},
		{
			ID: ActionReplaceCredential, Keys: []string{"c"}, Label: "Replace credential", Enabled: hasAccount,
			DisabledReason: "no account is selected",
			Help: "Give this account a new credential, keeping its identity and every connection " +
				"that uses it. The new credential is validated before it replaces the old one.",
		},
		{
			ID: ActionRemoveAccount, Keys: []string{"x"}, Label: "Remove account", Enabled: hasAccount,
			DisabledReason: "no account is selected",
			Help: "Forget the credential Portico stored for this account. Nothing is deleted at the " +
				"provider, and the removal is refused while any connection still uses it.",
		},
	}
}

// describeConfigureAction names and explains the configure action in the terms
// of what the flow actually does, and says why when there is nothing to run.
func describeConfigureAction(setupKind string) (label, help, reason string) {
	switch setupKind {
	case "account":
		return "Add account",
			"Give Portico a credential for this provider. It is stored encrypted and never displayed.",
			""
	case "guidance":
		return "Set up",
			"Show this provider's setup guide. Portico cannot hold this provider's credential, " +
				"so nothing is stored — the screen tells you what to do in the provider's own tools.",
			""
	default:
		// No setup flow: the provider either needs nothing from the user or is
		// not configurable through Portico at all. Advertising a key whose
		// submission is guaranteed to be refused is the exact "looks broken"
		// state this gate exists to prevent.
		return "Set up",
			"Configure this provider so Portico can use it.",
			"this provider has nothing to configure here"
	}
}

// setupActions is the readiness screen. `E` produced a support export and was
// never advertised.
//
// Enter is offered only for a provider that declares a setup flow: pressing it
// on a provider with nothing to configure used to fire a request the
// supervisor was guaranteed to refuse.
func (m Model) setupActions() ActionSet {
	selected := m.setup != nil && m.setup.Selected() != nil
	hasFlow := selected && m.setup.Selected().SetupKind != ""
	pinned := m.setup != nil && m.setup.Readiness != nil && m.setup.Readiness.LaunchModePinned

	launchHelp := "Switch between opening marked connections at startup and opening nothing by itself."
	if pinned {
		launchHelp = "Launch mode is fixed by an environment variable; unset it to change the mode here."
	}

	setupReason := "no provider is selected"
	if selected && !hasFlow {
		setupReason = "this provider has nothing to configure here"
	}

	return ActionSet{
		{
			ID: ActionUp, Keys: []string{"up", "k"}, Label: "Up", Enabled: true,
			Help: "Move up the list of providers.",
		},
		{
			ID: ActionDown, Keys: []string{"down", "j"}, Label: "Down", Enabled: true,
			Help: "Move down the list of providers.",
		},
		{
			ID: ActionConfigureProvider, Keys: []string{"enter"}, Label: "Set up", Enabled: hasFlow,
			DisabledReason: setupReason, Primary: true,
			Help: "Configure the highlighted provider so Portico can use it.",
		},
		{
			ID: ActionLaunchMode, Keys: []string{"l"}, Label: "Launch mode", Enabled: !pinned,
			DisabledReason: "fixed by an environment variable", Primary: true,
			Help: launchHelp,
		},
		{
			ID: ActionRefresh, Keys: []string{"r"}, Label: "Re-check", Enabled: true,
			Help: "Run the readiness checks again.",
		},
		{
			ID: ActionSupportExport, Keys: []string{"E"}, Label: "Write report", Enabled: true,
			Help: "Write a diagnostic report for a bug report. It carries no credentials, " +
				"authorization headers, cookies, private keys or command environments.",
		},
	}
}

// discoveryActions is the list of services found listening locally.
func (m Model) discoveryActions() ActionSet {
	has := len(m.discovery) > 0
	return ActionSet{
		{
			ID: ActionUp, Keys: []string{"up", "k"}, Label: "Up", Enabled: has,
			DisabledReason: "nothing was found", Help: "Move up the list of discovered services.",
		},
		{
			ID: ActionDown, Keys: []string{"down", "j"}, Label: "Down", Enabled: has,
			DisabledReason: "nothing was found", Help: "Move down the list of discovered services.",
		},
		{
			ID: ActionConfirm, Keys: []string{"enter"}, Label: "Use this service", Enabled: has,
			DisabledReason: "nothing was found", Primary: true,
			Help: "Start a new connection that publishes the highlighted service.",
		},
		{
			ID: ActionEvidence, Keys: []string{"i"}, Label: "Why this?", Enabled: has,
			DisabledReason: "nothing was found",
			Help:           "Show the evidence behind the classification: what was probed and what answered.",
		},
		{
			ID: ActionRefresh, Keys: []string{"r"}, Label: "Scan again", Enabled: true, Primary: true,
			Help: "Look again. A service started since the last scan will be found now.",
		},
		{
			ID: ActionManualEntry, Keys: []string{"m"}, Label: "Enter an address", Enabled: true, Primary: true,
			Help: "Type an address yourself instead of choosing from the list.",
		},
	}
}

// operationsActions is the history screen.
func (m Model) operationsActions() ActionSet {
	has := len(m.operations) > 0
	return ActionSet{
		{
			ID: ActionUp, Keys: []string{"up", "k"}, Label: "Up", Enabled: has,
			DisabledReason: "there are no operations", Help: "Move up the history.",
		},
		{
			ID: ActionDown, Keys: []string{"down", "j"}, Label: "Down", Enabled: has,
			DisabledReason: "there are no operations", Help: "Move down the history.",
		},
		{
			ID: ActionFilter, Keys: []string{"f"}, Label: "Filter", Enabled: true, Primary: true,
			Help: "Cycle between everything, what is still running, what failed, " +
				"and operations for the selected connection.",
		},
		{
			ID: ActionShowMore, Keys: []string{"m"}, Label: "Older", Enabled: m.operationsTruncated,
			DisabledReason: "this is the whole history",
			Help:           "Load older operations beyond those already shown.",
		},
		{
			ID: ActionRefresh, Keys: []string{"r"}, Label: "Refresh", Enabled: true,
			Help: "Re-read the history from the supervisor.",
		},
	}
}

// planPreviewActions is the confirmation for a plan.
//
// The confirm label names the outcome rather than saying "Apply", because the
// intent is known and a user approving a deletion should be told that is what
// they are approving.
func (m Model) planPreviewActions() ActionSet {
	label := "Apply"
	if m.plan != nil {
		label = planConfirmLabel(m.plan.Intent)
	}
	return ActionSet{
		{
			ID: ActionApply, Keys: []string{"enter"}, Label: label, Enabled: m.plan != nil && !m.applying,
			DisabledReason: applyDisabledReason(m.plan, m.applying), Primary: true,
			Help: "Carry out exactly the steps listed above. Nothing beyond them is done, and the " +
				"plan is re-checked against the provider before it starts.",
		},
	}
}

// applyDisabledReason says why a preview cannot be approved right now.
func applyDisabledReason(plan *ipc.PlanDTO, applying bool) string {
	switch {
	case plan == nil:
		return "there is no plan to apply"
	case applying:
		return "this plan is already being applied"
	default:
		return ""
	}
}

// operationProgressActions is the running-operation screen.
func (m Model) operationProgressActions() ActionSet {
	return ActionSet{
		{
			ID: ActionRefresh, Keys: []string{"r"}, Label: "Refresh", Enabled: m.operation != nil,
			DisabledReason: "no operation is being watched",
			Help:           "Ask the supervisor for the operation's current state.",
		},
	}
}

// repairActions is the diagnosis screen.
func (m Model) repairActions() ActionSet {
	has := len(m.diagnostics) > 0
	return ActionSet{
		{
			ID: ActionUp, Keys: []string{"up", "k"}, Label: "Up", Enabled: has,
			DisabledReason: "there are no findings", Help: "Move up the findings.",
		},
		{
			ID: ActionDown, Keys: []string{"down", "j"}, Label: "Down", Enabled: has,
			DisabledReason: "there are no findings", Help: "Move down the findings.",
		},
		{
			ID: ActionPreview, Keys: []string{"enter"}, Label: "Preview repair", Enabled: has,
			DisabledReason: "there is nothing to repair", Primary: true,
			Help: "Ask the supervisor for the smallest set of steps that would fix what was found. " +
				"Nothing is changed until you approve it.",
		},
		{
			ID: ActionRefresh, Keys: []string{"r"}, Label: "Check again", Enabled: true, Primary: true,
			Help: "Run the diagnosis again.",
		},
	}
}

// accountRemovalActions is the removal confirmation.
func (m Model) accountRemovalActions() ActionSet {
	removable := m.accountRemovalPreview != nil &&
		m.accountRemovalPreview.Removable &&
		m.accountRemovalError == ""
	return ActionSet{
		{
			ID: ActionConfirm, Keys: []string{"enter"}, Label: "Remove account", Enabled: removable,
			DisabledReason: removalDisabledReason(m), Primary: true,
			Help: "Forget this account's credential. Nothing is deleted at the provider.",
		},
	}
}

// removalDisabledReason says why the removal cannot be confirmed.
func removalDisabledReason(m Model) string {
	switch {
	case m.accountRemovalError != "":
		return "the supervisor refused this removal"
	case m.accountRemovalPreview == nil:
		return "still checking what this would remove"
	case !m.accountRemovalPreview.Removable:
		return "something still depends on this account"
	default:
		return ""
	}
}

// recoveryActions is the screen shown when the supervisor cannot be reached.
func (m Model) recoveryActions() ActionSet {
	return ActionSet{
		{
			ID: ActionRetry, Keys: []string{"r"}, Label: "Retry", Enabled: true, Primary: true,
			Help: "Try the whole sequence again: start the supervisor, check that it answers, " +
				"and load the current state.",
		},
		{
			ID: ActionQuit, Keys: []string{"q"}, Label: "Quit", Enabled: true, Primary: true,
			Help: "Leave Portico.",
		},
	}
}
