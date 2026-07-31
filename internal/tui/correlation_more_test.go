package tui

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The correlation authority was introduced for plans and diagnostics and not
// applied to everything else that replaces screen state. These pin the paths a
// review found still open.

// TestALateOperationRefreshDoesNotReplaceTheOneOnScreen pins that watching one
// operation cannot be interrupted by a refresh started for another.
//
// Sequence: operation A is displayed, a refresh for A starts, the user leaves
// and starts operation B, A's refresh lands. Without correlation B's progress
// screen is replaced by A — the user watches the wrong operation finish.
func TestALateOperationRefreshDoesNotReplaceTheOneOnScreen(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.operation = &ipc.OperationDTO{ID: "op-a", State: ipc.OperationRunning}
	m.transitionTo(ScreenOperationProgress)

	m.getOperationCmd("op-a")
	stale := m.operationRequests.current

	// The user moves to another operation.
	m.operation = &ipc.OperationDTO{ID: "op-b", State: ipc.OperationRunning}
	m.getOperationCmd("op-b")

	next, _ := m.Update(operationLoadedMsg{
		Token:     stale,
		Operation: &ipc.OperationDTO{ID: "op-a", State: ipc.OperationCompleted},
	})
	m = next.(Model)

	if m.operation == nil || m.operation.ID != "op-b" {
		t.Fatalf("the screen now shows %#v, want op-b", m.operation)
	}
}

// TestLeavingProgressAbandonsItsRefresh pins that a refresh in flight cannot
// install an operation after the user has left the screen.
func TestLeavingProgressAbandonsItsRefresh(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.operation = &ipc.OperationDTO{ID: "op-a", State: ipc.OperationRunning}
	m.transitionTo(ScreenOperationProgress)
	m.getOperationCmd("op-a")
	stale := m.operationRequests.current

	next, _ := m.Update(keyMsg("esc"))
	m = next.(Model)

	next, _ = m.Update(operationLoadedMsg{
		Token:     stale,
		Operation: &ipc.OperationDTO{ID: "op-a", State: ipc.OperationCompleted},
	})
	m = next.(Model)

	if m.operation != nil && m.operation.State == ipc.OperationCompleted {
		t.Fatal("a refresh landed after the screen was abandoned")
	}
}

// TestACopyCannotBeSubmittedTwice pins that a second Enter does not create a
// second connection while the first create is in flight.
func TestACopyCannotBeSubmittedTwice(t *testing.T) {
	client := &fakeClient{}
	m := cloningModel(t, client, exposedDetail())
	m.clone.hostField.SetValue("copy.example.com")

	next, first := m.Update(keyMsg("enter"))
	m = next.(Model)
	if first == nil {
		t.Fatal("the first enter did not create the copy")
	}
	if !m.clone.submitting {
		t.Fatal("the copy is not marked as being created")
	}

	next, second := m.Update(keyMsg("enter"))
	m = next.(Model)
	if second != nil {
		t.Fatal("a second enter created a second connection")
	}

	if view := m.renderClone(); !contains(view, "Creating the copy") {
		t.Fatalf("the screen does not say a copy is being created:\n%s", view)
	}
}

// TestALateCopyReplyDoesNotClearAnotherCopy pins the worse sequence: submit
// copy A, leave, begin copy B, then A's reply arrives. Without correlation it
// clears B and reports A's result.
func TestALateCopyReplyDoesNotClearAnotherCopy(t *testing.T) {
	client := &fakeClient{}
	m := cloningModel(t, client, exposedDetail())
	m.clone.hostField.SetValue("copy-a.example.com")

	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)
	stale := m.clone.requests.current

	// The user abandons it and starts another copy.
	next, _ = m.Update(keyMsg("esc"))
	m = next.(Model)
	next2, _ := m.beginClone("conn-b", "beta")
	m = next2
	next, _ = m.Update(connectionDetailMsg{ConnectionID: "conn-b", Detail: forwardDetail()})
	m = next.(Model)

	next, _ = m.Update(connectionClonedMsg{
		Token:      stale,
		Connection: &ipc.ConnectionDTO{ID: "conn-copy-a", Name: "alpha copy"},
	})
	m = next.(Model)

	if m.clone == nil {
		t.Fatal("an abandoned copy's reply cleared the copy in progress")
	}
	if m.clone.sourceID != "conn-b" {
		t.Fatalf("the screen now copies %q", m.clone.sourceID)
	}
}

// TestAnOlderSnapshotDoesNotReplaceANewerOne pins the worst ordering defect
// available here.
//
// Installing an older snapshot after a newer one leaves stale connection state
// beside a newer event cursor — so the events that would have corrected it have
// already been skipped, and nothing will replay them.
func TestAnOlderSnapshotDoesNotReplaceANewerOne(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())

	newer := testSnapshot()
	newer.LastSeq = 200
	newer.Connections = []ipc.ConnectionDTO{{ID: "conn-new", Name: "current", DesiredState: "open"}}
	next, _ := m.Update(snapshotMsg{Snapshot: newer})
	m = next.(Model)

	older := testSnapshot()
	older.LastSeq = 100
	older.Connections = []ipc.ConnectionDTO{{ID: "conn-old", Name: "stale", DesiredState: "closed"}}
	next, _ = m.Update(snapshotMsg{Snapshot: older})
	m = next.(Model)

	if len(m.snapshot.Connections) != 1 || m.snapshot.Connections[0].ID != "conn-new" {
		t.Fatalf("an older snapshot replaced a newer one: %#v", m.snapshot.Connections)
	}
	if m.snapshot.LastSeq != 200 {
		t.Fatalf("the event cursor went backwards to %d", m.snapshot.LastSeq)
	}
}

