package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/diagnostics"
	"github.com/B-A-M-N/portico/internal/discovery"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/lock"
	"github.com/B-A-M-N/portico/internal/store"
)

// SupervisorIdentity stores the identity of a running supervisor.
type SupervisorIdentity struct {
	PID        int    `json:"pid"`
	StartTime  uint64 `json:"start_time"` // clock ticks since boot (/proc/pid/stat field 22)
	Executable string `json:"executable"`
}

// SupervisorLock ensures only one supervisor runs per machine.
type SupervisorLock struct {
	path     string
	file     *os.File
	identity SupervisorIdentity
	released bool
}

// NewSupervisorLock creates or acquires a supervisor lock file.
func NewSupervisorLock(paths app.Paths) (*SupervisorLock, error) {
	lockDir := filepath.Dir(paths.SocketPath)
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		return nil, fmt.Errorf("lock dir: %w", err)
	}

	lockPath := filepath.Join(lockDir, "portico-supervisor.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("lock open: %w", err)
	}

	// Try to acquire exclusive lock.
	if err := lock.Lock(file); err != nil {
		// Have not closed the file yet -- check for stale lock
		stale, checkErr := checkStaleLock(file, lockPath)
		if stale && checkErr == nil {
			slog.Info("recovering stale lock")
			// Stale lock -- clear and retry
			if err := lock.Unlock(file); err != nil {
				file.Close()
				return nil, fmt.Errorf("unlock stale: %w", err)
			}
			if err := lock.Lock(file); err != nil {
				file.Close()
				return nil, fmt.Errorf("stale recovery failed: %w", err)
			}
		} else {
			file.Close()
			return nil, fmt.Errorf("supervisor already running: %w", err)
		}
	}

	// Write our identity to the lock file
	exe, err := os.Executable()
	if err != nil {
		lock.Unlock(file)
		file.Close()
		return nil, fmt.Errorf("resolve executable: %w", err)
	}

	// Read /proc/self/stat field 22 for start time
	startTime := readProcStartTime(os.Getpid())

	identity := SupervisorIdentity{
		PID:        os.Getpid(),
		StartTime:  startTime,
		Executable: exe,
	}

	// Write identity to lock file
	if err := writeIdentity(file, identity); err != nil {
		lock.Unlock(file)
		file.Close()
		return nil, fmt.Errorf("write identity: %w", err)
	}

	return &SupervisorLock{path: lockPath, file: file, identity: identity}, nil
}

// readProcStartTime reads /proc/pid/stat field 22 (starttime in clock ticks).
func readProcStartTime(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// Find closing paren of comm field
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0
	}
	fields := strings.Fields(s[idx+2:])
	if len(fields) < 20 {
		return 0
	}
	val, _ := strconv.ParseUint(fields[19], 10, 64)
	return val
}

// checkStaleLock checks if the lock file contains a stale supervisor identity.
// Does NOT close the file. Reads from current position.
func checkStaleLock(file *os.File, lockPath string) (bool, error) {
	// Seek to beginning
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, err
	}

	var ident SupervisorIdentity
	if err := json.NewDecoder(file).Decode(&ident); err != nil {
		// No valid identity = stale
		return true, nil
	}

	if ident.PID <= 0 {
		return true, nil
	}

	// Read /proc/PID/stat to check if process with this PID and start time exists
	procStat := fmt.Sprintf("/proc/%d/stat", ident.PID)
	data, err := os.ReadFile(procStat)
	if err != nil {
		// Process does not exist
		return true, nil
	}

	// Check start time match
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return false, nil
	}
	fields := strings.Fields(s[idx+2:])
	if len(fields) < 20 {
		return false, nil
	}
	currentStart, _ := strconv.ParseUint(fields[19], 10, 64)
	if ident.StartTime != 0 && currentStart != ident.StartTime {
		// PID reused
		return true, nil
	}

	// Check executable
	exePath := fmt.Sprintf("/proc/%d/exe", ident.PID)
	exe, err := os.Readlink(exePath)
	if err == nil && ident.Executable != "" && exe != ident.Executable {
		// Different executable
		return true, nil
	}

	// Process is alive and matches identity
	return false, nil
}

// writeIdentity writes the supervisor identity to the lock file.
func writeIdentity(file *os.File, ident SupervisorIdentity) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ident); err != nil {
		return err
	}
	return file.Sync()
}

// Release closes and removes the lock. Idempotent.
func (l *SupervisorLock) Release() {
	if l.released {
		return
	}
	l.released = true
	if l.file != nil {
		lock.Unlock(l.file)
		_ = l.file.Sync()
		l.file.Close()
	}
	os.Remove(l.path)
}

// Identity returns the supervisor identity stored in the lock.
func (l *SupervisorLock) Identity() SupervisorIdentity {
	return l.identity
}

// --------------- Startup sequence (SPEC §10.3) ---------------

