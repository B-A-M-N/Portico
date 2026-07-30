package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/diagnostics"
	"github.com/B-A-M-N/portico/internal/discovery"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/origin"
	"github.com/B-A-M-N/portico/internal/process"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/store"
)

// Supervisor is the local daemon that owns the database, manages processes,
// publishes lifecycle events, and reconciles desired with observed state.
type Supervisor struct {
	mu           sync.RWMutex
	store        *store.Store
	controller   *controller.Controller
	procMgr      *process.Manager
	registry     provider.Registry
	ipcServer    *ipc.Server
	paths        app.Paths
	lock         *SupervisorLock
	ready        bool
	mutating     bool // true while accepting mutations; set false at shutdown start
	stopCh       chan struct{}
	serveWG      sync.WaitGroup // tracks the IPC serve goroutine
	shutdownOnce sync.Once      // guards idempotent shutdown

	// Discovery, diagnostics, and origins
	discoverer discovery.Discoverer
	diagEngine *diagnostics.Engine
	origins    *origin.Manager

	// Event-driven reconciliation: a buffered channel of connection IDs
	// that need re-evaluation. The reconcileLoop coalesces duplicates
	// via reconcilePending and reconciles only affected connections.
	reconcileCh      chan core.ConnectionID
	reconcilePending map[core.ConnectionID]struct{}
	reconcileMu      sync.Mutex
}

type cleanupRecorder struct{ store *store.Store }

func (r cleanupRecorder) RecordCleanupItem(ctx context.Context, operationID core.OperationID, connectionID core.ConnectionID, providerID core.ProviderID, resourceType core.ResourceType, externalID, state, lastError string) error {
	return r.store.RecordCleanupItem(ctx, store.CleanupItem{
		OperationID: operationID, ConnectionID: connectionID, ProviderID: providerID,
		ResourceType: resourceType, ExternalID: externalID, State: state, LastError: lastError,
	})
}

// New creates a new supervisor.
// The procMgr is shared with providers for connector lifecycle management.
func New(paths app.Paths, registry provider.Registry, procMgr *process.Manager, st *store.Store) (*Supervisor, error) {
	// Ensure directories exist.
	for _, dir := range []string{
		filepath.Dir(paths.SocketPath),
		filepath.Dir(paths.DatabasePath),
		paths.LogDir,
		paths.ConnectorLogDir(),
	} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	// Acquire supervisor lock before any initialization
	lock, err := NewSupervisorLock(paths)
	if err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}

	// Create controller.
	ctrl := controller.New(registry, st)
	ctrl.SetDeleteFinalizer(st)
	ctrl.SetConnectionStorer(st)
	ctrl.SetResourceRemover(st)
	ctrl.SetRuntimeCommitter(st)
	ctrl.SetRuntimeSaver(st)
	ctrl.SetResourceSaver(st)
	ctrl.SetCredentialStorer(st)
	ctrl.SetStepCommitter(st)
	ctrl.SetCleanupRecorder(cleanupRecorder{store: st})
	ctrl.SetProfileUpdater(st)
	origins := origin.NewManager()
	ctrl.SetOriginManager(origins)

	sup := &Supervisor{
		store:            st,
		controller:       ctrl,
		procMgr:          procMgr,
		registry:         registry,
		paths:            paths,
		lock:             lock,
		origins:          origins,
		stopCh:           make(chan struct{}),
		reconcileCh:      make(chan core.ConnectionID, 64),
		reconcilePending: make(map[core.ConnectionID]struct{}),
		mutating:         true, // accepting mutations until shutdown begins
	}
	procMgr.SetEventSink(sup.handleProcessEvent)
	return sup, nil
}

// handleProcessEvent consumes actor lifecycle events and keeps the durable
// runtime projection synchronized with the real connector process.
func (s *Supervisor) handleProcessEvent(event process.ProcessEvent) {
	select {
	case <-s.stopCh:
		return // shutdown owns ordering; never write after it begins closing storage
	default:
	}
	rt, ok := s.controller.GetRuntime(event.ConnectionID)
	if !ok {
		return
	}
	rt.Connector.PID = event.Identity.PID
	rt.Connector.StartTime = event.Identity.StartTime
	rt.Connector.Executable = event.Identity.ExecutablePath
	rt.Connector.CommandHash = event.Identity.CommandHash
	rt.Connector.Restarts = event.Restarts
	rt.Connector.LastError = event.Error
	switch event.Status {
	case process.ProcessStatusRunning:
		rt.Connector.Status = core.ConnectorStatusRunning
	case process.ProcessStatusStarting:
		rt.Connector.Status = core.ConnectorStatusStarting
	case process.ProcessStatusStopped:
		rt.Connector.Status = core.ConnectorStatusStopped
	case process.ProcessStatusUnstable:
		rt.Connector.Status = core.ConnectorStatusUnstable
		rt.State = core.RuntimeDegraded
	default:
		rt.Connector.Status = core.ConnectorStatusCrashed
		if rt.State == core.RuntimeOpen {
			rt.State = core.RuntimeDegraded
		}
	}
	rt.LastTransition = event.Timestamp
	payload := map[string]interface{}{
		"connection_id": string(event.ConnectionID),
		"stage":         string(event.Status),
		"restarts":      event.Restarts,
		"error":         event.Error,
	}
	if _, err := s.store.CommitConnectorRuntimeEvent(
		context.Background(), rt, "connector."+string(event.Type), string(event.Status), event.Timestamp, payload,
	); err != nil {
		slog.Error("persist connector process event", "connection", event.ConnectionID, "type", event.Type, "err", err)
		return
	}
	s.controller.RestoreRuntime(rt)
	if s.ipcServer != nil {
		if err := s.ipcServer.DispatchCommittedEvents(context.Background()); err != nil {
			slog.Error("dispatch connector process event", "connection", event.ConnectionID, "type", event.Type, "err", err)
		}
	}

	// Trigger event-driven reconciliation for terminal process states.
	// The reconcile loop will evaluate whether a repair is needed.
	switch event.Status {
	case process.ProcessStatusStopped, process.ProcessStatusUnstable:
		s.TriggerReconcile(event.ConnectionID)
	default:
		// Crashed is the default case (line 147)
		if rt.Connector.Status == core.ConnectorStatusCrashed {
			s.TriggerReconcile(event.ConnectionID)
		}
	}
}

// Start initializes the supervisor and begins serving.
func (s *Supervisor) Start(ctx context.Context) error {
	err := s.startup(ctx)
	if err != nil {
		s.shutdown(ctx)
	}
	return err
}

