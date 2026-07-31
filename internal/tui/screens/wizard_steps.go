package screens

// The question sequence, derived rather than hand-written.
//
// Every step used to carry its own forward transition and its own "esc"
// transition, each restating the source-kind branch. The exposure step's back
// handler alone reimplemented the whole branch — command, directory with and
// without a single-page mode, MCP, port, plain service — and nothing kept the
// two copies in agreement. They had already drifted: leaving the name step
// always returned to the outcome step, even for a user who had reached it
// through the intent question, silently discarding that answer's context.
//
// Declaring which steps apply, once, makes back-navigation the inverse of
// forward navigation by construction instead of by maintenance.

// stepRule pairs a step with the condition under which it is asked.
type stepRule struct {
	id      int
	applies func(m *WizardModel) bool
}

// always is a step that is asked on every path.
func always(*WizardModel) bool { return true }

// wizardStepOrder is the question sequence, in the order the wizard asks them.
// A step whose predicate is false is skipped in both directions.
var wizardStepOrder = []stepRule{
	{WizardStepOutcome, always},
	// The intent question is asked only when the chosen outcome did not
	// already determine the source.
	{WizardStepIntent, func(m *WizardModel) bool { return m.state.Advanced }},
	{WizardStepName, always},
	{WizardStepMCPMode, func(m *WizardModel) bool { return m.state.SourceType == "mcp_server" }},
	{WizardStepSource, always},
	{WizardStepPort, func(m *WizardModel) bool { return m.hasPortStep() }},
	// Protocol is asked only for a service that is already listening; a
	// command, directory or MCP source determines it another way.
	{WizardStepProtocol, func(m *WizardModel) bool { return m.state.SourceType == "existing_service" }},
	{WizardStepCommandArgs, func(m *WizardModel) bool { return m.isCommandOrigin() }},
	{WizardStepCommandWorkingDir, func(m *WizardModel) bool { return m.isCommandOrigin() }},
	{WizardStepDirectoryMode, func(m *WizardModel) bool { return m.state.SourceType == "directory" }},
	{WizardStepDirectorySPA, func(m *WizardModel) bool {
		return m.state.SourceType == "directory" && m.state.DirectoryMode == "read"
	}},
	{WizardStepMCPTransport, func(m *WizardModel) bool { return m.state.SourceType == "mcp_server" }},
	{WizardStepExposure, always},
	{WizardStepHostname, func(m *WizardModel) bool { return m.state.ExposureMode == "permanent_public" }},
	{WizardStepProtection, always},
	{WizardStepProtectionRules, func(m *WizardModel) bool { return m.state.Protection == "email_otp" }},
	{WizardStepProvider, always},
	// The account question is worth asking only when there is a choice to make.
	{WizardStepAccount, func(m *WizardModel) bool { return len(m.accounts) > 1 }},
	{WizardStepReview, always},
}

// applicableSteps returns the questions this connection actually needs, in
// order.
func (m *WizardModel) applicableSteps() []int {
	steps := make([]int, 0, len(wizardStepOrder))
	for _, rule := range wizardStepOrder {
		if rule.applies(m) {
			steps = append(steps, rule.id)
		}
	}
	return steps
}

// previousStep returns the question before the current one, skipping the ones
// that do not apply. It reports false at the first question, where there is
// nothing to go back to.
func (m *WizardModel) previousStep() (int, bool) {
	steps := m.applicableSteps()
	for i, id := range steps {
		if id != m.state.Step {
			continue
		}
		if i == 0 {
			return 0, false
		}
		return steps[i-1], true
	}
	// The current step is not in the sequence — the post-creation states are
	// not questions — so there is nothing to go back to.
	return 0, false
}

// goBack moves to the previous applicable question and restores whatever that
// question was showing.
//
// Restoring the input here, once, is the other half of what the hand-written
// handlers each did separately: several of them set m.input and several forgot
// to, so going back could present an empty field over a stored answer.
func (m *WizardModel) goBack() {
	previous, ok := m.previousStep()
	if !ok {
		return
	}
	m.err = nil
	m.state.Step = previous
	m.restoreStepInput()

	// Stepping back invalidates any recommendation: it was computed from
	// answers the user is now revisiting, and showing it against changed
	// requirements would describe a different connection.
	m.recommendation = nil
	m.recommendFingerprint = ""
	m.recommendPending = false
	m.recommendErr = nil
}

// restoreStepInput puts the stored answer back on screen for the current step.
func (m *WizardModel) restoreStepInput() {
	m.input = ""
	switch m.state.Step {
	case WizardStepName:
		m.input = m.state.Name
	case WizardStepSource:
		m.input = m.state.SourceAddress
	case WizardStepPort:
		m.input = m.state.Port
	case WizardStepCommandArgs:
		m.input = commandArgsInput(m.state.CommandArgs)
	case WizardStepCommandWorkingDir:
		m.input = m.state.WorkingDir
	case WizardStepHostname:
		m.input = m.state.Hostname
	case WizardStepProtectionRules:
		m.input = protectionRulesInput(m.state.AllowedEmails, m.state.AllowedDomains)

	// Menus restore a cursor rather than text, positioned on the stored answer
	// so going back does not silently move the user's choice.
	case WizardStepOutcome:
		m.selected = 0
	case WizardStepIntent:
		m.selected = indexOfString(wizardSourceKinds, m.state.SourceType)
	case WizardStepMCPMode:
		m.selected = 0
		if m.state.MCPCommand {
			m.selected = 1
		}
	case WizardStepProtocol:
		m.selected = indexOfString([]string{"http", "https"}, m.state.SourceProtocol)
	case WizardStepDirectoryMode:
		m.selected = directoryModeIndex(m.directoryModeChoices(), m.state.DirectoryMode)
	case WizardStepDirectorySPA:
		m.selected = 0
		if m.state.DirectorySPA {
			m.selected = 1
		}
	case WizardStepMCPTransport:
		m.selected = indexOfString(m.mcpTransports(), m.state.MCPTransport)
	case WizardStepExposure:
		m.selected = choiceIndex(m.exposureChoices(), m.state.ExposureMode)
	case WizardStepProtection:
		m.selected = choiceIndex(m.protectionChoices(), m.state.Protection)
	case WizardStepProvider:
		m.selected = choiceIndex(m.providerChoices(), m.state.Provider)
	case WizardStepAccount:
		m.selected = 0
		for i, account := range m.accounts {
			if account.ID == m.state.AccountID {
				m.selected = i
			}
		}
	}
}

// indexOfString returns the position of a value, or zero when absent.
func indexOfString(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return 0
}

// choiceIndex returns the position of a choice by value, or the first pickable
// one when the value is not present.
func choiceIndex(choices []wizardChoice, want string) int {
	for i, c := range choices {
		if c.Value == want {
			return i
		}
	}
	return firstAvailable(choices)
}

// directoryModeIndex returns the position of a directory mode.
func directoryModeIndex(choices []directoryModeChoice, want string) int {
	for i, c := range choices {
		if c.mode == want {
			return i
		}
	}
	return 0
}
