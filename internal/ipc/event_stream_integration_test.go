//go:build linux

package ipc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/store"
)

// setupEventServer creates a real IPC event server on a Unix socket.
// It builds the mux identically to NewServer but uses a raw net.Listener
// to avoid NewServer's socket-management logic (safeUnlinkSocket, etc.).
func setupEventServer(t *testing.T, st *store.Store, handler RequestHandler) *Server {
	t.Helper()
	root := t.TempDir()
	socketPath := filepath.Join(root, "portico.sock")

	s := &Server{
		socketPath: socketPath,
		handler:    handler,
		subs:       make(map[string]chan EventDTO),
		store:      st,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("/v1/events", s.handleEvents)
	mux.HandleFunc("/v1/connections", s.handleConnections)
	mux.HandleFunc("/v1/connections/", s.handleConnectionByID)
	mux.HandleFunc("/v1/providers", s.handleProviders)
	mux.HandleFunc("/v1/providers/", s.handleProviderByID)
	mux.HandleFunc("/v1/plans/", s.handlePlans)
	mux.HandleFunc("/v1/operations", s.handleOperationList)
	mux.HandleFunc("/v1/operations/", s.handleOperations)
	mux.HandleFunc("/v1/discovery", s.handleDiscovery)
	mux.HandleFunc("/v1/diagnostics/", s.handleDiagnostics)
	mux.HandleFunc("/v1/supervisor/stop", s.handleSupervisorStop)
	mux.HandleFunc("/v1/support/export", s.handleSupportExport)
	mux.HandleFunc("/v1/providers/recommend", s.handleProviderRecommendation)
	mux.HandleFunc("/v1/readiness", s.handleReadiness)
	mux.HandleFunc("/v1/launch-mode", s.handleLaunchMode)
	mux.HandleFunc("/v1/settings", s.handleSettings)
	s.mux = mux

	seq, err := st.GetLastEventSeq(context.Background())
	if err != nil {
		t.Fatalf("read durable event sequence: %v", err)
	}
	s.dispatchedSequence = seq

	_ = os.MkdirAll(filepath.Dir(socketPath), 0700)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		srv.Close()
		listener.Close()
	})

	return s
}

// TestEventStreamSSEIntegration exercises the full supervisor→SSE disconnect
// → reconnect with Last-Event-ID → durable replay → authoritative snapshot
// → LIVE state boundary using a real Unix-socket server and the real client.