// loadProfiles loads persisted profiles and runtimes into the controller
// using hydration methods that preserve IDs, revisions, and timestamps.
func (s *Supervisor) loadProfiles(ctx context.Context) error {
	profiles, err := s.store.ListProfiles(ctx)
	if err != nil {
		return err
	}

	for _, p := range profiles {
		// Use RestoreProfile to preserve IDs, revisions, and timestamps
		// without triggering provider mutations or creating new closed runtimes
		s.controller.RestoreProfile(p)

		// Load runtime if exists. Distinguish sql.ErrNoRows (benign)
		// from database I/O errors (real problems).
		rt, runtimeErr := s.store.LoadRuntime(ctx, p.ID)
		switch runtimeErr {
		case nil:
			if rt != nil {
				s.controller.RestoreRuntime(rt)
				slog.Info("loaded runtime for connection", "id", p.ID, "state", rt.State)
			}
		default:
			slog.Warn("failed to load runtime for connection", "id", p.ID, "err", runtimeErr)
		}

		// Load provider resources regardless of runtime state.
		resources, err := s.store.ListResourcesByConnection(ctx, p.ID)
		if err != nil {
			slog.Warn("failed to load resources", "connection", p.ID, "err", err)
			continue
		}
		if len(resources) > 0 {
			s.controller.RestoreResources(p.ID, resources)
		}
	}

	// Load incomplete operations and plans
	plans, err := s.store.ListIncompletePlans(ctx)
	if err == nil {
		for _, plan := range plans {
			s.controller.RestorePlan(plan)
		}
	}

	// Load findings
	for _, p := range profiles {
		findings, err := s.store.ListUnresolvedFindings(ctx, p.ID)
		if err == nil {
			for _, f := range findings {
				s.controller.RestoreFinding(&f)
			}
		}
	}

	slog.Info("loaded profiles", "count", len(profiles))
	return nil
}

// reconcileLoop periodically reconciles desired vs observed state and
// responds to event-driven reconciliation triggers. The periodic ticker
// acts as a safety net; events from operation completions, connector
// exits, and profile changes trigger immediate targeted reconciliation.
func (s *Supervisor) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.reconcileAll(ctx)
		case connID := <-s.reconcileCh:
			// Coalesce: drain all pending events before reconciling.
			s.reconcileMu.Lock()
			pending := make(map[core.ConnectionID]struct{})
			pending[connID] = struct{}{}
			for k := range s.reconcilePending {
				pending[k] = struct{}{}
			}
			s.reconcilePending = make(map[core.ConnectionID]struct{})
			s.reconcileMu.Unlock()

			// Drain any additional queued events.
		drain:
			for {
				select {
				case id := <-s.reconcileCh:
					pending[id] = struct{}{}
				default:
					break drain
				}
			}

			// Reconcile only the affected connections.
			for id := range pending {
				s.reconcileOne(ctx, id)
			}
		}
	}
}

// TriggerReconcile enqueues a connection for event-driven reconciliation.
// Duplicate triggers are coalesced — the connection is reconciled once
// per drain cycle regardless of how many events arrive.
func (s *Supervisor) TriggerReconcile(connID core.ConnectionID) {
	s.reconcileMu.Lock()
	s.reconcilePending[connID] = struct{}{}
	s.reconcileMu.Unlock()

	select {
	case s.reconcileCh <- connID:
	default:
		// Channel full — the pending map already records the trigger,
		// and the next ticker cycle will pick it up.
	}
}

// reconcileOne reconciles a single connection by ID.
func (s *Supervisor) reconcileOne(ctx context.Context, connID core.ConnectionID) {
	p, ok := s.controller.GetProfile(connID)
	if !ok {
		return
	}
	if s.registry.Get(p.GetProvider().ProviderID) == nil {
		s.markProviderUnavailable(ctx, p)
		return
	}

	input := ReconcileInput{Profile: p}
	if rt, ok := s.controller.GetRuntime(p.ID); ok {
		input.Runtime = rt
		input.Resources = rt.Provider.Resources
	} else if resources, err := s.store.ListResourcesByConnection(ctx, p.ID); err != nil {
		slog.Warn("reconcile: load persisted resources", "connection", p.ID, "err", err)
	} else {
		input.Resources = resources
	}
	if obs, err := s.observeConnection(ctx, p.ID); err == nil {
		input.Observed = obs
	}

	decision, err := s.computeReconcileDecision(ctx, input)
	if err != nil {
		slog.Warn("reconcile error", "connection", p.ID, "err", err)
		return
	}
	if decision == nil || decision.Action == "none" || decision.Plan == nil {
		return
	}

	slog.Info("reconcile: applying decision", "connection", p.ID, "action", decision.Action)
	go func(connID core.ConnectionID, action string, candidate *core.OperationPlan) {
		plan, err := s.persistCanonicalPlan(ctx, candidate)
		if err != nil {
			slog.Warn("reconcile: persist plan failed", "connection", connID, "action", action, "err", err)
			return
		}
		if _, err := s.controller.ApplyPlan(ctx, plan.ID); err != nil {
			slog.Warn("reconcile: apply failed", "connection", connID, "action", action, "err", err)
		}
	}(p.ID, decision.Action, decision.Plan)
}

func (s *Supervisor) reconcileAll(ctx context.Context) {
	profiles := s.controller.ListProfiles()
	for _, p := range profiles {
		if s.registry.Get(p.GetProvider().ProviderID) == nil {
			s.markProviderUnavailable(ctx, p)
			continue
		}
		input := ReconcileInput{Profile: p}
		if rt, ok := s.controller.GetRuntime(p.ID); ok {
			input.Runtime = rt
			input.Resources = rt.Provider.Resources
		} else if resources, err := s.store.ListResourcesByConnection(ctx, p.ID); err != nil {
			slog.Warn("reconcile: load persisted resources", "connection", p.ID, "err", err)
		} else {
			// Runtime is a projection and may be absent after an interrupted
			// bootstrap. Durable resource inventory remains authoritative for
			// deciding whether a full open would duplicate infrastructure.
			input.Resources = resources
		}
		// Best-effort provider observation for authoritative decisions.
		if obs, err := s.observeConnection(ctx, p.ID); err == nil {
			input.Observed = obs
		}

		decision, err := s.computeReconcileDecision(ctx, input)
		if err != nil {
			slog.Warn("reconcile error", "connection", p.ID, "err", err)
			continue
		}
		if decision == nil || decision.Action == "none" || decision.Plan == nil {
			continue
		}

		slog.Info("reconcile: applying decision", "connection", p.ID, "action", decision.Action)
		go func(connID core.ConnectionID, action string, candidate *core.OperationPlan) {
			plan, err := s.persistCanonicalPlan(ctx, candidate)
			if err != nil {
				slog.Warn("reconcile: persist plan failed", "connection", connID, "action", action, "err", err)
				return
			}
			if _, err := s.controller.ApplyPlan(ctx, plan.ID); err != nil {
				slog.Warn("reconcile: apply failed", "connection", connID, "action", action, "err", err)
			}
		}(p.ID, decision.Action, decision.Plan)
	}
}

