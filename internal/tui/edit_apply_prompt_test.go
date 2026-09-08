package tui

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// An applied edit's changes are saved. Leaving the edit afterwards must not
// ask whether to discard "unsaved changes" — the operation completed saving
// them, and a prompt that says nothing was saved after an operation that says
// it completed is the screen contradicting itself. The pending deltas are
// cleared on apply, so dirty() is false and escape leaves without asking.
func TestAppliedEditLeavesWithoutADiscardPrompt(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.screen = ScreenEdit
	// The pending delta is exactly what committing a renamed name field sets;
	// building it directly keeps the test about the apply boundary rather
	// than about driving the field. The detail is what rows() reads the
	// current value from.
	m.edit = &editState{
		connectionID: "conn-a",
		detail:       &ipc.ConnectionDetailDTO{Summary: ipc.ConnectionDTO{ID: "conn-a", Name: "conn-a"}},
	}
	renamed := "conn-a-renamed"
	m.edit.name = &renamed
	if !m.edit.dirty() {
		t.Fatal("precondition: a pending name change must mark the edit dirty")
	}

	// The plan for that change is applied. planConnectionID names the
	// connection the applied plan was previewed for — the guard that keeps an
	// apply for one connection from clearing another edit's pending values.
	m.planConnectionID = "conn-a"
	next, _ := m.Update(planAppliedMsg{Operation: &ipc.OperationDTO{ID: "op-1", State: "running"}})
	m = next.(Model)
	if m.edit == nil {
		t.Fatal("the applied edit was dropped entirely; only its pending values should clear")
	}
	if m.edit.dirty() {
		t.Fatal("the applied change is still pending after the operation started")
	}

	// Escape leaves the edit directly: no discard question stands in the way.
	next, _ = m.Update(keyMsg("esc"))
	m = next.(Model)
	if m.edit != nil && m.edit.confirmingDiscard {
		t.Fatal("escape after an applied edit asked whether to discard saved changes")
	}
}

// Clearing the pending deltas is only half of applying: the edit still holds
// the snapshot it was opened with, so without a detail refresh the screen
// clears "unsaved changes" and then displays the old name — a silent revert.
// When the operation reaches a terminal state, the edit's detail must be
// refetched, and the delivered committed values must be what the edit shows.
func TestAppliedEditRefreshesDetailFromTheCommittedState(t *testing.T) {
	fake := &fakeClient{}
	m := readyModel(fake, twoConnectionSnapshot())
	m.screen = ScreenEdit
	m.edit = &editState{
		connectionID: "conn-a",
		detail:       &ipc.ConnectionDetailDTO{Summary: ipc.ConnectionDTO{ID: "conn-a", Name: "conn-a"}},
	}
	renamed := "conn-a-renamed"
	m.edit.name = &renamed
	m.planConnectionID = "conn-a"
	next, _ := m.Update(planAppliedMsg{Operation: &ipc.OperationDTO{ID: "op-1", ConnectionID: "conn-a", State: "running"}})
	m = next.(Model)

	// While the operation runs, its state is not yet the connection's
	// committed state: no detail refresh can be justified from it.
	running := &ipc.OperationDTO{ID: "op-1", ConnectionID: "conn-a", State: "running"}
	next, _ = m.Update(operationLoadedMsg{Token: m.operationRequests.current, Operation: running})
	m = next.(Model)
	if fake.detailCalls != 0 {
		t.Fatalf("a running operation already refetched the edit's detail (%d calls); only a terminal state commits", fake.detailCalls)
	}

	// The operation completes. The edit's snapshot is now behind the durable
	// state, so it must be refetched.
	completed := &ipc.OperationDTO{ID: "op-1", ConnectionID: "conn-a", State: "completed"}
	next, cmd := m.Update(operationLoadedMsg{Token: m.operationRequests.current, Operation: completed})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("a terminal operation for an open edit issued no follow-up work")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if fake.detailCalls == 0 {
		t.Fatal("the edit's detail was not refreshed after its operation reached a terminal state; the edit still displays the pre-edit snapshot")
	}

	// The committed values arrive and land in the edit: what the screen shows
	// is what the operation saved, not the snapshot the edit opened with.
	committed := &ipc.ConnectionDetailDTO{Summary: ipc.ConnectionDTO{ID: "conn-a", Name: renamed}}
	next, _ = m.Update(connectionDetailMsg{ConnectionID: "conn-a", Detail: committed})
	m = next.(Model)
	if m.edit == nil || m.edit.detail == nil || m.edit.detail.Summary.Name != renamed {
		t.Fatalf("the applied rename did not land in the edit's detail: %+v", m.edit)
	}
}
