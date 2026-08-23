package tui

import "github.com/B-A-M-N/portico/internal/tui/screens"

// Action sets for the multi-step input screens.
//
// Edit, Clone and the wizard previously had no contextual help at all: each
// hardcoded a footer string and the Help screen knew nothing about them. These
// sets are what both now read.

// editActions is the connection edit screen.
//
// The discard confirmation is an action of its own rather than a hidden branch
// of Back, so the footer can say that leaving will ask about unsaved changes.
func (m Model) editActions() ActionSet {
	if m.edit == nil {
		return nil
	}
	// While a discard confirmation is up, those are the only two answers.
	if m.edit.confirmingDiscard {
		return ActionSet{
			{
				ID: ActionDiscard, Keys: []string{"y"}, Label: "Discard changes", Enabled: true, Primary: true,
				Help: "Leave without saving. Everything you changed here is lost.",
			},
			{
				ID: ActionKeepEditing, Keys: []string{"n", "esc"}, Label: "Keep editing", Enabled: true,
				Primary: true, Help: "Return to the edit with your changes still on it.",
			},
		}
	}

	rows := m.edit.rows()
	cursorRow, hasRow := m.edit.currentRow()
	dirty := m.edit.dirty()

	changeEnabled := hasRow && cursorRow.editable
	changeReason := ""
	switch {
	case !hasRow:
		changeReason = "nothing is selected"
	case !cursorRow.editable:
		changeReason = cursorRow.reason
	}

	// While typing, the field owns the keyboard and only commit and cancel
	// apply. Advertising the list keys here would name keys the field consumes.
	if m.edit.typing {
		return ActionSet{
			{
				ID: ActionConfirm, Keys: []string{"enter"}, Label: "Accept value", Enabled: true, Primary: true,
				Help: "Accept what you typed. Nothing is saved until you preview and apply.",
			},
			{
				ID: ActionBack, Keys: []string{"esc"}, Label: "Cancel value", Enabled: true, Primary: true,
				Help: "Leave this field as it was.",
			},
		}
	}

	return ActionSet{
		{
			ID: ActionUp, Keys: []string{"up", "k"}, Label: "Up", Enabled: len(rows) > 1,
			DisabledReason: "there is only one property to change",
			Help:           "Move up the list of properties.",
		},
		{
			ID: ActionDown, Keys: []string{"down", "j"}, Label: "Down", Enabled: len(rows) > 1,
			DisabledReason: "there is only one property to change",
			Help:           "Move down the list of properties.",
		},
		{
			ID: ActionConfirm, Keys: []string{"enter"}, Label: "Change", Enabled: changeEnabled,
			DisabledReason: changeReason, Primary: true,
			Help: "Change the highlighted property. A property with a fixed set of answers steps " +
				"to the next one; a free value opens a field to type in.",
		},
		{
			ID: ActionPreview, Keys: []string{"p"}, Label: "Preview change", Enabled: dirty,
			DisabledReason: "nothing has been changed yet", Primary: true,
			Help: "Ask the supervisor exactly what this change would do. Nothing is saved until " +
				"you approve the plan it returns.",
		},
	}
}

// cloneActions is the copy prompt.
func (m Model) cloneActions() ActionSet {
	if m.clone == nil {
		return nil
	}
	if m.clone.submitting {
		return ActionSet{{
			ID: ActionConfirm, Enabled: false, Label: "Creating the copy",
			DisabledReason: "the copy is already being created",
			Help:           "Portico is creating the copy. Pressing enter again will not create a second one.",
		}}
	}
	ready := m.clone.detail != nil
	set := ActionSet{}
	if m.clone.fieldCount() > 1 {
		set = append(set, Action{
			ID: ActionNextField, Keys: []string{"tab"}, Label: "Next field", Enabled: true, Primary: true,
			Help: "Move between the name and the hostname.",
		})
	}
	return append(set, Action{
		ID: ActionConfirm, Keys: []string{"enter"}, Label: "Create the copy", Enabled: ready,
		DisabledReason: "still loading the connection to copy", Primary: true,
		Help: "Create the copy. It is created closed — nothing is opened until you ask. " +
			"Access protection is copied, so the same people will be able to reach it.",
	})
}