// --------------- IPC handler bridge ---------------

// supervisorHandler bridges IPC requests to supervisor operations.
type supervisorHandler struct {
	sup *Supervisor
}

func (h *supervisorHandler) HandleSnapshot() (*ipc.SnapshotDTO, error) {
	state, err := h.sup.store.ReadSnapshot(context.Background())
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	profiles := state.Profiles
	runtimes := state.Runtimes

	rtMap := make(map[core.ConnectionID]*core.ConnectionRuntime)
	for _, rt := range runtimes {
		rtMap[rt.ConnectionID] = rt
	}

	connDTOs := make([]ipc.ConnectionDTO, 0, len(profiles))
	for _, p := range profiles {
		rt := rtMap[p.ID]
		dto := ipc.ConnectionDTO{
			ID:                string(p.ID),
			Name:              p.Name,
			DesiredState:      string(p.Desired),
			ProviderID:        string(p.GetProvider().ProviderID),
			ProviderAccountID: string(p.GetProvider().AccountID),
		}
		if rt != nil {
			dto.RuntimeState = string(rt.State)
			dto.UserState = rt.State.UserFacingState()
			dto.PublicAddress = rt.Endpoint.PublicAddress
			dto.PrivateAddress = rt.Endpoint.PrivateAddress
			dto.ConnectorPID = rt.Connector.PID
			dto.ConnectorState = string(rt.Connector.Status)
			if rt.Error != nil {
				dto.Error = rt.Error.Message
			}
		}
		connDTOs = append(connDTOs, dto)
	}

	providers := h.sup.registry.List()
	provDTOs := make([]ipc.ProviderDTO, 0, len(providers))
	for _, p := range providers {
		dto := ipc.ProviderDTO{
			ID:            string(p.ID),
			Name:          p.Name,
			DisplayName:   p.DisplayName,
			Authenticated: p.Authenticated,
		}

		// Map capabilities to DTO
		caps := p.Capabilities
		dto.Capabilities = &ipc.CapabilitySetDTO{
			TemporaryAddresses: caps.TemporaryAddresses.Supported,
			CustomHostnames:    caps.CustomHostnames.Supported,
			PrivateExposure:    caps.PrivateExposure.Supported,
			ManagedDNS:         caps.ManagedDNS.Supported,
			TelemetrySupported: caps.Telemetry.Supported,
			MaxConnectors:      caps.Redundancy.MaxConnectors,
		}
		// Map protection modes
		for _, prot := range caps.BuiltInProtection {
			if prot.Supported {
				dto.Capabilities.ProtectionModes = append(dto.Capabilities.ProtectionModes, string(prot.Kind))
			}
		}
		// Map protocols
		for proto, pc := range caps.Protocols {
			if pc.Supported {
				dto.Capabilities.Protocols = append(dto.Capabilities.Protocols, string(proto))
			}
		}
		if caps.Expiration.Supported && caps.Expiration.MaxDuration > 0 {
			dto.Capabilities.ExpirationMaxSecs = int(caps.Expiration.MaxDuration.Seconds())
		}

		// Determine availability and readiness. The registry reports why a
		// catalogued provider has no usable adapter; only fall back to
		// account-based inference for entries that carry no verdict.
		dto.Availability = string(p.Availability)
		dto.LastError = p.Reason
		dto.SetupActions = append([]string(nil), p.SetupActions...)

		switch p.Availability {
		case provider.AvailabilityReady:
			dto.Readiness = "ready"
		case provider.AvailabilityUnconfigured:
			if len(p.Accounts) > 0 {
				dto.Readiness = "needs_auth"
			} else {
				dto.Readiness = "needs_config"
			}
		case provider.AvailabilityClientMissing:
			dto.Readiness = "needs_client"
		case provider.AvailabilityExperimental:
			dto.Readiness = "experimental"
		case provider.AvailabilityNotImplemented:
			dto.Readiness = "not_implemented"
		case provider.AvailabilityDegraded:
			dto.Readiness = "error"
		default:
			if p.Authenticated {
				dto.Availability, dto.Readiness = "ready", "ready"
			} else if len(p.Accounts) > 0 {
				dto.Availability, dto.Readiness = "unconfigured", "needs_auth"
			} else {
				dto.Availability, dto.Readiness = "unconfigured", "needs_config"
			}
		}

		for _, account := range p.Accounts {
			dto.Accounts = append(dto.Accounts, ipc.ProviderAccountDTO{
				ID: string(account.ID), Label: account.Label, Status: account.Status,
			})
		}
		provDTOs = append(provDTOs, dto)
	}

	return &ipc.SnapshotDTO{
		Connections: connDTOs,
		Providers:   provDTOs,
		LastSeq:     state.LastSeq,
	}, nil
}

func (h *supervisorHandler) HandleListConnections() ([]ipc.ConnectionDTO, error) {
	snap, err := h.HandleSnapshot()
	if err != nil {
		return nil, err
	}
	return snap.Connections, nil
}

