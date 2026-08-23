package screens

// What the wizard can do, per question.
//
// The wizard had no contextual help at all: each step's View hardcoded its own
// footer line, the root Help screen knew nothing about the wizard, and the keys
// a step accepted were discoverable only by reading HandleKey. A user in the
// middle of describing a connection could not ask what the current question
// meant or what the keys did.
//
// WizardAction is declared here rather than in the tui package because the
// wizard is the authority on its own steps, and the tui package must not
// reimplement the step model to describe it. The root model converts these into
// its own Action type, which is what the footer and Help read.

// WizardAction is one thing the current wizard question can do.
type WizardAction struct {
	// Keys are every binding that invokes it; the first is advertised.
	Keys []string
	// Label names it in the words a user would use.
	Label string
	// Enabled is false when it cannot be taken on this question right now.
	Enabled bool
	// DisabledReason says why, in one clause.
	DisabledReason string
	// Help is the longer explanation for the Help screen.
	Help string
	// Primary marks the question's main action, so a narrow footer keeps it.
	Primary bool
}

// WizardActions is the set for the current question.
type WizardActions []WizardAction

// Actions describes what the current question accepts.
//
// It is derived from the same step identity HandleKey switches on, so a
// question that accepts a key advertises it and a question that does not,
// does not.
func (m *WizardModel) Actions() WizardActions {
	if m == nil {
		return nil
	}
	switch m.state.Step {
	case WizardStepCreating:
		return WizardActions{{
			Label: "Creating the connection", Enabled: false,
			DisabledReason: "Portico is saving the connection",
			Help:           "The connection is being created. Nothing is open yet.",
		}}
	case WizardStepApplying:
		return WizardActions{{
			Label: "Applying the plan", Enabled: false,
			DisabledReason: "the plan is being carried out",
			Help: "Portico is carrying out the plan it showed you. Leaving now would not " +
				"cancel it: the supervisor finishes the work.",
		}}
	case WizardStepPlanPreview:
		return m.planPreviewActions()
	case WizardStepOperationWait:
		return m.operationWaitActions()
	case WizardStepCreated, WizardStepComplete:
		return m.completeActions()
	case WizardStepReview:
		return m.reviewActions()
	}
	return m.questionActions()
}

// planPreviewActions is the wizard's own plan confirmation.
//
// The label names the outcome the plan carries rather than assuming the wizard
// only ever previews an open.
func (m *WizardModel) planPreviewActions() WizardActions {
	label := "Apply"
	if m.plan != nil {
		label = PlanConfirmLabel(m.plan.Intent)
	}
	return WizardActions{
		{
			Keys: []string{"enter"}, Label: label, Enabled: m.plan != nil,
			DisabledReason: "there is no plan to apply", Primary: true,
			Help: "Carry out exactly the steps listed. Nothing beyond them is done.",
		},
		{
			Keys: []string{"esc"}, Label: "Not now", Enabled: true, Primary: true,
			Help: "Leave the connection saved and closed. You can open it from the list at any time.",
		},
	}
}

// operationWaitActions is the wait while the supervisor works.
func (m *WizardModel) operationWaitActions() WizardActions {
	return WizardActions{{
		Keys: []string{"esc"}, Label: "Stop watching", Enabled: true, Primary: true,
		Help: "Stop watching. The supervisor carries on opening the connection — leaving " +
			"this screen does not cancel it.",
	}}
}

// completeActions is the end of the wizard.
func (m *WizardModel) completeActions() WizardActions {
	return WizardActions{{
		Keys: []string{"esc", "enter"}, Label: "Done", Enabled: true, Primary: true,
		Help: "Return to the connection list.",
	}}
}
