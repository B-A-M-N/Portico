package ipc

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/paoloanzn/portico/internal/store"
)

type nullHandler struct{}

func (nullHandler) HandleSnapshot() (*SnapshotDTO, error)           { return &SnapshotDTO{}, nil }
func (nullHandler) HandleListConnections() ([]ConnectionDTO, error) { return nil, nil }
func (nullHandler) HandleGetConnection(string) (*ConnectionDTO, error) {
	return nil, nil
}
func (nullHandler) HandleCreateConnection(CreateConnectionRequest) (*ConnectionDTO, error) {
	return nil, nil
}
func (nullHandler) HandleUpdateConnection(string, UpdateConnectionRequest) (*ConnectionDTO, error) {
	return nil, nil
}
func (nullHandler) HandlePlanOpen(string) (*PlanDTO, error)   { return nil, nil }
func (nullHandler) HandlePlanClose(string) (*PlanDTO, error)  { return nil, nil }
func (nullHandler) HandlePlanRepair(string) (*PlanDTO, error) { return nil, nil }
func (nullHandler) HandlePlanDelete(string) (*PlanDTO, error) { return nil, nil }
func (nullHandler) HandleApplyPlan(string) (*OperationDTO, error) {
	return nil, nil
}
func (nullHandler) HandleListProviders() ([]ProviderDTO, error) { return nil, nil }
func (nullHandler) HandleAuthenticateProvider(string) error     { return nil }
func (nullHandler) HandleDeleteConnection(string) error         { return nil }
func (nullHandler) HandleGetOperation(string) (*OperationDTO, error) {
	return nil, nil
}
func (nullHandler) HandleGetOperationEvents(string) ([]EventDTO, error) {
	return nil, nil
}
func (nullHandler) HandleDiscovery() (*DiscoveryDTO, error)        { return nil, nil }
func (nullHandler) HandleRefreshDiscovery() (*DiscoveryDTO, error) { return nil, nil }
func (nullHandler) HandleDiagnostics(string) ([]DiagnosticDTO, error) {
	return nil, nil
}
func (nullHandler) HandleSupervisorStop(ctx context.Context) error { return nil }

func newTestServer(t *testing.T, st *store.Store) *Server {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "test.sock")
	s, err := NewServer(sock, nullHandler{}, st)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Published events must receive strictly monotonic database-assigned sequences.
func TestPublishEventMonotonicSequences(t *testing.T) {
	st := openTestStore(t)
	s := newTestServer(t, st)

	for i := 0; i < 5; i++ {
		s.PublishEvent(EventDTO{Type: "test.event", Data: map[string]interface{}{"n": i}})
	}

	events, err := st.GetEventsSince(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("GetEventsSince: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("expected 5 persisted events, got %d", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].Sequence <= events[i-1].Sequence {
			t.Fatalf("sequence not monotonic: %d after %d", events[i].Sequence, events[i-1].Sequence)
		}
	}
	if got := s.CurrentSeq(); got != events[len(events)-1].Sequence {
		t.Fatalf("CurrentSeq %d != last persisted %d", got, events[len(events)-1].Sequence)
	}
}

// Replay must survive a server restart: a new Server over the same store
// resumes the sequence and serves earlier events.
func TestReplaySurvivesRestart(t *testing.T) {
	st := openTestStore(t)

	s1 := newTestServer(t, st)
	s1.PublishEvent(EventDTO{Type: "before.restart", Data: map[string]interface{}{}})
	firstSeq := s1.CurrentSeq()
	if firstSeq == 0 {
		t.Fatal("expected non-zero sequence after publish")
	}

	// Simulate restart: fresh Server instance over the same store.
	s2 := newTestServer(t, st)
	if s2.CurrentSeq() != firstSeq {
		t.Fatalf("restarted server lost sequence: %d != %d", s2.CurrentSeq(), firstSeq)
	}
	s2.PublishEvent(EventDTO{Type: "after.restart", Data: map[string]interface{}{}})

	events, err := st.GetEventsSince(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("GetEventsSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events across restart, got %d", len(events))
	}
	if events[0].Type != "before.restart" || events[1].Type != "after.restart" {
		t.Fatalf("unexpected replay order: %s, %s", events[0].Type, events[1].Type)
	}
	if events[1].Sequence <= events[0].Sequence {
		t.Fatal("post-restart sequence did not continue monotonically")
	}
}

// Replay from a cursor must return only events after it.
func TestReplayFromCursor(t *testing.T) {
	st := openTestStore(t)
	s := newTestServer(t, st)

	for i := 0; i < 3; i++ {
		s.PublishEvent(EventDTO{Type: "evt", Data: map[string]interface{}{"n": i}})
	}
	all, err := st.GetEventsSince(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("GetEventsSince: %v", err)
	}
	cursor := all[0].Sequence

	after, err := st.GetEventsSince(context.Background(), cursor, 100)
	if err != nil {
		t.Fatalf("GetEventsSince(cursor): %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("expected 2 events after cursor, got %d", len(after))
	}
	for _, e := range after {
		if e.Sequence <= cursor {
			t.Fatalf("replay returned event at or before cursor: %d <= %d", e.Sequence, cursor)
		}
	}
}
