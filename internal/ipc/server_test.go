package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/store"
)

type nullHandler struct{}

type diagnosticsErrorHandler struct {
	nullHandler
	err error
}

func (h diagnosticsErrorHandler) HandleDiagnostics(string) ([]DiagnosticDTO, error) {
	return nil, h.err
}

func (nullHandler) HandleSnapshot() (*SnapshotDTO, error)           { return &SnapshotDTO{}, nil }
func (nullHandler) HandleListConnections() ([]ConnectionDTO, error) { return nil, nil }
func (nullHandler) HandleGetConnection(string) (*ConnectionDTO, error) {
	return nil, nil
}
func (nullHandler) HandleGetConnectionDetail(string) (*ConnectionDetailDTO, error) {
	return nil, nil
}
func (nullHandler) HandleCloneConnection(string, CloneConnectionRequest) (*ConnectionDTO, error) {
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
func (nullHandler) HandleApplyPlan(string, string) (*OperationDTO, error) {
	return nil, nil
}
func (nullHandler) HandleListProviders() ([]ProviderDTO, error) { return nil, nil }
func (nullHandler) HandleProviderRecommendation(ProviderRecommendationRequest) (*ProviderRecommendationResponse, error) {
	return &ProviderRecommendationResponse{}, nil
}
func (nullHandler) HandleAuthenticateProvider(string) error { return nil }
func (nullHandler) HandleConfigureProviderAccount(string, ConfigureProviderAccountRequest) (*ConfigureProviderAccountResponse, error) {
	return &ConfigureProviderAccountResponse{}, nil
}
func (nullHandler) HandleGetOperation(string) (*OperationDTO, error) {
	return nil, nil
}
func (nullHandler) HandleGetOperationEvents(string) ([]EventDTO, error) {
	return nil, nil
}
func (nullHandler) HandleOperationHistory() (*OperationHistoryDTO, error) {
	return &OperationHistoryDTO{}, nil
}
func (nullHandler) HandleDiscovery() (*DiscoveryDTO, error)        { return nil, nil }
func (nullHandler) HandleRefreshDiscovery() (*DiscoveryDTO, error) { return nil, nil }
func (nullHandler) HandleDiagnostics(string) ([]DiagnosticDTO, error) {
	return nil, nil
}
func (nullHandler) HandleSupportExport() (*SupportExportDTO, error) {
	return &SupportExportDTO{}, nil
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

func TestPublishEventDoesNotBroadcastWhenDurableAppendFails(t *testing.T) {
	st := openTestStore(t)
	s := newTestServer(t, st)
	listener := make(chan EventDTO, 1)
	s.subs["listener"] = listener
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := s.PublishEvent(EventDTO{Type: "must-not-broadcast"}); err == nil {
		t.Fatal("expected durable append failure")
	}
	select {
	case event := <-listener:
		t.Fatalf("received non-durable event: %+v", event)
	default:
	}
}

func TestPublishEventPreservesSuppliedTimestamp(t *testing.T) {
	st := openTestStore(t)
	s := newTestServer(t, st)
	want := "2026-07-25T12:34:56Z"
	if err := s.PublishEvent(EventDTO{
		OperationID: "op-1", ConnectionID: "conn-1", Type: "timestamped", Stage: "succeeded", Timestamp: want,
		Data: map[string]string{"connection_id": "payload-value-is-not-indexed"},
	}); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	events, err := st.GetDurableEventsSince(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("GetDurableEventsSince: %v", err)
	}
	if len(events) != 1 || events[0].Event.Timestamp.Format(time.RFC3339) != want {
		t.Fatalf("stored timestamp = %+v, want %s", events, want)
	}
	if events[0].OperationID != "op-1" || events[0].ConnectionID != "conn-1" || events[0].Stage != "succeeded" {
		t.Fatalf("durable metadata = %+v", events[0])
	}
}

func TestDispatchCommittedEventsBroadcastsWithoutAppendingDuplicate(t *testing.T) {
	st := openTestStore(t)
	s := newTestServer(t, st)
	_, events := s.subscribe()
	defer s.unsubscribe("1")

	seq, err := st.AppendEvent(context.Background(), "op-1", "conn-1", "operation.step_succeeded", "succeeded", time.Now().UTC(), []byte(`{"step_id":"step-1"}`))
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := s.DispatchCommittedEvents(context.Background()); err != nil {
		t.Fatalf("DispatchCommittedEvents: %v", err)
	}
	select {
	case got := <-events:
		if got.Sequence != seq || got.OperationID != "op-1" || got.ConnectionID != "conn-1" || got.Type != "operation.step_succeeded" {
			t.Fatalf("unexpected dispatched event: %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for committed event dispatch")
	}
	all, err := st.GetDurableEventsSince(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("dispatch must not append a duplicate event, got %d rows", len(all))
	}
}

func TestNewServerRequiresDurableStore(t *testing.T) {
	_, err := NewServer(filepath.Join(t.TempDir(), "test.sock"), nullHandler{}, nil)
	if err == nil {
		t.Fatal("expected nil durable store to be rejected")
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

func TestReplayPagesBeyondThousandEvents(t *testing.T) {
	st := openTestStore(t)
	s := newTestServer(t, st)
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin event seed: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO events (operation_id, connection_id, occurred_at, event_type, stage, payload_json)
		VALUES ('op-1', 'conn-1', '2026-07-25T12:00:00Z', 'replay.test', 'succeeded', '{}')`)
	if err != nil {
		tx.Rollback()
		t.Fatalf("prepare event seed: %v", err)
	}
	for i := 0; i < 1001; i++ {
		if _, err := stmt.Exec(); err != nil {
			stmt.Close()
			tx.Rollback()
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		tx.Rollback()
		t.Fatalf("close event statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit event seed: %v", err)
	}
	highWater := s.CurrentSeq()
	replayed, err := s.getPersistentEventsUntil(0, highWater)
	if err != nil {
		t.Fatalf("getPersistentEventsUntil: %v", err)
	}
	if len(replayed) != 1001 {
		t.Fatalf("replayed %d events, want 1001", len(replayed))
	}
	if replayed[0].Sequence != 1 || replayed[len(replayed)-1].Sequence != highWater {
		t.Fatalf("unexpected replay bounds: first=%d last=%d highWater=%d", replayed[0].Sequence, replayed[len(replayed)-1].Sequence, highWater)
	}
}

func TestWriteHandlerErrorUsesTypedDomainStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "validation", err: core.ErrValidation("invalid input"), want: http.StatusUnprocessableEntity},
		{name: "not found", err: core.ErrProfileNotFound("missing"), want: http.StatusNotFound},
		{name: "conflict", err: core.ErrConnectionExists("existing"), want: http.StatusConflict},
		{name: "stale", err: core.ErrStalePlan("plan", "changed"), want: http.StatusPreconditionFailed},
		{name: "untyped is internal", err: errors.New("profile not found but untyped"), want: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			writeHandlerError(rr, "TEST", tt.err)
			if rr.Code != tt.want {
				t.Fatalf("status=%d, want %d", rr.Code, tt.want)
			}
		})
	}
}

func TestDiagnosticsMapsTypedHandlerError(t *testing.T) {
	st := openTestStore(t)
	socket := filepath.Join(t.TempDir(), "test.sock")
	s, err := NewServer(socket, diagnosticsErrorHandler{err: core.ErrProfileNotFound("missing")}, st)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/diagnostics/missing", nil)
	rr := httptest.NewRecorder()
	s.handleDiagnostics(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("diagnostics status=%d, want %d", rr.Code, http.StatusNotFound)
	}
}

// detailHandler serves a fixed connection detail so the route can be exercised
// independently of the supervisor.
type detailHandler struct {
	nullHandler
	detail *ConnectionDetailDTO
	err    error
	gotID  string
}

func (h *detailHandler) HandleGetConnectionDetail(id string) (*ConnectionDetailDTO, error) {
	h.gotID = id
	return h.detail, h.err
}

// TestConnectionDetailRouteIsServed pins the detail endpoint. The supervisor
// implemented HandleGetConnectionDetail but no route exposed it and no client
// called it, so the inspect screen had no way to obtain authoritative state.
func TestConnectionDetailRouteIsServed(t *testing.T) {
	st := openTestStore(t)
	socket := filepath.Join(t.TempDir(), "test.sock")
	h := &detailHandler{detail: &ConnectionDetailDTO{
		Revision:  4,
		Resources: []ManagedResourceDTO{{Type: "tunnel", ExternalID: "tun-1", Ownership: "managed"}},
	}}
	s, err := NewServer(socket, h, st)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/connections/conn-1/detail", nil)
	rr := httptest.NewRecorder()
	s.handleConnectionByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if h.gotID != "conn-1" {
		t.Fatalf("handler received id %q, want conn-1", h.gotID)
	}
	var got ConnectionDetailDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Revision != 4 {
		t.Fatalf("revision = %d, want 4", got.Revision)
	}
	if len(got.Resources) != 1 || got.Resources[0].ExternalID != "tun-1" {
		t.Fatalf("resources did not survive the round trip: %+v", got.Resources)
	}
}

// TestConnectionDetailRouteMapsTypedErrors ensures a missing connection is a
// 404 rather than a 500.
func TestConnectionDetailRouteMapsTypedErrors(t *testing.T) {
	st := openTestStore(t)
	socket := filepath.Join(t.TempDir(), "test.sock")
	h := &detailHandler{err: core.ErrProfileNotFound("missing")}
	s, err := NewServer(socket, h, st)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/connections/missing/detail", nil)
	rr := httptest.NewRecorder()
	s.handleConnectionByID(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}