// startup performs the full 20-phase startup sequence.
func (s *Supervisor) startup(ctx context.Context) error {
	slog.Info("supervisor starting", "socket", s.paths.SocketPath, "db", s.paths.DatabasePath)

	// Phase 1: Acquire supervisor lock (already done in New()).
	slog.Info("startup: supervisor lock acquired")

	// Phase 2: Configure structured supervisor logger (already configured).
	slog.Info("startup: structured logger configured")

	// Phase 3: Open SQLite + run migrations (already done in New()).
	slog.Info("startup: database open and migrations applied")

	// Phase 4: Load provider accounts.
	slog.Info("startup: loading provider accounts")
	// Providers are built from the account rows rather than the rows being
	// projected onto whatever adapters happen to exist already. This is the
	// same call a live account change makes, so a restart and an in-process
	// change cannot produce different results from the same state.
	s.activateAll(ctx)

	// Phase 5: Create provider adapters.
	slog.Info("startup: creating provider adapters")
	// Provider adapters are registered during New() or via discovery.
	// This phase validates they are ready.
	if err := s.validateProviders(ctx); err != nil {
		slog.Warn("startup: some providers not available", "err", err)
	}

	// Phase 5b: Initialize discovery, diagnostics, and origin services.
	s.discoverer = discovery.NewDiscoverer(discovery.Options{})
	s.diagEngine = diagnostics.New(diagnostics.Deps{
		Origin:    diagnostics.NewHTTPOriginProber(2 * time.Second),
		Connector: diagnostics.ConnectorObserverFunc(s.observeConnectorStatus),
		Provider:  diagnostics.ProviderObserverFunc(s.observeProviderState),
		DNS:       diagnostics.NewNetDNSProber(),
		Endpoint:  diagnostics.NewHTTPSEndpointProber(4 * time.Second),
	})
	slog.Info("startup: discovery and diagnostics initialized")

	// Phase 6: Load connection profiles WITHOUT changing them.
	slog.Info("startup: restoring profiles")
	if err := s.loadProfiles(ctx); err != nil {
		slog.Error("startup: failed to load profiles", "err", err)
	}

	// Runtimes are restored by loadProfiles, above. There was a second pass here
	// doing the same thing: for every profile, load its runtime and restore it.
	// Two implementations of one step, and this was the weaker one — it logged a
	// database I/O error at debug level as "no runtime for connection", so a
	// failure to read stored state was indistinguishable from there being none.
	// loadProfiles distinguishes them.

	// Phase 8: Load provider resources.
	slog.Info("startup: restoring provider resources")
	if err := s.loadProviderResources(ctx); err != nil {
		slog.Warn("startup: failed to load some resources", "err", err)
	}

	// Phase 9: Load incomplete plans and operations.
	slog.Info("startup: restoring incomplete operations")
	if err := s.loadIncompleteOperations(ctx); err != nil {
		slog.Warn("startup: failed to load some operations", "err", err)
	}

	// Phase 10: Recover interrupted operation journals.
	slog.Info("startup: recovering operation journals")
	if err := s.recoverOperationJournals(ctx); err != nil {
		slog.Warn("startup: operation recovery incomplete", "err", err)
	}

	// Phase 11: Verify recorded connector identities.
	slog.Info("startup: verifying connector identities")
	s.verifyConnectorIdentities(ctx)

	// Phase 12: Observe retained provider resources.
	slog.Info("startup: observing provider resources")
	s.observeProviderResources(ctx)

	// Phase 12b: Rehydrate tunnel credentials from durable storage.
	// This must happen before restarting connections so the provider has
	// the tunnel token needed to restart permanent connectors (P0 #4).
	slog.Info("startup: rehydrating tunnel credentials")
	s.rehydrateTunnelCredentials(ctx)

	// Phase 13: Classify orphaned or drifted resources.
	slog.Info("startup: classifying resource state")
	s.classifyResourceState(ctx)

	// Phase 14: Restart desired-open connections.
	slog.Info("startup: restarting desired-open connections")
	s.restartDesiredOpen(ctx)

	// Phase 15: Bind the Unix listener (before marking ready).
	slog.Info("startup: binding IPC listener")
	handler := &supervisorHandler{sup: s}
	server, err := ipc.NewServer(s.paths.SocketPath, handler, s.store)
	if err != nil {
		return fmt.Errorf("ipc server: %w", err)
	}
	s.ipcServer = server
	// The controller writes operation events inside its state transactions;
	// the IPC server only dispatches those committed rows to live SSE clients.
	s.controller.SetCommittedEventDispatcher(server)

	listener, err := ipc.NewUnixListener(s.paths.SocketPath)
	if err != nil {
		return fmt.Errorf("ipc listen: %w", err)
	}
	if err := os.Chmod(s.paths.SocketPath, 0600); err != nil {
		listener.Close()
		return fmt.Errorf("ipc chmod: %w", err)
	}

	// Phase 16: Begin event broker.
	go s.eventBroker(ctx)

	// Phase 17: Begin reconciliation.
	go s.reconcileLoop(ctx)

	// Phase 18: Publish supervisor.ready.
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()

	// Publish ready event on the newly created server.
	s.ipcServer.PublishEvent(ipc.EventDTO{
		Type:      "supervisor.ready",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data: map[string]string{
			"pid":     fmt.Sprintf("%d", os.Getpid()),
			"db_path": s.paths.DatabasePath,
			"socket":  s.paths.SocketPath,
		},
	})
	slog.Info("supervisor ready")

	// Phase 19: Answer readiness checks (handler replies via health endpoint).

	// Phase 20: Block on Serve().
	return s.serveWithListener(ctx, listener, server)
}

// serveWithListener serves HTTP on the given listener.
// Splits Listen from Serve so readiness can be signaled before blocking.
// Returns once Serve() exits (cleanly or via shutdown).
func (s *Supervisor) serveWithListener(ctx context.Context, listener net.Listener, srv *ipc.Server) error {
	// Track the serve goroutine so shutdown can wait for it.
	s.serveWG.Add(1)
	errCh := make(chan error, 1)
	go func() {
		defer s.serveWG.Done()
		errCh <- srv.Serve(ctx, listener)
	}()

	// Wait for one of: serve error, context cancel, or explicit stop.
	select {
	case err := <-errCh:
		if err != nil && err.Error() == "http: Server closed" {
			slog.Info("server closed cleanly")
			return nil
		}
		if err != nil {
			slog.Error("server error", "err", err)
			s.shutdown(ctx)
		}
		return err
	case <-ctx.Done():
		// Context cancelled (SIGINT/SIGTERM via signal.NotifyContext).
		slog.Info("context cancelled, shutting down")
		return s.shutdown(ctx)
	case <-s.stopCh:
		slog.Info("shutdown requested, stopping server")
		return s.shutdown(ctx)
	}
}

// --------------- Phase helpers ---------------

func (s *Supervisor) validateProviders(ctx context.Context) error {
	providers := s.registry.List()
	for _, p := range providers {
		prov := s.registry.Get(p.ID)
		if prov == nil {
			slog.Warn("provider not available", "provider", p.ID)
			continue
		}
		caps, err := prov.Capabilities(ctx)
		if err != nil {
			slog.Warn("provider capabilities unavailable", "provider", p.ID, "err", err)
		}
		_ = caps
	}
	return nil
}