func (h *supervisorHandler) HandleGetConnectionDetail(id string) (*ipc.ConnectionDetailDTO, error) {
	cid := core.ConnectionID(id)
	p, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, core.ErrProfileNotFound(cid)
	}
	rt, _ := h.sup.controller.GetRuntime(cid)

	summary, err := h.HandleGetConnection(id)
	if err != nil {
		return nil, err
	}

	detail := &ipc.ConnectionDetailDTO{
		Summary:  *summary,
		Revision: p.Revision,
		DesiredSpec: ipc.ConnectionSpecDTO{
			Source: ipc.SourceDTO{Kind: string(p.GetSource().Kind)},
			Exposure: ipc.ExposureDTO{
				Mode:             string(p.GetExposure().Mode),
				Protocol:         string(p.GetExposure().Protocol),
				RequestedAddress: p.GetExposure().RequestedAddress,
			},
			Protection: ipc.ProtectionDTO{
				Kind:           string(p.GetProtection().Kind),
				AllowedEmails:  p.GetProtection().AllowedEmails,
				AllowedDomains: p.GetProtection().AllowedDomains,
			},
		},
		Lifecycle: ipc.LifecycleDTO{
			AutoStart:    p.Lifecycle.AutoStart,
			OnDisconnect: string(p.Lifecycle.OnDisconnect),
		},
		Driver: ipc.DriverSelectionDTO{
			ProviderID: string(p.GetProvider().ProviderID),
			AccountID:  string(p.GetProvider().AccountID),
			Options:    p.Driver.Options,
		},
		CreatedAt: p.CreatedAt.Format(time.RFC3339),
		UpdatedAt: p.UpdatedAt.Format(time.RFC3339),
	}

	if p.GetSource().Existing != nil {
		detail.DesiredSpec.Source.Existing = &ipc.ExistingSourceDTO{Address: p.GetSource().Existing.Address}
	}
	if p.GetSource().Directory != nil {
		detail.DesiredSpec.Source.Directory = &ipc.DirectorySourceDTO{
			Path: p.GetSource().Directory.Path, Mode: string(p.GetSource().Directory.Mode),
			SPAFallback: p.GetSource().Directory.SPAFallback, AllowUpload: p.GetSource().Directory.AllowUpload, AllowDelete: p.GetSource().Directory.AllowDelete,
		}
	}
	if p.GetSource().Command != nil {
		detail.DesiredSpec.Source.Command = &ipc.CommandSourceDTO{
			Executable: p.GetSource().Command.Executable, Args: p.GetSource().Command.Args, WorkingDir: p.GetSource().Command.WorkingDir,
			Env: p.GetSource().Command.Env, Port: p.GetSource().Command.Port, Protocol: string(p.GetSource().Command.Protocol), UseShell: p.GetSource().Command.UseShell,
		}
	}
	if p.GetSource().MCP != nil {
		detail.DesiredSpec.Source.MCP = &ipc.MCPSourceDTO{Transport: string(p.GetSource().MCP.Transport), Endpoint: p.GetSource().MCP.Endpoint}
		if p.GetSource().MCP.Command != nil {
			detail.DesiredSpec.Source.MCP.Command = &ipc.CommandSourceDTO{
				Executable: p.GetSource().MCP.Command.Executable, Args: p.GetSource().MCP.Command.Args, WorkingDir: p.GetSource().MCP.Command.WorkingDir,
				Env: p.GetSource().MCP.Command.Env, Port: p.GetSource().MCP.Command.Port, Protocol: string(p.GetSource().MCP.Command.Protocol), UseShell: p.GetSource().MCP.Command.UseShell,
			}
		}
	}

	if rt != nil {
		detail.LastVerified = rt.LastObservedAt.Format(time.RFC3339)
		if rt.Endpoint.PublicAddress != "" {
			detail.Endpoints = append(detail.Endpoints, ipc.EndpointDTO{Address: rt.Endpoint.PublicAddress, Public: true})
		}
		if rt.Endpoint.PrivateAddress != "" {
			detail.Endpoints = append(detail.Endpoints, ipc.EndpointDTO{Address: rt.Endpoint.PrivateAddress, Public: false})
		}
		for _, r := range rt.Provider.Resources {
			detail.Resources = append(detail.Resources, ipc.ManagedResourceDTO{
				ID: string(r.ID), Type: string(r.Type), ExternalID: r.ExternalID, Ownership: string(r.Ownership), Metadata: r.Metadata,
			})
		}
		if rt.Connector.PID > 0 {
			detail.Processes = append(detail.Processes, ipc.ProcessDTO{PID: rt.Connector.PID, ConnectionID: string(cid), Status: string(rt.Connector.Status)})
		}
		for _, f := range rt.Diagnostics {
			detail.Findings = append(detail.Findings, ipc.DiagnosticDTO{
				ID: string(f.ID), Segment: string(f.Segment), Severity: string(f.Severity), Summary: f.Summary, Explanation: f.Explanation,
			})
		}
	}

	// The runtime projection is not an authoritative record of what Portico
	// created. Controller.RestoreResources drops restored resources when a
	// connection has no runtime row, so a connection can hold managed provider
	// resources in the database while the projection reports none.
	//
	// Reporting an empty resource list in that case would tell the operator
	// that nothing exists to clean up, which is exactly wrong: these are the
	// external IDs a manual provider-side cleanup has to act on. Fall back to
	// the durable record.
	if len(detail.Resources) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if durable, err := h.sup.store.ListResourcesByConnection(ctx, cid); err == nil {
			for _, r := range durable {
				detail.Resources = append(detail.Resources, ipc.ManagedResourceDTO{
					ID: string(r.ID), Type: string(r.Type), ExternalID: r.ExternalID,
					Ownership: string(r.Ownership), Metadata: r.Metadata,
				})
			}
		}
	}

	return detail, nil
}

func (h *supervisorHandler) HandleProviderRecommendation(req ipc.ProviderRecommendationRequest) (*ipc.ProviderRecommendationResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// The request's stated requirements drive the result. The previous
	// implementation ignored them entirely and returned whichever provider
	// happened to be authenticated first.
	input := controller.RecommendationInput{
		Kind:             core.ConnectionKind(req.ConnectionKind),
		SourceKind:       core.SourceKind(req.SourceKind),
		MCPTransport:     core.MCPTransport(req.MCPTransport),
		ExposureMode:     core.ExposureMode(req.ExposureMode),
		Protocol:         core.Protocol(req.Protocol),
		ProtectionKind:   core.ProtectionKind(req.ProtectionKind),
		RequestedAddress: req.RequestedAddress,
		PreferredAccount: core.ProviderAccountID(req.PreferredAccount),
	}

	rec, err := h.sup.controller.Recommend(ctx, input)
	if err != nil {
		return nil, err
	}

	toChoice := func(e controller.ProviderEvaluation) ipc.ProviderChoiceDTO {
		return ipc.ProviderChoiceDTO{
			ProviderID:   string(e.ProviderID),
			DisplayName:  e.DisplayName,
			AccountID:    string(e.AccountID),
			Reasons:      e.Strengths,
			Tradeoffs:    e.Tradeoffs,
			SetupActions: e.SetupActions,
			Score:        e.Score,
		}
	}

	resp := &ipc.ProviderRecommendationResponse{Summary: rec.Summary}
	if rec.Recommended != nil {
		choice := toChoice(*rec.Recommended)
		resp.Recommended = &choice
	}
	for _, alt := range rec.Alternatives {
		resp.Alternatives = append(resp.Alternatives, toChoice(alt))
	}
	for _, bad := range rec.Ineligible {
		resp.Filtered = append(resp.Filtered, ipc.FilteredChoiceDTO{
			ProviderID:  string(bad.ProviderID),
			DisplayName: bad.DisplayName,
			Reason:      strings.Join(bad.BlockingReasons, "; "),
			Reasons:     bad.BlockingReasons,
		})
	}
	return resp, nil
}

func (h *supervisorHandler) HandleOperationHistory() (*ipc.OperationHistoryDTO, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	summaries, err := h.sup.store.ListRecentOperations(ctx, 0)
	if err != nil {
		// Report the subsystem as unavailable rather than returning an empty
		// list. An empty list is an authoritative claim that no work has
		// happened, which is exactly what a caller must not conclude here.
		return &ipc.OperationHistoryDTO{
			Operations:  []ipc.OperationDTO{},
			Available:   false,
			Unavailable: err.Error(),
		}, nil
	}

	operations := make([]ipc.OperationDTO, 0, len(summaries))
	for _, s := range summaries {
		operations = append(operations, ipc.OperationDTO{
			ID:              string(s.ID),
			PlanID:          string(s.PlanID),
			ConnectionID:    string(s.ConnectionID),
			State:           s.State,
			StartedAt:       s.StartedAt,
			CompletedAt:     s.CompletedAt,
			Error:           s.Error,
			Intent:          s.Intent,
			ProviderID:      s.ProviderID,
			Fingerprint:     s.Fingerprint,
			ProfileRevision: s.ProfileRevision,
		})
	}
	return &ipc.OperationHistoryDTO{Operations: operations, Available: true}, nil
}

