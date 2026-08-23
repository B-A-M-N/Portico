package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/config"
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

	// accountValidator checks a provider credential before the account is
	// recorded. It is a field so tests can substitute a stub.
	accountValidator AccountValidator

	// launch gates whether connections are armed at startup.
	launch string

	// activation turns durable account state into installed providers. It is
	// the only path that mutates the registry, so startup and a live account
	// change cannot produce different results from the same state.
	activation *activationCoordinator

	// Discovery, diagnostics, and origins
	discoverer discovery.Discoverer
	diagEngine *diagnostics.Engine
	origins    *origin.Manager

	// Gateway manager owns per-connection gateway instances.
	gatewayMgr *gatewayManager

	// Event-driven reconciliation: a buffered channel of connection IDs
	// that need re-evaluation. The reconcileLoop coalesces duplicates
	// via reconcilePending and reconciles only affected connections.
	reconcileCh      chan core.ConnectionID
	reconcilePending map[core.ConnectionID]struct{}
	reconcileMu      sync.Mutex
}

type cleanupRecorder struct{ store *store.Store }

func (r cleanupRecorder) RecordCleanupItem(ctx context.Context, operationID core.OperationID,
	connectionID core.ConnectionID, providerID core.ProviderID, accountID core.ProviderAccountID,
	resourceType core.ResourceType, externalID, state, lastError string) error {
	return r.store.RecordCleanupItem(ctx, store.CleanupItem{
		OperationID: operationID, ConnectionID: connectionID,
		ProviderID: providerID, AccountID: accountID,
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
		gatewayMgr:       newGatewayManager(),
		// The stored launch mode is loaded here rather than defaulted, so a
		// user's explicit choice is in force from the first startup after they
		// made it. It was previously runtime-only state that began every
		// process as auto whatever had been selected.
		launch: config.LoadOperationalSettings().LaunchMode,
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
		// Start gateway if the connection needs one.
		s.maybeStartGateway(event.ConnectionID, rt)
	case process.ProcessStatusStarting:
		rt.Connector.Status = core.ConnectorStatusStarting
	case process.ProcessStatusStopped:
		rt.Connector.Status = core.ConnectorStatusStopped
		// Stop gateway when connector stops.
		s.maybeStopGateway(event.ConnectionID)
	case process.ProcessStatusUnstable:
		rt.Connector.Status = core.ConnectorStatusUnstable
		rt.State = core.RuntimeDegraded
		s.maybeStopGateway(event.ConnectionID)
	default:
		rt.Connector.Status = core.ConnectorStatusCrashed
		if rt.State == core.RuntimeOpen {
			rt.State = core.RuntimeDegraded
		}
		s.maybeStopGateway(event.ConnectionID)
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

// maybeStartGateway starts the gateway for a connection if needed.
//
// NOTE: Currently disabled. The gateway traffic path is not yet designed.
// The OpenAI Secure MCP Tunnel provider points tunnel-client directly at the
// local MCP server, not at a gateway. Wiring the gateway into that path
// requires starting the gateway BEFORE the tunnel client and passing the
// gateway endpoint as --mcp.server-url. That topology is not yet implemented.
func (s *Supervisor) maybeStartGateway(connID core.ConnectionID, rt *core.ConnectionRuntime) {
	// Disabled until gateway traffic path is designed.
}

// maybeStopGateway stops the gateway for a connection if running.
func (s *Supervisor) maybeStopGateway(connID core.ConnectionID) {
	s.mu.RLock()
	rt, ok := s.controller.GetRuntime(connID)
	s.mu.RUnlock()
	if !ok || rt.Gateway == nil {
		return
	}
	if err := s.gatewayMgr.StopGateway(connID); err != nil {
		slog.Warn("failed to stop gateway", "connection", connID, "error", err)
	}
	rt.Gateway = nil
	s.controller.RestoreRuntime(rt)
}

// gatewayNeeded reports whether the connection needs a gateway.
func (s *Supervisor) gatewayNeeded(p *core.ConnectionProfile, rt *core.ConnectionRuntime) bool {
	// Only client tunnels need the gateway for now.
	if p.Kind != core.ConnectionClientTunnel {
		return false
	}
	if p.Spec.ClientTunnel == nil {
		return false
	}
	// Only OpenAI Secure MCP Tunnel uses the gateway.
	return p.Spec.ClientTunnel.Client == core.ClientOpenAISecureMCPTunnel
}

// runHealthChecks performs three-state health checks (PROCESS, TRANSPORT, SERVICE)
// for a connection and stores the result in the runtime.
func (s *Supervisor) runHealthChecks(ctx context.Context, connID core.ConnectionID) {
	rt, ok := s.controller.GetRuntime(connID)
	if !ok {
		return
	}

	p, ok := s.controller.GetProfile(connID)
	if !ok {
		return
	}

	report := &core.HealthReport{
		ConnectionID: connID,
	}

	desired := p.Desired

	// PROCESS check: Is the connector alive?
	if desired == core.DesiredClosed {
		report.Process = core.HealthCheck{
			State:       core.CheckNotApplicable,
			Detail:      "connection is closed",
			LastChecked: time.Now().UTC(),
		}
	} else if rt.Connector.Status == core.ConnectorStatusRunning && rt.Connector.PID > 0 {
		report.Process = core.HealthCheck{
			State:       core.CheckPass,
			Detail:      fmt.Sprintf("connector running (PID %d)", rt.Connector.PID),
			LastChecked: time.Now().UTC(),
		}
	} else {
		report.Process = core.HealthCheck{
			State:       core.CheckFail,
			Detail:      fmt.Sprintf("connector %s", rt.Connector.Status),
			LastChecked: time.Now().UTC(),
		}
	}

	// TRANSPORT check: Is the tunnel reachable?
	if desired == core.DesiredClosed {
		report.Transport = core.HealthCheck{
			State:       core.CheckNotApplicable,
			Detail:      "connection is closed",
			LastChecked: time.Now().UTC(),
		}
	} else if rt.Endpoint.PublicAddress != "" {
		report.Transport = s.checkTransport(ctx, rt.Endpoint.PublicAddress)
	} else if rt.Endpoint.PrivateAddress != "" {
		report.Transport = s.checkTransport(ctx, rt.Endpoint.PrivateAddress)
	} else {
		report.Transport = core.HealthCheck{
			State:       core.CheckUnknown,
			Detail:      "no endpoint address",
			LastChecked: time.Now().UTC(),
		}
	}

	// SERVICE check: Does the application work?
	if report.Process.State == core.CheckPass && report.Transport.State == core.CheckPass {
		report.Service = s.checkService(ctx, rt)
	} else if desired == core.DesiredClosed {
		report.Service = core.HealthCheck{
			State:       core.CheckNotApplicable,
			Detail:      "connection is closed",
			LastChecked: time.Now().UTC(),
		}
	} else {
		report.Service = core.HealthCheck{
			State:       core.CheckSkipped,
			Detail:      "skipped (process or transport down)",
			LastChecked: time.Now().UTC(),
		}
	}

	report.Refresh(desired)
	rt.Health = report
	s.controller.RestoreRuntime(rt)
}

// checkTransport verifies the endpoint is reachable.
func (s *Supervisor) checkTransport(ctx context.Context, endpoint string) core.HealthCheck {
	err := core.DefaultServiceCheck(ctx, endpoint)
	return core.HealthCheck{
		State:       errState(err),
		Detail:      errString(err),
		LastChecked: time.Now().UTC(),
	}
}

// checkService verifies the service works (for OpenAI: /v1/models).
func (s *Supervisor) checkService(ctx context.Context, rt *core.ConnectionRuntime) core.HealthCheck {
	endpoint := rt.Endpoint.PublicAddress
	if endpoint == "" {
		endpoint = rt.Endpoint.PrivateAddress
	}

	p, ok := s.controller.GetProfile(rt.ConnectionID)
	if !ok {
		return core.HealthCheck{State: core.CheckFail, Detail: "profile not found", LastChecked: time.Now().UTC()}
	}

	var err error
	switch p.Kind {
	case core.ConnectionClientTunnel:
		if p.Spec.ClientTunnel != nil && p.Spec.ClientTunnel.Client == core.ClientOpenAISecureMCPTunnel {
			// OpenAI Secure MCP Tunnel uses MCP, not OpenAI HTTP API.
			// MCP check is not implemented yet.
			return core.HealthCheck{State: core.CheckUnknown, Detail: "MCP health check not implemented", LastChecked: time.Now().UTC()}
		}
		err = core.DefaultServiceCheck(ctx, endpoint)
	default:
		err = core.DefaultServiceCheck(ctx, endpoint)
	}

	return core.HealthCheck{
		State:       errState(err),
		Detail:      errString(err),
		LastChecked: time.Now().UTC(),
	}
}

func errState(err error) core.CheckState {
	if err == nil {
		return core.CheckPass
	}
	return core.CheckFail
}

func errString(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
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

	// Run three-state health checks.
	s.runHealthChecks(ctx, connID)

	decision, err := s.computeReconcileDecision(ctx, input)
	if err != nil {
		slog.Warn("reconcile error", "connection", p.ID, "err", err)
		return
	}
	if decision.Blocked != nil {
		// Not "nothing to do": the desired state cannot be realised, and that
		// is recorded where the user will see it.
		s.markProfileInvalidForOpen(ctx, input.Profile, decision.Blocked)
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
		if decision.Blocked != nil {
			// One connection that cannot be opened must not stop the others
			// being reconciled.
			s.markProfileInvalidForOpen(ctx, input.Profile, decision.Blocked)
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
		connDTOs = append(connDTOs, connectionSummaryDTO(p, rt))
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
		// Map supported connection kinds.
		for _, kind := range caps.Kinds {
			dto.Capabilities.Kinds = append(dto.Capabilities.Kinds, string(kind))
		}
		sort.Strings(dto.Capabilities.Kinds)
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
		sort.Strings(dto.Capabilities.Protocols)
		sort.Strings(dto.Capabilities.ProtectionModes)
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
			// An account that exists but is not usable needs finishing, not
			// adding. Reporting "needs_config" would tell the user to start
			// over on setup they already did.
			if len(p.PendingAccounts) > 0 {
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
			} else if len(p.PendingAccounts) > 0 {
				dto.Availability, dto.Readiness = "unconfigured", "needs_auth"
			} else {
				dto.Availability, dto.Readiness = "unconfigured", "needs_config"
			}
		}

		// One decision, made here, about whether a connection can be planned
		// against this provider. Clients were each deriving it from the
		// availability string and reaching different answers.
		dto.Selectable = p.Availability.Selectable()
		dto.Stability = string(p.Stability)

		for _, account := range p.Accounts {
			dto.Accounts = append(dto.Accounts, ipc.ProviderAccountDTO{
				ID:             string(account.ID),
				Label:          account.Label,
				Status:         account.Status,
				UnusableReason: account.UnusableReason,
			})
		}
		// Kept separate from Accounts so nothing offers them for selection or
		// planning, while the provider screen can still show and repair them.
		for _, account := range p.PendingAccounts {
			dto.PendingAccounts = append(dto.PendingAccounts, ipc.ProviderAccountDTO{
				ID:             string(account.ID),
				Label:          account.Label,
				Status:         account.Status,
				UnusableReason: account.UnusableReason,
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
		Summary:     *summary,
		Revision:    p.Revision,
		DesiredSpec: describeSpec(p),
		Origin:      describeOriginOwnership(p, rt),
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

	// Describe the route as a chain of segments. A single open/failed verdict
	// cannot say which hop is broken, so each segment reports what the
	// available evidence supports and no more.
	var segmentResources []core.ProviderResource
	if rt != nil {
		segmentResources = rt.Provider.Resources
	}
	if len(segmentResources) == 0 {
		if durable, err := h.sup.store.ListResourcesByConnection(context.Background(), cid); err == nil {
			segmentResources = durable
		}
	}
	detail.Segments = computeRouteSegments(p, rt, segmentResources)

	// Health assessment.
	if rt != nil && rt.Health != nil {
		detail.Health = &ipc.HealthDTO{
			State: string(rt.Health.State),
			Process: ipc.HealthCheckDTO{
				State:       string(rt.Health.Process.State),
				Detail:      rt.Health.Process.Detail,
				LastChecked: rt.Health.Process.LastChecked.Format(time.RFC3339),
			},
			Transport: ipc.HealthCheckDTO{
				State:       string(rt.Health.Transport.State),
				Detail:      rt.Health.Transport.Detail,
				LastChecked: rt.Health.Transport.LastChecked.Format(time.RFC3339),
			},
			Service: ipc.HealthCheckDTO{
				State:       string(rt.Health.Service.State),
				Detail:      rt.Health.Service.Detail,
				LastChecked: rt.Health.Service.LastChecked.Format(time.RFC3339),
			},
			ComputedAt: rt.Health.ComputedAt.Format(time.RFC3339),
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
		Kind:              core.ConnectionKind(req.ConnectionKind),
		SourceKind:        core.SourceKind(req.SourceKind),
		MCPTransport:      core.MCPTransport(req.MCPTransport),
		ExposureMode:      core.ExposureMode(req.ExposureMode),
		Protocol:          core.Protocol(req.Protocol),
		ProtectionKind:    core.ProtectionKind(req.ProtectionKind),
		RequestedAddress:  req.RequestedAddress,
		PreferredAccount:  core.ProviderAccountID(req.PreferredAccount),
		PreferredProvider: core.ProviderID(req.PreferredProvider),
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
			// What to do about it is the half the user can act on.
			SetupActions: bad.SetupActions,
		})
	}
	return resp, nil
}

func (h *supervisorHandler) HandleOperationHistory() (*ipc.OperationHistoryDTO, error) {
	return h.HandleOperationHistoryLimit(0)
}

// defaultOperationHistoryPage is how many operations are returned when a caller
// states no preference.
const defaultOperationHistoryPage = 50

// HandleOperationHistoryLimit returns the most recent operations, reporting
// whether older ones were left out.
//
// The list was silently capped. A user looking for an operation from last week
// saw the cap and concluded it had not happened, or that the history had been
// pruned — with nothing on screen to distinguish "this is all of it" from
// "this is the first page".
func (h *supervisorHandler) HandleOperationHistoryLimit(limit int) (*ipc.OperationHistoryDTO, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// One extra row answers "is there more?" without a second query.
	effective := limit
	if effective <= 0 {
		effective = defaultOperationHistoryPage
	}
	summaries, err := h.sup.store.ListRecentOperations(ctx, effective+1)
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

	truncated := len(summaries) > effective
	if truncated {
		summaries = summaries[:effective]
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
	return &ipc.OperationHistoryDTO{
		Operations: operations, Available: true,
		Limit: effective, Truncated: truncated,
	}, nil
}

func (h *supervisorHandler) HandleGetConnection(id string) (*ipc.ConnectionDTO, error) {
	cid := core.ConnectionID(id)
	p, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, core.ErrProfileNotFound(cid)
	}
	rt, _ := h.sup.controller.GetRuntime(cid)

	dto := connectionSummaryDTO(p, rt)
	return &dto, nil
}

// validateCreateRequest enforces exactly-one-arm for the create request tagged
// union. Without this, a malformed request with multiple populated arms is
// silently coerced into whichever branch is checked first. The validation:
//
//  1. normalizes empty Kind to service_exposure (backward compatibility)
//  2. determines which logical spec arms are populated
//  3. requires exactly one arm to be populated
//  4. requires the populated arm to match the declared kind
//  5. rejects all foreign-arm data
func validateCreateRequest(req ipc.CreateConnectionRequest) error {
	kind := core.ConnectionKind(req.Kind)
	if kind == "" {
		kind = core.ConnectionServiceExposure
	}

	// Count populated arms.
	const (
		armServiceExposure = 1 << iota
		armPortForward
		armPrivateNetwork
		armClientTunnel
	)
	var populated int
	var which int

	// Service exposure: populated if any flat field is set. This preserves
	// backward compatibility where service_exposure requests carry flat
	// Source/Exposure/Protection rather than an explicit arm pointer.
	if req.Source.Kind != "" || req.Exposure.Mode != "" ||
		req.Protection.Kind != "" || req.Source.Existing != nil ||
		req.Source.Directory != nil || req.Source.Command != nil ||
		req.Source.MCP != nil {
		populated++
		which = armServiceExposure
	}
	if req.PortForward != nil {
		populated++
		which = armPortForward
	}
	if req.PrivateNetwork != nil {
		populated++
		which = armPrivateNetwork
	}
	if req.ClientTunnel != nil {
		populated++
		which = armClientTunnel
	}

	switch populated {
	case 0:
		return core.ErrValidation("the connection requires exactly one spec arm, but none were populated")
	case 1:
		// Exactly one arm populated. Verify it matches the declared kind.
	default:
		return core.ErrValidation("the connection requires exactly one spec arm, but multiple were populated")
	}

	switch kind {
	case core.ConnectionServiceExposure:
		if which != armServiceExposure {
			return core.ErrValidation("a service_exposure connection requires service_exposure fields, not another kind")
		}
		// Reject foreign arms (defense in depth: already caught by count).
		if req.PortForward != nil || req.PrivateNetwork != nil || req.ClientTunnel != nil {
			return core.ErrValidation("a service_exposure connection cannot carry port_forward, private_network, or client_tunnel arms")
		}
	case core.ConnectionPortForward:
		if which != armPortForward {
			return core.ErrValidation("a port forward connection requires a port_forward specification")
		}
	case core.ConnectionPrivateNetwork:
		if which != armPrivateNetwork {
			return core.ErrValidation("a private network connection requires a private_network specification")
		}
	case core.ConnectionClientTunnel:
		if which != armClientTunnel {
			return core.ErrValidation("a client tunnel connection requires a client_tunnel specification")
		}
	default:
		return core.ErrValidation(fmt.Sprintf("unknown connection kind %q", kind))
	}

	return nil
}

func (h *supervisorHandler) HandleCreateConnection(req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error) {
	// Validate the tagged union before dispatching. A malformed request must be
	// refused with a reason rather than silently coerced into a service_exposure
	// connection.
	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	// The kind is no longer hardcoded. Kinds that cannot be executed are
	// refused with the reason rather than accepted and left inert.
	switch core.ConnectionKind(req.Kind) {
	case "", core.ConnectionServiceExposure:
		// handled below
	case core.ConnectionPortForward:
		return h.createPortForward(req)
	case core.ConnectionPrivateNetwork:
		return nil, core.ErrValidation(
			"private network connections are not implemented: Portico ships no adapter that can join or expose " +
				"through a private network yet")
	case core.ConnectionClientTunnel:
		return nil, core.ErrValidation(
			"client tunnel connections are created through their provider's setup flow, not this endpoint")
	default:
		return nil, core.ErrValidation(fmt.Sprintf("unknown connection kind %q", req.Kind))
	}

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

	profile, _ := h.sup.controller.GetProfile(canonicalPlan.ConnectionID)
	return planToDTO(canonicalPlan, profile), nil
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

	profile, _ := h.sup.controller.GetProfile(canonicalPlan.ConnectionID)
	return planToDTO(canonicalPlan, profile), nil
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

	// The request accepts spec, driver and lifecycle changes, and this handler
	// applied none of them. A caller could change the origin, provider, account,
	// hostname or protection, receive a success response, and have nothing
	// happen. Silently discarding a requested change is worse than refusing it,
	// so unsupported edits are now rejected explicitly.
	//
	// These fields alter what the connection does at runtime and require a
	// change plan that observes current resources and previews the creations,
	// replacements and deletions involved. Until that exists, they are refused
	// with the reason rather than accepted and dropped.
	var unsupported []string
	if req.Spec != nil {
		unsupported = append(unsupported, "source, exposure or protection")
	}
	if req.Driver != nil {
		unsupported = append(unsupported, "provider or account")
	}
	if req.Lifecycle != nil {
		unsupported = append(unsupported, "lifecycle")
	}
	if len(unsupported) > 0 {
		// These changes are applied through the edit plan, which pauses the
		// connection, removes the provider resources the new profile no longer
		// describes, commits the profile and reopens. Applying them here would
		// change the profile while leaving those resources behind.
		return nil, core.ErrValidation(fmt.Sprintf(
			"changing %s must go through an edit plan so the affected provider resources are reconciled; "+
				"preview it with POST /v1/connections/%s/plan/edit and apply the returned plan",
			strings.Join(unsupported, " and "), id))
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, core.ErrValidation("connection name cannot be empty")
		}
		profile.Name = name
	}

	// Honour the caller's expected revision. Reading the current revision back
	// out of the profile made the check vacuous, so two concurrent edits both
	// succeeded and the later one silently overwrote the earlier.
	expectedRevision := profile.Revision
	if req.ExpectedRevision != 0 {
		expectedRevision = req.ExpectedRevision
	}
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

// createPortForward creates a local port forward connection.
func (h *supervisorHandler) createPortForward(req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error) {
	if req.PortForward == nil {
		return nil, core.ErrValidation("a port forward connection requires a port_forward specification")
	}
	pf := req.PortForward

	direction := core.PortForwardDirection(pf.Direction)
	if direction == "" {
		direction = core.PortForwardLocal
	}
	protocol := core.Protocol(pf.Protocol)
	if protocol == "" {
		protocol = core.ProtocolTCP
	}

	providerID := core.ProviderID(req.Provider.ProviderID)
	if providerID == "" {
		providerID = "portforward"
	}

	profile := &core.ConnectionProfile{
		Name: req.Name,
		Kind: core.ConnectionPortForward,
		Spec: core.ConnectionSpec{
			PortForward: &core.PortForwardSpec{
				LocalPort:  pf.LocalPort,
				RemoteHost: pf.RemoteHost,
				RemotePort: pf.RemotePort,
				Protocol:   protocol,
				Direction:  direction,
			},
		},
		Driver: core.DriverSelection{
			ProviderID: providerID,
			AccountID:  core.ProviderAccountID(req.Provider.AccountID),
		},
		Lifecycle: core.LifecycleSpec{
			AutoStart:    req.Lifecycle.AutoStart,
			OnDisconnect: core.DisconnectPolicy(req.Lifecycle.OnDisconnect),
		},
		Desired: core.DesiredClosed,
	}
	// The identity is assigned by CreateProfile, which validates the completed
	// profile; validating here would fail on the not-yet-assigned ID.
	created, _, err := h.sup.controller.CreateProfile(context.Background(), profile)
	if err != nil {
		return nil, err
	}
	if h.sup.ipcServer != nil {
		if dispatchErr := h.sup.ipcServer.DispatchCommittedEvents(context.Background()); dispatchErr != nil {
			slog.Warn("dispatch create event", "connection", created.ID, "err", dispatchErr)
		}
	}
	return h.HandleGetConnection(string(created.ID))
}

// HandleCloneConnection copies a connection's desired state into a new
// connection.
//
// Cloning is the safe alternative to editing a live connection: it never
// touches the source connection or its provider resources. Every identifier
// tying the profile to existing infrastructure is dropped, so the clone plans
// its own resources rather than adopting the original's. The clone is created
// closed regardless of the source's desired state, so copying an open
// connection never starts a second one by surprise.
func (h *supervisorHandler) HandleCloneConnection(id string, req ipc.CloneConnectionRequest) (*ipc.ConnectionDTO, error) {
	if !h.sup.mutating {
		return nil, fmt.Errorf("supervisor is shutting down and not accepting mutations")
	}
	source, ok := h.sup.controller.GetProfile(core.ConnectionID(id))
	if !ok {
		return nil, core.ErrProfileNotFound(core.ConnectionID(id))
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = source.Name + " (copy)"
	}

	clone := source.DeepCopy()
	clone.ID = core.NewConnectionID()
	clone.Name = name
	clone.Revision = 0
	clone.Desired = core.DesiredClosed
	clone.CreatedAt = time.Now().UTC()
	clone.UpdatedAt = clone.CreatedAt

	// A permanent hostname is unique to one connection. Carrying it over would
	// make the clone contend with the original for the same DNS record, so the
	// caller must supply a new one.
	if clone.Spec.ServiceExposure != nil && clone.Spec.ServiceExposure.Exposure.Mode == core.ExposurePermanent {
		hostname := strings.TrimSpace(req.RequestedAddress)
		if hostname == "" {
			return nil, core.ErrValidation(
				"this connection uses a permanent hostname; supply a different hostname for the clone")
		}
		if hostname == clone.Spec.ServiceExposure.Exposure.RequestedAddress {
			return nil, core.ErrValidation("the clone must use a different hostname from the original")
		}
		clone.Spec.ServiceExposure.Exposure.RequestedAddress = hostname
	}

	if err := clone.Validate(); err != nil {
		return nil, core.ErrValidation(fmt.Sprintf("cloned connection is not valid: %v", err))
	}

	ctx := context.Background()
	created, _, err := h.sup.controller.CreateProfile(ctx, clone)
	if err != nil {
		return nil, err
	}
	if h.sup.ipcServer != nil {
		if dispatchErr := h.sup.ipcServer.DispatchCommittedEvents(ctx); dispatchErr != nil {
			slog.Warn("dispatch clone event", "connection", created.ID, "err", dispatchErr)
		}
	}
	return h.HandleGetConnection(string(created.ID))
}

// HandlePlanEdit previews an edit as a change plan.
//
// The plan is a preview only: the proposed profile is not written until its
// apply-profile step runs, so a caller that never applies the plan leaves the
// connection exactly as it was.
func (h *supervisorHandler) HandlePlanEdit(id string, req ipc.UpdateConnectionRequest) (*ipc.PlanDTO, error) {
	if !h.sup.mutating {
		return nil, fmt.Errorf("supervisor is shutting down and not accepting mutations")
	}
	cid := core.ConnectionID(id)
	current, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, core.ErrProfileNotFound(cid)
	}

	// Honour the revision the caller built its edit from. Without this an edit
	// computed against a stale view is planned against the current profile:
	// fields the caller did not intend to change carry the values they held
	// when it loaded, and applying the plan silently reverts whatever someone
	// else changed in between. ApplyPlan's own revision check does not catch
	// it, because the plan was built against the current revision.
	if req.ExpectedRevision != 0 && req.ExpectedRevision != current.Revision {
		return nil, core.ErrValidation(fmt.Sprintf(
			"this connection was changed by someone else (it is now at revision %d, "+
				"and this edit was prepared from revision %d); reopen it and make the change again",
			current.Revision, req.ExpectedRevision))
	}

	proposed, err := applyEditRequest(current, req)
	if err != nil {
		return nil, err
	}

	// The account is checked whatever the connection's state. A closed
	// connection generates no reopen steps, so nothing else in the edit path
	// consults the provider — and an account that does not exist or cannot be
	// used was saved happily and failed the next time someone opened it.
	if err := h.validateEditAccount(proposed); err != nil {
		return nil, err
	}

	ctx := context.Background()
	plan, delta, err := h.sup.controller.PlanEdit(ctx, cid, proposed)
	if err != nil {
		return nil, err
	}

	if len(plan.Steps) == 0 {
		dto := planToDTO(plan, current)
		dto.Noop = true
		dto.Outcome = "Nothing would change."
		return dto, nil
	}

	canonicalPlan, err := h.sup.persistCanonicalPlan(ctx, plan)
	if err != nil {
		return nil, err
	}
	// PlanEdit recorded the proposed profile against the plan it built; the
	// canonical plan may carry a different ID, so re-associate it.
	h.sup.controller.RebindPendingEdit(plan.ID, canonicalPlan.ID)

	dto := planToDTO(canonicalPlan, proposed)
	dto.Outcome = describeEditOutcome(delta, current, proposed)
	return dto, nil
}

// applyEditRequest builds the proposed profile from the current one plus the
// requested changes. Only the fields the request carries are altered.
func applyEditRequest(current *core.ConnectionProfile, req ipc.UpdateConnectionRequest) (*core.ConnectionProfile, error) {
	proposed := current.DeepCopy()

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, core.ErrValidation("connection name cannot be empty")
		}
		proposed.Name = name
	}
	if req.Driver != nil {
		proposed.Driver.ProviderID = core.ProviderID(req.Driver.ProviderID)
		proposed.Driver.AccountID = core.ProviderAccountID(req.Driver.AccountID)
		if req.Driver.Options != nil {
			proposed.Driver.Options = req.Driver.Options
		}
	}
	if req.Lifecycle != nil {
		proposed.Lifecycle.AutoStart = req.Lifecycle.AutoStart
		proposed.Lifecycle.OnDisconnect = core.DisconnectPolicy(req.Lifecycle.OnDisconnect)
	}
	if req.Spec != nil {
		if proposed.Spec.ServiceExposure == nil {
			return nil, core.ErrValidation("only service exposure connections can have their spec edited")
		}
		spec := proposed.Spec.ServiceExposure
		if req.Spec.Exposure.Mode != "" {
			spec.Exposure.Mode = core.ExposureMode(req.Spec.Exposure.Mode)
		}
		if req.Spec.Exposure.Protocol != "" {
			spec.Exposure.Protocol = core.Protocol(req.Spec.Exposure.Protocol)
		}
		spec.Exposure.RequestedAddress = req.Spec.Exposure.RequestedAddress
		if req.Spec.Protection.Kind != "" {
			spec.Protection.Kind = core.ProtectionKind(req.Spec.Protection.Kind)
			spec.Protection.AllowedEmails = req.Spec.Protection.AllowedEmails
			spec.Protection.AllowedDomains = req.Spec.Protection.AllowedDomains
		}
		if req.Spec.Source.Existing != nil {
			spec.Source.Kind = core.SourceExisting
			spec.Source.Existing = &core.ExistingServiceSpec{
				Network:  req.Spec.Source.Existing.Network,
				Address:  req.Spec.Source.Existing.Address,
				Protocol: core.Protocol(req.Spec.Source.Existing.Protocol),
			}
		}
	}
	if req.PortForward != nil {
		// The specs are a tagged union, so a forward request against a published
		// service is a request to change the kind — which the delta classifier
		// refuses for good reason, and which is refused here with the reason
		// rather than by silently writing a spec the profile does not have.
		if proposed.Spec.PortForward == nil {
			return nil, core.ErrValidation(
				"this connection is not a port forward, so it has no forwarding to change")
		}
		spec := proposed.Spec.PortForward
		if req.PortForward.LocalPort != 0 {
			spec.LocalPort = req.PortForward.LocalPort
		}
		if req.PortForward.RemoteHost != "" {
			spec.RemoteHost = req.PortForward.RemoteHost
		}
		if req.PortForward.RemotePort != 0 {
			spec.RemotePort = req.PortForward.RemotePort
		}
		if req.PortForward.Protocol != "" {
			spec.Protocol = core.Protocol(req.PortForward.Protocol)
		}
		if req.PortForward.Direction != "" {
			spec.Direction = core.PortForwardDirection(req.PortForward.Direction)
		}
	}
	return proposed, nil
}

// describeEditOutcome states what the edit achieves in plain language.
func describeEditOutcome(delta controller.ProfileDelta, current, proposed *core.ConnectionProfile) string {
	if delta.Empty() {
		return "Nothing would change."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This changes %s.", joinWithAnd(delta.Changes))
	if len(delta.InvalidatedResources) > 0 {
		b.WriteString(" Provider resources that no longer match will be removed and recreated.")
	}
	if current.Desired == core.DesiredOpen {
		b.WriteString(" The connection is paused while this happens and reopened afterwards.")
	}
	return b.String()
}

func (h *supervisorHandler) HandlePlanRepair(id string) (*ipc.PlanDTO, error) {
	cid := core.ConnectionID(id)
	ctx := context.Background()

	profile, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, core.ErrProfileNotFound(cid)
	}
	// A closed connection has nothing to repair toward. Falling through to the
	// controller's connector-focused fallback would produce a step that starts
	// the connector of a connection the user deliberately closed — desired
	// state must dominate a manual repair exactly as it dominates
	// reconciliation.
	if profile.Desired != core.DesiredOpen {
		return nil, core.ErrValidation(
			"this connection is closed, so there is nothing to repair; open it instead")
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
		steps[i] = ipc.StepDTO{ID: s.ID, Kind: string(s.Kind), Summary: s.Summary}
	}

	repairProfile, _ := h.sup.controller.GetProfile(cid)
	dto := planToDTO(repairPlan, repairProfile)
	dto.ConnectionID = id
	dto.Steps = steps
	return dto, nil
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

	profile, _ := h.sup.controller.GetProfile(cid)
	dto := planToDTO(removePlan, profile)
	dto.ConnectionID = id
	dto.Intent = "delete"
	dto.Steps = steps
	return dto, nil
}

// HandleSetLaunchMode changes the startup gate and persists it.
//
// It reports the mode actually in effect rather than echoing the request. An
// environment override wins over the stored value, so a caller that assumed
// success would display a mode the supervisor is not using.
func (h *supervisorHandler) HandleSetLaunchMode(mode string) (*ipc.LaunchModeDTO, error) {
	if !ValidLaunchMode(mode) {
		return nil, core.ErrValidation(fmt.Sprintf(
			"launch mode must be %q or %q, not %q", LaunchManual, LaunchAuto, mode))
	}
	// A failure to persist is reported rather than swallowed: the mode is in
	// force for this process either way, and a user told their choice was saved
	// when it was not would find it reverted at the next restart.
	if err := h.sup.SetLaunchMode(mode); err != nil {
		return nil, fmt.Errorf("launch mode changed for this session but could not be saved: %w", err)
	}

	dto := &ipc.LaunchModeDTO{Mode: h.sup.launchMode()}
	if _, pinned := launchModeOverride(); pinned {
		dto.Pinned = true
		dto.PinnedBy = launchModeEnv
	}
	// The mode is written to the config file the supervisor owns, so it
	// survives a restart. A pinned mode is not stored, because it is the
	// environment's choice rather than the user's.
	dto.Persistent = LaunchModePersistent()
	return dto, nil
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
// HandleSettings reports the operational settings currently in effect.
//
// Launch mode is reported from the supervisor rather than straight from the
// config file, so an environment override is visible as the mode actually in
// force together with the fact that it is pinned.
func (h *supervisorHandler) HandleSettings() (*ipc.SettingsDTO, error) {
	stored := config.LoadOperationalSettings()
	dto := &ipc.SettingsDTO{
		LaunchMode:          h.sup.launchMode(),
		DefaultAutoStart:    stored.DefaultAutoStart,
		DefaultOnDisconnect: stored.DefaultOnDisconnect,
	}
	if _, pinned := launchModeOverride(); pinned {
		dto.LaunchModePinned = true
		dto.LaunchModePinnedBy = launchModeEnv
	}
	return dto, nil
}

// HandleUpdateSettings changes operational settings and reports what is in
// effect afterwards.
//
// Each field is validated before it is written, and an absent field is left
// alone. The reply is recomputed from the supervisor and the store rather than
// assembled from the request, so a value the environment overrides is reported
// as the override's, not the caller's.
func (h *supervisorHandler) HandleUpdateSettings(req ipc.SettingsRequest) (*ipc.SettingsDTO, error) {
	if req.LaunchMode != nil {
		if !ValidLaunchMode(*req.LaunchMode) {
			return nil, core.ErrValidation(fmt.Sprintf(
				"launch mode must be %q or %q, not %q", LaunchManual, LaunchAuto, *req.LaunchMode))
		}
		if err := h.sup.SetLaunchMode(*req.LaunchMode); err != nil {
			return nil, fmt.Errorf("could not save the launch mode: %w", err)
		}
	}
	if req.DefaultAutoStart != nil {
		if err := config.SaveDefaultAutoStart(*req.DefaultAutoStart); err != nil {
			return nil, fmt.Errorf("could not save the startup default: %w", err)
		}
	}
	if req.DefaultOnDisconnect != nil {
		policy := *req.DefaultOnDisconnect
		if policy != "keep_alive" && policy != "close" {
			return nil, core.ErrValidation(fmt.Sprintf(
				"disconnect behaviour must be \"keep_alive\" or \"close\", not %q", policy))
		}
		if err := config.SaveDefaultOnDisconnect(policy); err != nil {
			return nil, fmt.Errorf("could not save the disconnect default: %w", err)
		}
	}
	return h.HandleSettings()
}

// HandleProviderSetupFlow returns a provider's declarative setup requirements.
// A provider that declares none cannot be configured, which the caller must
// state rather than presenting an empty form.
func (h *supervisorHandler) HandleProviderSetupFlow(id string) (*ipc.SetupFlowDTO, error) {
	// Resolved from the definition, not from a live adapter. A provider's setup
	// requirements are static and known before anything is constructed, so
	// needing an adapter first meant a provider that was switched off or whose
	// client was missing could not tell you how to make it work — which is
	// exactly when you need it to.
	flow, err := h.sup.setupFlowFor(id)
	if err != nil {
		return nil, err
	}
	dto := &ipc.SetupFlowDTO{
		ProviderID:      id,
		Kind:            string(flow.Kind),
		Summary:         flow.Summary,
		CapabilityNotes: flow.CapabilityNotes,
	}
	if dto.Kind == "" {
		dto.Kind = string(core.SetupAccount)
	}
	if !flow.StoresAccount() {
		dto.GuidanceReason = flow.GuidanceReason
		if dto.GuidanceReason == "" {
			dto.GuidanceReason = "Portico cannot hold this provider's credential, " +
				"so setting it here would store a value nothing reads."
		}
	}
	for _, field := range flow.Fields {
		dto.Fields = append(dto.Fields, ipc.SetupFieldDTO{
			ID:          field.ID,
			Label:       field.Label,
			Description: field.Description,
			Secret:      field.Secret,
			Required:    field.Required,
			Placeholder: field.Placeholder,
			EnvVars:     field.EnvVars,
		})
	}
	return dto, nil
}

// catalogOnlySetupError explains a provider that is visible but has no adapter.
//
// It returns nil when the provider is not catalogued either, leaving the caller
// to report a genuine not-found.
func (s *Supervisor) catalogOnlySetupError(id string) error {
	for _, snap := range s.registry.Snapshot() {
		if string(snap.ID) != id {
			continue
		}
		// "Switched off" and "does not exist yet" are different answers, and
		// telling a user to enable something Portico has not written is worse
		// than saying nothing.
		var message string
		if snap.Availability == provider.AvailabilityNotImplemented {
			message = fmt.Sprintf("Portico has no %s adapter yet, so there is nothing to configure",
				snap.DisplayName)
		} else {
			message = fmt.Sprintf("%s is not switched on in this supervisor, so it cannot be configured yet",
				snap.DisplayName)
		}
		if snap.Reason != "" {
			message += ": " + snap.Reason
		}
		// The catalog already records what to do about it. Repeating it here
		// keeps the answer with the refusal rather than on another screen.
		if len(snap.SetupActions) > 0 {
			message += "\n\nTo enable it:\n  • " + strings.Join(snap.SetupActions, "\n  • ")
		}
		return core.ErrValidation(message)
	}
	return nil
}

// HandleRemoveProviderAccount removes an account after reporting what depends
// on it. An account still selected by a connection is never removed silently,
// because doing so strands that connection with an unexplained
// "provider account unavailable" error.
// HandleAccountRemovalPreview reports what removing an account would do.
//
// The supervisor composes the consequences, so every client says the same true
// thing about the same account. The confirmation screen used to write its own,
// and promised to forget a credential Portico might not hold.
func (h *supervisorHandler) HandleAccountRemovalPreview(providerID, accountID string) (
	*ipc.AccountRemovalPreviewDTO, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	preview, err := h.sup.store.PreviewProviderAccountRemoval(ctx,
		core.ProviderID(providerID), core.ProviderAccountID(accountID))
	if err != nil {
		return nil, err
	}
	return accountRemovalPreviewDTO(preview, h.providerAccountCount(providerID)), nil
}

// providerAccountCount reports how many usable accounts a provider has, so the
// preview can say whether removing this one leaves the provider unusable.
func (h *supervisorHandler) providerAccountCount(providerID string) int {
	// A missing registry is not a reason to crash while explaining a removal.
	// The conservative answer is zero, which makes the preview say this is the
	// only usable account — the more cautious of the two sentences.
	if h.sup == nil || h.sup.registry == nil {
		return 0
	}
	for _, snapshot := range h.sup.registry.Snapshot() {
		if string(snapshot.ID) == providerID {
			return len(snapshot.Accounts)
		}
	}
	return 0
}

// accountRemovalPreviewDTO projects a preview and composes what it means.
func accountRemovalPreviewDTO(preview *store.AccountRemovalPreview, accountsForProvider int) *ipc.AccountRemovalPreviewDTO {
	dto := &ipc.AccountRemovalPreviewDTO{
		ProviderID:       string(preview.ProviderID),
		AccountID:        string(preview.AccountID),
		Label:            preview.Label,
		Removable:        preview.Removable,
		CredentialStored: preview.CredentialStored,
		Fingerprint:      preview.Fingerprint,
	}
	for _, dep := range preview.Dependencies {
		dto.Dependencies = append(dto.Dependencies, ipc.AccountDependencyDTO{
			Kind: dep.Kind, ID: dep.ID, Name: dep.Name, Explanation: dep.Explanation,
		})
	}
	dto.Consequences = describeAccountRemoval(preview, accountsForProvider)
	return dto
}

// describeAccountRemoval says what removing this account does, from what is
// actually true of it.
func describeAccountRemoval(preview *store.AccountRemovalPreview, accountsForProvider int) []string {
	var lines []string
	if preview.CredentialStored {
		lines = append(lines, "Portico will forget the credential it stored for this account.")
	} else {
		lines = append(lines,
			"Portico holds no stored credential for this account; removing it only forgets the account itself.")
	}
	lines = append(lines, "Nothing is deleted at the provider, and you can add it again.")

	if accountsForProvider <= 1 {
		lines = append(lines,
			"This is the only usable account for this provider, so afterwards it can only do "+
				"whatever it supports without an account.")
	} else {
		lines = append(lines, "Other accounts for this provider are unaffected.")
	}
	return lines
}

func (h *supervisorHandler) HandleRemoveProviderAccount(providerID, accountID string, req ipc.RemoveProviderAccountRequest) (*ipc.RemoveProviderAccountResponse, error) {
	if !h.sup.mutating {
		return nil, fmt.Errorf("supervisor is shutting down and not accepting mutations")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Deciding and deleting happen in one transaction. As two operations, a
	// connection created or edited in between could bind an account that was
	// about to be removed — and because the account reference lives inside the
	// profile's driver JSON rather than behind a foreign key, nothing at the
	// database level would refuse it.
	deps, err := h.sup.store.DeleteProviderAccountIfUnused(ctx,
		core.ProviderID(providerID), core.ProviderAccountID(accountID), req.Fingerprint)
	if err != nil {
		// A preview that no longer describes this account is refused, and the
		// caller is handed the one that does rather than a bare rejection.
		var stale *store.StalePreviewError
		if errors.As(err, &stale) {
			resp := &ipc.RemoveProviderAccountResponse{Removed: false}
			if stale.Current != nil {
				resp.Preview = accountRemovalPreviewDTO(stale.Current, h.providerAccountCount(providerID))
			}
			return resp, core.ErrValidation(stale.Error())
		}
		return nil, err
	}
	if len(deps) > 0 {
		resp := &ipc.RemoveProviderAccountResponse{Removed: false}
		for _, dep := range deps {
			resp.Dependencies = append(resp.Dependencies, ipc.AccountDependencyDTO{
				Kind: dep.Kind, ID: dep.ID, Name: dep.Name, Explanation: dep.Explanation,
			})
			if dep.Kind == "connection" {
				resp.DependentConnections = append(resp.DependentConnections, dep.ID)
			}
		}
		return resp, core.ErrValidation(describeAccountDependencies(deps))
	}

	// Every provider reactivates through the same path, so removing an account
	// takes effect immediately whichever provider it belonged to.
	restartRequired := false
	if activateErr := h.sup.ActivateProvider(ctx, core.ProviderID(providerID)); activateErr != nil {
		slog.Warn("could not reactivate the provider in place", "provider", providerID, "err", activateErr)
		restartRequired = true
	}
	return &ipc.RemoveProviderAccountResponse{Removed: true, RestartRequired: restartRequired}, nil
}

// describeAccountDependencies says what is in the way, in terms of what the
// user would have to do about it.
func describeAccountDependencies(deps store.AccountDependencies) string {
	var connections, cleanups int
	for _, dep := range deps {
		switch dep.Kind {
		case "connection":
			connections++
		case "cleanup_item":
			cleanups++
		}
	}
	switch {
	case connections > 0 && cleanups > 0:
		return fmt.Sprintf(
			"%d connection(s) still use this account, and %d provider resource(s) still need removing "+
				"with its credential", connections, cleanups)
	case cleanups > 0:
		return fmt.Sprintf(
			"%d provider resource(s) created with this account still need removing; "+
				"its credential is what can remove them", cleanups)
	default:
		return fmt.Sprintf(
			"%d connection(s) still use this account; reassign or delete them before removing it",
			connections)
	}
}

// HandleConfigureProviderAccount configures any provider that declares a setup
// flow.
//
// Previously this refused everything but Cloudflare, so the declarative flow
// existed and no provider could use it. What replaces the provider-ID check is
// not "trust every provider" but a rule about what may be claimed: an account
// whose credential could not be checked is stored as pending and reported as
// unverified, never as authenticated.
func (h *supervisorHandler) HandleConfigureProviderAccount(id string, req ipc.ConfigureProviderAccountRequest) (*ipc.ConfigureProviderAccountResponse, error) {
	// The definition, not a live adapter: a provider must be configurable
	// before it has been constructed, which is the whole point of configuring
	// it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setup, err := h.sup.setupDefinitionFor(id)
	if err != nil {
		return nil, err
	}
	flow := setup.SetupFlow()

	// A guidance flow describes what to do elsewhere. Storing its values would
	// write something nothing reads and report success for a setup that had no
	// effect.
	if !flow.StoresAccount() {
		return nil, core.ErrValidation(fmt.Sprintf(
			"provider %q is configured outside Portico; its client reads the credential from the "+
				"supervisor's environment, so there is nothing here to save", id))
	}

	values := setupValues(flow, req)
	for _, field := range flow.Fields {
		if field.Required && strings.TrimSpace(values[field.ID]) == "" {
			return nil, core.ErrValidation(fmt.Sprintf("%s is required", field.Label))
		}
	}

	// The provider builds its own account. Only it knows which field is the
	// identity, what the label defaults to, which values are metadata and how
	// its credential reference is derived — four things the supervisor used to
	// guess by treating every unrecognised field as metadata.
	prepared, err := setup.PrepareAccount(values)
	if err != nil {
		return nil, core.ErrValidation(err.Error())
	}
	defer zeroBytes(prepared.Secret)

	// Cloudflare keeps its own verification path: the zone-membership check and
	// capability reporting are real provider semantics, and moving them is a
	// separate piece of work from making setup generic.
	if id == "cloudflare" {
		return h.configureCloudflareAccount(values)
	}
	return h.configureDeclaredAccount(ctx, id, setup, prepared)
}

// setupValues merges a request's generic field map with the named fields that
// predate it, so both the new setup screen and existing callers work.
func setupValues(flow core.SetupFlow, req ipc.ConfigureProviderAccountRequest) map[string]string {
	values := map[string]string{}
	for k, v := range req.Fields {
		values[k] = strings.TrimSpace(v)
	}
	// The named fields are the reserved IDs by another name. They fill in only
	// where the generic map said nothing, so a caller using both is not
	// silently overridden.
	for id, value := range map[string]string{
		"account_id": req.AccountID,
		"label":      req.Label,
		"zone_id":    req.ZoneID,
		"credential": req.Credential,
	} {
		if values[id] == "" && strings.TrimSpace(value) != "" {
			values[id] = strings.TrimSpace(value)
		}
	}
	if flow.IdentityField != "" && values[flow.IdentityField] == "" && values["account_id"] != "" {
		values[flow.IdentityField] = values["account_id"]
	}
	return values
}

// configureDeclaredAccount stores an account the provider itself constructed.
//
// The account status is the whole point of this function. A provider that can
// check its own credential gets an authenticated account; one that cannot gets
// a pending account and a response that says why. Recording the second case as
// authenticated is the defect this audit already corrected once.
func (h *supervisorHandler) configureDeclaredAccount(
	ctx context.Context, id string, setup provider.SetupDefinition, prepared provider.PreparedAccount,
) (*ipc.ConfigureProviderAccountResponse, error) {
	account := prepared.Account
	if account.Provider == "" {
		account.Provider = core.ProviderID(id)
	}
	if account.ID == "" {
		// A provider with no notion of account identity has a single implicit
		// account, named for the provider itself.
		account.ID = core.ProviderAccountID(id)
	}
	if account.Label == "" {
		account.Label = string(account.ID)
	}
	if account.CredentialRef == "" {
		account.CredentialRef = providerCredentialRef(id, string(account.ID))
	}

	resp := &ipc.ConfigureProviderAccountResponse{}
	account.Status = core.AccountPending

	if verifier, ok := setup.(provider.SetupVerifier); ok {
		validation, err := verifier.VerifyAccount(ctx, prepared)
		if err != nil {
			resp.MissingPermissions = validation.MissingPermissions
			return resp, core.ErrValidation(err.Error())
		}
		account.Status = core.AccountAuthenticated
		resp.Validated = true
		resp.MissingPermissions = validation.MissingPermissions
	} else {
		// Saying it will be tested when a connection opens would be false: an
		// unverified account is not usable, so the controller refuses it before
		// the provider is ever reached.
		resp.VerificationUnavailable = fmt.Sprintf(
			"Portico cannot check a %s credential, so this account is saved but cannot be "+
				"selected until it is verified.", id)
	}

	if err := h.sup.store.UpsertProviderAccountCredential(ctx, account, prepared.Secret); err != nil {
		return nil, fmt.Errorf("save %s account: %w", id, err)
	}

	// Any provider reactivates in place, so a newly stored account is usable in
	// this process. This used to be an unconditional "restart required", into a
	// restart that changed nothing because no code built an adapter for a
	// provider the supervisor did not know by name.
	if activateErr := h.sup.ActivateProvider(ctx, core.ProviderID(id)); activateErr != nil {
		slog.Warn("could not activate the provider in place", "provider", id, "err", activateErr)
		resp.RestartRequired = true
	}
	resp.Status = string(account.Status)
	return resp, nil
}

func (h *supervisorHandler) configureCloudflareAccount(values map[string]string) (*ipc.ConfigureProviderAccountResponse, error) {
	accountID := strings.TrimSpace(values["account_id"])
	zoneID := strings.TrimSpace(values["zone_id"])
	credential := strings.TrimSpace(values["credential"])

	// A zone is only required for DNS and custom-hostname work. Quick Tunnels
	// need no account at all, and managed tunnels need only an account and a
	// credential. Requiring a zone here contradicted the setup UI, which
	// correctly offers to skip it.
	if accountID == "" {
		return nil, core.ErrValidation("a Cloudflare account ID is required")
	}
	if credential == "" {
		return nil, core.ErrValidation("a Cloudflare API token is required")
	}

	secret := []byte(credential)
	defer zeroBytes(secret)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Validate before persisting. Recording an account as authenticated on the
	// strength of a non-empty string is how Portico came to advertise providers
	// that could not perform a single operation.
	validator := h.sup.accountValidator
	if validator == nil {
		validator = cloudflareAccountValidator{}
	}
	validation, err := validator.Validate(ctx, "cloudflare", accountID, credential)
	if err != nil {
		resp := &ipc.ConfigureProviderAccountResponse{}
		if validation != nil {
			resp.MissingPermissions = validation.MissingPermissions
		}
		return resp, core.ErrValidation(err.Error())
	}
	if validation == nil || !validation.AccountAccessible {
		return nil, core.ErrValidation("the token could not be confirmed against that Cloudflare account")
	}

	// A supplied zone must actually belong to the account, otherwise DNS work
	// would fail later with an error far from its cause.
	if zoneID != "" && len(validation.Zones) > 0 {
		known := false
		for _, z := range validation.Zones {
			if z.ID == zoneID {
				known = true
				break
			}
		}
		if !known {
			return nil, core.ErrValidation(fmt.Sprintf(
				"zone %s is not visible to this token; leave the zone blank to configure tunnels without DNS", zoneID))
		}
	}

	label := strings.TrimSpace(values["label"])
	if label == "" {
		label = accountID
	}
	metadata := map[string]string{}
	if zoneID != "" {
		metadata["zone_id"] = zoneID
	}
	credentialRef := providerCredentialRef("cloudflare", accountID)
	account := core.ProviderAccount{
		ID:            core.ProviderAccountID(accountID),
		Provider:      "cloudflare",
		Label:         label,
		CredentialRef: credentialRef,
		Metadata:      metadata,
		Status:        core.AccountAuthenticated,
	}
	if err := h.sup.store.UpsertProviderAccountCredential(ctx, account, secret); err != nil {
		return nil, fmt.Errorf("save Cloudflare account: %w", err)
	}

	// Reactivate so the account is usable immediately. A restart is reported
	// only when activation could not be attempted at all.
	restartRequired := false
	if activateErr := h.sup.ActivateProvider(ctx, "cloudflare"); activateErr != nil {
		slog.Warn("could not reactivate Cloudflare in place", "err", activateErr)
		restartRequired = true
	}

	resp := &ipc.ConfigureProviderAccountResponse{
		RestartRequired:    restartRequired,
		Validated:          true,
		MissingPermissions: validation.MissingPermissions,
	}
	for _, z := range validation.Zones {
		resp.Zones = append(resp.Zones, ipc.ZoneDTO{ID: z.ID, Name: z.Name})
	}
	if zoneID == "" {
		resp.CapabilityLevel = "tunnels_without_dns"
	} else {
		resp.CapabilityLevel = "tunnels_with_dns"
	}
	return resp, nil
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

// operationSteps rebuilds each step's outcome from the durable journal.
//
// It is a fold over every event for a step, not a last-event-wins overwrite.
// Overwriting kept only the final event's fields, so a step's state, error and
// timings were dropped and every reconstructed step rendered as still pending —
// however it had actually ended. After a restart that is the only account of
// what happened.
func operationSteps(events []store.OperationJournalEvent) []ipc.StepDTO {
	byID := make(map[string]*ipc.StepDTO)
	order := make([]string, 0, len(events))

	for _, event := range events {
		if event.StepID == "" {
			continue
		}
		step, seen := byID[event.StepID]
		if !seen {
			step = &ipc.StepDTO{ID: event.StepID, State: ipc.StepPending}
			byID[event.StepID] = step
			order = append(order, event.StepID)
		}

		// A later event with no summary must not erase the one that had it:
		// a failure event often carries only the error.
		if event.Summary != "" {
			step.Summary = event.Summary
		}
		if event.Error != "" {
			step.Error = event.Error
		}

		switch event.EventType {
		case string(core.EventOperationStepStarted):
			step.State = ipc.StepRunning
			if step.StartedAt == "" {
				step.StartedAt = event.Timestamp
			}
		case string(core.EventOperationStepSucceeded):
			// Compensation reports success through the same event type, and
			// the two mean different things: one is the step having worked,
			// the other is it having been undone.
			if event.Stage == string(core.StageCompensated) {
				step.State = ipc.StepCompensated
			} else {
				step.State = ipc.StepSucceeded
			}
			step.CompletedAt = event.Timestamp
		case string(core.EventOperationStepFailed):
			if event.Stage == string(core.StageCompensated) {
				step.State = ipc.StepCompensationFailed
			} else {
				step.State = ipc.StepFailed
			}
			step.CompletedAt = event.Timestamp
		}
	}

	steps := make([]ipc.StepDTO, 0, len(order))
	for _, id := range order {
		steps = append(steps, *byID[id])
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

// validateEditAccount refuses an edit that would point a connection at an
// account it cannot use.
//
// It is deliberately checked at plan time and again at apply time: an account
// can be removed between previewing a change and approving it, and the whole
// point of the preview is that what it describes is what happens.
func (h *supervisorHandler) validateEditAccount(profile *core.ConnectionProfile) error {
	selection := profile.GetProvider()
	if selection.ProviderID == "" || selection.AccountID == "" {
		return nil
	}

	for _, snapshot := range h.sup.registry.Snapshot() {
		if snapshot.ID != selection.ProviderID {
			continue
		}
		for _, account := range snapshot.Accounts {
			if account.ID == selection.AccountID {
				return nil
			}
		}
		// Named separately, because "not usable yet" is a different problem
		// from "does not exist" and has a different remedy.
		for _, account := range snapshot.PendingAccounts {
			if account.ID == selection.AccountID {
				return core.ErrValidation(fmt.Sprintf(
					"account %q cannot be used yet: its credential has not been confirmed, "+
						"so this connection could not open", selection.AccountID))
			}
		}
		return core.ErrValidation(fmt.Sprintf(
			"%s has no account %q; choose one that exists", selection.ProviderID, selection.AccountID))
	}
	return core.ErrValidation(fmt.Sprintf("unknown provider %q", selection.ProviderID))
}
