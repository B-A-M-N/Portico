package screens

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/B-A-M-N/portico/internal/ipc"
)

func typeInto(m *WizardModel, text string) {
	for _, r := range text {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func pressKey(m *WizardModel, name string) (tea.Cmd, bool) {
	switch name {
	case "left":
		return m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	case "right":
		return m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	case "home":
		return m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	case "end":
		return m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	case "backspace":
		return m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	case "enter":
		return m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	case "esc":
		return m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	}
	t := name
	return m.Update(tea.KeyPressMsg{Code: rune(t[0]), Text: t})
}

func textWizard(step int) *WizardModel {
	m := NewWizard(nil, []ipc.ProviderDTO{})
	m.state.Step = step
	return m
}

// TestATypoInTheMiddleCanBeFixedWithoutRetypingTheRest pins audit finding 15.
//
// The field was a plain string with two operations: append a rune, or drop the
// last one. Correcting the third character of a long filesystem path meant
// deleting back to it and retyping everything after.
func TestATypoInTheMiddleCanBeFixedWithoutRetypingTheRest(t *testing.T) {
	m := textWizard(WizardStepCommandWorkingDir)
	typeInto(m, "/home/usr/projects")

	// Move back to just after "/home/us" and insert the missing "e".
	for i := 0; i < len("r/projects"); i++ {
		pressKey(m, "left")
	}
	typeInto(m, "e")

	if got := m.inputValue(); got != "/home/user/projects" {
		t.Fatalf("value = %q, want /home/user/projects", got)
	}
}

// TestHomeAndEndReachBothEndsOfTheValue pins that the cursor can move as it
// does in every other text field.
func TestHomeAndEndReachBothEndsOfTheValue(t *testing.T) {
	m := textWizard(WizardStepHostname)
	typeInto(m, "example.com")

	pressKey(m, "home")
	typeInto(m, "app.")
	if got := m.inputValue(); got != "app.example.com" {
		t.Fatalf("after home, value = %q, want app.example.com", got)
	}

	pressKey(m, "end")
	typeInto(m, ".uk")
	if got := m.inputValue(); got != "app.example.com.uk" {
		t.Fatalf("after end, value = %q, want app.example.com.uk", got)
	}
}

// TestAPastedValueIsAccepted pins audit finding 16.
//
// A paste arrives as its own message, not as a key press, so it never reached
// the input at all. A user who had copied an API token or a hostname had to
// retype it by hand — the exact values most likely to be pasted, and the ones
// where a typo is hardest to spot.
func TestAPastedValueIsAccepted(t *testing.T) {
	m := textWizard(WizardStepHostname)

	_, consumed := m.Update(tea.PasteMsg{Content: "api.example.com"})
	if !consumed {
		t.Fatal("a paste was not accepted by the field")
	}
	if got := m.inputValue(); got != "api.example.com" {
		t.Fatalf("value = %q, want the pasted text", got)
	}
}

// TestAPasteLandsAtTheCursorNotTheEnd pins that paste is an insert.
func TestAPasteLandsAtTheCursorNotTheEnd(t *testing.T) {
	m := textWizard(WizardStepHostname)
	typeInto(m, "app..com")
	for i := 0; i < len(".com"); i++ {
		pressKey(m, "left")
	}

	m.Update(tea.PasteMsg{Content: "example"})
	if got := m.inputValue(); got != "app.example.com" {
		t.Fatalf("value = %q, want the paste inserted at the cursor", got)
	}
}

// TestNavigationKeysAreNotSwallowedByTheField pins that the field takes text
// and leaves the screen's own keys alone. If enter were consumed, the wizard
// could never advance past a text question.
func TestNavigationKeysAreNotSwallowedByTheField(t *testing.T) {
	m := textWizard(WizardStepName)
	for _, key := range []string{"enter", "esc"} {
		if _, consumed := pressKey(m, key); consumed {
			t.Errorf("the field swallowed %q, which belongs to the screen", key)
		}
	}
}

// TestAMenuQuestionDoesNotRouteToTheField pins that j and k still move the
// selection on a menu rather than being typed into an invisible field.
func TestAMenuQuestionDoesNotRouteToTheField(t *testing.T) {
	m := textWizard(WizardStepOutcome)
	if _, consumed := pressKey(m, "j"); consumed {
		t.Fatal("a menu question routed a navigation key into a text field")
	}
	if m.inputValue() != "" {
		t.Fatalf("a menu question collected text: %q", m.inputValue())
	}
}

// TestAValueRestoredForEditingPutsTheCursorAtTheEnd pins that going back to a
// question does not require the user to find the end of their own answer.
func TestAValueRestoredForEditingPutsTheCursorAtTheEnd(t *testing.T) {
	m := textWizard(WizardStepName)
	m.setInput("my connection")
	typeInto(m, "!")
	if got := m.inputValue(); got != "my connection!" {
		t.Fatalf("value = %q, want the cursor at the end", got)
	}
}

// TestASecretFieldDoesNotEchoWhatIsTyped pins that a credential is masked as it
// is entered, not shown to anyone who can see the screen.
func TestASecretFieldDoesNotEchoWhatIsTyped(t *testing.T) {
	field := NewSecretField()
	field.SetValue("cf-token-DO-NOT-LEAK")

	if view := field.View(); strings.Contains(view, "cf-token-DO-NOT-LEAK") {
		t.Fatalf("a secret field rendered its value: %q", view)
	}
	if field.Value() != "cf-token-DO-NOT-LEAK" {
		t.Fatal("masking changed the value the form will submit")
	}
}