func (h *supervisorHandler) HandleGetConnection(id string) (*ipc.ConnectionDTO, error) {
	cid := core.ConnectionID(id)
	p, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, core.ErrProfileNotFound(cid)
	}
	rt, _ := h.sup.controller.GetRuntime(cid)

	dto := ipc.ConnectionDTO{
		ID:                string(p.ID),
		Name:              p.Name,
		DesiredState:      string(p.Desired),
		ProviderID:        string(p.GetProvider().ProviderID),
		ProviderAccountID: string(p.GetProvider().AccountID),
	}
	if rt != nil {
		dto.RuntimeState = string(rt.State)
		dto.UserState = rt.State.UserFacingState()
		dto.PublicAddress = rt.Endpoint.PublicAddress
		dto.PrivateAddress = rt.Endpoint.PrivateAddress
		dto.ConnectorPID = rt.Connector.PID
		dto.ConnectorState = string(rt.Connector.Status)
		if rt.Error != nil {
			dto.Error = rt.Error.Message
		}
	}
	return &dto, nil
}

func (h *supervisorHandler) HandleCreateConnection(req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error) {
	// Convert IPC DTO to core profile.
	profile := &core.ConnectionProfile{
		Name: req.Name,
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind: core.SourceKind(req.Source.Kind),
				},
				Exposure: core.ExposureSpec{
					Mode: core.ExposureMode(req.Exposure.Mode),
				},
				Protection: core.ProtectionSpec{
					Kind: core.ProtectionKind(req.Protection.Kind),
				},
			},
		},
		Driver: core.DriverSelection{
			ProviderID: core.ProviderID(req.Provider.ProviderID),
		},
		Desired: core.DesiredClosed,
	}

	// Map source fields.
	if req.Source.Existing != nil {
		profile.Spec.ServiceExposure.Source.Existing = &core.ExistingServiceSpec{
			Network:  req.Source.Existing.Network,
			Address:  req.Source.Existing.Address,
			Protocol: core.Protocol(req.Source.Existing.Protocol),
		}
		if profile.Spec.ServiceExposure.Source.Existing.Protocol == "" {
			profile.Spec.ServiceExposure.Source.Existing.Protocol = core.ProtocolHTTP
		}
	}
	if req.Source.Directory != nil {
		profile.Spec.ServiceExposure.Source.Directory = &core.DirectorySpec{
			Path:        req.Source.Directory.Path,
			Mode:        core.DirectoryMode(req.Source.Directory.Mode),
			SPAFallback: req.Source.Directory.SPAFallback,
			AllowUpload: req.Source.Directory.AllowUpload,
			AllowDelete: req.Source.Directory.AllowDelete,
		}
	}
	if req.Source.Command != nil {
		profile.Spec.ServiceExposure.Source.Command = &core.CommandSpec{
			Executable: req.Source.Command.Executable,
			Args:       req.Source.Command.Args,
			WorkingDir: req.Source.Command.WorkingDir,
			Env:        req.Source.Command.Env,
			Port:       req.Source.Command.Port,
			Protocol:   core.Protocol(req.Source.Command.Protocol),
			UseShell:   req.Source.Command.UseShell,
		}
		if profile.Spec.ServiceExposure.Source.Command.Protocol == "" {
			profile.Spec.ServiceExposure.Source.Command.Protocol = core.ProtocolHTTP
		}
	}
	if req.Source.MCP != nil {
		mc := &core.MCPServiceSpec{
			Transport: core.MCPTransport(req.Source.MCP.Transport),
			Endpoint:  req.Source.MCP.Endpoint,
		}
		if req.Source.MCP.Command != nil {
			mc.Command = &core.CommandSpec{
				Executable: req.Source.MCP.Command.Executable,
				Args:       req.Source.MCP.Command.Args,
				WorkingDir: req.Source.MCP.Command.WorkingDir,
				Env:        req.Source.MCP.Command.Env,
				Port:       req.Source.MCP.Command.Port,
				Protocol:   core.Protocol(req.Source.MCP.Command.Protocol),
				UseShell:   req.Source.MCP.Command.UseShell,
			}
		}
		profile.Spec.ServiceExposure.Source.MCP = mc
	}

	// Map exposure fields.
	if profile.Spec.ServiceExposure != nil {
		if req.Exposure.Protocol != "" {
			profile.Spec.ServiceExposure.Exposure.Protocol = core.Protocol(req.Exposure.Protocol)
		}
		profile.Spec.ServiceExposure.Exposure.RequestedAddress = req.Exposure.RequestedAddress
	}

	// Map protection fields.
	if profile.Spec.ServiceExposure != nil {
		profile.Spec.ServiceExposure.Protection.AllowedEmails = req.Protection.AllowedEmails
		profile.Spec.ServiceExposure.Protection.AllowedDomains = req.Protection.AllowedDomains
	}

	// Map provider fields.
	profile.Driver.AccountID = core.ProviderAccountID(req.Provider.AccountID)
	profile.Driver.Options = req.Provider.Options

	// Map lifecycle fields.
	profile.Lifecycle.AutoStart = req.Lifecycle.AutoStart
	if req.Lifecycle.OnDisconnect != "" {
		profile.Lifecycle.OnDisconnect = core.DisconnectPolicy(req.Lifecycle.OnDisconnect)
	}

	// Apply defaults for empty fields.
	if profile.Spec.ServiceExposure.Source.Kind == "" {
		profile.Spec.ServiceExposure.Source.Kind = core.SourceExisting
	}
	if profile.Spec.ServiceExposure.Source.Existing == nil && profile.Spec.ServiceExposure.Source.Kind == core.SourceExisting {
		profile.Spec.ServiceExposure.Source.Existing = &core.ExistingServiceSpec{Protocol: core.ProtocolHTTP}
	}
	if profile.GetExposure().Mode == "" {
		profile.Spec.ServiceExposure.Exposure.Mode = core.ExposureTemporary
	}
	if profile.Spec.ServiceExposure.Protection.Kind == "" {
		profile.Spec.ServiceExposure.Protection.Kind = core.ProtectionNone
	}
	if profile.Lifecycle.OnDisconnect == "" {
		profile.Lifecycle.OnDisconnect = core.DisconnectKeepAlive
	}

	canonicalProfile, _, err := h.sup.controller.CreateProfile(context.Background(), profile)
	if err != nil {
		return nil, err
	}

	// Persist the canonical values returned by the controller (revision=1, timestamps set).
	// Do NOT persist the original caller object which has revision 0. (audit #3)
	// CreateProfile persists atomically via the ConnectionStorer when configured;
	// the canonical values are returned for event publication and the Get response.

	// Connection creation committed its normalized event in the same database
	// transaction. IPC only broadcasts committed rows; reconnecting clients can
	// replay it if delivery fails here.
	if h.sup.ipcServer != nil {
		if err := h.sup.ipcServer.DispatchCommittedEvents(context.Background()); err != nil {
			slog.Warn("dispatch connection creation event", "connection", canonicalProfile.ID, "err", err)
		}
	}

	return h.HandleGetConnection(string(canonicalProfile.ID))
}

