package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/store"
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
	store      *store.Store
	dispatchMu sync.Mutex
	// dispatchedSequence is the highest durable event broadcast live. SSE
	// replay remains independent, so reconnecting clients always read the
	// journal rather than trusting this in-memory cursor.
	dispatchedSequence int64
}

// Handler returns the HTTP handler serving the IPC routes.
//
// It exists so a test can put the real routing behind a real socket and drive
// it with the real client. Testing each side against its own fake proved they
// each worked and not that they agreed.
func (s *Server) Handler() http.Handler { return s.mux }

// RequestHandler is the interface the supervisor implements to handle IPC requests.
type RequestHandler interface {
	HandleSnapshot() (*SnapshotDTO, error)
	HandleListConnections() ([]ConnectionDTO, error)
	HandleGetConnection(id string) (*ConnectionDTO, error)
	HandleGetConnectionDetail(id string) (*ConnectionDetailDTO, error)
	HandleCloneConnection(id string, req CloneConnectionRequest) (*ConnectionDTO, error)
	HandleCreateConnection(req CreateConnectionRequest) (*ConnectionDTO, error)
	HandleUpdateConnection(id string, req UpdateConnectionRequest) (*ConnectionDTO, error)
	HandlePlanOpen(id string) (*PlanDTO, error)
	HandlePlanClose(id string) (*PlanDTO, error)
	HandlePlanEdit(id string, req UpdateConnectionRequest) (*PlanDTO, error)
	HandlePlanRepair(id string) (*PlanDTO, error)
	HandlePlanDelete(id string) (*PlanDTO, error)
	HandleApplyPlan(planID string, idempotencyKey string) (*OperationDTO, error)
	HandleListProviders() ([]ProviderDTO, error)
	HandleProviderRecommendation(req ProviderRecommendationRequest) (*ProviderRecommendationResponse, error)
	HandleAuthenticateProvider(id string) error
	HandleConfigureProviderAccount(id string, req ConfigureProviderAccountRequest) (*ConfigureProviderAccountResponse, error)
	HandleRemoveProviderAccount(providerID, accountID string) (*RemoveProviderAccountResponse, error)
	HandleProviderSetupFlow(id string) (*SetupFlowDTO, error)
	HandleGetOperation(id string) (*OperationDTO, error)
	HandleGetOperationEvents(id string) ([]EventDTO, error)
	HandleOperationHistory() (*OperationHistoryDTO, error)
	HandleOperationHistoryLimit(limit int) (*OperationHistoryDTO, error)
	HandleDiscovery() (*DiscoveryDTO, error)
	HandleRefreshDiscovery() (*DiscoveryDTO, error)
	HandleDiagnostics(connID string) ([]DiagnosticDTO, error)
	HandleConnectionLogs(id string, lines int) (*ConnectionLogsDTO, error)
	HandleReadiness() (*ReadinessDTO, error)
	HandleSetLaunchMode(mode string) (*LaunchModeDTO, error)
	HandleSupportExport() (*SupportExportDTO, error)
	HandleSupervisorStop(ctx context.Context) error
}

