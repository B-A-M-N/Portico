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
	{WizardStepIntent, func(m *WizardModel) bool { return m.state.Advanced }},
	{WizardStepName, always},
	// Port-forward steps: asked only when the connection kind is port_forward.
	{WizardStepPortForwardLocalPort, func(m *WizardModel) bool { return m.state.ConnectionKind == "port_forward" }},
	{WizardStepPortForwardRemoteHost, func(m *WizardModel) bool { return m.state.ConnectionKind == "port_forward" }},
	{WizardStepPortForwardRemotePort, func(m *WizardModel) bool { return m.state.ConnectionKind == "port_forward" }},
	{WizardStepPortForwardProtocol, func(m *WizardModel) bool { return m.state.ConnectionKind == "port_forward" }},
	// The client-tunnel questions: which tunnel, what it reaches, which profile.
	{WizardStepTunnelID, func(m *WizardModel) bool { return m.state.ConnectionKind == "client_tunnel" }},
	{WizardStepTunnelMCP, func(m *WizardModel) bool { return m.state.ConnectionKind == "client_tunnel" }},
	{WizardStepTunnelProfile, func(m *WizardModel) bool { return m.state.ConnectionKind == "client_tunnel" }},
	{WizardStepMCPMode, func(m *WizardModel) bool { return m.state.SourceType == "mcp_server" }},
	// The discovery question belongs to an existing service only: a directory, a
	// command and an MCP endpoint are all things the user names rather than
	// things Portico can find listening.
	{WizardStepDiscovery, func(m *WizardModel) bool {
		return m.state.SourceType == "existing_service" && m.state.ConnectionKind == "service_exposure"
	}},
	// Service-exposure steps: only for service_exposure kind.
	{WizardStepSource, func(m *WizardModel) bool { return m.state.ConnectionKind == "service_exposure" }},
	{WizardStepPort, func(m *WizardModel) bool { return m.hasPortStep() && m.state.ConnectionKind == "service_exposure" }},
	{WizardStepProtocol, func(m *WizardModel) bool {
		return m.state.SourceType == "existing_service" && m.state.ConnectionKind == "service_exposure"
	}},
	{WizardStepCommandArgs, func(m *WizardModel) bool { return m.isCommandOrigin() && m.state.ConnectionKind == "service_exposure" }},
	{WizardStepCommandWorkingDir, func(m *WizardModel) bool { return m.isCommandOrigin() && m.state.ConnectionKind == "service_exposure" }},
	// How the command runs, and what it is given. Both are properties of a command
	// Portico starts, so they are asked only where Portico starts one.
	{WizardStepCommandShell, func(m *WizardModel) bool {
		return m.isCommandOrigin() && m.state.ConnectionKind == "service_exposure"
	}},
	{WizardStepCommandEnv, func(m *WizardModel) bool {
		return m.isCommandOrigin() && m.state.ConnectionKind == "service_exposure"
	}},
	{WizardStepDirectoryMode, func(m *WizardModel) bool {
		return m.state.SourceType == "directory" && m.state.ConnectionKind == "service_exposure"
	}},
	{WizardStepDirectorySPA, func(m *WizardModel) bool {
		return m.state.SourceType == "directory" && m.state.DirectoryMode == "read" && m.state.ConnectionKind == "service_exposure"
	}},
	{WizardStepMCPTransport, func(m *WizardModel) bool {
		return m.state.SourceType == "mcp_server" && m.state.ConnectionKind == "service_exposure"
	}},
	{WizardStepExposure, func(m *WizardModel) bool { return m.state.ConnectionKind == "service_exposure" }},
	{WizardStepHostname, func(m *WizardModel) bool {
		return m.state.ExposureMode == "permanent_public" && m.state.ConnectionKind == "service_exposure"
	}},
	{WizardStepProtection, func(m *WizardModel) bool { return m.state.ConnectionKind == "service_exposure" }},
	{WizardStepProtectionRules, func(m *WizardModel) bool {
		return m.state.Protection == "email_otp" && m.state.ConnectionKind == "service_exposure"
	}},
	{WizardStepProvider, func(m *WizardModel) bool { return m.state.ConnectionKind == "service_exposure" }},
	{WizardStepAccount, func(m *WizardModel) bool {
		return m.state.ConnectionKind == "service_exposure" && len(m.accountsFor(m.state.Provider)) > 1
	}},
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
	// The current step is not in the sequence. That is normal for the
	// post-creation states, which are not questions, but it can also happen to
	// a question whose predicate stopped holding while the user was on it — an
	// account question after the provider it belonged to went away. Returning
	// "nowhere to go" would strand them there, so fall back to the last
	// applicable question that precedes it.
	for i := len(steps) - 1; i >= 0; i-- {
		if precedes(steps[i], m.state.Step) {
			return steps[i], true
		}
	}
	return 0, false
}