func (h *supervisorHandler) HandlePlanOpen(id string) (*ipc.PlanDTO, error) {
	plan, err := h.sup.controller.PlanOpen(context.Background(), core.ConnectionID(id))
	if err != nil {
		return nil, err
	}

	// Canonicalize through persistence first to get the immutable plan ID.
	canonicalPlan, err := h.sup.store.SaveOrGetPlan(context.Background(), plan)
	if err != nil {
		return nil, fmt.Errorf("save or get plan: %w", err)
	}

	// Install the canonical plan into controller memory.
	if err := h.sup.controller.SavePlan(canonicalPlan); err != nil {
		return nil, fmt.Errorf("save plan to controller: %w", err)
	}

	dto := &ipc.PlanDTO{
		ID:           string(canonicalPlan.ID),
		ConnectionID: string(canonicalPlan.ConnectionID),
		Intent:       string(canonicalPlan.Intent),
		Provider:     string(canonicalPlan.Provider),
		Fingerprint:  canonicalPlan.Fingerprint,
	}
	for _, step := range canonicalPlan.Steps {
		dto.Steps = append(dto.Steps, ipc.StepDTO{
			ID:          step.ID,
			Summary:     step.Summary,
			Destructive: step.Destructive,
		})
	}
	return dto, nil
}

func (h *supervisorHandler) HandlePlanClose(id string) (*ipc.PlanDTO, error) {
	plan, err := h.sup.controller.PlanClose(context.Background(), core.ConnectionID(id))
	if err != nil {
		return nil, err
	}

	// Canonicalize through persistence first to get the immutable plan ID.
	canonicalPlan, err := h.sup.store.SaveOrGetPlan(context.Background(), plan)
	if err != nil {
		return nil, fmt.Errorf("save or get plan: %w", err)
	}

	// Install the canonical plan into controller memory.
	if err := h.sup.controller.SavePlan(canonicalPlan); err != nil {
		return nil, fmt.Errorf("save plan to controller: %w", err)
	}

	dto := &ipc.PlanDTO{
		ID:           string(canonicalPlan.ID),
		ConnectionID: string(canonicalPlan.ConnectionID),
		Intent:       string(canonicalPlan.Intent),
		Provider:     string(canonicalPlan.Provider),
		Fingerprint:  canonicalPlan.Fingerprint,
	}
	for _, step := range canonicalPlan.Steps {
		dto.Steps = append(dto.Steps, ipc.StepDTO{
			ID:      step.ID,
			Summary: step.Summary,
		})
	}
	return dto, nil
}

func (h *supervisorHandler) HandleApplyPlan(planID string, idempotencyKey string) (*ipc.OperationDTO, error) {
	ctx := context.Background()

	// Idempotency: if a key was provided and we already recorded an operation
	// for it, return the cached result without re-executing.
	if idempotencyKey != "" {
		existingOpID, err := h.sup.store.LookupIdempotentKey(ctx, idempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("idempotency lookup: %w", err)
		}
		if existingOpID != "" {
			existingOp, err := h.sup.store.GetOperation(ctx, existingOpID)
			if err == nil && existingOp != nil {
				return &ipc.OperationDTO{
					ID:           string(existingOp.ID),
					PlanID:       planID,
					ConnectionID: string(existingOp.ConnectionID),
					State:        string(existingOp.State),
					StartedAt:    existingOp.StartedAt,
				}, nil
			}
		}
	}

	op, err := h.sup.controller.ApplyPlan(ctx, core.PlanID(planID))
	if err != nil {
		return nil, err
	}

	// Record the idempotency key to operation mapping for future replays.
	if idempotencyKey != "" {
		if err := h.sup.store.RecordIdempotentKey(ctx, idempotencyKey, op.ID); err != nil {
			slog.Warn("failed to record idempotency key", "key", idempotencyKey, "operation", op.ID, "err", err)
		}
	}

	if h.sup.ipcServer != nil {
		if err := h.sup.ipcServer.DispatchCommittedEvents(ctx); err != nil {
			slog.Warn("dispatch operation creation event", "operation", op.ID, "err", err)
		}
	}

	dto := &ipc.OperationDTO{
		ID:           string(op.ID),
		PlanID:       planID,
		ConnectionID: string(op.ConnectionID),
		State:        string(op.State),
		StartedAt:    op.StartedAt.Format(time.RFC3339),
	}
	return dto, nil
}

func (h *supervisorHandler) HandleListProviders() ([]ipc.ProviderDTO, error) {
	snap, err := h.HandleSnapshot()
	if err != nil {
		return nil, err
	}
	return snap.Providers, nil
}

// --------------- handler methods ---------------

func (h *supervisorHandler) HandleUpdateConnection(id string, req ipc.UpdateConnectionRequest) (*ipc.ConnectionDTO, error) {
	if !h.sup.mutating {
		return nil, fmt.Errorf("supervisor is shutting down and not accepting mutations")
	}
	cid := core.ConnectionID(id)
	ctx := context.Background()

	profile, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, core.ErrProfileNotFound(cid)
	}

	if req.Name != nil {
		profile.Name = *req.Name
	}

	// Capture current revision for validation
	expectedRevision := profile.Revision
	profile.UpdatedAt = time.Now().UTC()
	if err := h.sup.controller.UpdateProfile(ctx, profile, expectedRevision); err != nil {
		return nil, err
	}
	if h.sup.ipcServer != nil {
		if err := h.sup.ipcServer.DispatchCommittedEvents(context.Background()); err != nil {
			slog.Warn("dispatch connection update event", "connection", cid, "err", err)
		}
	}

	return h.HandleGetConnection(id)
}