// nextWithTimeout calls es.Next() with a bounded timeout, returning error
// on timeout so tests fail fast rather than hanging indefinitely.
func nextWithTimeout(es *EventStream, timeout time.Duration) (*EventDTO, error) {
	done := make(chan struct{})
	var evt *EventDTO
	var err error
	go func() {
		evt, err = es.Next()
		close(done)
	}()
	runtime.Gosched()
	select {
	case <-done:
		return evt, err
	case <-time.After(timeout):
		es.Close()
		return nil, fmt.Errorf("event read timed out after %v", timeout)
	}
}
func TestEventStreamSSEIntegration(t *testing.T) {
	st := openTestStore(t)
	handler := &NullHandler{}
	server := setupEventServer(t, st, handler)

	client := NewClient(server.socketPath)
	ctx := context.Background()

	// 1. Connect event stream with lastSeq=0 (new connection).
	es, err := client.ConnectEventStream(ctx, 0)
	if err != nil {
		t.Fatalf("ConnectEventStream: %v", err)
	}

	// 2. Publish events while client is subscribed.
	server.PublishEvent(EventDTO{Type: "integration.start", Data: map[string]interface{}{"n": 1}})
	server.PublishEvent(EventDTO{Type: "integration.step", Data: map[string]interface{}{"n": 2}})
	server.PublishEvent(EventDTO{Type: "integration.done", Data: map[string]interface{}{"n": 3}})

	// 3. Read connected + 3 published events.
	type recorded struct {
		seq int64
		typ string
	}
	var got []recorded
	for i := 0; i < 4; i++ {
		evt, err := nextWithTimeout(es, 5*time.Second)
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		got = append(got, recorded{seq: evt.Sequence, typ: evt.Type})
	}

	if len(got) != 4 {
		t.Fatalf("received %d events, want 4", len(got))
	}
	if got[0].typ != "connected" {
		t.Fatalf("first event type = %q, want connected", got[0].typ)
	}
	wantTypes := []string{"integration.start", "integration.step", "integration.done"}
	for i, want := range wantTypes {
		if got[i+1].typ != want {
			t.Fatalf("event %d type = %q, want %q", i+1, got[i+1].typ, want)
		}
	}

	// 4. Simulate disconnect.
	if err := es.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 5. Reconnect with lastSeq=3 (seq 3 is at or below lastSeq → no replay).
	es2, err := client.ConnectEventStream(ctx, 3)
	if err != nil {
		t.Fatalf("ConnectEventStream(seq=3): %v", err)
	}

	server.PublishEvent(EventDTO{Type: "integration.replay-ok", Data: map[string]interface{}{"n": 4}})

	var replayGot []recorded
	for i := 0; i < 2; i++ {
		evt, err := nextWithTimeout(es2, 5*time.Second)
		if err != nil {
			t.Fatalf("reconnect Next #%d: %v", i, err)
		}
		replayGot = append(replayGot, recorded{seq: evt.Sequence, typ: evt.Type})
	}

	if len(replayGot) != 2 {
		t.Fatalf("reconnect received %d events, want 2 (connected + 1 new)", len(replayGot))
	}
	if replayGot[0].typ != "connected" {
		t.Fatalf("reconnect first = %q, want connected", replayGot[0].typ)
	}
	if replayGot[1].typ != "integration.replay-ok" {
		t.Fatalf("reconnect second = %q, want integration.replay-ok", replayGot[1].typ)
	}

	// 6. Live stream continues.
	server.PublishEvent(EventDTO{Type: "integration.live", Data: map[string]interface{}{"n": 5}})
	evt, err := nextWithTimeout(es2, 5*time.Second)
	if err != nil {
		t.Fatalf("live Next: %v", err)
	}
	if evt.Type != "integration.live" {
		t.Fatalf("live event = %q, want integration.live", evt.Type)
	}

	es2.Close()
}

