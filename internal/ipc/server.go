package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paoloanzn/portico/internal/core"
	"github.com/paoloanzn/portico/internal/store"
)

// Server is the HTTP/SSE IPC server over a Unix domain socket.
type Server struct {
	socketPath string
	mux        *http.ServeMux
	handler    RequestHandler
	srv        *http.Server
	listener   net.Listener
	mu         sync.Mutex
	subs       map[string]chan EventDTO
	nextSubID  int
	seqCounter int64
	seqMu      sync.Mutex
	store      *store.Store
}

// RequestHandler is the interface the supervisor implements to handle IPC requests.
type RequestHandler interface {
	HandleSnapshot() (*SnapshotDTO, error)
	HandleListConnections() ([]ConnectionDTO, error)
	HandleGetConnection(id string) (*ConnectionDTO, error)
	HandleCreateConnection(req CreateConnectionRequest) (*ConnectionDTO, error)
	HandleUpdateConnection(id string, req UpdateConnectionRequest) (*ConnectionDTO, error)
	HandlePlanOpen(id string) (*PlanDTO, error)
	HandlePlanClose(id string) (*PlanDTO, error)
	HandlePlanRepair(id string) (*PlanDTO, error)
	HandlePlanDelete(id string) (*PlanDTO, error)
	HandleApplyPlan(planID string) (*OperationDTO, error)
	HandleListProviders() ([]ProviderDTO, error)
	HandleAuthenticateProvider(id string) error
	HandleDeleteConnection(id string) error
	HandleGetOperation(id string) (*OperationDTO, error)
	HandleGetOperationEvents(id string) ([]EventDTO, error)
	HandleDiscovery() (*DiscoveryDTO, error)
	HandleRefreshDiscovery() (*DiscoveryDTO, error)
	HandleDiagnostics(connID string) ([]DiagnosticDTO, error)
	HandleSupervisorStop(ctx context.Context) error
}

// NewServer creates a new IPC server.
func NewServer(socketPath string, handler RequestHandler, st *store.Store) (*Server, error) {
	// Ensure parent directory exists.
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("ipc mkdir: %w", err)
	}

	// Check if socket exists and is owned by current user before removing
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		// If remove failed because it's owned by different user, that's an error
		if !os.IsPermission(err) {
			return nil, fmt.Errorf("ipc remove socket: %w", err)
		}
		// Socket owned by different user - can't proceed
		return nil, fmt.Errorf("socket %s owned by different user", socketPath)
	}

	// Initialize sequence from store
	seq, err := st.GetSequence(context.Background())
	if err != nil {
		return nil, fmt.Errorf("ipc get sequence: %w", err)
	}

	s := &Server{
		socketPath: socketPath,
		handler:    handler,
		subs:       make(map[string]chan EventDTO),
		seqCounter: seq,
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
	mux.HandleFunc("/v1/operations/", s.handleOperations)
	mux.HandleFunc("/v1/discovery", s.handleDiscovery)
	mux.HandleFunc("/v1/diagnostics/", s.handleDiagnostics)
	mux.HandleFunc("/v1/supervisor/stop", s.handleSupervisorStop)
	s.mux = mux

	return s, nil
}

// Start begins listening on the Unix socket with peer credential validation.
func (s *Server) Start() error {
	listener, err := NewUnixListener(s.socketPath)
	if err != nil {
		return fmt.Errorf("ipc listen: %w", err)
	}
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		listener.Close()
		return fmt.Errorf("ipc chmod: %w", err)
	}
	s.listener = listener

	slog.Info("IPC server listening", "socket", s.socketPath)
	return s.Serve(context.Background(), listener)
}

// Serve serves HTTP on the given listener (for split Listen/Serve pattern).
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	s.srv = &http.Server{
		Handler:        s.mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   0, // No write timeout for SSE streams
		MaxHeaderBytes: 1 << 10,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	slog.Info("IPC server serving", "socket", s.socketPath)
	if err := s.srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() error {
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	}
	os.Remove(s.socketPath)
	return nil
}