// restartDesiredOpen restarts all connections that should be open.
// Uses the delta planner (computeReconcileDecision) to determine the smallest
// action needed, avoiding unnecessary recreation of infrastructure.
func (s *Supervisor) restartDesiredOpen(ctx context.Context) {
	// A single gate in front of the per-connection flag: manual means nothing
	// is armed at startup whatever the connections say.
	if s.launchMode() == LaunchManual {
		slog.Info("startup: launch mode is manual; no connection will be opened automatically")
		return
	}
	profiles := s.controller.ListProfiles()
	for _, p := range profiles {
		if p.Desired == core.DesiredOpen && p.Lifecycle.AutoStart {
			if s.registry.Get(p.GetProvider().ProviderID) == nil {
				s.markProviderUnavailable(ctx, p)
				continue
			}
			profile := p
			go func(connID core.ConnectionID, profile *core.ConnectionProfile) {
				// Use the delta planner to determine what needs to be done.
				rt, _ := s.controller.GetRuntime(connID)
				resources, _ := s.store.ListResourcesByConnection(ctx, connID)

				input := ReconcileInput{
					Profile:   profile,
					Runtime:   rt,
					Resources: resources,
				}
				// Best-effort provider observation for authoritative decisions.
				if obs, oErr := s.observeConnection(ctx, connID); oErr == nil {
					input.Observed = obs
				}

				decision, err := s.computeReconcileDecision(ctx, input)
				if err != nil {
					slog.Warn("restart: reconcile decision failed", "connection", connID, "err", err)
					return
				}
				if decision.Blocked != nil {
					// A connection stored under a rule that has since tightened
					// is not restarted at boot, and says why.
					s.markProfileInvalidForOpen(ctx, input.Profile, decision.Blocked)
					return
				}
				if decision.Action == "none" || decision.Plan == nil {
					slog.Info("restart: no action needed", "connection", connID)
					return
				}

				slog.Info("restart: delta planner decision", "connection", connID, "action", decision.Action)
				plan, err := s.persistCanonicalPlan(ctx, decision.Plan)
				if err != nil {
					slog.Warn("restart plan persist failed", "connection", connID, "err", err)
					return
				}
				_, err = s.controller.ApplyPlan(ctx, plan.ID)
				if err != nil {
					slog.Warn("restart apply failed", "connection", connID, "err", err)
				}
			}(profile.ID, profile)
		}
	}
}

// buildConnectorRestartPlan creates a plan that only starts the connector
// for a connection with existing infrastructure. This avoids recreating
// Cloudflare resources that already exist.
func (s *Supervisor) buildConnectorRestartPlan(connID core.ConnectionID, p *core.ConnectionProfile) *core.OperationPlan {
	_ = p // Profile access and provider selection are owned by controller planning.
	plan, err := s.controller.PlanRepair(context.Background(), connID)
	if err == controller.ErrNoRepairNeeded {
		return nil
	}
	if err != nil {
		slog.Warn("restart: plan failed", "connection", connID, "err", err)
		return nil
	}
	return plan
}

func (s *Supervisor) loadProviderResources(ctx context.Context) error {
	profiles := s.controller.ListProfiles()
	for _, p := range profiles {
		resources, err := s.store.ListResourcesByConnection(ctx, p.ID)
		if err != nil {
			continue
		}
		if len(resources) > 0 {
			s.controller.RestoreResources(p.ID, resources)
		}
	}
	return nil
}

func (s *Supervisor) loadIncompleteOperations(ctx context.Context) error {
	plans, err := s.store.ListIncompletePlans(ctx)
	if err != nil {
		return err
	}
	for _, plan := range plans {
		s.controller.RestorePlan(plan)
	}
	return nil
}

// --------------- incomplete operation recovery (SPEC §10.3) ---------------

// stepRecoveryClass classifies one plan step of an interrupted operation
// from durable evidence (operation journal + persisted provider resources).
type stepRecoveryClass string

const (
	stepRecoveryNotStarted stepRecoveryClass = "not_started"
	stepRecoveryCommitted  stepRecoveryClass = "committed"
	stepRecoveryFailed     stepRecoveryClass = "failed"
	stepRecoveryUnknown    stepRecoveryClass = "provider_outcome_unknown"
)

// recoveryOutcome is the operation-level recovery decision.
type recoveryOutcome string

const (
	// recoveryComplete: every step committed durably — the operation can
	// be finished with the normal terminal success commit.
	recoveryComplete recoveryOutcome = "complete"
	// recoveryFailSafe: no step committed and no step has an uncertain
	// provider outcome — safe to mark the operation failed.
	recoveryFailSafe recoveryOutcome = "fail_safe"
	// recoveryRequired: partially committed or provider outcome unknown —
	// the operation must be failed with recovery-required semantics and a
	// diagnostic finding preserving the evidence.
	recoveryRequired recoveryOutcome = "recovery_required"
)

// stepRecoveryState pairs a plan step with its recovery classification.
type stepRecoveryState struct {
	StepID string
	Kind   core.StepKind
	Class  stepRecoveryClass
}

// recoveryDecision is the full classification of an interrupted operation.
type recoveryDecision struct {
	Outcome           recoveryOutcome
	Steps             []stepRecoveryState
	LastStartedStepID string
	// KnownResourceIDs lists persisted provider resources ("type/external_id").
	KnownResourceIDs []string
	// PossiblyUnpersisted lists observed provider resources with no
	// persisted record ("type/external_id") — candidates created by an
	// interrupted step whose commit never landed. Without durable
	// provenance they must NOT be marked orphaned; they are preserved as
	// finding evidence for repair/reconcile.
	PossiblyUnpersisted []string
}

// producedResourceType maps a creation step kind to the resource type it
// persists on success. Steps that persist no resource return "".
func producedResourceType(kind core.StepKind) core.ResourceType {
	switch kind {
	case core.StepCreateTunnel, core.StepRecreateTunnel:
		return core.ResourceTunnel
	case core.StepCreateDNSRecord:
		return core.ResourceDNSRecord
	case core.StepCreateAccessApp:
		return core.ResourceAccessApp
	case core.StepCreateAccessPolicy:
		return core.ResourceAccessPolicy
	}
	return ""
}

