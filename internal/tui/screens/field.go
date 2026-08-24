package screens

import (
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// Text entry.
//
// Every text answer was a plain string with one operation: append a rune, or
// drop the last one. There was no cursor, so a typo in a long path meant
// deleting back to it and retyping the rest. There was no paste: a pasted value
// arrives as a multi-rune key or a dedicated paste message, and both were
// discarded by the "exactly one rune" test — which is how the user was expected
// to enter an API token or a hostname they had copied.
//
// This wraps the standard component so the fields behave the way every other
// text field the user has ever used behaves: arrows, home and end, word motion,
// delete forward, and paste.

// NewField builds a text field with Portico's conventions applied.
func NewField() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.SetWidth(60)
	ti.Focus()
	return ti
}

// NewSecretField builds a text field for a value that must not be shown.
//
// A credential was previously echoed in full while being typed, which is a
// disclosure to anyone near the screen and to any recording of it.
func NewSecretField() textinput.Model {
	ti := NewField()
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	return ti
}

// FieldAccepts reports whether a message is text entry rather than navigation.
//
// The field must see paste messages and printable keys, but must not swallow
// the keys the surrounding screen uses to move between questions. Enter and Esc
// in particular belong to the screen: the field would otherwise consume them
// and the wizard could never advance.
func FieldAccepts(msg tea.Msg) bool {
	switch m := msg.(type) {
	case tea.PasteMsg:
		return true
	case tea.KeyPressMsg:
		switch m.String() {
		case "enter", "esc", "tab", "shift+tab":
			return false
		}
		return true
	default:
		return false
	}
}

// inputValue returns the text currently entered for this question.
func (m *WizardModel) inputValue() string { return m.field.Value() }

// setInput replaces the text and puts the cursor at the end, which is where a
// user resuming an answer expects to continue from.
func (m *WizardModel) setInput(v string) {
	m.field.SetValue(v)
	m.field.CursorEnd()
}

// isTextStep reports whether the current question is answered by typing.
//
// The list is derived from restoreStepInput in wizard_steps.go, which is the
// existing authority on which questions carry text.
func isTextStep(step int) bool {
	switch step {
	case WizardStepName, WizardStepSource, WizardStepPort,
		WizardStepCommandArgs, WizardStepCommandWorkingDir,
		WizardStepHostname, WizardStepProtectionRules,
		WizardStepPortForwardLocalPort, WizardStepPortForwardRemoteHost,
		WizardStepPortForwardRemotePort,
		WizardStepTunnelID, WizardStepTunnelMCP, WizardStepTunnelProfile,
		WizardStepCommandEnv, WizardStepPrivateNetworkAddress:
		return true
	default:
		return false
	}
}

// Update routes a message to the text field when the current question is
// answered by typing, and otherwise leaves it for the screen's key handling.
//
// It returns whether the message was consumed, so the caller does not also
// treat a typed character as a navigation key.
func (m *WizardModel) Update(msg tea.Msg) (tea.Cmd, bool) {
	if !isTextStep(m.state.Step) || !FieldAccepts(msg) {
		return nil, false
	}
	var cmd tea.Cmd
	m.field, cmd = m.field.Update(msg)
	return cmd, true
}
