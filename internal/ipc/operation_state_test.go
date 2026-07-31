package ipc

import "testing"

// TestAnOperationNeverReachesAStepState pins the distinction that made
// successful workflows wait forever.
//
// An operation reaches "completed"; a step reaches "succeeded". Callers waiting
// for an operation tested for the step's word, so a workflow that had already
// succeeded went on polling for a state the system never produces.
func TestAnOperationNeverReachesAStepState(t *testing.T) {
	if OperationTerminal(StepSucceeded) {
		t.Fatal("a step's success state was accepted as an operation's")
	}
	if OperationSucceeded(StepSucceeded) {
		t.Fatal("a step's success state was read as operation success")
	}

	if !OperationTerminal(OperationCompleted) || !OperationSucceeded(OperationCompleted) {
		t.Fatal("a completed operation was not recognised as finished")
	}
	if !OperationTerminal(OperationFailed) {
		t.Fatal("a failed operation was not recognised as finished")
	}
	if OperationSucceeded(OperationFailed) {
		t.Fatal("a failed operation was read as success")
	}

	for _, state := range []string{OperationPending, OperationRunning, ""} {
		if OperationTerminal(state) {
			t.Fatalf("state %q was treated as finished", state)
		}
	}

	// There is no cancelled state. Accepting one invites waiting for something
	// the controller never produces.
	if OperationTerminal("cancelled") {
		t.Fatal("a state the controller never produces was treated as terminal")
	}
}