func (h *supervisorHandler) HandlePlanRepair(id string) (*ipc.PlanDTO, error) {
	cid := core.ConnectionID(id)
	ctx := context.Background()

	profile, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, core.ErrProfileNotFound(cid)
	}
	// Prefer the authoritative desired-versus-observed delta planner. It can
	// produce narrowly scoped repairs (for example one DNS record) that the
	// controller's connector-focused fallback cannot infer from runtime alone.
	var repairPlan *core.OperationPlan
	if profile.Desired == core.DesiredOpen {
		resources, err := h.sup.store.ListResourcesByConnection(ctx, cid)
		if err != nil {
			return nil, fmt.Errorf("load repair resources: %w", err)
		}
		runtime, _ := h.sup.controller.GetRuntime(cid)
		observed, observeErr := h.sup.observeConnection(ctx, cid)
		if observeErr == nil {
			decision, decisionErr := h.sup.computeReconcileDecision(ctx, ReconcileInput{
				Profile: profile, Runtime: runtime, Resources: resources, Observed: observed,
			})
			if decisionErr != nil {
				return nil, decisionErr
			}
			if decision.Action == "repair" && decision.Plan != nil && decision.Plan.Intent == core.IntentRepair {
				repairPlan = decision.Plan
			} else if decision.Action == "none" {
				return &ipc.PlanDTO{ConnectionID: id, Intent: "repair", Steps: []ipc.StepDTO{}, Noop: true}, nil
			}
		}
	}
	if repairPlan == nil {
		var err error
		repairPlan, err = h.sup.controller.PlanRepair(ctx, cid)
		if err != nil {
			if errors.Is(err, controller.ErrNoRepairNeeded) {
				return &ipc.PlanDTO{ConnectionID: id, Intent: "repair", Steps: []ipc.StepDTO{}, Noop: true}, nil
			}
			return nil, err
		}
	}

	// Persist the plan through canonical path so controller and store agree.
	canonicalPlan, err := h.sup.persistCanonicalPlan(ctx, repairPlan)
	if err != nil {
		return nil, err
	}
	repairPlan = canonicalPlan

	steps := make([]ipc.StepDTO, len(repairPlan.Steps))
	for i, s := range repairPlan.Steps {
		steps[i] = ipc.StepDTO{ID: s.ID, Summary: s.Summary}
	}

	return &ipc.PlanDTO{
		ID:           string(repairPlan.ID),
		ConnectionID: id,
		Intent:       string(repairPlan.Intent),
		Provider:     string(repairPlan.Provider),
		Steps:        steps,
	}, nil
}

func (h *supervisorHandler) HandlePlanDelete(id string) (*ipc.PlanDTO, error) {
	cid := core.ConnectionID(id)
	ctx := context.Background()

	removePlan, err := h.sup.controller.PlanDelete(ctx, cid)
	if err != nil {
		return nil, err
	}

	// Persist the plan through canonical path so controller and store agree.
	canonicalPlan, err := h.sup.persistCanonicalPlan(ctx, removePlan)
	if err != nil {
		return nil, err
	}
	removePlan = canonicalPlan

	steps := make([]ipc.StepDTO, len(removePlan.Steps))
	for i, s := range removePlan.Steps {
		steps[i] = ipc.StepDTO{
			ID:           s.ID,
			Summary:      s.Summary,
			Destructive:  s.Destructive,
			Irreversible: s.Irreversible,
		}
	}
	if len(steps) == 0 {
		steps = append(steps, ipc.StepDTO{
			ID:           "delete-profile",
			Summary:      "Delete connection profile (local only)",
			Destructive:  true,
			Irreversible: true,
		})
	}

	return &ipc.PlanDTO{
		ID:           string(removePlan.ID),
		ConnectionID: id,
		Intent:       "delete",
		Provider:     string(removePlan.Provider),
		Steps:        steps,
	}, nil
}

func (h *supervisorHandler) HandleAuthenticateProvider(id string) error {
	prov := h.sup.registry.Get(core.ProviderID(id))
	if prov == nil {
		return core.ErrProviderNotFound(core.ProviderID(id))
	}
	return prov.Authenticate(context.Background(), core.AuthRequest{
		ProviderID: core.ProviderID(id),
	})
}

