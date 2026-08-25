package screens

// The question actions: menus, text fields, and Review.

// reviewActions is the last question before anything is created.
//
// The two outcomes are a menu, so the keys are the menu keys plus the one that
// commits. Provider is checked here because a connection with no provider
// cannot open, and the refusal belongs on the action rather than appearing
// after the key is pressed.
func (m *WizardModel) reviewActions() WizardActions {
	ready := m.state.Provider != ""
	return WizardActions{
		{
			Keys: []string{"up", "k"}, Label: "Up", Enabled: true,
			Help: "Choose between saving the connection closed and opening it now.",
		},
		{
			Keys: []string{"down", "j"}, Label: "Down", Enabled: true,
			Help: "Choose between saving the connection closed and opening it now.",
		},
		{
			Keys: []string{"enter"}, Label: "Create connection", Enabled: ready,
			DisabledReason: "no provider was chosen, so this connection could not open",
			Primary:        true,
			Help: "Create the connection. Nothing is published yet unless you chose to open it, " +
				"and opening always shows you the plan first.",
		},
		{
			Keys: []string{"L"}, Label: "Change startup and closing behaviour", Enabled: true,
			Help: "Change whether this connection opens by itself when the supervisor starts, " +
				"and whether it keeps running after you quit Portico.",
		},
		{
			Keys: []string{"esc"}, Label: "Back", Enabled: true, Primary: true,
			Help: "Return to the previous question. Your answers are kept.",
		},
	}
}

// questionActions describes an ordinary question: a text field or a menu.
func (m *WizardModel) questionActions() WizardActions {
	_, canGoBack := m.previousStep()
	back := WizardAction{
		Keys: []string{"esc"}, Label: "Back", Enabled: canGoBack, Primary: true,
		DisabledReason: "this is the first question",
		Help:           "Return to the previous question. Every answer you have given is kept.",
	}

	if isTextStep(m.state.Step) {
		return WizardActions{
			{
				Keys: []string{"enter"}, Label: "Continue", Enabled: true, Primary: true,
				Help: "Accept this answer and move to the next question. The value is checked " +
					"first, so a mistake is reported here rather than at create time.",
			},
			back,
			{
				Label: "Editing", Enabled: true,
				Help: "The field takes the usual editing keys: arrows, home and end, word motion, " +
					"delete forward, and paste.",
			},
		}
	}

	options := m.currentMenuLength()
	return WizardActions{
		{
			Keys: []string{"up", "k"}, Label: "Up", Enabled: options > 1,
			DisabledReason: "there is only one option", Help: "Move up the list of options.",
		},
		{
			Keys: []string{"down", "j"}, Label: "Down", Enabled: options > 1,
			DisabledReason: "there is only one option", Help: "Move down the list of options.",
		},
		{
			Keys: []string{"enter"}, Label: "Choose", Enabled: options > 0,
			DisabledReason: "there is nothing to choose", Primary: true,
			Help: "Take the highlighted option. An option Portico cannot deliver says what it " +
				"needs instead of being taken.",
		},
		m.setupAction(),
		back,
	}
}

// setupAction is the provider-setup escape hatch.
//
// A user who reaches the provider question and finds the recommended provider
// unconfigured had to abandon the wizard, configure the provider elsewhere, and
// start again — losing every answer. On this question the action is live; on
// every other question it is absent.
func (m *WizardModel) setupAction() WizardAction {
	if m.state.Step != WizardStepProvider {
		return WizardAction{}
	}
	choice, ok := choiceAt(m.providerChoices(), m.selected)
	needsSetup := ok && !choice.Available && choice.Value != ""
	reason := "this provider is already usable"
	if !ok || choice.Value == "" {
		reason = "no provider is highlighted"
	}
	return WizardAction{
		Keys: []string{"s"}, Label: "Set up this provider", Enabled: needsSetup,
		DisabledReason: reason, Primary: true,
		Help: "Configure the highlighted provider without leaving the wizard. Your answers are " +
			"kept, and Portico returns here with the provider usable.",
	}
}

// currentMenuLength is how many options the current menu offers.
//
// It reads the same choice functions the renderer and HandleKey read, so the
// count cannot disagree with what is drawn.
func (m *WizardModel) currentMenuLength() int {
	switch m.state.Step {
	case WizardStepOutcome:
		return len(wizardRecipes)
	case WizardStepIntent:
		return len(wizardSourceKinds)
	case WizardStepMCPMode, WizardStepDirectorySPA:
		return 2
	case WizardStepProtocol:
		return 2
	case WizardStepHealth:
		return 2
	case WizardStepPortForwardProtocol:
		return len(portForwardProtocolChoices())
	case WizardStepDirectoryMode:
		return len(m.directoryModeChoices())
	case WizardStepMCPTransport:
		return len(m.mcpTransports())
	case WizardStepExposure:
		return len(m.exposureChoices())
	case WizardStepProtection:
		return len(m.protectionChoices())
	case WizardStepProvider:
		return len(m.providerChoices())
	case WizardStepAccount:
		return len(m.accountsFor(m.state.Provider))
	default:
		return 0
	}
}