// PublishEvent broadcasts an event to all SSE subscribers.
// Events are persisted to the append-only events table before broadcasting.
func (s *Server) PublishEvent(evt EventDTO) {
	// Extract connection ID and stage from the Data field for persistence.
	var connID, stage string
	if dataMap, ok := evt.Data.(map[string]interface{}); ok {
		if v, ok := dataMap["connection_id"].(string); ok {
			connID = v
		}
		if v, ok := dataMap["stage"].(string); ok {
			stage = v
		}
	}

	// Persist event to the append-only table and get the sequence number.
	if s.store != nil {
		payloadJSON, err := json.Marshal(evt.Data)
		if err != nil {
			slog.Error("failed to marshal event payload", "err", err)
		} else {
			seq, err := s.store.AppendEvent(context.Background(),
				"", core.ConnectionID(connID), evt.Type, stage, payloadJSON)
			if err != nil {
				slog.Error("failed to append event to journal", "err", err)
			} else {
				evt.Sequence = seq
			}
		}
	}

	// Fallback to in-memory counter if store unavailable.
	if evt.Sequence == 0 {
		s.seqMu.Lock()
		s.seqCounter++
		evt.Sequence = s.seqCounter
		s.seqMu.Unlock()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subs {
		select {
		case ch <- evt:
		default:
			// Slow consumer; unsubscribe.
			slog.Warn("dropping slow SSE subscriber", "id", id)
			close(ch)
			delete(s.subs, id)
		}
	}
}

// CurrentSeq returns the current event sequence number.
func (s *Server) CurrentSeq() int64 {
	if s.store != nil {
		seq, err := s.store.GetLastEventSeq(context.Background())
		if err == nil {
			return seq
		}
	}
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	return s.seqCounter
}

func (s *Server) subscribe() (string, chan EventDTO) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSubID++
	id := strconv.Itoa(s.nextSubID)
	ch := make(chan EventDTO, 64)
	s.subs[id] = ch
	return id, ch
}

func (s *Server) unsubscribe(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subs[id]; ok {
		close(ch)
		delete(s.subs, id)
	}
}

// --------------- HTTP handlers ---------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "GET")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Protocol-Version", "1")
	w.Header().Set("X-Schema-Version", "2")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "GET")
		return
	}
	// Capture the sequence before building the snapshot: events published
	// while the snapshot is assembled will then be replayed by the client
	// (at-least-once), never skipped.
	lastSeq := s.CurrentSeq()
	snap, err := s.handler.HandleSnapshot()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PTO-SNAP", err.Error())
		return
	}
	snap.LastSeq = lastSeq
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Protocol-Version", "1")
	w.WriteHeader(http.StatusOK)

	// Limit snapshot body size by not including connections if too many
	maxConns := 100
	if len(snap.Connections) > maxConns {
		truncated := snap.Connections[:maxConns]
		snap.Connections = truncated
	}
	json.NewEncoder(w).Encode(snap)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "GET")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "PTO-SSE", "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Support replay from Last-Event-ID or last_seq query param
	var lastSeq int64
	if lastEventID := r.Header.Get("Last-Event-ID"); lastEventID != "" {
		fmt.Sscanf(lastEventID, "%d", &lastSeq)
	} else if ls := r.URL.Query().Get("last_seq"); ls != "" {
		fmt.Sscanf(ls, "%d", &lastSeq)
	}

	// Check if we need to send a resync_required event.
	// With persistent events, we can detect gaps more reliably.
	oldestSeq := s.oldestRetainedSequence()
	currentSeq := s.CurrentSeq()
	if lastSeq > 0 && oldestSeq > 0 && lastSeq+1 < oldestSeq {
		fmt.Fprintf(w, "event: resync_required\ndata: {\"reason\":\"retention_gap\",\"current_seq\":%d,\"oldest_seq\":%d}\n\n", currentSeq, oldestSeq)
		flusher.Flush()
		return
	}

	// Subscribe FIRST, then replay — this closes the race window where events
	// published between replay and subscription would be lost.
	subID, ch := s.subscribe()
	defer s.unsubscribe(subID)

	// Capture sequence AFTER subscribing to ensure no events between subscription
	// and the replay sequence snapshot are missed.
	replaySeq := s.CurrentSeq()

	// Replay events from the persistent store if reconnecting.
	// highWater tracks the highest sequence delivered via replay; live
	// events at or below it are duplicates and must be dropped.
	highWater := lastSeq
	if lastSeq > 0 {
		replayEvents, err := s.getPersistentEvents(lastSeq)
		if err != nil {
			slog.Warn("failed to replay events from store", "err", err)
		} else {
			for _, evt := range replayEvents {
				data, _ := json.Marshal(evt)
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", evt.Sequence, evt.Type, data)
				flusher.Flush()
				if evt.Sequence > highWater {
					highWater = evt.Sequence
				}
			}
		}
	}

	// Send initial sequence info — use the captured replaySeq so the client
	// knows the sequence at the point subscription began.
	fmt.Fprintf(w, "event: connected\ndata: {\"last_seq\":%d}\n\n", replaySeq)
	flusher.Flush()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case evt, ok := <-ch:
			if !ok {
				// Channel closed by unsubscribe due to slow consumer
				fmt.Fprintf(w, "event: disconnected\ndata: {\"reason\":\"slow_consumer\"}\n\n")
				flusher.Flush()
				return
			}
			// Drop events already delivered through replay.
			if evt.Sequence > 0 && evt.Sequence <= highWater {
				continue
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", evt.Sequence, evt.Type, data)
			flusher.Flush()
		}
	}
}

