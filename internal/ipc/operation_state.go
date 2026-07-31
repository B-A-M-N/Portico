package ipc

// Operation and step states are different vocabularies, and confusing them
// makes a successful operation look like one still in progress.
//
// An operation reaches "completed" or "failed"; a step reaches "succeeded" or
// "failed". Callers waiting for an operation to finish tested for "succeeded",
// which an operation is never set to, so a workflow that had already succeeded
// waited for a state that could not arrive. The history screen was corrected
// once and the correction did not reach the repair and creation flows, so these
// helpers exist to be the only place the question is answered.
const (
	// OperationPending is admitted but not started.
	OperationPending = "pending"
	// OperationRunning is executing.
	OperationRunning = "running"
	// OperationCompleted is the terminal success state of an operation.
	OperationCompleted = "completed"
	// OperationFailed is the terminal failure state of an operation.
	OperationFailed = "failed"

	// StepSucceeded is the terminal success state of a single step. It is
	// deliberately a different word from an operation's, and comparing one
	// against the other is the mistake this file exists to prevent.
	StepSucceeded = "succeeded"
)

// OperationTerminal reports whether an operation has finished, either way.
//
// There is no cancelled state: the controller admits pending, running,
// completed and failed, and waiting for a state the system never produces is
// how a finished workflow waits forever.
func OperationTerminal(state string) bool {
	return state == OperationCompleted || state == OperationFailed
}

// OperationSucceeded reports whether an operation finished successfully.
func OperationSucceeded(state string) bool {
	return state == OperationCompleted
}