// NewServer creates a new IPC server.
func NewServer(socketPath string, handler RequestHandler, st *store.Store) (*Server, error) {
	if st == nil {
		return nil, fmt.Errorf("ipc server requires durable event store")
	}
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

	s := &Server{
		socketPath: socketPath,
		handler:    handler,
		subs:       make(map[string]chan EventDTO),
		store:      st,
	}
	seq, err := st.GetLastEventSeq(context.Background())
	if err != nil {
		return nil, fmt.Errorf("read durable event sequence: %w", err)
	}
	s.dispatchedSequence = seq

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

// PublishEvent appends an event durably before broadcasting it to SSE
// subscribers. A persistence failure is returned and deliberately produces no
// live-only event: clients must be able to replay every event they receive.
func (s *Server) PublishEvent(evt EventDTO) error {
	if s.store == nil {
		return fmt.Errorf("publish event: durable event store unavailable")
	}
	payloadJSON, err := json.Marshal(evt.Data)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	occurredAt := time.Now().UTC()
	if evt.Timestamp != "" {
		parsed, err := time.Parse(time.RFC3339, evt.Timestamp)
		if err != nil {
			return fmt.Errorf("parse event timestamp: %w", err)
		}
		occurredAt = parsed
	}
	seq, err := s.store.AppendEvent(context.Background(), core.OperationID(evt.OperationID), core.ConnectionID(evt.ConnectionID), evt.Type, evt.Stage, occurredAt, payloadJSON)
	if err != nil {
		return fmt.Errorf("append event to journal: %w", err)
	}
	_ = seq // DispatchCommittedEvents reads the committed row and its payload.
	return s.DispatchCommittedEvents(context.Background())
}

// DispatchCommittedEvents broadcasts every event that has already committed
// to the durable journal. State transactions call this only after their
// commit succeeds; the method itself never writes an unrelated event row.
// It is also safe to call after a failed dispatch, because the cursor moves
// only after an event has been offered to all current subscribers.
func (s *Server) DispatchCommittedEvents(ctx context.Context) error {
	if s.store == nil {
		return fmt.Errorf("dispatch events: durable event store unavailable")
	}
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()

	for {
		events, err := s.store.GetDurableEventsSince(ctx, s.dispatchedSequence, 1000)
		if err != nil {
			return fmt.Errorf("load committed events: %w", err)
		}
		if len(events) == 0 {
			return nil
		}
		for _, event := range events {
			evt := eventDTOFromDurable(event)
			s.broadcast(evt)
			s.dispatchedSequence = evt.Sequence
		}
	}
}

func (s *Server) broadcast(evt EventDTO) {
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
	if s.store == nil {
		return 0
	}
	seq, err := s.store.GetLastEventSeq(context.Background())
	if err != nil {
		slog.Warn("read durable event sequence", "err", err)
		return 0
	}
	return seq
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
	snap, err := s.handler.HandleSnapshot()
	if err != nil {
		writeHandlerError(w, "PTO-SNAP", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Protocol-Version", "1")
	w.WriteHeader(http.StatusOK)

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
		replayEvents, err := s.getPersistentEventsUntil(lastSeq, replaySeq)
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

// getPersistentEventsUntil replays a fixed sequence interval in bounded
// queries. A reconnect with a large retained backlog therefore remains
// lossless while publication after highWater stays on the live stream.
func (s *Server) getPersistentEventsUntil(afterSeq, highWater int64) ([]EventDTO, error) {
	if s.store == nil {
		return nil, fmt.Errorf("durable event store unavailable")
	}
	const replayPageSize = 1000
	result := make([]EventDTO, 0)
	cursor := afterSeq
	for cursor < highWater {
		events, err := s.store.GetDurableEventsBetween(context.Background(), cursor, highWater, replayPageSize)
		if err != nil {
			return nil, err
		}
		if len(events) == 0 {
			break
		}
		for _, evt := range events {
			result = append(result, eventDTOFromDurable(evt))
		}
		next := events[len(events)-1].Event.Sequence
		if next <= cursor {
			return nil, fmt.Errorf("non-monotonic durable event replay at sequence %d", next)
		}
		cursor = next
	}
	return result, nil
}

func eventDTOFromDurable(evt store.DurableEvent) EventDTO {
	return EventDTO{
		Sequence:     evt.Event.Sequence,
		OperationID:  string(evt.OperationID),
		ConnectionID: string(evt.ConnectionID),
		Type:         string(evt.Event.Type),
		Stage:        evt.Stage,
		Timestamp:    evt.Event.Timestamp.Format(time.RFC3339),
		Data:         evt.Event.Data,
	}
}

// oldestRetainedSequence returns the oldest sequence in the store, or 0 if empty.
func (s *Server) oldestRetainedSequence() int64 {
	if s.store == nil {
		return 0
	}
	seq, err := s.store.OldestEventSeq(context.Background())
	if err != nil {
		slog.Warn("failed to read oldest retained event", "err", err)
		return 0
	}
	return seq
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		conns, err := s.handler.HandleListConnections()
		if err != nil {
			writeHandlerError(w, "PTO-CONN-LIST", err)
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
			writeHandlerError(w, "PTO-CONN-CREATE", err)
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
			writeHandlerError(w, "PTO-CONN-GET", err)
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
			writeHandlerError(w, "PTO-CONN-UPDATE", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(conn)

	case len(parts) == 1 && r.Method == http.MethodDelete:
		// Destruction is intentionally a plan-preview-confirm-apply workflow.
		// Do not let a conventional DELETE bypass the destructive preview.
		writeMethodNotAllowed(w, "GET, PATCH")

	case len(parts) == 2 && parts[1] == "clone" && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req CloneConnectionRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "PTO-CONN-CLONE", "invalid request body")
			return
		}
		conn, err := s.handler.HandleCloneConnection(id, req)
		if err != nil {
			writeHandlerError(w, "PTO-CONN-CLONE", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(conn)

	case len(parts) == 2 && parts[1] == "logs" && r.Method == http.MethodGet:
		lines := 0
		if raw := r.URL.Query().Get("lines"); raw != "" {
			if parsed, convErr := strconv.Atoi(raw); convErr == nil {
				lines = parsed
			}
		}
		logs, err := s.handler.HandleConnectionLogs(id, lines)
		if err != nil {
			writeHandlerError(w, "PTO-CONN-LOGS", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(logs)

	case len(parts) == 2 && parts[1] == "detail" && r.Method == http.MethodGet:
		detail, err := s.handler.HandleGetConnectionDetail(id)
		if err != nil {
			writeHandlerError(w, "PTO-CONN-DETAIL", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(detail)

	case len(parts) == 3 && parts[1] == "plan" && parts[2] == "edit" && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req UpdateConnectionRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "PTO-PLAN-EDIT", "invalid request body")
			return
		}
		plan, err := s.handler.HandlePlanEdit(id, req)
		if err != nil {
			writeHandlerError(w, "PTO-PLAN-EDIT", err)
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
			writeHandlerError(w, "PTO-PLAN-"+action, err)
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

	// Extract optional idempotency key from header.
	idempotencyKey := r.Header.Get("Idempotency-Key")

	op, err := s.handler.HandleApplyPlan(planID, idempotencyKey)
	if err != nil {
		writeHandlerError(w, "PTO-PLAN-APPLY", err)
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
		writeHandlerError(w, "PTO-PROV-LIST", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(providers)
}

func (s *Server) handleProviderByID(w http.ResponseWriter, r *http.Request) {
	// POST /v1/providers/{id}/authenticate
	// POST /v1/providers/{id}/accounts
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/providers/"), "/"), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		writeError(w, http.StatusNotFound, "PROV-001", "unknown provider endpoint")
		return
	}
	id, action := parts[0], parts[1]

	// DELETE /v1/providers/{id}/accounts/{accountID}
	//
	// Removal was implemented on the supervisor and never routed, so an account
	// could be added and never taken away: a revoked token stayed listed as a
	// working account with no way to say otherwise.
	if len(parts) == 3 {
		if action != "accounts" || parts[2] == "" {
			writeError(w, http.StatusNotFound, "PROV-006", "unknown provider endpoint")
			return
		}
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "PROV-002", "method not allowed")
			return
		}
		response, err := s.handler.HandleRemoveProviderAccount(id, parts[2])
		if err != nil {
			// A refusal because connections still depend on the account is not
			// a failure to report and forget: each dependent connection is
			// something the user has to go and deal with, so they are sent as
			// the recovery actions rather than dropped in favour of a count.
			if response != nil && len(response.Dependencies) > 0 {
				apiErr := APIError{
					Version: 1, Code: "PROV-008", Summary: err.Error(),
					ProviderID: id,
					// Typed, so the caller does not have to parse prose back
					// into structure to say what is in the way.
					AccountDependencies: response.Dependencies,
				}
				for _, dep := range response.Dependencies {
					label := "Reassign or delete connection " + dep.Name
					if dep.Kind == "cleanup_item" {
						label = "Portico still has to remove " + dep.Name
					}
					apiErr.RecoveryActions = append(apiErr.RecoveryActions, RecoveryAction{
						Label: label, Action: dep.Kind + ":" + dep.ID,
					})
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(apiErr)
				return
			}
			writeHandlerError(w, "PROV-008", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
		return
	}

	// The method is checked per action rather than once for the group: reading
	// a provider's setup requirements is a GET, while the actions that change
	// state are POSTs.
	wantMethod := http.MethodPost
	if action == "setup-flow" {
		wantMethod = http.MethodGet
	}
	if r.Method != wantMethod {
		writeError(w, http.StatusMethodNotAllowed, "PROV-002", "method not allowed")
		return
	}

	switch action {
	case "setup-flow":
		flow, err := s.handler.HandleProviderSetupFlow(id)
		if err != nil {
			writeHandlerError(w, "PROV-007", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(flow)
	case "authenticate":
		if err := s.handler.HandleAuthenticateProvider(id); err != nil {
			writeHandlerError(w, "PROV-003", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "authenticated"})
	case "accounts":
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		defer r.Body.Close()
		var req ConfigureProviderAccountRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "PROV-004", "invalid account configuration")
			return
		}
		response, err := s.handler.HandleConfigureProviderAccount(id, req)
		if err != nil {
			// A rejected credential is not just a failure: the response says
			// which permissions are missing, and a user cannot act on
			// "validation failed". Those details were computed and discarded
			// with the response, so the client saw only the summary.
			if response != nil && (len(response.MissingPermissions) > 0 || len(response.Zones) > 0) {
				apiErr := APIError{
					Version: 1, Code: "PROV-005", Summary: err.Error(), ProviderID: id,
					ProviderValidation: &ProviderValidationDetails{
						MissingPermissions: response.MissingPermissions,
						AvailableZones:     response.Zones,
						AccountAccessible:  response.Validated,
						VerificationState:  response.Status,
					},
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(apiErr)
				return
			}
			writeHandlerError(w, "PROV-005", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(response)
	default:
		writeError(w, http.StatusNotFound, "PROV-006", "unknown provider endpoint")
	}
}

func (s *Server) handleOperationList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "OP-005", "method not allowed")
		return
	}
	// A caller can ask for more than the default page, which is how the
	// interface offers "show me older operations" without a cursor protocol.
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "OP-007", "limit must be a non-negative whole number")
			return
		}
		limit = parsed
	}
	history, err := s.handler.HandleOperationHistoryLimit(limit)
	if err != nil {
		writeHandlerError(w, "OP-006", err)
		return
	}
	json.NewEncoder(w).Encode(history)
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
			writeHandlerError(w, "OP-002", err)
			return
		}
		json.NewEncoder(w).Encode(op)

	case len(parts) >= 2 && parts[1] == "events" && r.Method == http.MethodGet:
		events, err := s.handler.HandleGetOperationEvents(id)
		if err != nil {
			writeHandlerError(w, "OP-003", err)
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
			writeHandlerError(w, "DISC-001", err)
			return
		}
		json.NewEncoder(w).Encode(result)
	case http.MethodPost:
		result, err := s.handler.HandleRefreshDiscovery()
		if err != nil {
			writeHandlerError(w, "DISC-002", err)
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
		writeHandlerError(w, "PTO-STOP", err)
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
		writeHandlerError(w, "DIAG-002", err)
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

// writeHandlerError converts expected controller/store failures into stable
// HTTP categories. Clients use these categories for actionable exit codes;
// only unexpected faults remain 500s.
func writeHandlerError(w http.ResponseWriter, fallbackCode string, err error) {
	status := http.StatusInternalServerError
	code := fallbackCode

	var porticoErr *core.PorticoError
	var resourceConflict *store.ResourceAssociationConflict
	switch {
	case errors.As(err, &porticoErr):
		code = porticoErr.Code
		switch porticoErr.Code {
		case core.ErrCorePrefix + "001", core.ErrCorePrefix + "002", core.ErrCorePrefix + "003", core.ErrCorePrefix + "005":
			status = http.StatusNotFound
		case core.ErrCorePrefix + "004", core.ErrCorePrefix + "008":
			status = http.StatusPreconditionFailed
		case core.ErrCorePrefix + "006", core.ErrCorePrefix + "007":
			status = http.StatusConflict
		case core.ErrCorePrefix + "009":
			status = http.StatusUnprocessableEntity
		default:
			status = http.StatusUnprocessableEntity
		}
	case errors.As(err, &resourceConflict):
		status = http.StatusConflict
		code = "PTO-RESOURCE-CONFLICT"
	}

	writeError(w, status, code, err.Error())
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

// handleSupportExport serves a redacted diagnostic report.
func (s *Server) handleSupportExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "PTO-SUPPORT-METHOD", "method not allowed")
		return
	}
	export, err := s.handler.HandleSupportExport()
	if err != nil {
		writeHandlerError(w, "PTO-SUPPORT-EXPORT", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(export)
}

// handleProviderRecommendation answers which provider suits a set of stated
// requirements.
//
// The engine behind this has existed and been tested for some time with no
// route to reach it, so every caller picked a provider by other means — which
// in the TUI meant a hardcoded list of one.
//
// It is a POST because the requirements are a body, not because it changes
// anything: recommending is a pure function of the request and current provider
// state.
func (s *Server) handleProviderRecommendation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "PTO-RECOMMEND-METHOD", "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	defer r.Body.Close()
	var req ProviderRecommendationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "PTO-RECOMMEND-BODY", "invalid request body")
		return
	}
	result, err := s.handler.HandleProviderRecommendation(req)
	if err != nil {
		writeHandlerError(w, "PTO-RECOMMEND", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}

// handleLaunchMode changes the startup gate.
//
// POST only: reading the mode is already answered by /v1/readiness, and a
// second read path would be a second thing to keep true.
func (s *Server) handleLaunchMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "PTO-LAUNCH-METHOD", "method not allowed")
		return
	}
	var req LaunchModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "PTO-LAUNCH-BODY", "invalid request body")
		return
	}
	result, err := s.handler.HandleSetLaunchMode(req.Mode)
	if err != nil {
		writeHandlerError(w, "PTO-LAUNCH", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}

// handleReadiness serves the aggregated setup view.
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "PTO-READY-METHOD", "method not allowed")
		return
	}
	readiness, err := s.handler.HandleReadiness()
	if err != nil {
		writeHandlerError(w, "PTO-READY", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(readiness)
}