// classifyOperationRecovery classifies an interrupted operation from durable
// evidence only. It is a pure function: no I/O, no clock reads beyond the
// inputs, so it is unit-testable without a Supervisor.
//
// Per step: a terminal succeeded journal event means the step committed
// (the terminal event is written atomically with its resources). A started
// event with no terminal event and no attributable persisted resource means
// the provider outcome is unknown. No events means the step never started.
func classifyOperationRecovery(plan *core.OperationPlan, events []store.OperationJournalEvent,
	resources []core.ProviderResource, observed *core.ObservedConnection) recoveryDecision {

	var d recoveryDecision

	// Index journal evidence per plan step ID.
	started := make(map[string]bool)
	succeeded := make(map[string]bool)
	failed := make(map[string]bool)
	for _, e := range events {
		if e.StepID == "" {
			continue
		}
		switch core.EventStage(e.Stage) {
		case core.StageStarted:
			started[e.StepID] = true
		case core.StageSucceeded:
			succeeded[e.StepID] = true
		case core.StageFailed, core.StageCompensated:
			// Compensated means rolled back — the forward step did not
			// remain committed.
			failed[e.StepID] = true
		}
	}

	// Persisted resources are commit evidence. Count live resources per
	// type and pre-attribute those belonging to journal-committed steps so
	// resource-existence evidence is never double counted.
	liveByType := make(map[core.ResourceType]int)
	for _, r := range resources {
		d.KnownResourceIDs = append(d.KnownResourceIDs, fmt.Sprintf("%s/%s", r.Type, r.ExternalID))
		if r.Lifecycle == "" || r.Lifecycle == core.LifecyclePresent {
			liveByType[r.Type]++
		}
	}
	attributed := make(map[core.ResourceType]int)
	for _, step := range plan.Steps {
		if succeeded[step.ID] && !failed[step.ID] {
			if rt := producedResourceType(step.Kind); rt != "" {
				attributed[rt]++
			}
		}
	}

	committedCount := 0
	anyUnknown := false
	for _, step := range plan.Steps {
		class := stepRecoveryNotStarted
		switch {
		case succeeded[step.ID] && !failed[step.ID]:
			class = stepRecoveryCommitted
		case failed[step.ID]:
			class = stepRecoveryFailed
		case started[step.ID]:
			// Started with no terminal journal event. A persisted live
			// resource of the produced type beyond those attributed to
			// committed steps proves the commit landed (defensive: the
			// commit is normally atomic with the terminal event).
			rt := producedResourceType(step.Kind)
			if rt != "" && liveByType[rt] > attributed[rt] {
				attributed[rt]++
				class = stepRecoveryCommitted
			} else {
				class = stepRecoveryUnknown
			}
		}
		if class != stepRecoveryNotStarted {
			d.LastStartedStepID = step.ID
		}
		if class == stepRecoveryCommitted {
			committedCount++
		}
		if class == stepRecoveryUnknown {
			anyUnknown = true
		}
		d.Steps = append(d.Steps, stepRecoveryState{StepID: step.ID, Kind: step.Kind, Class: class})
	}

	// Observed provider resources without a persisted record are evidence
	// of possibly-created-but-unpersisted state.
	if observed != nil {
		persisted := make(map[string]bool, len(resources))
		for _, r := range resources {
			persisted[fmt.Sprintf("%s/%s", r.Type, r.ExternalID)] = true
		}
		var obs []string
		if observed.Tunnel != nil {
			obs = append(obs, fmt.Sprintf("%s/%s", core.ResourceTunnel, observed.Tunnel.ID))
		}
		for _, rec := range observed.DNSRecords {
			obs = append(obs, fmt.Sprintf("%s/%s", core.ResourceDNSRecord, rec.ID))
		}
		for _, app := range observed.AccessApps {
			obs = append(obs, fmt.Sprintf("%s/%s", core.ResourceAccessApp, app.ID))
		}
		for _, id := range obs {
			if !persisted[id] {
				d.PossiblyUnpersisted = append(d.PossiblyUnpersisted, id)
			}
		}
	}

	switch {
	case len(plan.Steps) > 0 && committedCount == len(plan.Steps):
		d.Outcome = recoveryComplete
	case committedCount == 0 && !anyUnknown && len(d.PossiblyUnpersisted) == 0:
		// Nothing committed, no uncertain provider outcome, and nothing
		// observed beyond durable records: safe to fail the operation.
		d.Outcome = recoveryFailSafe
	default:
		d.Outcome = recoveryRequired
	}
	return d
}

// recoveryStore is the subset of store operations needed to apply a
// recovery decision. *store.Store satisfies it; tests use a real temp store.
type recoveryStore interface {
	CommitOpenSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID, startedAt time.Time) (*core.RuntimeCommitResult, error)
	CommitCloseSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) (*core.RuntimeCommitResult, error)
	CommitRepairSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) (*core.RuntimeCommitResult, error)
	CommitDeleteSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) error
	CommitOperationFailure(ctx context.Context, connID core.ConnectionID, opID core.OperationID, errMsg string, provider core.ProviderID, retryable bool) error
	CommitOperationRecoveryRequired(ctx context.Context, connID core.ConnectionID, opID core.OperationID, errMsg string, provider core.ProviderID) error
	UpsertStepResultStatus(ctx context.Context, opID core.OperationID, stepID string, stepKind core.StepKind, status store.StepResultStatus) error
	MarkStepRecoveryRequired(ctx context.Context, opID core.OperationID, stepID string) error
	SaveFinding(ctx context.Context, f *core.DiagnosticFinding) error
}

// applyRecoveryDecision durably applies a recovery decision for one
// interrupted operation.
func applyRecoveryDecision(ctx context.Context, st recoveryStore, op store.IncompleteOperation,
	plan *core.OperationPlan, d recoveryDecision) error {

	switch d.Outcome {
	case recoveryComplete:
		var err error
		switch plan.Intent {
		case core.IntentOpen:
			startedAt, pErr := time.Parse(time.RFC3339, op.StartedAt)
			if pErr != nil {
				startedAt = time.Now().UTC()
			}
			_, err = st.CommitOpenSuccess(ctx, op.ConnectionID, op.ID, startedAt)
		case core.IntentClose:
			_, err = st.CommitCloseSuccess(ctx, op.ConnectionID, op.ID)
		case core.IntentRepair:
			_, err = st.CommitRepairSuccess(ctx, op.ConnectionID, op.ID)
		case core.IntentDelete:
			err = st.CommitDeleteSuccess(ctx, op.ConnectionID, op.ID)
		default:
			err = fmt.Errorf("unknown plan intent %q", plan.Intent)
		}
		if err == nil {
			slog.Info("recovery: completed interrupted operation",
				"operation", op.ID, "connection", op.ConnectionID, "intent", plan.Intent)
			return nil
		}
		// The terminal commit itself failed: degrade to recovery-required
		// rather than leaving the operation dangling or guessing.
		slog.Warn("recovery: terminal success commit failed, marking recovery required",
			"operation", op.ID, "err", err)
		return markOperationRecoveryRequired(ctx, st, op, plan, d,
			fmt.Sprintf("all steps committed but the terminal success commit failed during recovery: %v", err))

	case recoveryFailSafe:
		msg := "operation interrupted before any provider mutation was committed; marked failed during startup recovery"
		if err := st.CommitOperationFailure(ctx, op.ConnectionID, op.ID, msg, plan.Provider, true); err != nil {
			return fmt.Errorf("commit fail-safe failure: %w", err)
		}
		slog.Info("recovery: safely failed interrupted operation",
			"operation", op.ID, "connection", op.ConnectionID)
		return nil

	default: // recoveryRequired
		return markOperationRecoveryRequired(ctx, st, op, plan, d,
			"supervisor interrupted mid-operation with partially committed or unknown provider state")
	}
}

