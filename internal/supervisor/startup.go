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

	"github.com/paoloanzn/portico/internal/app"
	"github.com/paoloanzn/portico/internal/core"
	"github.com/paoloanzn/portico/internal/diagnostics"
	"github.com/paoloanzn/portico/internal/discovery"
	"github.com/paoloanzn/portico/internal/ipc"
	"github.com/paoloanzn/portico/internal/lock"
	"github.com/paoloanzn/portico/internal/process"
	"github.com/paoloanzn/portico/internal/store"
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
	if err := s.loadProviderAccounts(ctx); err != nil {
		slog.Warn("startup: provider accounts not available", "err", err)
	}

	// Phase 5: Create provider adapters.
	slog.Info("startup: creating provider adapters")
	// Provider adapters are registered during New() or via discovery.
	// This phase validates they are ready.
	if err := s.validateProviders(ctx); err != nil {
		slog.Warn("startup: some providers not available", "err", err)
	}

	// Phase 5b: Initialize discovery, diagnostics, and origin services.
	s.listenerEnum = discovery.NewSSEnumerator()
	s.diagEngine = diagnostics.New(nil) // Will be updated with real provider
	slog.Info("startup: discovery and diagnostics initialized")

	// Phase 6: Load connection profiles WITHOUT changing them.
	slog.Info("startup: restoring profiles")
	if err := s.loadProfiles(ctx); err != nil {
		slog.Error("startup: failed to load profiles", "err", err)
	}

	// Phase 7: Load saved runtimes.
	slog.Info("startup: restoring runtimes")
	if err := s.loadRuntimes(ctx); err != nil {
		slog.Warn("startup: failed to load some runtimes", "err", err)
	}

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

func (s *Supervisor) loadProviderAccounts(ctx context.Context) error {
	accounts, err := s.store.ListProviderAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list provider accounts: %w", err)
	}

	// Group account IDs by provider
	type providerGroup struct {
		providerID core.ProviderID
		ids        []core.ProviderAccountID
	}
	groups := make(map[core.ProviderID]*providerGroup)
	for _, a := range accounts {
		g, ok := groups[a.Provider]
		if !ok {
			g = &providerGroup{providerID: a.Provider}
			groups[a.Provider] = g
		}
		g.ids = append(g.ids, a.ID)
	}

	// Register accounts with each provider
	for provID, group := range groups {
		s.registry.SetAccounts(provID, group.ids)
		slog.Info("provider accounts loaded", "provider", provID, "count", len(group.ids))
	}

	slog.Info("loaded provider accounts", "count", len(accounts))
	return nil
}

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

func (s *Supervisor) loadRuntimes(ctx context.Context) error {
	profiles := s.controller.ListProfiles()
	for _, p := range profiles {
		rt, err := s.store.LoadRuntime(ctx, p.ID)
		if err != nil {
			continue
		}
		if rt != nil {
			s.controller.RestoreRuntime(rt)
		}
	}
	return nil
}

