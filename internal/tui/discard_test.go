package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Not losing work to one keystroke.
//
// Escape discarded an edit silently. An edit is an accumulation of decisions —
// the whole purpose of the screen is to collect them — and throwing them away for
// a single keypress, with no acknowledgement, is the cheapest possible way to
// destroy the most expensive thing on screen. The same was true of the wizard,
// which is a longer sequence still.

// TestLeavingADirtyEditAsksFirst pins the confirmation.
func TestLeavingADirtyEditAsksFirst(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	m.width = 100
	name := "renamed"
	m.edit.name = &name

	if !m.edit.dirty() {
		t.Fatal("the fixture is not dirty, so this proves nothing")
	}

	next, _ := m.Update(keyMsg("esc"))
	m = next.(Model)

	if m.screen != ScreenEdit {
		t.Fatalf("escape left the edit screen anyway: now on %s", m.screen)
	}
	if m.edit == nil {
		t.Fatal("escape discarded the edit without asking")
	}
	if !m.edit.confirmingDiscard {
		t.Fatal("escape did not raise a discard confirmation")
	}

	// The question says what would be lost.
	view := m.View().Content
	if !strings.Contains(view, "unsaved changes") {
		t.Errorf("the confirmation does not say there are unsaved changes:\n%s", view)
	}
	if !strings.Contains(view, "renamed") {
		t.Errorf("the confirmation does not show what would be discarded:\n%s", view)
	}
	// And it reassures: nothing has been applied, so nothing is half-done.
	if !strings.Contains(view, "Nothing has been saved") {
		t.Errorf("the confirmation does not say nothing was applied:\n%s", view)
	}
}

// TestKeepingADirtyEditReturnsToIt pins the answer that preserves the work.
func TestKeepingADirtyEditReturnsToIt(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	name := "renamed"
	m.edit.name = &name

	next, _ := m.Update(keyMsg("esc"))
	m = next.(Model)
	next, _ = m.Update(keyMsg("n"))
	m = next.(Model)

	if m.edit == nil {
		t.Fatal("declining the discard threw the edit away anyway")
	}
	if m.edit.confirmingDiscard {
		t.Fatal("the confirmation is still up after being answered")
	}
	if m.edit.name == nil || *m.edit.name != "renamed" {
		t.Fatal("the pending change did not survive the confirmation")
	}
	if m.screen != ScreenEdit {
		t.Fatalf("declining the discard navigated to %s", m.screen)
	}
}

// TestDiscardingADirtyEditLeaves pins the other answer.
func TestDiscardingADirtyEditLeaves(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	name := "renamed"
	m.edit.name = &name

	next, _ := m.Update(keyMsg("esc"))
	m = next.(Model)
	next, _ = m.Update(keyMsg("y"))
	m = next.(Model)

	if m.edit != nil {
		t.Fatal("confirming the discard kept the edit")
	}
	if m.screen == ScreenEdit {
		t.Fatal("confirming the discard stayed on the edit screen")
	}
}

// TestACleanEditLeavesWithoutAsking pins that the confirmation is not a nuisance.
//
// An edit with nothing changed has nothing to lose. Asking anyway would train the
// user to dismiss the question without reading it, which is how a confirmation
// stops protecting anything.
func TestACleanEditLeavesWithoutAsking(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	if m.edit.dirty() {
		t.Fatal("the fixture starts dirty")
	}

	next, _ := m.Update(keyMsg("esc"))
	m = next.(Model)

	if m.edit != nil && m.edit.confirmingDiscard {
		t.Fatal("leaving an unchanged edit asked for confirmation")
	}
	if m.screen == ScreenEdit {
		t.Fatal("leaving an unchanged edit did not leave")
	}
}

// TestTheConfirmationOnlyAcceptsItsOwnAnswers pins that the edit behind the
// question cannot be operated while it is up.
func TestTheConfirmationOnlyAcceptsItsOwnAnswers(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	name := "renamed"
	m.edit.name = &name
	next, _ := m.Update(keyMsg("esc"))
	m = next.(Model)

	before := m.edit.cursor
	for _, key := range []string{"down", "j", "enter", "p"} {
		next, cmd := m.Update(keyMsg(key))
		m = next.(Model)
		if cmd != nil {
			t.Errorf("%q did something while the confirmation was up", key)
		}
		if m.edit == nil {
			t.Fatalf("%q discarded the edit while the confirmation was up", key)
		}
		if m.edit.cursor != before {
			t.Errorf("%q moved the cursor behind the confirmation", key)
		}
		if !m.edit.confirmingDiscard {
			t.Fatalf("%q dismissed the confirmation", key)
		}
	}
}

// TestTheConfirmationAdvertisesItsAnswers pins that the two answers are offered.
func TestTheConfirmationAdvertisesItsAnswers(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	m.width = 100
	name := "renamed"
	m.edit.name = &name
	next, _ := m.Update(keyMsg("esc"))
	m = next.(Model)

	actions := m.actionsFor(ScreenEdit)
	for _, id := range []ActionID{ActionDiscard, ActionKeepEditing} {
		action, ok := actions.Find(id)
		if !ok {
			t.Fatalf("the confirmation does not offer %s", id)
		}
		if !action.Enabled {
			t.Errorf("%s is offered but disabled", id)
		}
	}
	view := m.View().Content
	if !strings.Contains(view, "Discard") || !strings.Contains(view, "Keep") {
		t.Errorf("the confirmation does not advertise both answers:\n%s", view)
	}
}