// markOperationRecoveryRequired records the recovery-required outcome:
// uncertain steps are durably marked outcome_unknown + recovery_required,
// a diagnostic finding preserves the operation, last started step, and the
// exact known external IDs, and the runtime records PTO-OP-RECOVERY-REQUIRED.
// The finding is written before the operation is converted to a failure so
// the evidence is never lost (never blindly convert to ordinary failure).
func markOperationRecoveryRequired(ctx context.Context, st recoveryStore, op store.IncompleteOperation,
	plan *core.OperationPlan, d recoveryDecision, reason string) error {

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	var unknownSteps []string
	for _, sc := range d.Steps {
		if sc.Class != stepRecoveryUnknown {
			continue
		}
		unknownSteps = append(unknownSteps, sc.StepID)
		keep(st.UpsertStepResultStatus(ctx, op.ID, sc.StepID, sc.Kind, store.StepOutcomeUnknown))
		keep(st.MarkStepRecoveryRequired(ctx, op.ID, sc.StepID))
	}

	finding := &core.DiagnosticFinding{
		ID:           core.FindingID(fmt.Sprintf("recovery-%s", op.ID)),
		ConnectionID: op.ConnectionID,
		Segment:      core.SegmentConnector,
		Severity:     core.SeverityError,
		Summary:      fmt.Sprintf("Operation %s was interrupted and requires recovery", op.ID),
		Explanation: fmt.Sprintf(
			"%s. Last started step: %s. Repair must re-observe provider state before further mutations.",
			reason, orNone(d.LastStartedStepID)),
		Evidence: []core.Evidence{{
			Type:    "operation_recovery",
			Source:  "supervisor.startup",
			Message: reason,
			Data: map[string]string{
				"operation_id":         string(op.ID),
				"plan_id":              string(op.PlanID),
				"intent":               string(plan.Intent),
				"last_started_step":    d.LastStartedStepID,
				"unknown_steps":        strings.Join(unknownSteps, ","),
				"known_resources":      strings.Join(d.KnownResourceIDs, ","),
				"possibly_unpersisted": strings.Join(d.PossiblyUnpersisted, ","),
			},
		}},
		ObservedAt: time.Now().UTC(),
	}
	keep(st.SaveFinding(ctx, finding))

	msg := fmt.Sprintf("%s (last started step: %s)", reason, orNone(d.LastStartedStepID))
	keep(st.CommitOperationRecoveryRequired(ctx, op.ConnectionID, op.ID, msg, plan.Provider))

	slog.Warn("recovery: operation marked recovery-required",
		"operation", op.ID, "connection", op.ConnectionID,
		"last_started_step", d.LastStartedStepID, "unknown_steps", len(unknownSteps))
	return firstErr
}

// orNone substitutes "none" for empty evidence strings in messages.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// recoverOperationJournals recovers operations that were interrupted
// mid-execution (SPEC §10.3 "Recover incomplete operations"). Evidence is
// the operation journal (operation_events), persisted provider resources,
// and a best-effort provider re-observation.
func (s *Supervisor) recoverOperationJournals(ctx context.Context) error {
	incompleteOps, err := s.store.ListNonTerminalOperations(ctx)
	if err != nil {
		return err
	}

	for _, op := range incompleteOps {
		plan, err := s.store.LoadPlan(ctx, op.PlanID)
		if err != nil {
			// Without the plan the steps cannot be classified. Preserve a
			// finding and fail with recovery-required semantics.
			slog.Warn("recovery: plan unavailable for interrupted operation",
				"operation", op.ID, "plan", op.PlanID, "err", err)
			stub := &core.OperationPlan{ID: op.PlanID, ConnectionID: op.ConnectionID}
			if aErr := markOperationRecoveryRequired(ctx, s.store, op, stub,
				recoveryDecision{Outcome: recoveryRequired},
				"interrupted operation has no loadable plan"); aErr != nil {
				slog.Warn("recovery: failed to mark recovery required", "operation", op.ID, "err", aErr)
			}
			continue
		}

		events, err := s.store.GetOperationEvents(ctx, op.ID)
		if err != nil {
			slog.Warn("recovery: journal unavailable, leaving operation for next startup",
				"operation", op.ID, "err", err)
			continue
		}
		resources, err := s.store.ListResourcesByConnection(ctx, op.ConnectionID)
		if err != nil {
			slog.Warn("recovery: resources unavailable, leaving operation for next startup",
				"operation", op.ID, "err", err)
			continue
		}

		// Best-effort provider re-observation for additional evidence.
		var observed *core.ObservedConnection
		if obs, oErr := s.observeConnection(ctx, op.ConnectionID); oErr == nil {
			observed = obs
		} else {
			slog.Warn("recovery: provider observation failed",
				"operation", op.ID, "provider", plan.Provider, "err", oErr)
		}

		decision := classifyOperationRecovery(plan, events, resources, observed)
		slog.Info("recovery: classified interrupted operation",
			"operation", op.ID, "connection", op.ConnectionID,
			"intent", plan.Intent, "outcome", decision.Outcome)

		if err := applyRecoveryDecision(ctx, s.store, op, plan, decision); err != nil {
			slog.Warn("recovery: applying decision failed", "operation", op.ID, "err", err)
		}
	}

	return nil
}

func (s *Supervisor) verifyConnectorIdentities(ctx context.Context) {
	// Re-attach persisted connector processes to the process manager.
	// The manager map is empty after a supervisor restart; adoption
	// verifies the FULL persisted identity (PID, start time, executable,
	// command hash) before the process is tracked. A mismatch means the
	// PID may have been reused: the process is never signaled, the
	// connector status becomes unknown, and repair is required.
	for _, rt := range s.controller.ListRuntimes() {
		conn := rt.Connector
		if conn.PID <= 0 {
			continue
		}
		if conn.Status != core.ConnectorStatusRunning && conn.Status != core.ConnectorStatusStarting {
			continue
		}
		if _, ok := s.procMgr.GetProcess(rt.ConnectionID); ok {
			continue
		}

		identity := core.ProcessIdentity{
			PID:            conn.PID,
			StartTime:      conn.StartTime,
			ExecutablePath: conn.Executable,
			CommandHash:    conn.CommandHash,
		}
		spec := core.ProcessSpec{Executable: conn.Executable}

		if err := s.procMgr.Adopt(rt.ConnectionID, identity, spec); err != nil {
			slog.Warn("connector adoption failed - marking status unknown",
				"connection", rt.ConnectionID,
				"pid", conn.PID,
				"err", err,
			)
			rt.Connector.Status = core.ConnectorStatusUnknown
			s.controller.RestoreRuntime(rt)
			if sErr := s.store.SaveRuntime(ctx, rt); sErr != nil {
				slog.Warn("failed to persist unknown connector status",
					"connection", rt.ConnectionID, "err", sErr)
			}
			finding := core.DiagnosticFinding{
				ID:           core.FindingID(fmt.Sprintf("finding-adopt-%s-%d", rt.ConnectionID, conn.PID)),
				ConnectionID: rt.ConnectionID,
				Segment:      core.SegmentConnector,
				Severity:     core.SeverityError,
				Summary:      "Connector process identity could not be verified after restart",
				Explanation: fmt.Sprintf(
					"Persisted connector PID %d failed identity verification (%v). The PID may have been reused by another process; it will not be signaled. Run repair to start a fresh connector.",
					conn.PID, err),
				ObservedAt: time.Now().UTC(),
			}
			if fErr := s.store.SaveFinding(ctx, &finding); fErr != nil {
				slog.Warn("failed to persist adoption finding",
					"connection", rt.ConnectionID, "err", fErr)
			}
			continue
		}

		slog.Info("connector process adopted after restart",
			"connection", rt.ConnectionID,
			"pid", conn.PID,
		)
	}
}