// HandleConfigureProviderAccount stores one Cloudflare account through the
// supervisor-owned secret store. Provider instances are built at supervisor
// startup, so the caller must restart after a successful write before this
// account becomes selectable for operations.
func (h *supervisorHandler) HandleConfigureProviderAccount(id string, req ipc.ConfigureProviderAccountRequest) (*ipc.ConfigureProviderAccountResponse, error) {
	if id != "cloudflare" {
		return nil, core.ErrProviderNotFound(core.ProviderID(id))
	}
	accountID := strings.TrimSpace(req.AccountID)
	zoneID := strings.TrimSpace(req.ZoneID)
	credential := strings.TrimSpace(req.Credential)
	if accountID == "" || zoneID == "" || credential == "" {
		return nil, core.ErrValidation("cloudflare account ID, zone ID, and credential are required")
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = accountID
	}
	credentialRef := fmt.Sprintf("cloudflare:%s:api-token", accountID)
	account := core.ProviderAccount{
		ID:            core.ProviderAccountID(accountID),
		Provider:      "cloudflare",
		Label:         label,
		CredentialRef: credentialRef,
		Metadata:      map[string]string{"zone_id": zoneID},
		Status:        core.AccountAuthenticated,
	}
	secret := []byte(credential)
	defer zeroBytes(secret)
	if err := h.sup.store.UpsertProviderAccountCredential(context.Background(), account, secret); err != nil {
		return nil, fmt.Errorf("save Cloudflare account: %w", err)
	}
	return &ipc.ConfigureProviderAccountResponse{RestartRequired: true}, nil
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

// observeConnectorStatus backs the diagnostics engine's ConnectorObserver.
// A connector tracked by the process manager is running; otherwise the
// controller runtime's last observed status is used.
func (s *Supervisor) observeConnectorStatus(ctx context.Context, connID core.ConnectionID) (core.ConnectorStatus, bool) {
	if managed, ok := s.procMgr.GetProcess(connID); ok {
		switch managed.Status {
		case process.ProcessStatusRunning:
			return core.ConnectorStatusRunning, true
		case process.ProcessStatusStarting:
			return core.ConnectorStatusStarting, true
		case process.ProcessStatusStopped:
			return core.ConnectorStatusStopped, true
		case process.ProcessStatusUnstable:
			return core.ConnectorStatusUnstable, true
		default:
			return core.ConnectorStatusCrashed, true
		}
	}
	if rt, ok := s.controller.GetRuntime(connID); ok {
		return rt.Connector.Status, true
	}
	return core.ConnectorStatusUnknown, false
}

// observeProviderState backs the diagnostics engine's ProviderObserver
// using the provider registry's observation.
func (s *Supervisor) observeProviderState(ctx context.Context, connID core.ConnectionID) (*core.ObservedConnection, error) {
	return s.observeConnection(ctx, connID)
}

// observeConnection is the sole supervisor observation path. Controller
// observation supplies persisted resources to ResourceAwareObserver providers
// and updates the runtime projection in the same place.
func (s *Supervisor) observeConnection(ctx context.Context, connID core.ConnectionID) (*core.ObservedConnection, error) {
	return s.controller.Observe(ctx, connID)
}

// persistCanonicalPlan canonicalizes a plan through persistence first,
// installs the canonical plan into the controller, and returns it.
// This is the single path for all plan persistence to ensure consistency.
func (s *Supervisor) persistCanonicalPlan(ctx context.Context, candidate *core.OperationPlan) (*core.OperationPlan, error) {
	canonicalPlan, err := s.store.SaveOrGetPlan(ctx, candidate)
	if err != nil {
		return nil, fmt.Errorf("save or get plan: %w", err)
	}
	if err := s.controller.SavePlan(canonicalPlan); err != nil {
		return nil, fmt.Errorf("save plan to controller: %w", err)
	}
	return canonicalPlan, nil
}

func (h *supervisorHandler) HandleGetOperation(id string) (*ipc.OperationDTO, error) {
	op, err := h.sup.store.GetOperation(context.Background(), core.OperationID(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrOperationNotFound(core.OperationID(id))
	}
	if err != nil {
		return nil, fmt.Errorf("load operation: %w", err)
	}
	events, err := h.sup.store.GetOperationEvents(context.Background(), op.ID)
	if err != nil {
		return nil, fmt.Errorf("load operation events: %w", err)
	}
	steps := operationSteps(events)
	dto := &ipc.OperationDTO{
		ID:           string(op.ID),
		PlanID:       string(op.PlanID),
		ConnectionID: string(op.ConnectionID),
		State:        string(op.State),
		Steps:        steps,
		StartedAt:    op.StartedAt,
	}
	if op.CompletedAt != "" {
		dto.CompletedAt = op.CompletedAt
	}
	if op.Error != "" {
		dto.Error = op.Error
	}
	return dto, nil
}

func (h *supervisorHandler) HandleGetOperationEvents(id string) ([]ipc.EventDTO, error) {
	op, err := h.sup.store.GetOperation(context.Background(), core.OperationID(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrOperationNotFound(core.OperationID(id))
	}
	if err != nil {
		return nil, fmt.Errorf("load operation: %w", err)
	}
	journal, err := h.sup.store.GetDurableEventsForOperation(context.Background(), op.ID)
	if err != nil {
		return nil, fmt.Errorf("load operation events: %w", err)
	}
	events := make([]ipc.EventDTO, len(journal))
	for i, event := range journal {
		events[i] = ipc.EventDTO{
			Sequence:     event.Event.Sequence,
			OperationID:  string(event.OperationID),
			ConnectionID: string(event.ConnectionID),
			Type:         string(event.Event.Type),
			Stage:        event.Stage,
			Timestamp:    event.Event.Timestamp.Format(time.RFC3339),
			Data:         event.Event.Data,
		}
	}
	return events, nil
}

func operationSteps(events []store.OperationJournalEvent) []ipc.StepDTO {
	byID := make(map[string]ipc.StepDTO)
	order := make([]string, 0, len(events))
	for _, event := range events {
		if event.StepID == "" {
			continue
		}
		if _, seen := byID[event.StepID]; !seen {
			order = append(order, event.StepID)
		}
		byID[event.StepID] = ipc.StepDTO{ID: event.StepID, Summary: event.Summary}
	}
	steps := make([]ipc.StepDTO, 0, len(order))
	for _, id := range order {
		steps = append(steps, byID[id])
	}
	return steps
}

func (h *supervisorHandler) HandleDiscovery() (*ipc.DiscoveryDTO, error) {
	return h.runDiscovery(false)
}

func (h *supervisorHandler) HandleRefreshDiscovery() (*ipc.DiscoveryDTO, error) {
	return h.runDiscovery(true)
}

// runDiscovery runs the discovery pipeline. When refresh is false the
// cached result is returned if still fresh; refresh forces a re-scan.
func (h *supervisorHandler) runDiscovery(refresh bool) (*ipc.DiscoveryDTO, error) {
	if h.sup.discoverer == nil {
		return &ipc.DiscoveryDTO{Services: []ipc.DiscoveredServiceDTO{}}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var result *discovery.Result
	var err error
	if refresh {
		result, err = h.sup.discoverer.Refresh(ctx)
	} else {
		result, err = h.sup.discoverer.Discover(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}

	dto := &ipc.DiscoveryDTO{Services: make([]ipc.DiscoveredServiceDTO, 0, len(result.Services))}
	for _, svc := range result.Services {
		dto.Services = append(dto.Services, ipc.DiscoveredServiceDTO{
			Address:    svc.Address,
			Port:       svc.Port,
			Protocol:   svc.Protocol,
			Framework:  svc.Framework,
			Confidence: string(svc.Confidence),
			PID:        svc.PID,
			Process:    svc.Process,
			Evidence:   strings.Join(svc.Evidence, "; "),
		})
	}
	return dto, nil
}

func (h *supervisorHandler) HandleDiagnostics(connID string) ([]ipc.DiagnosticDTO, error) {
	cid := core.ConnectionID(connID)

	var findings []core.DiagnosticFinding
	if h.sup.diagEngine != nil {
		profile, ok := h.sup.controller.GetProfile(cid)
		if !ok {
			return nil, core.ErrProfileNotFound(cid)
		}
		rt, _ := h.sup.controller.GetRuntime(cid)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		fs, err := h.sup.diagEngine.Diagnose(ctx, profile, rt)
		if err != nil {
			return nil, err
		}
		findings = fs
	} else {
		fs, err := h.sup.controller.Diagnose(context.Background(), cid)
		if err != nil {
			return nil, err
		}
		findings = fs
	}
	if err := h.sup.store.SyncFindings(context.Background(), cid, findings); err != nil {
		return nil, fmt.Errorf("persist diagnostics: %w", err)
	}
	h.sup.controller.ReplaceDiagnostics(cid, findings)
	if h.sup.ipcServer != nil {
		if err := h.sup.ipcServer.DispatchCommittedEvents(context.Background()); err != nil {
			return nil, fmt.Errorf("dispatch diagnostic events: %w", err)
		}
	}

	dtos := make([]ipc.DiagnosticDTO, len(findings))
	for i, f := range findings {
		dtos[i] = ipc.DiagnosticDTO{
			ID:          string(f.ID),
			Segment:     string(f.Segment),
			Severity:    string(f.Severity),
			Summary:     f.Summary,
			Explanation: f.Explanation,
		}
	}
	return dtos, nil
}

func (h *supervisorHandler) HandleSupervisorStop(ctx context.Context) error {
	// Signal stop in a goroutine so the HTTP handler can return its response
	// before the IPC server closes. shutdown is idempotent via sync.Once.
	go h.sup.shutdown(ctx)

	// Shutdown the IPC server so the handler stops accepting requests.
	// The goroutine above will call ipcServer.Stop() as part of shutdown,
	// but we trigger it immediately so this HTTP handler can return before
	// the server is fully shut down.
	return nil
}