// TestLeavingAWizardWithAnswersAsksFirst pins the same protection on the longest
// sequence of decisions in the product.
func TestLeavingAWizardWithAnswersAsksFirst(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.width = 100
	m, _ = press(t, m, "n")
	if m.screen != ScreenNewConnection {
		t.Fatalf("screen = %q, want the wizard", m.screen)
	}

	// Nothing answered yet: leaving is free.
	clean, _ := press(t, m, "esc")
	if clean.screen != ScreenHome {
		t.Fatalf("leaving an untouched wizard asked for confirmation: now %s", clean.screen)
	}

	// Answer something, then try to leave.
	m, _ = press(t, m, "enter") // take the first outcome
	for _, key := range []string{"d", "e", "m", "o"} {
		m, _ = press(t, m, key) // name it
	}
	if !m.wizard.HasAnswers() {
		t.Fatal("the wizard does not consider a typed name an answer")
	}

	m, _ = press(t, m, "esc")
	if m.screen != ScreenNewConnection {
		t.Fatalf("escape abandoned the wizard without asking: now %s", m.screen)
	}
	if !m.wizardConfirmingDiscard {
		t.Fatal("escape did not raise a discard confirmation")
	}

	view := m.View().Content
	if !strings.Contains(view, "not been created") {
		t.Errorf("the confirmation does not say nothing was created:\n%s", view)
	}

	// Declining keeps the answers.
	m, _ = press(t, m, "n")
	if m.wizardConfirmingDiscard {
		t.Fatal("the confirmation survived being declined")
	}
	if m.wizard == nil || !m.wizard.HasAnswers() {
		t.Fatal("declining the discard lost the answers")
	}

	// Confirming leaves.
	m, _ = press(t, m, "esc")
	m, _ = press(t, m, "y")
	if m.screen != ScreenHome {
		t.Fatalf("confirming the discard stayed on %s", m.screen)
	}
	if m.wizard != nil {
		t.Fatal("confirming the discard kept the wizard")
	}
}

// TestWizardProviderSetupPreservesEveryAnswer pins the child-workflow handoff.
//
// A user who reached the provider question and found the recommended provider
// unconfigured had to abandon the wizard, configure the provider elsewhere, and
// answer every question again from the beginning.
func TestWizardProviderSetupPreservesEveryAnswer(t *testing.T) {
	client := &fakeClient{
		setupFlow: &ipc.SetupFlowDTO{
			ProviderID: "cloudflare",
			Kind:       "account",
			Summary:    "Give Portico a Cloudflare API token",
			Fields: []ipc.SetupFieldDTO{
				{ID: "account_id", Label: "Account ID", Required: true},
				{ID: "credential", Label: "API token", Required: true, Secret: true},
			},
		},
	}
	m := readyModel(client, twoConnectionSnapshot())
	m.width = 100
	m, _ = press(t, m, "n")

	// Drive to the provider question with a name committed. Enter is what commits
	// a typed answer, which is why it is pressed here: an uncommitted field is
	// still being edited, not yet an answer the wizard holds.
	m, _ = press(t, m, "enter")
	for _, key := range []string{"w", "e", "b"} {
		m, _ = press(t, m, key)
	}
	m, _ = press(t, m, "enter")
	nameBefore := "web"
	if got := m.wizard.NameAnswer(); got != nameBefore {
		t.Fatalf("the name was not recorded before setup: %q", got)
	}

	// Ask the wizard to configure a provider from wherever it currently is. The
	// request is recorded by the wizard and performed by the root model, so the
	// wizard is not torn down.
	m.wizard.RequestProviderSetup("cloudflare")
	next, cmd, started := m.wizardProviderSetupCmd()
	if !started {
		t.Fatal("the wizard's setup request was not picked up")
	}
	m = next
	if cmd == nil {
		t.Fatal("starting provider setup produced no command")
	}

	// The wizard survives, and so does the answer.
	if m.wizard == nil {
		t.Fatal("provider setup tore down the wizard")
	}
	if m.screen != ScreenProviderSetup {
		t.Fatalf("provider setup did not begin: screen=%s", m.screen)
	}
	if m.resumeWizardAfterSetup != "cloudflare" {
		t.Fatalf("the return path was not recorded: %q", m.resumeWizardAfterSetup)
	}

	// Finishing setup returns to the wizard, on the provider question, with the
	// answers intact and the recommendation recomputed.
	resumeCmd := m.finishProviderSetup()
	if resumeCmd == nil {
		t.Fatal("finishing setup did not refresh anything")
	}
	if m.screen != ScreenNewConnection {
		t.Fatalf("finishing setup landed on %s, not the wizard", m.screen)
	}
	if m.screen == ScreenProviderSetup {
		t.Fatal("the setup form is still open after finishing")
	}
	if m.resumeWizardAfterSetup != "" {
		t.Fatal("the return path was not consumed, so it could fire twice")
	}
	if got := m.wizard.NameAnswer(); got != nameBefore {
		t.Fatalf("the name answer was lost: %q, want %q", got, nameBefore)
	}
}
