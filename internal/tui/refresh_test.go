package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// TestAnOpenDetailViewRefreshesWhenItsConnectionChanges pins audit finding 26.
//
// A connection event refreshed only the list. The detail view kept the
// endpoints, resources, findings and route segments it had when the screen
// opened, so watching a connection open showed the list going green next to a
// detail still reporting no address and an unopened route.
func TestAnOpenDetailViewRefreshesWhenItsConnectionChanges(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.screen = ScreenInspect
	m.selectedID = "conn-a"

	cmd := m.handleEvent(ipc.EventDTO{Type: "connection.opened", ConnectionID: "conn-a", Sequence: 1})
	if cmd == nil {
		t.Fatal("a connection event produced no refresh at all")
	}

	// The batch must include a detail fetch, which is observable through the
	// client the command calls.
	if refresh := m.refreshOpenDetailCmd("conn-a"); refresh == nil {
		t.Fatal("the open detail view was not refreshed")
	}
}

// TestAnotherConnectionsEventDoesNotRefetchThisDetail pins that an event storm
// for other connections does not become a fetch storm for this screen.
func TestAnotherConnectionsEventDoesNotRefetchThisDetail(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.screen = ScreenInspect
	m.selectedID = "conn-a"

	if cmd := m.refreshOpenDetailCmd("conn-b"); cmd != nil {
		t.Fatal("another connection's event refetched this connection's detail")
	}
}

// TestNoDetailIsFetchedWhenNoDetailIsOpen pins that the refresh is scoped to a
// screen someone is looking at.
func TestNoDetailIsFetchedWhenNoDetailIsOpen(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.screen = ScreenHome
	m.selectedID = "conn-a"

	if cmd := m.refreshOpenDetailCmd("conn-a"); cmd != nil {
		t.Fatal("a detail was fetched for a screen that is not showing one")
	}
}

// TestACappedHistorySaysItIsCapped pins audit finding 27.
//
// The list was silently limited. A user looking for an operation from last week
// saw the cap and concluded it had not happened, with nothing on screen to
// distinguish "this is all of it" from "this is the first page".
func TestACappedHistorySaysItIsCapped(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenOperations
	m.operations = []ipc.OperationDTO{{ID: "op-1", State: ipc.OperationCompleted, Intent: "open"}}
	m.operationsAvailable = true
	m.operationsTruncated = true
	m.operationsLimit = 50

	view := m.renderOperations()
	if !strings.Contains(view, "There are older ones") {
		t.Fatalf("a capped history does not say so:\n%s", view)
	}
	if !strings.Contains(view, "[m] show more") {
		t.Fatalf("a capped history offers no way to see more:\n%s", view)
	}
}

// TestACompleteHistorySaysItIsComplete pins the other half: when there is
// nothing older, the screen says so rather than leaving it ambiguous.
func TestACompleteHistorySaysItIsComplete(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenOperations
	m.operations = []ipc.OperationDTO{{ID: "op-1", State: ipc.OperationCompleted, Intent: "open"}}
	m.operationsAvailable = true
	m.operationsTruncated = false

	view := m.renderOperations()
	if !strings.Contains(view, "complete history") {
		t.Fatalf("a complete history does not say so:\n%s", view)
	}
	if strings.Contains(view, "show more") {
		t.Fatalf("a complete history offers more that does not exist:\n%s", view)
	}
}

// TestAskingForMoreAsksForMore pins that the key requests a larger page.
func TestAskingForMoreAsksForMore(t *testing.T) {
	client := &fakeClient{history: &ipc.OperationHistoryDTO{Available: true}}
	m := readyModel(client, testSnapshot())
	m.screen = ScreenOperations
	m.operationsTruncated = true
	m.operationsLimit = 50

	next, cmd := m.Update(keyMsg("m"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("asking for more did not load anything")
	}
	cmd()

	if client.historyLimit <= 50 {
		t.Fatalf("asked for %d operations, want more than the 50 already shown", client.historyLimit)
	}
}

// TestAskingForMoreDoesNothingWhenThereIsNoMore pins that the key is inert when
// the history is already complete.
func TestAskingForMoreDoesNothingWhenThereIsNoMore(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenOperations
	m.operationsTruncated = false

	if _, cmd := m.Update(keyMsg("m")); cmd != nil {
		t.Fatal("a complete history was refetched for more")
	}
}