func (s *Supervisor) observeProviderResources(ctx context.Context) {
	// For each provider, observe the current state of resources
	// and persist the results to the store and controller runtime.
	// Observation must NEVER assign ownership. Ownership comes from durable
	// provenance: created by Portico, explicitly adopted, or externally observed.
	profiles := s.controller.ListProfiles()
	for _, p := range profiles {
		if s.registry.Get(p.GetProvider().ProviderID) == nil {
			s.markProviderUnavailable(ctx, p)
			continue
		}
		observed, err := s.observeConnection(ctx, p.ID)
		if err != nil {
			slog.Warn("observe failed", "connection", p.ID, "err", err)
			continue
		}
		if observed == nil {
			continue
		}

		// Load existing resources to preserve ownership from durable provenance.
		existing, err := s.store.ListResourcesByConnection(ctx, p.ID)
		if err != nil {
			slog.Warn("observe: load existing resources failed", "connection", p.ID, "err", err)
			continue
		}
		existingByExtID := make(map[string]core.ResourceOwnership, len(existing))
		for _, r := range existing {
			key := string(r.ProviderID) + ":" + string(r.Type) + ":" + r.ExternalID
			existingByExtID[key] = r.Ownership
		}

		// Build provider resources from observed data.
		// Use existing ownership if known; otherwise mark as external (not managed).
		// Check global resource lookup to prevent cross-connection conflicts.
		var resources []core.ProviderResource
		if observed.Tunnel != nil {
			ownership := ownershipForObserved(existingByExtID, observed.ProviderID, core.ResourceTunnel, observed.Tunnel.ID)
			resources = append(resources, core.ProviderResource{
				ConnectionID: p.ID,
				ProviderID:   observed.ProviderID,
				Type:         core.ResourceTunnel,
				ExternalID:   observed.Tunnel.ID,
				Ownership:    ownership,
				Metadata: map[string]string{
					"name":  observed.Tunnel.Name,
					"state": observed.Tunnel.State,
				},
			})
		}
		for _, dns := range observed.DNSRecords {
			ownership := ownershipForObserved(existingByExtID, observed.ProviderID, core.ResourceDNSRecord, dns.ID)
			resources = append(resources, core.ProviderResource{
				ConnectionID: p.ID,
				ProviderID:   observed.ProviderID,
				Type:         core.ResourceDNSRecord,
				ExternalID:   dns.ID,
				Ownership:    ownership,
				Metadata: map[string]string{
					"name":   dns.Name,
					"type":   dns.Type,
					"target": dns.Target,
				},
			})
		}
		for _, app := range observed.AccessApps {
			ownership := ownershipForObserved(existingByExtID, observed.ProviderID, core.ResourceAccessApp, app.ID)
			resources = append(resources, core.ProviderResource{
				ConnectionID: p.ID,
				ProviderID:   observed.ProviderID,
				Type:         core.ResourceAccessApp,
				ExternalID:   app.ID,
				Ownership:    ownership,
				Metadata: map[string]string{
					"name":     app.Name,
					"domain":   app.Domain,
					"authMode": app.AuthMode,
				},
			})
		}

		// Persist resources to store.
		// Use a separate acceptedResources slice to ensure only resources that
		// belong to this connection (or were successfully inserted as new external
		// resources) are installed into the controller runtime. Skipped conflicting
		// resources must NOT be installed.
		var acceptedResources []core.ProviderResource
		for _, res := range resources {
			// Check global resource lookup to prevent cross-connection conflicts.
			existsSameConn := false
			globalRes, err := s.store.LoadResource(ctx, res.ProviderID, res.Type, res.ExternalID)
			if err == nil && globalRes != nil {
				// Resource exists globally - check if it's under the same connection.
				if globalRes.ConnectionID != p.ID {
					slog.Warn("observe: resource belongs to another connection, skipping",
						"connection", p.ID,
						"resource_type", res.Type,
						"external_id", res.ExternalID,
						"owner", globalRes.ConnectionID,
					)
					continue
				}
				existsSameConn = true
			} else if err != nil && err != sql.ErrNoRows {
				// Database error - stop observation for this resource.
				slog.Warn("observe: database error checking resource, skipping",
					"connection", p.ID,
					"resource_type", res.Type,
					"external_id", res.ExternalID,
					"err", err,
				)
				continue
			}

			// Observation refresh must never mutate ownership or
			// association; insertion is only for previously untracked
			// (external) resources.
			var persistErr error
			if existsSameConn {
				persistErr = s.store.UpdateResourceObservation(ctx, &res)
			} else {
				persistErr = s.store.SaveResource(ctx, &res)
			}
			if persistErr != nil {
				slog.Warn("observe: persist resource failed",
					"connection", p.ID,
					"resource_type", res.Type,
					"external_id", res.ExternalID,
					"err", persistErr,
				)
				// Do not restore resources whose persistence failed: the
				// database is the ownership authority and an unpersisted
				// resource must not be installed into runtime memory.
				continue
			}
			acceptedResources = append(acceptedResources, res)
		}

		// Restore only accepted resources into controller runtime.
		if len(acceptedResources) > 0 {
			s.controller.RestoreResources(p.ID, acceptedResources)
		}

		slog.Debug("observed and persisted provider resources",
			"connection", p.ID,
			"provider", observed.ProviderID,
			"resource_count", len(resources),
		)
	}
}

