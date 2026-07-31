package supervisor

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/store"
)

func journalEvent(step, eventType, stage, summary, errText, ts string) store.OperationJournalEvent {
	return store.OperationJournalEvent{
		StepID: step, EventType: eventType, Stage: stage,
		Summary: summary, Error: errText, Timestamp: ts,
	}
}

// TestStepsAreRebuiltFromEveryEventNotTheLast pins the reconstruction that an
// operation's history depends on after a restart.
//
// Overwriting each step with the latest event kept only that event's fields, so
// state, error and timings were lost and every step rendered as still pending
// however it had actually ended.
func TestStepsAreRebuiltFromEveryEventNotTheLast(t *testing.T) {
	steps := operationSteps([]store.OperationJournalEvent{
		journalEvent("step-1", string(core.EventOperationStepStarted), "", "Create tunnel", "", "t1"),
		journalEvent("step-1", string(core.EventOperationStepSucceeded), "", "Create tunnel", "", "t2"),
		journalEvent("step-2", string(core.EventOperationStepStarted), "", "Start connector", "", "t3"),
		// A failure often carries the error and no summary.
		journalEvent("step-2", string(core.EventOperationStepFailed), "", "", "connector exited", "t4"),
	})

	if len(steps) != 2 {
		t.Fatalf("rebuilt %d steps, want 2", len(steps))
	}
	if steps[0].State != ipc.StepSucceeded || steps[0].StartedAt != "t1" || steps[0].CompletedAt != "t2" {
		t.Fatalf("succeeded step = %#v", steps[0])
	}
	if steps[1].State != ipc.StepFailed || steps[1].Error != "connector exited" {
		t.Fatalf("failed step = %#v", steps[1])
	}
	// The summary must survive an event that did not carry one.
	if steps[1].Summary != "Start connector" {
		t.Fatalf("summary was erased by a later event: %q", steps[1].Summary)
	}
	// Plan order is preserved, so the account reads in the order it happened.
	if steps[0].ID != "step-1" || steps[1].ID != "step-2" {
		t.Fatalf("step order = %q, %q", steps[0].ID, steps[1].ID)
	}
}

// TestCompensationIsDistinguishedFromSuccessAndFailure pins that undoing a step
// is reported as its own outcome.
//
// Compensation reports through the same event types, so reading only the type
// showed a rolled-back step as having succeeded, and a compensation that failed
// — which leaves something behind — as an ordinary failure.
func TestCompensationIsDistinguishedFromSuccessAndFailure(t *testing.T) {
	steps := operationSteps([]store.OperationJournalEvent{
		journalEvent("step-1", string(core.EventOperationStepStarted), "", "Create DNS record", "", "t1"),
		journalEvent("step-1", string(core.EventOperationStepSucceeded), "", "Create DNS record", "", "t2"),
		journalEvent("step-1", string(core.EventOperationStepSucceeded),
			string(core.StageCompensated), "Remove DNS record", "", "t3"),
		journalEvent("step-2", string(core.EventOperationStepStarted), "", "Create tunnel", "", "t4"),
		journalEvent("step-2", string(core.EventOperationStepFailed),
			string(core.StageCompensated), "", "could not delete tunnel", "t5"),
	})

	if steps[0].State != ipc.StepCompensated {
		t.Fatalf("an undone step reports %q, want compensated", steps[0].State)
	}
	if steps[1].State != ipc.StepCompensationFailed {
		t.Fatalf("a failed rollback reports %q; it left something behind and must say so", steps[1].State)
	}
	if steps[1].Error != "could not delete tunnel" {
		t.Fatalf("the rollback error was lost: %#v", steps[1])
	}
}

// TestAStepWithNoStartEventIsStillReported covers a journal missing its first
// event, which a crash between write and commit can produce.
func TestAStepWithNoStartEventIsStillReported(t *testing.T) {
	steps := operationSteps([]store.OperationJournalEvent{
		journalEvent("step-1", string(core.EventOperationStepSucceeded), "", "Create tunnel", "", "t2"),
	})
	if len(steps) != 1 || steps[0].State != ipc.StepSucceeded {
		t.Fatalf("steps = %#v", steps)
	}
	if steps[0].CompletedAt != "t2" {
		t.Fatalf("completion time lost: %#v", steps[0])
	}
}

// TestAReplayedEventDoesNotChangeTheOutcome pins that the fold is idempotent,
// because the journal is replayed on reconnect.
func TestAReplayedEventDoesNotChangeTheOutcome(t *testing.T) {
	events := []store.OperationJournalEvent{
		journalEvent("step-1", string(core.EventOperationStepStarted), "", "Create tunnel", "", "t1"),
		journalEvent("step-1", string(core.EventOperationStepSucceeded), "", "Create tunnel", "", "t2"),
	}
	once := operationSteps(events)
	twice := operationSteps(append(events, events...))

	if len(once) != len(twice) {
		t.Fatalf("replay changed the step count: %d then %d", len(once), len(twice))
	}
	if once[0] != twice[0] {
		t.Fatalf("replay changed the outcome:\nonce  %#v\ntwice %#v", once[0], twice[0])
	}
}