// precedes reports whether one step is asked before another in the canonical
// order.
func precedes(a, b int) bool {
	var seenA bool
	for _, rule := range wizardStepOrder {
		switch rule.id {
		case a:
			seenA = true
		case b:
			return seenA
		}
	}
	return false
}

// goBack moves to the previous applicable question and restores whatever that
// question was showing.
//
// Restoring the input here, once, is the other half of what the hand-written
// handlers each did separately: several of them set m.inputValue() and several forgot
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
	m.setInput("")
	switch m.state.Step {
	case WizardStepName:
		m.setInput(m.state.Name)
	case WizardStepPortForwardLocalPort:
		m.setInput(m.state.PortForwardLocalPort)
	case WizardStepPortForwardRemoteHost:
		m.setInput(m.state.PortForwardRemoteHost)
	case WizardStepPortForwardRemotePort:
		m.setInput(m.state.PortForwardRemotePort)
	case WizardStepSource:
		m.setInput(m.state.SourceAddress)
	case WizardStepPort:
		m.setInput(m.state.Port)
	case WizardStepCommandArgs:
		m.setInput(commandArgsInput(m.state.CommandArgs))
	case WizardStepCommandWorkingDir:
		m.setInput(m.state.WorkingDir)
	case WizardStepCommandEnv:
		m.setInput(commandEnvInput(m.state.CommandEnv))
	case WizardStepCommandShell:
		m.selected = boolIndex(m.state.CommandUseShell)
	case WizardStepHostname:
		m.setInput(m.state.Hostname)
	case WizardStepProtectionRules:
		m.setInput(ProtectionRulesInput(m.state.AllowedEmails, m.state.AllowedDomains))
	case WizardStepTunnelID:
		m.setInput(m.state.TunnelID)
	case WizardStepTunnelMCP:
		m.setInput(m.state.SourceAddress)
	case WizardStepTunnelProfile:
		m.setInput(m.state.TunnelProfile)

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
	case WizardStepDiscovery:
		// Returning to the question positions the cursor on the service already
		// chosen, so going back does not silently move the user's answer.
		m.selected = 0
		for i, svc := range m.discovered {
			if svc.Address == m.state.SourceAddress {
				m.selected = i
			}
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
		for i, account := range m.accountsFor(m.state.Provider) {
			if account.ID == m.state.AccountID {
				m.selected = i
			}
		}
	// Port-forward protocol: default to TCP (index 0).
	case WizardStepPortForwardProtocol:
		m.selected = 0
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

// discardProtectionIfUnavailable drops a protection choice that the current
// answers can no longer carry.
//
// An answer must not survive the invalidation of an answer it depends on. The
// alternative is a combination that looks chosen, is rejected by core
// validation, and reports the problem at create — a long way from the question
// that caused it.
func (m *WizardModel) discardProtectionIfUnavailable() {
	if m.state.Protection == "" {
		return
	}
	for _, choice := range m.protectionChoices() {
		if choice.Value == m.state.Protection {
			if choice.Available {
				return
			}
			break
		}
	}
	m.state.Protection = ""
	m.state.AllowedEmails = nil
	m.state.AllowedDomains = nil
}