// TestASmallerHistoryPageDoesNotReplaceALargerOne pins that "show more" is not
// undone by an earlier, smaller request landing after it.
func TestASmallerHistoryPageDoesNotReplaceALargerOne(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.transitionTo(ScreenOperations)

	small := m.historyRequests.start("50")
	large := m.historyRequests.start("100")

	next, _ := m.Update(operationsLoadedMsg{
		Token: large, Available: true, Limit: 100,
		Operations: []ipc.OperationDTO{
			{ID: "op-1", State: ipc.OperationCompleted, Intent: "open"},
			{ID: "op-2", State: ipc.OperationCompleted, Intent: "close"},
		},
	})
	m = next.(Model)

	next, _ = m.Update(operationsLoadedMsg{
		Token: small, Available: true, Limit: 50,
		Operations: []ipc.OperationDTO{{ID: "op-1", State: ipc.OperationCompleted, Intent: "open"}},
	})
	m = next.(Model)

	if len(m.operations) != 2 {
		t.Fatalf("a smaller earlier page replaced the larger one: %d operations", len(m.operations))
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestApplyingAPlanCarriesAnIdempotencyKey pins that the supervisor can
// recognise a retry. The machinery existed end to end — client header, server
// extraction, store lookup — and nothing used it.
func TestApplyingAPlanCarriesAnIdempotencyKey(t *testing.T) {
	client := &fakeClient{operation: &ipc.OperationDTO{ID: "op-1", State: ipc.OperationRunning}}
	m := readyModel(client, twoConnectionSnapshot())
	m.plan = &ipc.PlanDTO{ID: "plan-1", ConnectionID: "conn-a", Intent: "open"}
	m.planConnectionID = "conn-a"
	m.transitionTo(ScreenPlanPreview)

	next, cmd := m.Update(keyMsg("enter"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter did not apply the plan")
	}
	cmd()

	if len(client.applyKeys) != 1 || client.applyKeys[0] == "" {
		t.Fatalf("apply carried no idempotency key: %#v", client.applyKeys)
	}
}

// TestRetryingAnApplyReusesTheSameKey pins that a retry of one approved preview
// is recognised as the same request rather than starting a second operation.
func TestRetryingAnApplyReusesTheSameKey(t *testing.T) {
	client := &fakeClient{operation: &ipc.OperationDTO{ID: "op-1", State: ipc.OperationRunning}}
	m := readyModel(client, twoConnectionSnapshot())
	m.plan = &ipc.PlanDTO{ID: "plan-1", ConnectionID: "conn-a", Intent: "open"}
	m.planConnectionID = "conn-a"

	m.applyPlanCmd("plan-1")()
	m.applyPlanCmd("plan-1")()

	if len(client.applyKeys) != 2 {
		t.Fatalf("expected two attempts, got %d", len(client.applyKeys))
	}
	if client.applyKeys[0] != client.applyKeys[1] {
		t.Fatalf("a retry used a different key: %q then %q", client.applyKeys[0], client.applyKeys[1])
	}
}

// TestANewPreviewIsANewAttempt pins that approving a different plan is not
// treated as a retry of the previous one.
func TestANewPreviewIsANewAttempt(t *testing.T) {
	client := &fakeClient{operation: &ipc.OperationDTO{ID: "op-1", State: ipc.OperationRunning}}
	m := readyModel(client, twoConnectionSnapshot())

	m.applyPlanCmd("plan-1")()
	m.applyPlanCmd("plan-2")()

	if client.applyKeys[0] == client.applyKeys[1] {
		t.Fatal("two different plans shared one idempotency key")
	}
}

// TestATimedOutApplyIsNotReportedAsFailed pins the honest report.
//
// The supervisor continues after the client's context is cancelled, so a
// timeout means the outcome is unknown. Calling it a failure invites a retry
// and states something the interface cannot support.
func TestATimedOutApplyIsNotReportedAsFailed(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.applying = true

	next, _ := m.Update(planAppliedMsg{
		Err:     context.DeadlineExceeded,
		Outcome: applyOutcome(context.DeadlineExceeded),
	})
	m = next.(Model)

	if contains(m.status, "failed") {
		t.Fatalf("a timeout was reported as a failure: %q", m.status)
	}
	if !contains(m.status, "may have started") {
		t.Fatalf("the unknown outcome is not stated: %q", m.status)
	}
	if !m.apply.unknown {
		t.Fatal("the unknown outcome was not recorded")
	}
}