// markProviderUnavailable turns an otherwise misleading runtime projection
// into an actionable state. Persisted profiles are retained intact: the user
// can restore the provider/account later, but Portico must never represent an
// open connection as healthy when it cannot load its selected adapter.
func (s *Supervisor) markProviderUnavailable(ctx context.Context, profile *core.ConnectionProfile) {
	if profile == nil {
		return
	}
	now := time.Now().UTC()
	finding := core.DiagnosticFinding{
		ID:           core.FindingID(fmt.Sprintf("find-%s-provider-unavailable", profile.ID)),
		ConnectionID: profile.ID,
		Segment:      core.SegmentProviderEdge,
		Severity:     core.SeverityError,
		Summary:      "Selected provider is unavailable",
		Explanation:  fmt.Sprintf("Portico cannot load the %q provider selected by this connection. The profile and its resources are retained, but opening, repair, and automatic reconciliation are paused until that provider is configured.", profile.GetProvider().ProviderID),
		Evidence: []core.Evidence{{
			Type: "provider_registry", Source: "supervisor", Message: "provider adapter is not registered",
			Data: map[string]string{"provider_id": string(profile.GetProvider().ProviderID)},
		}},
		ObservedAt: now,
	}

	if rt, ok := s.controller.GetRuntime(profile.ID); ok {
		// A desired-open connection cannot be truthfully presented as open
		// while no provider adapter exists. Closed profiles remain closed but
		// receive the same durable finding for visibility.
		if profile.Desired == core.DesiredOpen {
			rt.State = core.RuntimeError
			rt.LastTransition = now
			rt.Error = core.ErrProviderNotFound(profile.GetProvider().ProviderID)
		}
		if !hasFinding(rt.Diagnostics, finding.ID) {
			rt.Diagnostics = append(rt.Diagnostics, finding)
		}
		s.controller.RestoreRuntime(rt)
		if err := s.store.SaveRuntime(ctx, rt); err != nil {
			slog.Warn("persist unavailable provider runtime", "connection", profile.ID, "err", err)
		}
	}
	if err := s.store.SaveFinding(ctx, &finding); err != nil {
		slog.Warn("persist unavailable provider finding", "connection", profile.ID, "err", err)
	}
}

func hasFinding(findings []core.DiagnosticFinding, id core.FindingID) bool {
	for _, finding := range findings {
		if finding.ID == id && finding.ResolvedAt == nil {
			return true
		}
	}
	return false
}

// ownershipForObserved returns the ownership for an observed resource.
// If the resource was previously tracked, its ownership is preserved.
// Otherwise, observed resources without provenance are marked as external.
func ownershipForObserved(existing map[string]core.ResourceOwnership, providerID core.ProviderID, resourceType core.ResourceType, externalID string) core.ResourceOwnership {
	key := string(providerID) + ":" + string(resourceType) + ":" + externalID
	if ownership, ok := existing[key]; ok {
		return ownership
	}
	return core.OwnershipExternal
}

// rehydrateTunnelCredentials reconstructs provider connection state from
// stored tunnel credentials. This enables permanent connectors to survive
// supervisor restart (P0 #4).
func (s *Supervisor) rehydrateTunnelCredentials(ctx context.Context) {
	profiles := s.controller.ListProfiles()
	for _, p := range profiles {
		prov := s.registry.Get(p.GetProvider().ProviderID)
		if prov == nil {
			continue
		}

		// Find the tunnel resource for this connection.
		resources, err := s.store.ListResourcesByConnection(ctx, p.ID)
		if err != nil {
			continue
		}
		var tunnelIDs []string
		for _, r := range resources {
			if r.Type == core.ResourceTunnel && resourceIsLive(r) {
				tunnelIDs = append(tunnelIDs, r.ExternalID)
			}
		}
		if len(tunnelIDs) != 1 {
			if len(tunnelIDs) > 1 {
				slog.Warn("cannot select an unambiguous tunnel credential for rehydration", "connection", p.ID, "tunnels", len(tunnelIDs))
			}
			continue
		}
		tunnelID := tunnelIDs[0]

		// Try to rehydrate the connection from stored credentials.
		if cf, ok := prov.(interface {
			RehydrateConnection(ctx context.Context, connID core.ConnectionID, tunnelID string) bool
		}); ok {
			if cf.RehydrateConnection(ctx, p.ID, tunnelID) {
				slog.Info("rehydrated tunnel credential",
					"connection", p.ID, "tunnel", tunnelID)
			}
		}
	}
}

func (s *Supervisor) classifyResourceState(ctx context.Context) {
	// Classify resources as managed/adopted/external and detect orphans
	// by comparing observed resources with stored provider resources.
	profiles := s.controller.ListProfiles()
	for _, p := range profiles {
		if s.registry.Get(p.GetProvider().ProviderID) == nil {
			s.markProviderUnavailable(ctx, p)
			continue
		}
		observed, err := s.observeConnection(ctx, p.ID)
		if err != nil {
			slog.Warn("classify: observe failed", "connection", p.ID, "err", err)
			continue
		}
		if observed == nil {
			continue
		}

		// Compare stored resources against observed set.
		stored, err := s.store.ListResourcesByConnection(ctx, p.ID)
		if err != nil {
			slog.Warn("classify: list stored resources failed", "connection", p.ID, "err", err)
			continue
		}

		for _, res := range stored {
			if res.Lifecycle == core.LifecycleRemoved || res.Lifecycle == core.LifecycleExternallyRemoved {
				continue
			}
			if res.Ownership == core.OwnershipManaged && observedMissing(observed, res.Type, res.ExternalID) {
				// Observation supplied an exact resource inventory; this resource
				// was therefore authoritatively removed outside Portico.
				slog.Warn("orphaned managed resource",
					"connection", p.ID,
					"resource_type", res.Type,
					"external_id", res.ExternalID,
				)
				if err := s.store.MarkResourceExternallyRemoved(ctx, p.ID, res.ProviderID, res.Type, res.ExternalID); err != nil {
					slog.Warn("classify: persist externally removed resource", "connection", p.ID, "resource_type", res.Type, "external_id", res.ExternalID, "err", err)
					continue
				}
				finding := core.DiagnosticFinding{
					ID:           core.FindingID(fmt.Sprintf("finding-%s-resource-externally-removed-%s-%s", p.ID, res.Type, res.ExternalID)),
					ConnectionID: p.ID,
					Segment:      core.SegmentProviderEdge,
					Severity:     core.SeverityWarning,
					Summary:      "Managed provider resource was removed outside Portico",
					Explanation:  fmt.Sprintf("Portico could not find the managed %s resource %q during an exact provider observation. Review and apply the smallest proposed repair before opening the connection again.", res.Type, res.ExternalID),
					Evidence:     []core.Evidence{{Type: "provider_resource", Source: "exact_observation", Message: "provider returned resource not found", Data: map[string]string{"provider_id": string(res.ProviderID), "resource_type": string(res.Type), "external_id": res.ExternalID}}},
					ObservedAt:   time.Now().UTC(),
				}
				if err := s.store.SaveFinding(ctx, &finding); err != nil {
					slog.Warn("classify: persist externally removed finding", "connection", p.ID, "resource_type", res.Type, "external_id", res.ExternalID, "err", err)
				}
			}
		}
	}
}