// getPersistentEvents returns events after the given sequence from the store.
func (s *Server) getPersistentEvents(afterSeq int64) ([]EventDTO, error) {
	if s.store == nil {
		return nil, nil
	}
	events, err := s.store.GetEventsSince(context.Background(), afterSeq, 1000)
	if err != nil {
		return nil, err
	}
	result := make([]EventDTO, 0, len(events))
	for _, evt := range events {
		result = append(result, EventDTO{
			Sequence:  evt.Sequence,
			Type:      string(evt.Type),
			Timestamp: evt.Timestamp.Format(time.RFC3339),
			Data:      evt.Data,
		})
	}
	return result, nil
}

// oldestRetainedSequence returns the oldest sequence in the store, or 0 if empty.
func (s *Server) oldestRetainedSequence() int64 {
	if s.store == nil {
		return 0
	}
	// For now, we retain all events. In the future, this could query MIN(seq).
	return 0
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		conns, err := s.handler.HandleListConnections()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "PTO-CONN-LIST", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(conns)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req CreateConnectionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "PTO-CONN-CREATE", "invalid request body")
			return
		}
		conn, err := s.handler.HandleCreateConnection(req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "PTO-CONN-CREATE", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(conn)

	default:
		writeMethodNotAllowed(w, "GET, POST")
	}
}

func (s *Server) handleConnectionByID(w http.ResponseWriter, r *http.Request) {
	// Parse path: /v1/connections/{id} or /v1/connections/{id}/plan/open etc.
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/connections/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "PTO-CONN-ID", "missing connection ID")
		return
	}
	id, err := url.PathUnescape(parts[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "PTO-CONN-ID", "invalid connection ID encoding")
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		conn, err := s.handler.HandleGetConnection(id)
		if err != nil {
			writeError(w, http.StatusNotFound, "PTO-CONN-GET", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(conn)

	case len(parts) == 1 && r.Method == http.MethodPatch:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req UpdateConnectionRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "PTO-CONN-UPDATE", "invalid request body")
			return
		}
		conn, err := s.handler.HandleUpdateConnection(id, req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "PTO-CONN-UPDATE", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(conn)

	case len(parts) == 1 && r.Method == http.MethodDelete:
		plan, err := s.handler.HandlePlanDelete(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "PTO-CONN-DELETE", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(plan)

	case len(parts) == 3 && parts[1] == "plan" && r.Method == http.MethodPost:
		action := parts[2]
		var plan *PlanDTO
		var err error
		switch action {
		case "open":
			plan, err = s.handler.HandlePlanOpen(id)
		case "close":
			plan, err = s.handler.HandlePlanClose(id)
		case "repair":
			plan, err = s.handler.HandlePlanRepair(id)
		case "delete":
			plan, err = s.handler.HandlePlanDelete(id)
		default:
			writeError(w, http.StatusBadRequest, "PTO-PLAN-ACTION", "unknown plan action: "+action)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "PTO-PLAN-"+action, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(plan)

	default:
		writeError(w, http.StatusNotFound, "PTO-CONN-NOTFOUND", "unknown endpoint")
	}
}

func (s *Server) handlePlans(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, "POST")
		return
	}
	// POST /v1/plans/{id}/apply
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/plans/"), "/")
	if len(parts) < 2 || parts[1] != "apply" {
		writeError(w, http.StatusBadRequest, "PTO-PLAN-APPLY", "expected /v1/plans/{id}/apply")
		return
	}
	planID := parts[0]

	op, err := s.handler.HandleApplyPlan(planID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PTO-PLAN-APPLY", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(op)
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "GET")
		return
	}
	providers, err := s.handler.HandleListProviders()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PTO-PROV-LIST", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(providers)
}

