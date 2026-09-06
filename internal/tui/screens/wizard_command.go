package screens

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/core"
)

// Configuring a command Portico runs.
//
// The command source DTO carries an environment and a shell-mode flag, the origin
// manager honours both, the CLI exposes both, and core validates both. The wizard
// exposed neither — so a command needing an API key or a shell pipeline could be
// created from the command line and not from the interface most people use.
//
// Shell mode is a yes/no question. The environment is a list of names and values,
// where a value can be a reference — env:NAME — resolved from the supervisor's own
// environment when the command starts. That is what makes this safe to offer: the
// connection stores the name of a variable, never its contents, so nothing secret
// reaches the database, a plan, an event or a support export.

// handleCommandShellKey answers whether the command runs through a shell.
func (m *WizardModel) handleCommandShellKey(key string) tea.Cmd {
	switch key {
	case "esc":
		m.goBack()
	case "up", "k":
		if m.selected > 0 {
			m.selected--
		}
	case "down", "j":
		if m.selected < 1 {
			m.selected++
		}
	case "enter":
		useShell := m.selected == 1
		// Core refuses a shell command that also carries argv entries: the shell
		// parses the whole line itself, so separate arguments would be silently
		// ignored. Refusing here explains it on the question rather than at create.
		if useShell && len(m.state.CommandArgs) > 0 {
			m.err = fmt.Errorf(
				"this command already has separate arguments, which a shell cannot also " +
					"receive: go back and clear them, or run it without a shell")
			return nil
		}
		m.err = nil
		m.state.CommandUseShell = useShell
		m.state.Step = WizardStepCommandEnv
		m.setInput(commandEnvInput(m.state.CommandEnv))
	}
	return nil
}

// handleCommandEnvKey collects the command's environment.
func (m *WizardModel) handleCommandEnvKey(key string) tea.Cmd {
	switch key {
	case "esc":
		m.goBack()
	case "enter":
		env, err := ParseCommandEnv(m.inputValue())
		if err != nil {
			m.err = err
			return nil
		}
		m.err = nil
		m.state.CommandEnv = env
		m.state.Step = WizardStepExposure
		m.selected = firstAvailable(m.exposureChoices())
	default:
		m.setInput(editInput(m.inputValue(), key))
	}
	return nil
}

// ParseCommandEnv reads NAME=VALUE pairs, one per comma, into an environment.
//
// A value of env:OTHER is a reference: Portico reads OTHER from its own environment
// when the command starts. Core validates the result, so a credential-shaped name
// carrying a literal is refused here — on the question that asked for it, with the
// reference syntax in the refusal — rather than at create time.
func ParseCommandEnv(input string) (map[string]string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}

	env := map[string]string{}
	for _, entry := range strings.Split(input, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf(
				"%q is not a NAME=VALUE pair; separate several with commas", entry)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" {
			return nil, fmt.Errorf("%q has no variable name before the equals sign", entry)
		}
		if _, duplicate := env[name]; duplicate {
			return nil, fmt.Errorf("%s is given twice", name)
		}
		env[name] = value
	}

	// Core is the authority on what an environment may contain, so its refusal is
	// the one reported. Duplicating the credential-name test here would be a
	// second answer that could disagree with the one that actually blocks create.
	if err := core.ValidateCommandEnvironmentForInput(env); err != nil {
		return nil, err
	}
	return env, nil
}

// commandEnvInput renders an environment back into the line that produced it, so
// returning to the question shows what was entered rather than an empty field.
func commandEnvInput(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+env[name])
	}
	return strings.Join(parts, ", ")
}

// CommandEnvSummary describes an environment without disclosing a literal value.
//
// The review has to say what the command will be given, and a literal value is
// the user's business rather than something to print into a screen they may be
// sharing. A reference is shown in full, because the variable name is not a secret
// and seeing it is how someone checks they wrote the right one.
func CommandEnvSummary(env map[string]string) []string {
	redacted := core.RedactCommandEnvironment(env)
	if len(redacted) == 0 {
		return nil
	}
	names := make([]string, 0, len(redacted))
	for name := range redacted {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]string, 0, len(names))
	for _, name := range names {
		value := redacted[name]
		if core.IsEnvReference(value) {
			out = append(out, name+" is read from Portico's own "+
				core.EnvReferenceName(value)+" when the command starts.")
			continue
		}
		out = append(out, name+" is set.")
	}
	return out
}

// renderCommandShell draws the shell-mode question.
func (m *WizardModel) renderCommandShell() string {
	return m.renderChoices("How should Portico run this command?", []wizardChoice{
		{
			Value: "direct", Label: "Run it directly", Available: true,
			Detail: []string{
				"Portico starts the executable itself, with the arguments you gave.",
				"This is what you want unless the command needs shell features.",
			},
		},
		{
			Value: "shell", Label: "Run it through a shell", Available: true,
			Detail: []string{
				"The whole command line is handed to a shell, so pipes, redirection and",
				"variable expansion work. The shell parses the line itself, so separate",
				"arguments cannot also be given.",
			},
		},
	}, m.selected)
}

// commandShellActions describes the shell-mode question.
func (m *WizardModel) commandShellActions() WizardActions {
	_, canGoBack := m.previousStep()
	return WizardActions{
		{
			Keys: []string{"up", "k"}, Label: "Up", Enabled: true,
			Help: "Move between running the command directly and running it through a shell.",
		},
		{
			Keys: []string{"down", "j"}, Label: "Down", Enabled: true,
			Help: "Move between running the command directly and running it through a shell.",
		},
		{
			Keys: []string{"enter"}, Label: "Choose", Enabled: true, Primary: true,
			Help: "Take the highlighted option.",
		},
		{
			Keys: []string{"esc"}, Label: "Back", Enabled: canGoBack, Primary: true,
			DisabledReason: "this is the first question",
			Help:           "Return to the previous question. Your answers are kept.",
		},
	}
}

// commandEnvActions describes the environment question.
func (m *WizardModel) commandEnvActions() WizardActions {
	_, canGoBack := m.previousStep()
	return WizardActions{
		{
			Keys: []string{"enter"}, Label: "Continue", Enabled: true, Primary: true,
			Help: "Accept these variables. Write NAME=VALUE, separated by commas, or leave " +
				"the field empty for none.",
		},
		{
			Keys: []string{"esc"}, Label: "Back", Enabled: canGoBack, Primary: true,
			DisabledReason: "this is the first question",
			Help:           "Return to the previous question. Your answers are kept.",
		},
		{
			Label: "Secrets", Enabled: true,
			Help: "Do not paste a credential here: a connection's saved configuration is stored " +
				"in the database, shown in previews and written into support reports. Write " +
				"NAME=env:OTHER instead, and Portico reads OTHER from its own environment when " +
				"the command starts — so only the variable's name is ever saved.",
		},
	}
}
