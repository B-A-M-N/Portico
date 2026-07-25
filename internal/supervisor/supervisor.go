package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/paoloanzn/portico/internal/app"
	"github.com/paoloanzn/portico/internal/controller"
	"github.com/paoloanzn/portico/internal/core"
	"github.com/paoloanzn/portico/internal/diagnostics"
	"github.com/paoloanzn/portico/internal/discovery"
	"github.com/paoloanzn/portico/internal/ipc"
	"github.com/paoloanzn/portico/internal/process"
	"github.com/paoloanzn/portico/internal/provider"
	"github.com/paoloanzn/portico/internal/store"
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
	ctrl.SetResourceSaver(st)
	ctrl.SetCredentialStorer(st)
	ctrl.SetStepCommitter(st)
	ctrl.SetProfileUpdater(st)

	return &Supervisor{
		store:      st,
		controller: ctrl,
		procMgr:    procMgr,
		registry:   registry,
		paths:      paths,
		lock:       lock,
		stopCh:     make(chan struct{}),
	}, nil
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
		input := ReconcileInput{Profile: p}
		if rt, ok := s.controller.GetRuntime(p.ID); ok {
			input.Runtime = rt
			input.Resources = rt.Provider.Resources
		}
		// Best-effort provider observation for authoritative decisions.
		if prov := s.registry.Get(p.Provider.ProviderID); prov != nil {
			if obs, err := prov.Observe(ctx, p.ID); err == nil {
				input.Observed = obs
			}
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
	profiles := h.sup.controller.ListProfiles()
	runtimes := h.sup.controller.ListRuntimes()

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
		return nil, fmt.Errorf("connection not found: %s", id)
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

	// Publish event.
	h.sup.ipcServer.PublishEvent(ipc.EventDTO{
		Type:      "connection.created",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      map[string]string{"connection_id": string(canonicalProfile.ID)},
	})

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

	// Publish event.
	h.sup.ipcServer.PublishEvent(ipc.EventDTO{
		Type:      "operation.created",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data: map[string]string{
			"operation_id":  string(op.ID),
			"connection_id": string(op.ConnectionID),
		},
	})

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

func (h *supervisorHandler) HandleDeleteConnection(id string) error {
	cid := core.ConnectionID(id)
	ctx := context.Background()

	// Generate and apply a delete plan through the standard plan lifecycle,
	// so that operation events and transactional finalization both fire.
	plan, err := h.sup.controller.PlanDelete(ctx, cid)
	if err != nil {
		return fmt.Errorf("plan delete: %w", err)
	}

	if err := h.sup.controller.SavePlan(plan); err != nil {
		return fmt.Errorf("save delete plan: %w", err)
	}
	if err := h.sup.store.SavePlan(ctx, plan); err != nil {
		return fmt.Errorf("persist delete plan: %w", err)
	}

	op, err := h.sup.controller.ApplyPlan(ctx, plan.ID)
	if err != nil {
		return fmt.Errorf("apply delete plan: %w", err)
	}

	h.sup.ipcServer.PublishEvent(ipc.EventDTO{
		Type:      "operation.created",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data: map[string]string{
			"operation_id":  string(op.ID),
			"connection_id": string(op.ConnectionID),
		},
	})

	// Wait for the operation to complete so the EventOperationCompleted
	// handler can call FinalizeDeletion transactionally.
	// ApplyPlan starts the provider goroutine and returns immediately;
	// the operation completes asynchronously via the event channel.
	deadline := time.Now().Add(10 * time.Second)
	deleted := false
	failed := false
	for time.Now().Before(deadline) {
		snap, ok := h.sup.controller.GetOperation(op.ID)
		if !ok {
			break
		}
		if snap.State == controller.OperationStateCompleted {
			deleted = true
			break
		}
		if snap.State == controller.OperationStateFailed {
			failed = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if deleted {
		h.sup.ipcServer.PublishEvent(ipc.EventDTO{
			Type:      "connection.deleted",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Data:      map[string]string{"connection_id": id},
		})
		return nil
	}

	if failed {
		return fmt.Errorf("delete operation failed for connection %s (operation %s)", id, op.ID)
	}

	// Timeout or operation still running — return the operation ID for polling.
	slog.Warn("delete operation timed out or still running",
		"connection", id, "operation", op.ID)
	return fmt.Errorf("delete operation timed out for connection %s: operation %s still in progress", id, op.ID)
}

// --------------- new handler methods ---------------

func (h *supervisorHandler) HandleUpdateConnection(id string, req ipc.UpdateConnectionRequest) (*ipc.ConnectionDTO, error) {
	cid := core.ConnectionID(id)
	ctx := context.Background()

	profile, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, fmt.Errorf("connection not found: %s", id)
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

	return h.HandleGetConnection(id)
}

func (h *supervisorHandler) HandlePlanRepair(id string) (*ipc.PlanDTO, error) {
	cid := core.ConnectionID(id)
	ctx := context.Background()

	repairPlan, err := h.sup.controller.PlanRepair(ctx, cid)
	if errors.Is(err, controller.ErrNoRepairNeeded) {
		// Connection is healthy - return an explicit no-op response.
		return &ipc.PlanDTO{
			ID:           "",
			ConnectionID: id,
			Intent:       "repair",
			Steps:        []ipc.StepDTO{},
		}, nil
	}
	if err != nil {
		return nil, err
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
		Intent:       "repair",
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
		return fmt.Errorf("provider not found: %s", id)
	}
	return prov.Authenticate(context.Background(), core.AuthRequest{
		ProviderID: core.ProviderID(id),
	})
}

// observeConnectorStatus backs the diagnostics engine's ConnectorObserver.
// A connector tracked by the process manager is running; otherwise the
// controller runtime's last observed status is used.
func (s *Supervisor) observeConnectorStatus(ctx context.Context, connID core.ConnectionID) (core.ConnectorStatus, bool) {
	if _, ok := s.procMgr.Observe(connID); ok {
		return core.ConnectorStatusRunning, true
	}
	if rt, ok := s.controller.GetRuntime(connID); ok {
		return rt.Connector.Status, true
	}
	return core.ConnectorStatusUnknown, false
}

// observeProviderState backs the diagnostics engine's ProviderObserver
// using the provider registry's observation.
func (s *Supervisor) observeProviderState(ctx context.Context, connID core.ConnectionID) (*core.ObservedConnection, error) {
	p, ok := s.controller.GetProfile(connID)
	if !ok {
		return nil, fmt.Errorf("connection not found: %s", connID)
	}
	prov := s.registry.Get(p.Provider.ProviderID)
	if prov == nil {
		return nil, fmt.Errorf("provider not available: %s", p.Provider.ProviderID)
	}
	return prov.Observe(ctx, connID)
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
	op, ok := h.sup.controller.GetOperation(core.OperationID(id))
	if !ok {
		return nil, fmt.Errorf("operation not found: %s", id)
	}

	steps := make([]ipc.StepDTO, len(op.StepEvents))
	for i, se := range op.StepEvents {
		steps[i] = ipc.StepDTO{
			ID:      se.StepID,
			Summary: se.Summary,
		}
	}

	dto := &ipc.OperationDTO{
		ID:           string(op.ID),
		PlanID:       string(op.PlanID),
		ConnectionID: string(op.ConnectionID),
		State:        string(op.State),
		Steps:        steps,
		StartedAt:    op.StartedAt.Format(time.RFC3339),
	}
	if !op.CompletedAt.IsZero() {
		dto.CompletedAt = op.CompletedAt.Format(time.RFC3339)
	}
	if op.Error != nil {
		dto.Error = op.Error.Error()
	}
	return dto, nil
}

func (h *supervisorHandler) HandleGetOperationEvents(id string) ([]ipc.EventDTO, error) {
	op, ok := h.sup.controller.GetOperation(core.OperationID(id))
	if !ok {
		return nil, fmt.Errorf("operation not found: %s", id)
	}

	events := make([]ipc.EventDTO, len(op.StepEvents))
	for i, se := range op.StepEvents {
		events[i] = ipc.EventDTO{
			Sequence:  int64(i + 1),
			Type:      "operation.step_" + string(se.Stage),
			Timestamp: se.Timestamp.Format(time.RFC3339),
			Data: map[string]string{
				"step_id": se.StepID,
				"summary": se.Summary,
			},
		}
	}
	return events, nil
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
			return nil, fmt.Errorf("connection not found: %s", connID)
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