// restartDesiredOpen restarts all connections that should be open.
// Uses the delta planner (computeReconcileDecision) to determine the smallest
// action needed, avoiding unnecessary recreation of infrastructure.
func (s *Supervisor) restartDesiredOpen(ctx context.Context) {
	profiles := s.controller.ListProfiles()
	for _, p := range profiles {
		if p.Desired == core.DesiredOpen {
			go func(connID core.ConnectionID) {
				// Use the delta planner to determine what needs to be done.
				rt, _ := s.controller.GetRuntime(connID)
				resources, _ := s.store.ListResourcesByConnection(ctx, connID)

				input := ReconcileInput{
					Profile:   p,
					Runtime:   rt,
					Resources: resources,
				}
				// Best-effort provider observation for authoritative decisions.
				if prov := s.registry.Get(p.Provider.ProviderID); prov != nil {
					if obs, oErr := prov.Observe(ctx, connID); oErr == nil {
						input.Observed = obs
					}
				}

				decision, err := s.computeReconcileDecision(ctx, input)
				if err != nil {
					slog.Warn("restart: reconcile decision failed", "connection", connID, "err", err)
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
			}(p.ID)
		}
	}
}

// buildConnectorRestartPlan creates a plan that only starts the connector
// for a connection with existing infrastructure. This avoids recreating
// Cloudflare resources that already exist.
func (s *Supervisor) buildConnectorRestartPlan(connID core.ConnectionID, p *core.ConnectionProfile) *core.OperationPlan {
	// Check current connector state from runtime.
	rt, ok := s.controller.GetRuntime(connID)
	if ok && rt.Connector.Status == core.ConnectorStatusRunning {
		// Connector already running, no restart needed.
		slog.Info("restart: connector already running", "connection", connID)
		return nil
	}

	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    connID,
		ProfileRevision: p.Revision,
		Provider:        p.Provider.ProviderID,
		Intent:          core.IntentRepair,
		Steps: []core.PlanStep{
			{
				ID:      "restart-start-connector",
				Kind:    core.StepStartConnector,
				Summary: "Start connector process",
				Technical: core.TechnicalOperation{
					Provider:   p.Provider.ProviderID,
					Type:       "start_connector",
					Parameters: map[string]string{"mode": "permanent"},
				},
				Destructive:  false,
				Irreversible: false,
			},
		},
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}

	if err := plan.ComputeFingerprint(); err != nil {
		slog.Warn("restart: plan fingerprint failed", "connection", connID, "err", err)
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

func (s *Supervisor) recoverOperationJournals(ctx context.Context) error {
	// Load operations that were interrupted mid-execution.
	incompleteOps, err := s.store.ListIncompleteOperations(ctx)
	if err != nil {
		return err
	}

	for _, opID := range incompleteOps {
		results, err := s.store.GetStepResults(ctx, opID)
		if err != nil {
			slog.Warn("failed to load step results", "operation", opID, "err", err)
			continue
		}

		// Classify each step and determine recovery action.
		needsRecovery := false
		for _, r := range results {
			switch r.Status {
			case string(store.StepStarted), string(store.StepOutcomeUnknown):
				// Step started but outcome unknown - needs compensation.
				needsRecovery = true
				if err := s.store.MarkStepRecoveryRequired(ctx, opID, r.StepID); err != nil {
					slog.Warn("failed to mark step recovery", "operation", opID, "step", r.StepID, "err", err)
				}
				slog.Warn("operation has uncertain step outcome - recovery required",
					"operation", opID, "step", r.StepID, "kind", r.StepKind)
			case string(store.StepCompensationPending):
				// Compensation was in progress - needs retry.
				needsRecovery = true
				slog.Warn("operation has pending compensation - recovery required",
					"operation", opID, "step", r.StepID)
			}
		}

		if needsRecovery {
			slog.Error("operation requires recovery",
				"operation", opID, "steps", len(results))
			// Create a diagnostic finding for operator attention.
			finding := &core.DiagnosticFinding{
				ID:          core.FindingID(fmt.Sprintf("recovery-%s", opID)),
				Segment:     core.SegmentConnector,
				Severity:    core.SeverityError,
				Summary:     fmt.Sprintf("Operation %s was interrupted and requires recovery", opID),
				Explanation: "The supervisor was interrupted during operation execution. Some steps may need compensation.",
				ObservedAt:  time.Now().UTC(),
			}
			if err := s.store.SaveFinding(ctx, finding); err != nil {
				slog.Warn("failed to save recovery finding", "err", err)
			}
		} else {
			slog.Info("incomplete operation found but all steps resolved", "operation", opID)
		}
	}

	return nil
}

func (s *Supervisor) verifyConnectorIdentities(ctx context.Context) {
	// Check if any persisted connector processes are still running
	// by verifying /proc entries against stored identities.
	processes := s.procMgr.ListProcesses()
	for _, mp := range processes {
		if err := process.VerifyIdentity(mp); err != nil {
			slog.Warn("connector identity mismatch",
				"connection", mp.ConnectionID,
				"pid", mp.Identity.PID,
				"err", err,
			)
			continue
		}
		slog.Debug("connector identity verified",
			"connection", mp.ConnectionID,
			"pid", mp.Identity.PID,
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
		prov := s.registry.Get(p.Provider.ProviderID)
		if prov == nil {
			continue
		}
		observed, err := prov.Observe(ctx, p.ID)
		if err != nil {
			slog.Warn("observe failed", "connection", p.ID, "err", err)
			continue
		}
		if observed == nil {
			continue
		}

		// Persist observation into controller runtime.
		if _, err := s.controller.Observe(ctx, p.ID); err != nil {
			slog.Warn("observe persist failed", "connection", p.ID, "err", err)
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
				// Same connection - update will happen via SaveResource
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

			if err := s.store.SaveResource(ctx, &res); err != nil {
				slog.Warn("observe: save resource failed",
					"connection", p.ID,
					"resource_type", res.Type,
					"external_id", res.ExternalID,
					"err", err,
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
		prov := s.registry.Get(p.Provider.ProviderID)
		if prov == nil {
			continue
		}

		// Find the tunnel resource for this connection.
		resources, err := s.store.ListResourcesByConnection(ctx, p.ID)
		if err != nil {
			continue
		}
		var tunnelID string
		for _, r := range resources {
			if r.Type == core.ResourceTunnel {
				tunnelID = r.ExternalID
				break
			}
		}

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
		prov := s.registry.Get(p.Provider.ProviderID)
		if prov == nil {
			continue
		}

		observed, err := prov.Observe(ctx, p.ID)
		if err != nil {
			slog.Warn("classify: observe failed", "connection", p.ID, "err", err)
			continue
		}
		if observed == nil {
			continue
		}

		// Build set of observed resource external IDs by type.
		type observedKey struct {
			resourceType string
			externalID   string
		}
		observedSet := make(map[observedKey]bool)

		if observed.Tunnel != nil {
			observedSet[observedKey{"tunnel", observed.Tunnel.ID}] = true
		}
		for _, dns := range observed.DNSRecords {
			observedSet[observedKey{"dns_record", dns.ID}] = true
		}
		for _, app := range observed.AccessApps {
			observedSet[observedKey{"access_application", app.ID}] = true
		}

		// Compare stored resources against observed set.
		stored, err := s.store.ListResourcesByConnection(ctx, p.ID)
		if err != nil {
			slog.Warn("classify: list stored resources failed", "connection", p.ID, "err", err)
			continue
		}

		for _, res := range stored {
			key := observedKey{string(res.Type), res.ExternalID}
			if res.Ownership == core.OwnershipManaged && !observedSet[key] {
				// Managed resource not found in observed state — orphaned.
				slog.Warn("orphaned managed resource",
					"connection", p.ID,
					"resource_type", res.Type,
					"external_id", res.ExternalID,
				)
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

	// Stop connectors before shutting down IPC — no new work after this.
	slog.Info("shutdown: stopping connectors")
	s.procMgr.Cleanup()

	// Stop accepting mutations (IPC server shuts down first).
	// This causes Serve() to return, which unblocks serveWG.
	if s.ipcServer != nil {
		// Publish shutdown event
		s.ipcServer.PublishEvent(ipc.EventDTO{
			Type:      "supervisor.shutdown",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Data:      map[string]string{"reason": "graceful shutdown"},
		})
		s.ipcServer.Stop()
	}

	// Wait for the serve goroutine to exit before releasing resources.
	s.serveWG.Wait()

	// Complete or cancel active operations (v0.1: cancel all).
	slog.Info("shutdown: completing/cancelling active operations")

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
	s.ipcServer.PublishEvent(ipc.EventDTO{
		Type:      string(evt.Type),
		Timestamp: evt.Timestamp.Format(time.RFC3339),
		Data:      evt.Data,
	})
}
