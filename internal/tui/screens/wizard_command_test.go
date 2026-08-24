package screens

import (
	"strings"
	"testing"
)

// Configuring a command Portico runs.
//
// The command source DTO carries an environment and a shell-mode flag, the origin
// manager honours both, the CLI exposes both, and core validates both. The wizard
// exposed neither — so a command needing an API key or a shell pipeline could be
// created from the command line and not from the interface most people use.

// TestParseCommandEnvReadsPairs pins the input format.
func TestParseCommandEnvReadsPairs(t *testing.T) {
	env, err := ParseCommandEnv("PORT=8080, NODE_ENV=production")
	if err != nil {
		t.Fatalf("ParseCommandEnv: %v", err)
	}
	if env["PORT"] != "8080" || env["NODE_ENV"] != "production" {
		t.Fatalf("parsed %v", env)
	}

	// Empty means no environment, not an error: most commands need none.
	if env, err := ParseCommandEnv("   "); err != nil || env != nil {
		t.Fatalf("an empty environment gave %v, %v", env, err)
	}
}

// TestParseCommandEnvRefusesMalformedInput pins that a mistake is caught on the
// question that asked for it.
func TestParseCommandEnvRefusesMalformedInput(t *testing.T) {
	for _, input := range []string{
		"PORT",           // no equals sign
		"=8080",          // no name
		"A=1, A=2",       // the same variable twice
		"API_TOKEN=abcd", // a literal credential
	} {
		if _, err := ParseCommandEnv(input); err == nil {
			t.Errorf("%q was accepted", input)
		}
	}
}

// TestParseCommandEnvAcceptsAReference pins the safe construction.
func TestParseCommandEnvAcceptsAReference(t *testing.T) {
	env, err := ParseCommandEnv("API_TOKEN=env:MY_REAL_TOKEN")
	if err != nil {
		t.Fatalf("a reference was refused: %v", err)
	}
	if env["API_TOKEN"] != "env:MY_REAL_TOKEN" {
		t.Fatalf("the reference was rewritten to %q", env["API_TOKEN"])
	}
}

// TestTheEnvironmentRefusalTeachesTheAlternative pins that the refusal is useful.
//
// A user told only "that looks like a credential" has nowhere to go. The refusal
// comes from core, which is also what blocks create, so there is one rule and one
// wording.
func TestTheEnvironmentRefusalTeachesTheAlternative(t *testing.T) {
	_, err := ParseCommandEnv("DB_PASSWORD=hunter2")
	if err == nil {
		t.Fatal("a literal password was accepted")
	}
	if !strings.Contains(err.Error(), "env:") {
		t.Errorf("the refusal does not name the reference syntax: %v", err)
	}
}

// TestCommandEnvSummaryDisclosesNothing pins what the review may show.
func TestCommandEnvSummaryDisclosesNothing(t *testing.T) {
	lines := CommandEnvSummary(map[string]string{
		"PORT":      "8080",
		"API_TOKEN": "env:MY_REAL_TOKEN",
	})
	joined := strings.Join(lines, "\n")

	// A literal is not printed.
	if strings.Contains(joined, "8080") {
		t.Errorf("the summary discloses a literal value:\n%s", joined)
	}
	if !strings.Contains(joined, "PORT is set") {
		t.Errorf("the summary does not say a literal is present:\n%s", joined)
	}
	// A reference names the variable it reads, because that is not a secret and
	// seeing it is how a user checks they wrote the right one.
	if !strings.Contains(joined, "MY_REAL_TOKEN") {
		t.Errorf("the summary does not name the referenced variable:\n%s", joined)
	}
}

// TestCommandEnvInputRoundTrips pins that going back shows what was entered.
func TestCommandEnvInputRoundTrips(t *testing.T) {
	original := "A=1, B=env:C"
	env, err := ParseCommandEnv(original)
	if err != nil {
		t.Fatal(err)
	}
	// Rendered in a stable order, so returning to the question does not reshuffle
	// what the user typed.
	rendered := commandEnvInput(env)
	again, err := ParseCommandEnv(rendered)
	if err != nil {
		t.Fatalf("the rendered environment does not parse: %q: %v", rendered, err)
	}
	if len(again) != len(env) || again["A"] != "1" || again["B"] != "env:C" {
		t.Fatalf("the round trip changed the environment: %v -> %q -> %v", env, rendered, again)
	}
	if commandEnvInput(nil) != "" {
		t.Error("an empty environment rendered as something")
	}
}