// --------------- Shutdown sequence (SPEC §10.4) ---------------

// shutdown performs the graceful shutdown sequence.
// Idempotent: safe to call multiple times.
func (s *Supervisor) shutdown(ctx context.Context) error {
	var err error
	s.shutdownOnce.Do(func() {
		err = s.shutdownOnce_(ctx)
	})
	return err
}

func (s *Supervisor) shutdownOnce_(ctx context.Context) error {
	slog.Info("supervisor shutting down")

	s.beginShutdown()
	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()

	// Signal stop to all goroutines
	select {
	case <-s.stopCh:
		// Already closed
	default:
		close(s.stopCh)
	}

	// Stop accepting new mutations FIRST (SPEC §10.4). The IPC server is
	// closed before any operations are canceled or processes are torn
	// down so no handler can enter the store after the database begins
	// to close and no mutating call can land during teardown.
	if s.ipcServer != nil {
		s.ipcServer.PublishEvent(ipc.EventDTO{
			Type:      "supervisor.shutdown",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Data:      map[string]string{"reason": "graceful shutdown"},
		})
		s.ipcServer.Stop()
		s.serveWG.Wait()
	}

	// Settle every active operation. Use a fresh bounded context because
	// the caller's context is commonly already canceled.
	if s.controller != nil {
		settleCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := s.controller.ShutdownOperations(settleCtx); err != nil {
			slog.Warn("shutdown: active operations did not settle cleanly", "err", err)
		}
		cancel()
	}

	// v0.1 shutdown policy: stop every supervisor-owned child — both the
	// connector and its origin — together. Leaving a connector attached
	// to a stopped origin creates an invalid route (running cloudflared
	// with no upstream). Each subsystem gets its own fresh bounded
	// context so cancellation never leaks from the caller.
	for _, profile := range s.controller.ListProfiles() {
		if s.procMgr != nil {
			if err := s.procMgr.Stop(profile.ID, 5*time.Second); err != nil {
				slog.Warn("shutdown: stop connector", "connection", profile.ID, "err", err)
			}
		}
	}
	if s.origins != nil {
		originCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := s.origins.StopAll(originCtx); err != nil {
			slog.Warn("shutdown: stopping local origins", "err", err)
		}
		cancel()
	}

	// Flush events.
	slog.Info("shutdown: flushing events")

	// Close database.
	if s.store != nil {
		s.store.Close()
	}

	// Release the supervisor lock (idempotent).
	if s.lock != nil {
		s.lock.Release()
		s.lock = nil
	}

	slog.Info("supervisor stopped")
	return nil
}

// --------------- Event broker ---------------

// eventBroker is a goroutine that manages the supervisor's event stream.
func (s *Supervisor) eventBroker(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			if s.ipcServer != nil {
				s.ipcServer.PublishEvent(ipc.EventDTO{
					Type:      string(core.EventSnapshotChanged),
					Timestamp: time.Now().UTC().Format(time.RFC3339),
				})
			}
		}
	}
}

// PublishEvent publishes an event to all SSE subscribers.
func (s *Supervisor) PublishEvent(evt core.Event) {
	if s.ipcServer == nil {
		return
	}

	// Pass structured data directly - do not pre-marshal to JSON
	// to avoid double-marshalling when the IPC server encodes it.
	dto := ipc.EventDTO{Type: string(evt.Type), Timestamp: evt.Timestamp.Format(time.RFC3339), Data: evt.Data}
	switch data := evt.Data.(type) {
	case core.OperationEvent:
		dto.OperationID = string(data.OperationID)
		dto.ConnectionID = string(data.ConnectionID)
		dto.Stage = string(data.Stage)
	case core.ConnectionEvent:
		dto.ConnectionID = string(data.ConnectionID)
	case core.ConnectorEvent:
		dto.ConnectionID = string(data.ConnectionID)
		dto.Stage = string(data.Status)
	}
	s.ipcServer.PublishEvent(dto)
}

// markProfileInvalidForOpen records that a connection's desired state can no
// longer be realised.
//
// This is a durable, operator-actionable condition rather than a transient
// one: the connection says it should be open, Portico refuses to make it so,
// automatic reconciliation has stopped, and only editing, closing or deleting
// the connection will change that. Left in the log it is invisible in the place
// the user is looking.
//
// The connection is deliberately not closed. Validation covers far more than
// safety, and turning every rule that tightens into an automatic outage is too
// broad a policy to attach to a generic check. Whatever is already running keeps
// running, and the finding says so.
func (s *Supervisor) markProfileInvalidForOpen(ctx context.Context, profile *core.ConnectionProfile, reason error) {
	if profile == nil || reason == nil {
		return
	}
	now := time.Now().UTC()
	finding := core.DiagnosticFinding{
		ID:           core.FindingID(fmt.Sprintf("find-%s-profile-invalid-for-open", profile.ID)),
		ConnectionID: profile.ID,
		Segment:      core.SegmentProviderEdge,
		Severity:     core.SeverityError,
		Summary:      "This connection can no longer be opened as configured",
		Explanation: fmt.Sprintf(
			"Portico will not open or repair this connection because its saved settings are no longer "+
				"valid: %s. Anything already running is left alone and its address may still work, but "+
				"automatic recovery has stopped. Edit the connection to correct it, or close or delete it.",
			reason),
		Evidence: []core.Evidence{{
			Type: "profile_validation", Source: "supervisor", Message: reason.Error(),
		}},
		ObservedAt: now,
	}

	if rt, ok := s.controller.GetRuntime(profile.ID); ok {
		// The connector status, endpoint and resource inventory are preserved:
		// what is broken is the configuration, not necessarily the connection.
		if profile.Desired == core.DesiredOpen {
			rt.State = core.RuntimeError
			rt.LastTransition = now
		}
		if !hasFinding(rt.Diagnostics, finding.ID) {
			rt.Diagnostics = append(rt.Diagnostics, finding)
		}
		s.controller.RestoreRuntime(rt)
		if err := s.store.SaveRuntime(ctx, rt); err != nil {
			slog.Warn("persist invalid-profile runtime", "connection", profile.ID, "err", err)
		}
	}
	if err := s.store.SaveFinding(ctx, &finding); err != nil {
		slog.Warn("persist invalid-profile finding", "connection", profile.ID, "err", err)
	}
}
