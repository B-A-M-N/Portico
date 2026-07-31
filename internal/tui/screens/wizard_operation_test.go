package screens

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// TestACompletedOpenFinishesTheWizard pins that a connection which opened
// successfully stops the wizard waiting.
//
// The wizard watched for "succeeded", which an operation never reaches, so
// "save and open" polled indefinitely after the open had already worked and the
// screen went on saying it was in progress.
func TestACompletedOpenFinishesTheWizard(t *testing.T) {
	m := NewWizard(&fakeWizardClient{}, fullCloudflareSnapshot())
	m.state.Step = WizardStepOperationWait
	m.openAfterCreate = true

	m.HandleOperationLoaded(WizardOperationLoadedMsg{
		Operation: &ipc.OperationDTO{ID: "op-1", State: ipc.OperationCompleted},
	})

	if m.Step() != WizardStepComplete {
		t.Fatalf("step = %d, want complete; the operation had finished", m.Step())
	}
	view := m.renderComplete()
	if !strings.Contains(view, "opened successfully") {
		t.Fatalf("a successful open was not reported as one:\n%s", view)
	}
}

// TestAFailedOpenAlsoFinishesTheWizard covers the other terminal state.
func TestAFailedOpenAlsoFinishesTheWizard(t *testing.T) {
	m := NewWizard(&fakeWizardClient{}, fullCloudflareSnapshot())
	m.state.Step = WizardStepOperationWait
	m.openAfterCreate = true

	m.HandleOperationLoaded(WizardOperationLoadedMsg{
		Operation: &ipc.OperationDTO{ID: "op-1", State: ipc.OperationFailed},
	})

	if m.Step() != WizardStepComplete {
		t.Fatalf("step = %d, want complete", m.Step())
	}
	if !strings.Contains(m.renderComplete(), "opening failed") {
		t.Fatal("a failed open was not reported")
	}
}

// TestARunningOperationKeepsTheWizardWaiting keeps the check meaningful.
func TestARunningOperationKeepsTheWizardWaiting(t *testing.T) {
	m := NewWizard(&fakeWizardClient{}, fullCloudflareSnapshot())
	m.state.Step = WizardStepOperationWait
	m.openAfterCreate = true

	m.HandleOperationLoaded(WizardOperationLoadedMsg{
		Operation: &ipc.OperationDTO{ID: "op-1", State: ipc.OperationRunning},
	})

	if m.Step() == WizardStepComplete {
		t.Fatal("the wizard finished while the operation was still running")
	}
}