// TestACommandIsConfiguredThroughTheWizard pins the whole path: a command source
// reaching a request that carries the shell flag and the environment.
func TestACommandIsConfiguredThroughTheWizard(t *testing.T) {
	m := NewWizard(nil, quickTunnelOnlySnapshot())
	takeOutcome(t, m, "service_exposure", "api")

	// The fourth outcome runs a command. Reach it by choosing that recipe instead.
	m = NewWizard(nil, quickTunnelOnlySnapshot())
	index := -1
	for i, recipe := range wizardRecipes {
		if recipe.SourceType == "command" {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("no outcome runs a command")
	}
	for range index {
		m.HandleKey("down")
	}
	m.HandleKey("enter")
	typeInto(m, "api")
	m.HandleKey("enter")

	// The executable, the port, the arguments, the working directory.
	typeInto(m, "/usr/bin/myserver")
	m.HandleKey("enter")
	typeInto(m, "8080")
	m.HandleKey("enter")
	m.HandleKey("enter") // no arguments
	m.HandleKey("enter") // Portico's working directory

	if m.Step() != WizardStepCommandShell {
		t.Fatalf("step after the working directory = %d, want the shell question", m.Step())
	}

	// Run it directly.
	m.HandleKey("enter")
	if m.Step() != WizardStepCommandEnv {
		t.Fatalf("step after the shell question = %d, want the environment", m.Step())
	}

	typeInto(m, "API_TOKEN=env:MY_REAL_TOKEN")
	m.HandleKey("enter")
	if m.Step() == WizardStepCommandEnv {
		t.Fatalf("a reference was refused on the environment question: %v", m.err)
	}

	// Drive to review and check the request.
	for m.Step() != WizardStepReview {
		before := m.Step()
		m.HandleKey("enter")
		if m.Step() == before {
			t.Fatalf("the wizard stalled on step %d: %v", before, m.err)
		}
	}

	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Source.Command == nil {
		t.Fatal("the request carries no command")
	}
	if req.Source.Command.UseShell {
		t.Error("the command was marked as shell-mode when direct was chosen")
	}
	if got := req.Source.Command.Env["API_TOKEN"]; got != "env:MY_REAL_TOKEN" {
		t.Fatalf("the environment did not reach the request: %v", req.Source.Command.Env)
	}
	// The review says what the command is given, without the value.
	view := m.View()
	if !strings.Contains(view, "MY_REAL_TOKEN") {
		t.Errorf("the review does not say where the value comes from:\n%s", view)
	}
}

// TestShellModeRefusesSeparateArguments pins the combination core rejects.
//
// A shell parses the whole line itself, so separate argv entries would be silently
// ignored. Refusing on the question explains it where the user can act; core's
// refusal at create time is a long way from the decision that caused it.
func TestShellModeRefusesSeparateArguments(t *testing.T) {
	m := NewWizard(nil, quickTunnelOnlySnapshot())
	m.state = WizardState{
		Step:           WizardStepCommandShell,
		ConnectionKind: "service_exposure",
		SourceType:     "command",
		CommandArgs:    []string{"--flag"},
	}
	m.selected = 1 // run through a shell
	m.HandleKey("enter")

	if m.Step() != WizardStepCommandShell {
		t.Fatal("shell mode was accepted alongside separate arguments")
	}
	if m.err == nil || !strings.Contains(m.err.Error(), "arguments") {
		t.Errorf("the refusal does not explain the conflict: %v", m.err)
	}

	// Without arguments it is accepted, and recorded.
	m.state.CommandArgs = nil
	m.HandleKey("enter")
	if !m.state.CommandUseShell {
		t.Fatal("shell mode was not recorded")
	}
	if m.Step() != WizardStepCommandEnv {
		t.Fatalf("step = %d, want the environment question", m.Step())
	}
}