func (s *Server) handleProviderByID(w http.ResponseWriter, r *http.Request) {
	// POST /v1/providers/{id}/authenticate
	id := strings.TrimPrefix(r.URL.Path, "/v1/providers/")
	id = strings.TrimSuffix(id, "/authenticate")

	// Validate method
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "PROV-001", "method not allowed")
		return
	}

	// Use MaxBytesReader with reasonable limit
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1KB limit

	if err := s.handler.HandleAuthenticateProvider(id); err != nil {
		writeError(w, http.StatusInternalServerError, "PROV-002", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "authenticated"})
}

func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	// GET /v1/operations/{id} or GET /v1/operations/{id}/events
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/operations/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "OP-001", "missing operation ID")
		return
	}
	id := parts[0]

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		op, err := s.handler.HandleGetOperation(id)
		if err != nil {
			writeError(w, http.StatusNotFound, "OP-002", err.Error())
			return
		}
		json.NewEncoder(w).Encode(op)

	case len(parts) >= 2 && parts[1] == "events" && r.Method == http.MethodGet:
		events, err := s.handler.HandleGetOperationEvents(id)
		if err != nil {
			writeError(w, http.StatusNotFound, "OP-003", err.Error())
			return
		}
		json.NewEncoder(w).Encode(events)

	default:
		writeError(w, http.StatusNotFound, "OP-004", "unknown endpoint")
	}
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		result, err := s.handler.HandleDiscovery()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DISC-001", err.Error())
			return
		}
		json.NewEncoder(w).Encode(result)
	case http.MethodPost:
		result, err := s.handler.HandleRefreshDiscovery()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DISC-002", err.Error())
			return
		}
		json.NewEncoder(w).Encode(result)
	default:
		writeError(w, http.StatusMethodNotAllowed, "DISC-003", "method not allowed")
	}
}

func (s *Server) handleSupervisorStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeMethodNotAllowed(w, "POST")
		return
	}

	if err := s.handler.HandleSupervisorStop(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "PTO-STOP", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "shutdown initiated"})
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	connID := strings.TrimPrefix(r.URL.Path, "/v1/diagnostics/")
	if connID == "" {
		writeError(w, http.StatusBadRequest, "DIAG-001", "missing connection ID")
		return
	}

	findings, err := s.handler.HandleDiagnostics(connID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DIAG-002", err.Error())
		return
	}
	json.NewEncoder(w).Encode(findings)
}

func writeError(w http.ResponseWriter, code int, errCode, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Error-Code", errCode)
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(APIError{
		Version: 1,
		Code:    errCode,
		Summary: msg,
	})
}

// writeMethodNotAllowed sets Allow header and returns 405.
func writeMethodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMethodNotAllowed)
	json.NewEncoder(w).Encode(APIError{
		Version: 1,
		Code:    "PTO-METHOD",
		Summary: "method not allowed",
	})
}