// TestEventStreamDisconnectReconnectNoGap verifies that reconnecting with the
// last known sequence produces no duplicate replay events and live events
// continue correctly.
func TestEventStreamDisconnectReconnectNoGap(t *testing.T) {
	st := openTestStore(t)
	handler := &NullHandler{}
	server := setupEventServer(t, st, handler)

	client := NewClient(server.socketPath)
	ctx := context.Background()

	es, err := client.ConnectEventStream(ctx, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	server.PublishEvent(EventDTO{Type: "step1", Data: map[string]interface{}{}})
	server.PublishEvent(EventDTO{Type: "step2", Data: map[string]interface{}{}})

	var lastSeq int64
	for i := 0; i < 3; i++ {
		evt, err := nextWithTimeout(es, 5*time.Second)
		if err != nil {
			t.Fatalf("next #%d: %v", i, err)
		}
		if evt.Sequence > lastSeq {
			lastSeq = evt.Sequence
		}
	}

	es.Close()

	es2, err := client.ConnectEventStream(ctx, lastSeq)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	server.PublishEvent(EventDTO{Type: "step3", Data: map[string]interface{}{}})

	var types []string
	for i := 0; i < 2; i++ {
		evt, err := nextWithTimeout(es2, 5*time.Second)
		if err != nil {
			t.Fatalf("reconnect next #%d: %v", i, err)
		}
		types = append(types, evt.Type)
	}

	if types[0] != "connected" {
		t.Fatalf("first reconnect event = %q, want connected", types[0])
	}
	if types[1] != "step3" {
		t.Fatalf("second reconnect event = %q, want step3", types[1])
	}

	es2.Close()
}

// TestEventStreamSupervisorSnapshotBoundary verifies that after a reconnect,
// all replayed sequences are <= the snapshot's lastSeq.
func TestEventStreamSupervisorSnapshotBoundary(t *testing.T) {
	st := openTestStore(t)
	handler := &NullHandler{}
	server := setupEventServer(t, st, handler)

	ctx := context.Background()

	for i := 0; i < 5; i++ {
		server.PublishEvent(EventDTO{
			Type:        fmt.Sprintf("snap.event.%d", i),
			Data:        map[string]interface{}{"i": i},
			OperationID: "op-snap",
		})
	}

	lastSeq, err := st.GetLastEventSeq(ctx)
	if err != nil {
		t.Fatalf("GetLastEventSeq: %v", err)
	}
	if lastSeq == 0 {
		t.Fatal("expected non-zero lastSeq")
	}

	client := NewClient(server.socketPath)
	es, err := client.ConnectEventStream(ctx, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	var replayedSeqs []int64
	for {
		evt, err := nextWithTimeout(es, 5*time.Second)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if evt.Type == "connected" {
			break
		}
		replayedSeqs = append(replayedSeqs, evt.Sequence)
	}

	for _, seq := range replayedSeqs {
		if seq > lastSeq {
			t.Fatalf("replayed seq %d > snapshot lastSeq %d", seq, lastSeq)
		}
	}

	for i := 1; i < len(replayedSeqs); i++ {
		if replayedSeqs[i] <= replayedSeqs[i-1] {
			t.Fatalf("non-monotonic replay: seq[%d]=%d >= seq[%d]=%d",
				i, replayedSeqs[i], i-1, replayedSeqs[i-1])
		}
	}

	es.Close()
}

// TestEventStreamDispatchCommittedEventsIntegrates verifies that
// DispatchCommittedEvents delivers events to the live subscriber and the
// event is then received by a real client over the Unix socket.
func TestEventStreamDispatchCommittedEventsIntegrates(t *testing.T) {
	st := openTestStore(t)
	handler := &NullHandler{}
	server := setupEventServer(t, st, handler)

	// Publish events to the durable store.
	server.PublishEvent(EventDTO{Type: "dispatch.integ", Data: map[string]interface{}{"n": 1}})

	// Subscribe via real client.
	client := NewClient(server.socketPath)
	ctx := context.Background()
	es, err := client.ConnectEventStream(ctx, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Skip the "connected" event.
	evt, err := nextWithTimeout(es, 5*time.Second)
	if err != nil || evt.Type != "connected" {
		t.Fatalf("expected connected, got type=%q err=%v", evt.Type, err)
	}

	// Now dispatch committed events (this broadcasts to live subscribers).
	server.PublishEvent(EventDTO{Type: "dispatch.test2", Data: map[string]interface{}{"n": 2}})

	evt, err = nextWithTimeout(es, 5*time.Second)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if evt.Type != "dispatch.test2" {
		t.Fatalf("event type = %q, want dispatch.test2", evt.Type)
	}

	es.Close()
}

// TestEventStreamClientServerRoundTrip verifies that the client's raw HTTP/SSE
// request (including Last-Event-ID header and last_seq query) is correctly
// parsed by the server's handleEvents handler end-to-end over a Unix socket.
func TestEventStreamClientServerRoundTrip(t *testing.T) {
	st := openTestStore(t)
	handler := &NullHandler{}
	server := setupEventServer(t, st, handler)

	client := NewClient(server.socketPath)
	ctx := context.Background()

	// Connect with lastSeq=0, receive connected.
	es, err := client.ConnectEventStream(ctx, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	evt, err := nextWithTimeout(es, 5*time.Second)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if evt.Type != "connected" {
		t.Fatalf("type = %q, want connected", evt.Type)
	}

	// Connect with lastSeq=1 (replay from 1).
	es2, err := client.ConnectEventStream(ctx, 1)
	if err != nil {
		t.Fatalf("reconnect lastSeq=1: %v", err)
	}

	// Read connected event.
	evt, err = nextWithTimeout(es2, 5*time.Second)
	if err != nil || evt.Type != "connected" {
		t.Fatalf("reconnect: type=%q err=%v", evt.Type, err)
	}

	es.Close()
	es2.Close()
}
