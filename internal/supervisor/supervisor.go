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
	stopCh       chan struct{}
	serveWG      sync.WaitGroup // tracks the IPC serve goroutine
	shutdownOnce sync.Once      // guards idempotent shutdown

	// Discovery, diagnostics, and origins
	discoverer discovery.Discoverer
	diagEngine *diagnostics.Engine
	origins    *origin.Manager
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
		store:      st,
		controller: ctrl,
		procMgr:    procMgr,
		registry:   registry,
		paths:      paths,
		lock:       lock,
		origins:    origins,
		stopCh:     make(chan struct{}),
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

		// Load runtime if exists.
		rt, err := s.store.LoadRuntime(ctx, p.ID)
		if err != nil {
			// No runtime persisted yet — that's fine for v0.1
			slog.Debug("no runtime for connection", "id", p.ID)
			continue
		}
		if rt != nil {
			s.controller.RestoreRuntime(rt)
			slog.Info("loaded runtime for connection", "id", p.ID, "state", rt.State)
		}

		// Load provider resources
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

// reconcileLoop periodically reconciles desired vs observed state.
func (s *Supervisor) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.reconcileAll(ctx)
		}
	}
}

func (s *Supervisor) reconcileAll(ctx context.Context) {
	profiles := s.controller.ListProfiles()
	for _, p := range profiles {
		if s.registry.Get(p.Provider.ProviderID) == nil {
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

	// Persist runtime after reconcile
	for _, rt := range s.controller.ListRuntimes() {
		if err := s.store.SaveRuntime(ctx, rt); err != nil {
			slog.Warn("failed to persist runtime", "connection", rt.ConnectionID, "err", err)
		}
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
			ID:           string(p.ID),
			Name:         p.Name,
			DesiredState: string(p.Desired),
			ProviderID:   string(p.Provider.ProviderID),
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
		provDTOs = append(provDTOs, ipc.ProviderDTO{
			ID:            string(p.ID),
			Name:          p.Name,
			DisplayName:   p.DisplayName,
			Authenticated: p.Authenticated,
		})
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

func (h *supervisorHandler) HandleGetConnection(id string) (*ipc.ConnectionDTO, error) {
	cid := core.ConnectionID(id)
	p, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, core.ErrProfileNotFound(cid)
	}
	rt, _ := h.sup.controller.GetRuntime(cid)

	dto := ipc.ConnectionDTO{
		ID:           string(p.ID),
		Name:         p.Name,
		DesiredState: string(p.Desired),
		ProviderID:   string(p.Provider.ProviderID),
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
		Source: core.SourceSpec{
			Kind: core.SourceKind(req.Source.Kind),
		},
		Exposure: core.ExposureSpec{
			Mode: core.ExposureMode(req.Exposure.Mode),
		},
		Protection: core.ProtectionSpec{
			Kind: core.ProtectionKind(req.Protection.Kind),
		},
		Provider: core.ProviderSelection{
			ProviderID: core.ProviderID(req.Provider.ProviderID),
		},
		Desired: core.DesiredClosed,
	}

	// Map source fields.
	if req.Source.Existing != nil {
		profile.Source.Existing = &core.ExistingServiceSpec{
			Network:  req.Source.Existing.Network,
			Address:  req.Source.Existing.Address,
			Protocol: core.Protocol(req.Source.Existing.Protocol),
		}
		if profile.Source.Existing.Protocol == "" {
			profile.Source.Existing.Protocol = core.ProtocolHTTP
		}
	}
	if req.Source.Directory != nil {
		profile.Source.Directory = &core.DirectorySpec{
			Path:        req.Source.Directory.Path,
			Mode:        core.DirectoryMode(req.Source.Directory.Mode),
			SPAFallback: req.Source.Directory.SPAFallback,
			AllowUpload: req.Source.Directory.AllowUpload,
			AllowDelete: req.Source.Directory.AllowDelete,
		}
	}
	if req.Source.Command != nil {
		profile.Source.Command = &core.CommandSpec{
			Executable: req.Source.Command.Executable,
			Args:       req.Source.Command.Args,
			WorkingDir: req.Source.Command.WorkingDir,
			Env:        req.Source.Command.Env,
			Port:       req.Source.Command.Port,
			Protocol:   core.Protocol(req.Source.Command.Protocol),
			UseShell:   req.Source.Command.UseShell,
		}
		if profile.Source.Command.Protocol == "" {
			profile.Source.Command.Protocol = core.ProtocolHTTP
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
		profile.Source.MCP = mc
	}

	// Map exposure fields.
	if req.Exposure.Protocol != "" {
		profile.Exposure.Protocol = core.Protocol(req.Exposure.Protocol)
	}
	profile.Exposure.RequestedAddress = req.Exposure.RequestedAddress

	// Map protection fields.
	profile.Protection.AllowedEmails = req.Protection.AllowedEmails
	profile.Protection.AllowedDomains = req.Protection.AllowedDomains

	// Map provider fields.
	profile.Provider.AccountID = core.ProviderAccountID(req.Provider.AccountID)
	profile.Provider.Options = req.Provider.Options

	// Map lifecycle fields.
	profile.Lifecycle.AutoStart = req.Lifecycle.AutoStart
	if req.Lifecycle.OnDisconnect != "" {
		profile.Lifecycle.OnDisconnect = core.DisconnectPolicy(req.Lifecycle.OnDisconnect)
	}

	// Apply defaults for empty fields.
	if profile.Source.Kind == "" {
		profile.Source.Kind = core.SourceExisting
	}
	if profile.Source.Existing == nil && profile.Source.Kind == core.SourceExisting {
		profile.Source.Existing = &core.ExistingServiceSpec{Protocol: core.ProtocolHTTP}
	}
	if profile.Exposure.Mode == "" {
		profile.Exposure.Mode = core.ExposureTemporary
	}
	if profile.Protection.Kind == "" {
		profile.Protection.Kind = core.ProtectionNone
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

func (h *supervisorHandler) HandleApplyPlan(planID string) (*ipc.OperationDTO, error) {
	op, err := h.sup.controller.ApplyPlan(context.Background(), core.PlanID(planID))
	if err != nil {
		return nil, err
	}

	if h.sup.ipcServer != nil {
		if err := h.sup.ipcServer.DispatchCommittedEvents(context.Background()); err != nil {
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
	cid := core.ConnectionID(id)
	ctx := context.Background()

	profile, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, core.ErrProfileNotFound(cid)
	}

	if req.Name != nil {
		profile.Name = *req.Name
	}
	if req.DesiredState != nil {
		profile.Desired = core.DesiredConnectionState(*req.DesiredState)
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