// wizardActions describes the new-connection wizard.
//
// The wizard owns its own keyboard and its steps differ, so the set is derived
// from what the current step accepts. It is what the contextual Help reads,
// which the wizard previously had none of.
func (m Model) wizardActions() ActionSet {
	if m.wizard == nil {
		return nil
	}
	// While a discard confirmation is up, those are the only two answers.
	if m.wizardConfirmingDiscard {
		return ActionSet{
			{
				ID: ActionDiscard, Keys: []string{"y"}, Label: "Discard and leave", Enabled: true,
				Primary: true,
				Help: "Leave without creating anything. Your answers are lost and nothing is " +
					"left behind.",
			},
			{
				ID: ActionKeepEditing, Keys: []string{"n", "esc"}, Label: "Keep going", Enabled: true,
				Primary: true, Help: "Return to the question with every answer still in place.",
			},
		}
	}
	return wizardActionSet(m.wizard.Actions())
}

// wizardActionSet converts the wizard's own description of its current question
// into the root model's action type.
//
// The wizard owns the step model, so it owns the answer to what the current
// question accepts; this is the translation, not a second opinion. Identities
// are assigned from the keys because the wizard's actions are per-question and
// the root model only needs them for dispatch through the footer and Help.
func wizardActionSet(actions screens.WizardActions) ActionSet {
	set := make(ActionSet, 0, len(actions))
	for _, a := range actions {
		if len(a.Keys) == 0 && a.Label == "" {
			continue
		}
		set = append(set, Action{
			ID:             wizardActionID(a.Keys),
			Keys:           a.Keys,
			Label:          a.Label,
			Enabled:        a.Enabled,
			DisabledReason: a.DisabledReason,
			Help:           a.Help,
			Primary:        a.Primary,
		})
	}
	return set
}

// wizardActionID names a wizard action for dispatch. The wizard handles its own
// keys, so these identities exist for the footer, Help and the tests that check
// the two agree.
func wizardActionID(keys []string) ActionID {
	if len(keys) == 0 {
		return ActionID("wizard_note")
	}
	switch keys[0] {
	case "enter":
		return ActionConfirm
	case "esc":
		return ActionBack
	case "up":
		return ActionUp
	case "down":
		return ActionDown
	case "s":
		return ActionConfigureProvider
	case "L":
		return ActionLaunchMode
	default:
		return ActionID("wizard_" + keys[0])
	}
}

// settingsActions is the operational settings screen.
func (m Model) settingsActions() ActionSet {
	rows := m.settingsRows()
	pending := m.settings != nil && m.settings.saving
	cursorRow, hasRow := m.settingsCurrentRow()

	changeEnabled := hasRow && cursorRow.editable && !pending
	reason := ""
	switch {
	case pending:
		reason = "a change is being saved"
	case !hasRow:
		reason = "nothing is selected"
	case !cursorRow.editable:
		reason = cursorRow.reason
	}

	return ActionSet{
		{
			ID: ActionUp, Keys: []string{"up", "k"}, Label: "Up", Enabled: len(rows) > 1,
			DisabledReason: "the settings have not been read yet",
			Help:           "Move up the list of settings.",
		},
		{
			ID: ActionDown, Keys: []string{"down", "j"}, Label: "Down", Enabled: len(rows) > 1,
			DisabledReason: "the settings have not been read yet",
			Help:           "Move down the list of settings.",
		},
		{
			ID: ActionConfirm, Keys: []string{"enter", "space", " "}, Label: "Change", Enabled: changeEnabled,
			DisabledReason: reason, Primary: true,
			Help: "Change the highlighted setting. The supervisor stores it, so it survives a restart.",
		},
		{
			ID: ActionRefresh, Keys: []string{"r"}, Label: "Reload", Enabled: !pending,
			DisabledReason: "a change is being saved",
			Help:           "Re-read the settings the supervisor currently holds.",
		},
	}
}
