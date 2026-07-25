package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/paoloanzn/portico/internal/core"
	"github.com/paoloanzn/portico/internal/provider"
)

// Controller manages connections using providers.
//
// The controller validates desired state, selects a provider, requests
// provider plans, applies plans through a serialized operation runner,
// observes current state, computes drift, and produces repair plans.
//
// The controller does NOT render UI, read keyboard input, store credentials,
// parse provider log lines in generic code, or know Cloudflare API types.
type Controller struct {
	mu               sync.RWMutex
	profiles         map[core.ConnectionID]*core.ConnectionProfile
	runtimes         map[core.ConnectionID]*core.ConnectionRuntime
	operations       map[core.OperationID]*operationRecord
	plans            map[core.PlanID]*core.OperationPlan
	registry         provider.Registry
	accounts         []core.ProviderAccountID
	journal          Journal
	deleteFinalizer  DeleteConnectionFinalizer
	connectionStorer ConnectionStorer
	profileUpdater   ProfileUpdater
	resourceSaver    ResourceSaver
	resourceRemover  ResourceRemover
	credentialStorer CredentialStorer
	stepCommitter    StepResultCommitter
	runtimeCommitter RuntimeCommitter
}

// Journal defines the operation persistence interface used by the controller
// to durably record operation lifecycle and step events.
type Journal interface {
	SaveOperation(ctx context.Context, opID core.OperationID, planID core.PlanID, connID core.ConnectionID, state string, startedAt string) error
	CompleteOperation(ctx context.Context, opID core.OperationID, state string) error
	AppendOperationEvent(ctx context.Context, event core.Event, opID core.OperationID) error
	AppendFinding(ctx context.Context, finding core.DiagnosticFinding) error
}

// operationRecord wraps an Operation with synchronization for thread-safe access.
type operationRecord struct {
	mu   sync.RWMutex
	oper Operation
}

// Operation tracks an in-progress operation.
type Operation struct {
	ID           core.OperationID
	PlanID       core.PlanID
	ConnectionID core.ConnectionID
	State        OperationState
	StartedAt    time.Time
	CompletedAt  time.Time
	Error        error
	StepEvents   []StepEvent
}

// OperationState describes the state of an operation.
type OperationState string

const (
	OperationStatePending   OperationState = "pending"
	OperationStateRunning   OperationState = "running"
	OperationStateCompleted OperationState = "completed"
	OperationStateFailed    OperationState = "failed"
)

// StepEvent records one step's outcome.
type StepEvent struct {
	StepID    string
	Kind      core.StepKind
	Stage     core.EventStage
	Summary   string
	Error     string
	Timestamp time.Time
}

// New creates a new controller.
func New(registry provider.Registry, journal Journal) *Controller {
	return &Controller{
		profiles:   make(map[core.ConnectionID]*core.ConnectionProfile),
		runtimes:   make(map[core.ConnectionID]*core.ConnectionRuntime),
		operations: make(map[core.OperationID]*operationRecord),
		plans:      make(map[core.PlanID]*core.OperationPlan),
		registry:   registry,
		accounts:   nil,
		journal:    journal,
	}
}

// SetDeleteFinalizer sets the callback invoked when a delete operation
// completes successfully. The supervisor provides this to wire transactional
// deletion through the store.
func (c *Controller) SetDeleteFinalizer(f DeleteConnectionFinalizer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteFinalizer = f
}

// Snapshot returns an immutable copy of the operation.
func (r *operationRecord) Snapshot() Operation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Deep copy StepEvents
	oper := r.oper
	oper.StepEvents = make([]StepEvent, len(r.oper.StepEvents))
	copy(oper.StepEvents, r.oper.StepEvents)
	return oper
}

func (r *operationRecord) Transition(state OperationState, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.oper.State = state
	if err != nil {
		r.oper.Error = err
	}
	if state == OperationStateCompleted || state == OperationStateFailed {
		r.oper.CompletedAt = time.Now().UTC()
	}
}

// AddStepEvent adds a step event to the operation.
func (r *operationRecord) AddStepEvent(event StepEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.oper.StepEvents = append(r.oper.StepEvents, event)
}

// GetOperationID returns the operation ID.
func (r *operationRecord) GetOperationID() core.OperationID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.oper.ID
}

// SetAccounts sets the list of authenticated provider account IDs.
func (c *Controller) SetAccounts(accounts []core.ProviderAccountID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accounts = accounts
}

// AuthenticatedAccounts returns the list of authenticated account IDs.
func (c *Controller) AuthenticatedAccounts() []core.ProviderAccountID {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]core.ProviderAccountID, len(c.accounts))
	copy(result, c.accounts)
	return result
}

// --------------- profile operations ---------------

// CreateProfile creates a new connection profile and initializes its runtime.
// It validates the profile, creates a deep copy, and initializes the runtime
// in a closed state. Returns the canonical profile (revision=1, timestamps set)
// and the initial runtime so the caller can persist them durably.
// The input profile's ID is set to the assigned connection ID on success.
//
// When a ConnectionStorer is configured, both profile and runtime are persisted
// atomically in a single SQLite transaction before being added to controller memory.
// If the storer is not configured, only in-memory state is updated (legacy path).
func (c *Controller) CreateProfile(
	ctx context.Context,
	profile *core.ConnectionProfile,
) (*core.ConnectionProfile, *core.ConnectionRuntime, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Assign ID before validation
	if profile.ID == "" {
		profile.ID = core.NewConnectionID()
	}

	// Validate before storing
	if err := profile.Validate(); err != nil {
		return nil, nil, fmt.Errorf("profile validation failed: %w", err)
	}

	// Validate provider exists
	prov := c.registry.Get(profile.Provider.ProviderID)
	if prov == nil {
		return nil, nil, fmt.Errorf("provider not found: %s", profile.Provider.ProviderID)
	}

	// Deep copy before storing
	storedProfile := profile.DeepCopy()
	now := time.Now().UTC()
	storedProfile.CreatedAt = now
	storedProfile.UpdatedAt = now
	storedProfile.Revision = 1

	// Reject duplicate IDs
	if _, exists := c.profiles[storedProfile.ID]; exists {
		return nil, nil, fmt.Errorf("profile already exists: %s", storedProfile.ID)
	}

	// Initialize runtime in closed state (explicit initial state contract)
	rt := &core.ConnectionRuntime{
		ConnectionID:   storedProfile.ID,
		State:          core.RuntimeClosed,
		LastTransition: now,
		LastObservedAt: now,
	}

	// Persist atomically if a storer is configured.
	// This must happen before we add to in-memory maps so that
	// a persistence failure does not leave partial controller state.
	if c.connectionStorer != nil {
		if err := c.connectionStorer.CreateConnection(ctx, storedProfile, rt); err != nil {
			return nil, nil, fmt.Errorf("create connection: %w", err)
		}
	}

	c.profiles[storedProfile.ID] = storedProfile
	c.runtimes[storedProfile.ID] = rt

	return storedProfile.DeepCopy(), rt.DeepCopy(), nil
}

// GetProfile returns a defensive copy of a profile by ID.
func (c *Controller) GetProfile(id core.ConnectionID) (*core.ConnectionProfile, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.profiles[id]
	if !ok {
		return nil, false
	}
	return p.DeepCopy(), true
}

// GetRuntime returns a defensive copy of a runtime by ID.
func (c *Controller) GetRuntime(id core.ConnectionID) (*core.ConnectionRuntime, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.runtimes[id]
	if !ok {
		return nil, false
	}
	return r.DeepCopy(), true
}

// ListProfiles returns defensive copies of all profiles.
func (c *Controller) ListProfiles() []*core.ConnectionProfile {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]*core.ConnectionProfile, 0, len(c.profiles))
	for _, p := range c.profiles {
		result = append(result, p.DeepCopy())
	}
	return result
}

// ListRuntimes returns defensive copies of all runtimes.
func (c *Controller) ListRuntimes() []*core.ConnectionRuntime {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]*core.ConnectionRuntime, 0, len(c.runtimes))
	for _, r := range c.runtimes {
		result = append(result, r.DeepCopy())
	}
	return result
}

// UpdateProfile updates a profile and bumps the revision.
// It validates the profile and rejects stale updates using optimistic concurrency.
func (c *Controller) UpdateProfile(ctx context.Context, profile *core.ConnectionProfile, expectedRevision uint64) error {
	// Validate the updated profile before storing
	if err := profile.Validate(); err != nil {
		return fmt.Errorf("profile validation failed: %w", err)
	}

	// Validate provider exists
	prov := c.registry.Get(profile.Provider.ProviderID)
	if prov == nil {
		return fmt.Errorf("provider not found: %s", profile.Provider.ProviderID)
	}

	// Load existing profile to preserve CreatedAt
	c.mu.RLock()
	existing, ok := c.profiles[profile.ID]
	c.mu.RUnlock()

	if !ok {
		return fmt.Errorf("profile not found: %s", profile.ID)
	}

	// Preserve CreatedAt
	profile.CreatedAt = existing.CreatedAt

	// Use profileUpdater's optimistic update - this persists first
	var committed *core.ConnectionProfile
	if c.profileUpdater != nil {
		var err error
		committed, err = c.profileUpdater.UpdateProfile(ctx, profile, expectedRevision)
		if err != nil {
			return fmt.Errorf("store update failed: %w", err)
		}
	} else {
		// Fallback: update in-memory only (not recommended for production)
		c.mu.Lock()
		defer c.mu.Unlock()
		existing, ok := c.profiles[profile.ID]
		if !ok {
			return fmt.Errorf("profile not found: %s", profile.ID)
		}
		if existing.Revision != expectedRevision {
			return fmt.Errorf("revision mismatch: expected %d, got %d", expectedRevision, existing.Revision)
		}
		profile.Revision = existing.Revision + 1
		profile.UpdatedAt = time.Now().UTC()
		c.profiles[profile.ID] = profile.DeepCopy()
		committed = profile
		return nil
	}

	// Store succeeded - install the committed profile into memory
	c.mu.Lock()
	c.profiles[committed.ID] = committed
	c.mu.Unlock()

	return nil
}

// deleteProfileLocked removes a profile and its runtime from in-memory
// state. The caller MUST hold c.mu. This is intended for use during
// delete finalization after CommitConnectionDeletion has already
// committed the deletion transaction to persistent storage.
// It does NOT call any store methods or perform database work. (SPEC P0 #1)
func (c *Controller) deleteProfileLocked(id core.ConnectionID) {
	delete(c.profiles, id)
	delete(c.runtimes, id)
}

// DeleteConnectionFinalizer is the interface the controller uses to
// transactionally commit a connection deletion to persistent storage.
// The supervisor provides the store implementation.
type DeleteConnectionFinalizer interface {
	CommitConnectionDeletion(ctx context.Context, connID core.ConnectionID, operationID core.OperationID) error
}

// ProfileUpdater is the interface the controller uses to update
// a profile with optimistic concurrency control. The store returns
// the committed profile so the controller can install it atomically.
type ProfileUpdater interface {
	UpdateProfile(ctx context.Context, profile *core.ConnectionProfile, expectedRevision uint64) (*core.ConnectionProfile, error)
}

// ResourceRemover is the interface the controller uses to mark
// provider resources as removed after successful provider deletion steps.
// The supervisor provides the store implementation.
type ResourceRemover interface {
	MarkResourceRemovalPending(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, resourceType core.ResourceType, externalID string) error
	MarkResourceRemoved(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, resourceType core.ResourceType, externalID string) error
	MarkResourceRemovalFailed(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, resourceType core.ResourceType, externalID string) error
	MarkAssociatedPoliciesRemoved(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, appExternalID string) error
}

// ResourceSaver is the interface the controller uses to persist
// provider resources created during successful steps.
type ResourceSaver interface {
	SaveResource(ctx context.Context, res *core.ProviderResource) error
}

// CredentialStorer is the controller interface for persisting
// tunnel credentials returned by provider steps. The supervisor
// provides the store implementation. (SPEC P0 #3)
type CredentialStorer interface {
	SaveTunnelCredential(ctx context.Context, connID core.ConnectionID, tunnelID, token string) error
	DeleteTunnelCredential(ctx context.Context, connID core.ConnectionID) error
}

// StepCommitRequest describes the full result of executing a step
// that must be persisted atomically. (SPEC P0 atomic step commit)
type StepCommitRequest = core.StepCommitRequest

// LifecycleMark describes a single resource lifecycle change.
type LifecycleMark = core.LifecycleMark

// StepResultCommitter persists a step result atomically in a single
// transaction: terminal event, resources, credentials, lifecycle changes.
type StepResultCommitter interface {
	CommitStepResult(ctx context.Context, req core.StepCommitRequest) error
}

// ConnectionStorer is the interface the controller uses to create
// connection profiles and runtimes atomically in a single transaction.
type ConnectionStorer interface {
	CreateConnection(ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime) error
}

// SetConnectionStorer sets the store used for atomic connection creation.
func (c *Controller) SetConnectionStorer(s ConnectionStorer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connectionStorer = s
}

// SetResourceSaver sets the store used to persist provider resources.
func (c *Controller) SetResourceSaver(s ResourceSaver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resourceSaver = s
}

// SetCredentialStorer sets the store used to persist tunnel credentials.
func (c *Controller) SetCredentialStorer(s CredentialStorer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credentialStorer = s
}

// SetStepCommitter sets the store used for atomic step result commits.
func (c *Controller) SetStepCommitter(s StepResultCommitter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stepCommitter = s
}

// SetProfileUpdater sets the store used for optimistic profile updates.
func (c *Controller) SetProfileUpdater(u ProfileUpdater) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.profileUpdater = u
}

// SetResourceRemover sets the store used to mark provider resources as removed.
func (c *Controller) SetResourceRemover(r ResourceRemover) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resourceRemover = r
}

// RuntimeCommitter is the interface the controller uses to
// durably persist runtime state transitions immediately after
// operation completion, so that state survives supervisor crashes.
// (SPEC P0 #13)
type RuntimeCommitter interface {
	CommitOpenSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID, startedAt time.Time) (*core.RuntimeCommitResult, error)
	CommitCloseSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) (*core.RuntimeCommitResult, error)
	CommitRepairSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) (*core.RuntimeCommitResult, error)
	CommitOperationFailure(ctx context.Context, connID core.ConnectionID, opID core.OperationID, errMsg string, provider core.ProviderID, retryable bool) error
	CommitDeleteSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) error
}

// SetRuntimeCommitter sets the store used to durably commit
// runtime transitions immediately after operation completion.
func (c *Controller) SetRuntimeCommitter(r RuntimeCommitter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runtimeCommitter = r
}

// FinalizeDeletion transactionally commits a connection deletion to the store,
// then removes in-memory state. Returns an error if the store commit fails.
// The caller should not retry the operation ID — the commit marks it completed.
func (c *Controller) FinalizeDeletion(ctx context.Context, finalizer DeleteConnectionFinalizer, connID core.ConnectionID, opID core.OperationID) error {
	if err := finalizer.CommitConnectionDeletion(ctx, connID, opID); err != nil {
		return fmt.Errorf("commit deletion: %w", err)
	}
	c.deleteProfileLocked(connID)
	return nil
}

// --------------- planning ---------------

// PlanOpen creates an open plan for a connection.
func (c *Controller) PlanOpen(ctx context.Context, connID core.ConnectionID) (*core.OperationPlan, error) {
	c.mu.RLock()
	profile, ok := c.profiles[connID]
	c.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("profile not found: %s", connID)
	}

	prov := c.registry.Get(profile.Provider.ProviderID)
	if prov == nil {
		return nil, fmt.Errorf("provider not found: %s", profile.Provider.ProviderID)
	}

	// New profiles are stored as DesiredClosed; the provider needs an
	// open-intent profile to generate the correct plan.
	openProfile := profile.DeepCopy()
	openProfile.Desired = core.DesiredOpen

	// Prepare/resolve the local origin before calling the provider.
	resolvedOrigin, err := c.prepareOrigin(ctx, openProfile.Source)
	if err != nil {
		return nil, fmt.Errorf("origin preparation: %w", err)
	}

	desired := core.DesiredConnection{
		Profile: openProfile,
		Origin:  resolvedOrigin,
	}

	plan, err := prov.Plan(ctx, desired)
	if err != nil {
		return nil, err
	}

	// Compute fingerprint after receiving from provider.
	if plan.Fingerprint == "" {
		if err := plan.ComputeFingerprint(); err != nil {
			return nil, fmt.Errorf("fingerprint: %w", err)
		}
	}

	return plan, nil
}

// prepareOrigin resolves or starts the local origin based on the source spec.
// For existing services, it returns the resolved URL.
// For directory, command, and MCP sources, it returns the expected URL (origin will be started by the plan).
func (c *Controller) prepareOrigin(ctx context.Context, source core.SourceSpec) (*core.ResolvedOrigin, error) {
	switch source.Kind {
	case core.SourceExisting:
		if source.Existing == nil || source.Existing.Address == "" {
			return nil, fmt.Errorf("existing source has no address")
		}
		url := fmt.Sprintf("%s://%s", source.Existing.Protocol, source.Existing.Address)
		return &core.ResolvedOrigin{
			URL:      url,
			Protocol: source.Existing.Protocol,
			Owned:    false,
		}, nil

	case core.SourceDirectory:
		if source.Directory == nil || source.Directory.Path == "" {
			return nil, fmt.Errorf("directory source has no path")
		}
		// Note: In v0.1, we don't start the origin here — the plan
		// will include starting it. For now, return the expected URL.
		// TODO: Actually start the origin and wait for readiness.
		return &core.ResolvedOrigin{
			URL:      "http://127.0.0.1:0", // Placeholder — port assigned at start
			Protocol: core.ProtocolHTTP,
			Owned:    true,
		}, nil

	case core.SourceCommand:
		if source.Command == nil || source.Command.Executable == "" {
			return nil, fmt.Errorf("command source has no executable")
		}
		if source.Command.Port == 0 {
			return nil, fmt.Errorf("command source requires a port")
		}
		return &core.ResolvedOrigin{
			URL:      fmt.Sprintf("http://127.0.0.1:%d", source.Command.Port),
			Protocol: source.Command.Protocol,
			Owned:    true,
		}, nil

	case core.SourceMCP:
		if source.MCP == nil {
			return nil, fmt.Errorf("MCP source is nil")
		}
		if source.MCP.Endpoint == "" && source.MCP.Command == nil {
			return nil, fmt.Errorf("MCP source requires endpoint or command")
		}
		// For MCP, the origin is the MCP server itself
		var url string
		if source.MCP.Endpoint != "" {
			url = source.MCP.Endpoint
		} else {
			url = fmt.Sprintf("http://127.0.0.1:%d", source.MCP.Command.Port)
		}
		return &core.ResolvedOrigin{
			URL:      url,
			Protocol: core.ProtocolHTTP,
			Owned:    source.MCP.Command != nil,
		}, nil

	default:
		return nil, fmt.Errorf("unknown source kind %q", source.Kind)
	}
}

// PlanClose creates a close plan.
func (c *Controller) PlanClose(ctx context.Context, connID core.ConnectionID) (*core.OperationPlan, error) {
	c.mu.RLock()
	profile, ok := c.profiles[connID]
	c.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("profile not found: %s", connID)
	}

	// Derive a closed-profile view for the provider.
	closedProfile := *profile
	closedProfile.Desired = core.DesiredClosed

	prov := c.registry.Get(profile.Provider.ProviderID)
	if prov == nil {
		return nil, fmt.Errorf("provider not found: %s", profile.Provider.ProviderID)
	}

	desired := core.DesiredConnection{
		Profile: &closedProfile,
	}

	plan, err := prov.Plan(ctx, desired)
	if err != nil {
		return nil, err
	}

	if plan.Fingerprint == "" {
		if err := plan.ComputeFingerprint(); err != nil {
			return nil, fmt.Errorf("fingerprint: %w", err)
		}
	}

	return plan, nil
}

// PlanDelete creates a delete plan for a connection.
func (c *Controller) PlanDelete(ctx context.Context, connID core.ConnectionID) (*core.OperationPlan, error) {
	c.mu.RLock()
	profile, ok := c.profiles[connID]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("profile not found: %s", connID)
	}

	// Gather managed resources from runtime to inform the delete plan.
	c.mu.RLock()
	rt, rtOk := c.runtimes[connID]
	var resources []core.ProviderResource
	var connectorStatus core.ConnectorStatus
	if rtOk {
		resources = rt.Provider.Resources
		connectorStatus = rt.Connector.Status
	}
	c.mu.RUnlock()

	steps := []core.PlanStep{}

	// Step 1: Stop connector if running.
	if rtOk && connectorStatus == core.ConnectorStatusRunning {
		steps = append(steps, core.PlanStep{
			ID:      "delete-stop-connector",
			Kind:    core.StepStopConnector,
			Summary: "Stop connector process",
			Technical: core.TechnicalOperation{
				Provider: profile.Provider.ProviderID,
				Type:     "stop_connector",
			},
			Destructive:  true,
			Irreversible: false,
		})
	}

	// Step 2: Remove managed remote resources.
	// Create a step for each managed resource with its exact external ID.
	// Note: Access policy resources are deleted as part of Access application
	// deletion (cascading), so we don't generate separate steps for them.
	for _, res := range resources {
		if res.Ownership != core.OwnershipManaged {
			continue
		}
		var stepKind core.StepKind
		var stepType string
		var stepSummary string
		switch res.Type {
		case core.ResourceTunnel:
			stepKind = core.StepDeleteTunnel
			stepType = "delete_tunnel"
			stepSummary = "Delete named tunnel"
		case core.ResourceDNSRecord:
			stepKind = core.StepDeleteDNSRecord
			stepType = "delete_dns"
			stepSummary = "Delete DNS CNAME record"
		case core.ResourceAccessApp:
			stepKind = core.StepDeleteAccessApp
			stepType = "delete_access"
			stepSummary = "Delete Access application and policy"
		case core.ResourceAccessPolicy:
			// Policies are deleted as part of the Access application deletion.
			// Skip to avoid duplicate deletion attempts.
			continue
		default:
			continue
		}
		steps = append(steps, core.PlanStep{
			ID:      fmt.Sprintf("delete-%s-%s", strings.ToLower(string(res.Type)), res.ExternalID),
			Kind:    stepKind,
			Summary: fmt.Sprintf("%s %s", stepSummary, res.ExternalID),
			Technical: core.TechnicalOperation{
				Provider:   profile.Provider.ProviderID,
				Type:       stepType,
				ResourceID: res.ExternalID,
			},
			Destructive:  true,
			Irreversible: true,
		})
	}

	// If no resources are tracked, do not guess that remote resources exist.
	// A destructive plan must not assume ownership or invent remote IDs.
	// Instead, produce a local-only deletion plan (connector stop + local cleanup).
	// Remote resources can only be deleted when their exact external IDs and
	// ownership are known from persisted provider resources.
	if len(resources) == 0 {
		slog.Info("no tracked resources for delete plan, producing local-only deletion",
			"connection", connID)
	}

	// Always add finalization step to ensure plan is valid.
	// This step handles local cleanup after connector and remote resources are removed.
	steps = append(steps, core.PlanStep{
		ID:      "delete-finalize-local",
		Kind:    "finalize_local_deletion",
		Summary: "Finalize local connection deletion",
		Technical: core.TechnicalOperation{
			Provider: profile.Provider.ProviderID,
			Type:     "finalize_local_deletion",
		},
		Destructive:  true,
		Irreversible: false,
	})

	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    connID,
		ProfileRevision: profile.Revision,
		Provider:        profile.Provider.ProviderID,
		Intent:          core.IntentDelete,
		Steps:           steps,
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}

	return plan, nil
}

// ErrNoRepairNeeded is returned by PlanRepair when the connection is healthy
// and no repair action is required.
var ErrNoRepairNeeded = errors.New("no repair needed")

// PlanRepair creates a repair plan for a connection based on findings.
func (c *Controller) PlanRepair(ctx context.Context, connID core.ConnectionID) (*core.OperationPlan, error) {
	c.mu.RLock()
	profile, ok := c.profiles[connID]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("profile not found: %s", connID)
	}

	prov := c.registry.Get(profile.Provider.ProviderID)
	if prov == nil {
		return nil, fmt.Errorf("provider not found: %s", profile.Provider.ProviderID)
	}

	// Get current findings to determine repair actions.
	c.mu.RLock()
	rt, rtOk := c.runtimes[connID]
	c.mu.RUnlock()

	// Build a repair plan based on connector status.
	steps := []core.PlanStep{}
	if rtOk {
		switch rt.Connector.Status {
		case core.ConnectorStatusCrashed, core.ConnectorStatusStopped:
			steps = append(steps, core.PlanStep{
				ID:      "repair-restart-connector",
				Kind:    core.StepStartConnector,
				Summary: "Restart connector",
				Technical: core.TechnicalOperation{
					Provider:   profile.Provider.ProviderID,
					Type:       "start_connector",
					Parameters: map[string]string{"mode": "permanent"},
				},
			})
		}
	}

	// If no steps were generated, the connection needs no repair.
	if len(steps) == 0 {
		return nil, ErrNoRepairNeeded
	}

	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    connID,
		ProfileRevision: profile.Revision,
		Provider:        profile.Provider.ProviderID,
		Intent:          core.IntentRepair,
		Steps:           steps,
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}

	return plan, nil
}

// SavePlan validates, deep-copies, and stores a plan for later application.
// Returns an error if the plan is invalid or fingerprint verification fails.
func (c *Controller) SavePlan(plan *core.OperationPlan) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := plan.Validate(); err != nil {
		return fmt.Errorf("plan validation: %w", err)
	}

	// Compute and attach fingerprint if missing.
	if plan.Fingerprint == "" {
		if err := plan.ComputeFingerprint(); err != nil {
			return fmt.Errorf("plan fingerprint: %w", err)
		}
	}

	// Verify fingerprint matches content before storing.
	if err := plan.VerifyFingerprint(); err != nil {
		return fmt.Errorf("plan fingerprint mismatch: %w", err)
	}

	// Prevent duplicate plan IDs with different content.
	if existing, ok := c.plans[plan.ID]; ok {
		existingFingerprint := existing.Fingerprint
		if existingFingerprint != plan.Fingerprint {
			return fmt.Errorf("plan %s already exists with different content", plan.ID)
		}
		// Identical plan: return nil (idempotent save).
		return nil
	}

	// Store a deep copy to prevent external mutation of stored plans.
	c.plans[plan.ID] = plan.DeepCopy()
	return nil
}

// GetPlan returns a deep copy of a stored plan.
func (c *Controller) GetPlan(id core.PlanID) (*core.OperationPlan, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.plans[id]
	if !ok {
		return nil, false
	}
	return p.DeepCopy(), true
}

// ApplyPlan validates preconditions and applies a plan through the provider.
// It delegates to execPlan for the actual execution after validation.
// Dispatch by plan intent: open/close → Provider.Apply, repair → Provider.Repair, delete → Provider.Remove.
func (c *Controller) ApplyPlan(ctx context.Context, planID core.PlanID) (Operation, error) {
	plan, ok := c.GetPlan(planID)
	if !ok {
		return Operation{}, fmt.Errorf("plan not found: %s", planID)
	}

	// Verify fingerprint before execution — checks immutability by
	// recomputing the hash via VerifyFingerprint (which calls ComputeFingerprint
	// and compares against the stored value).
	if plan.Fingerprint != "" {
		if err := plan.VerifyFingerprint(); err != nil {
			return Operation{}, fmt.Errorf("plan integrity check: %w", err)
		}
	}

	// Enforce global operation limit
	if !c.canStartOperation() {
		return Operation{}, fmt.Errorf("global operation limit reached (%d concurrent)", GlobalOpLimit)
	}

	// Enforce one active mutation per connection
	if c.HasActiveOperation(plan.ConnectionID) {
		return Operation{}, fmt.Errorf("connection %s already has an active operation", plan.ConnectionID)
	}

	prov := c.registry.Get(plan.Provider)
	if prov == nil {
		return Operation{}, fmt.Errorf("provider not found: %s", plan.Provider)
	}

	// Validate preconditions
	if err := c.validatePlan(ctx, plan); err != nil {
		return Operation{}, fmt.Errorf("plan validation: %w", err)
	}

	// Dispatch by plan intent — all intents use the same execPlan path
	switch plan.Intent {
	case core.IntentOpen, core.IntentClose, core.IntentRepair, core.IntentDelete:
		return c.execPlan(ctx, plan, prov)
	default:
		return Operation{}, fmt.Errorf("unknown plan intent: %s", plan.Intent)
	}
}

// GetOperation returns an immutable snapshot of an operation by ID.
func (c *Controller) GetOperation(id core.OperationID) (*Operation, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	rec, ok := c.operations[id]
	if !ok {
		return nil, false
	}
	snap := rec.Snapshot()
	return &snap, true
}

// Observe returns the observed state of a connection from the provider.
func (c *Controller) Observe(ctx context.Context, connID core.ConnectionID) (*core.ObservedConnection, error) {
	c.mu.RLock()
	profile, ok := c.profiles[connID]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("profile not found: %s", connID)
	}

	prov := c.registry.Get(profile.Provider.ProviderID)
	if prov == nil {
		return nil, fmt.Errorf("provider not found: %s", profile.Provider.ProviderID)
	}

	observed, err := prov.Observe(ctx, connID)
	if err != nil {
		return nil, err
	}

	// Update runtime with observed state and increment ObservedRevision.
	c.mu.Lock()
	if rt, ok := c.runtimes[connID]; ok {
		rt.ObservedRevision++
		rt.LastObservedAt = time.Now().UTC()
		if observed.Connector != nil {
			rt.Connector.Status = core.ConnectorStatus(observed.Connector.Status)
			rt.Connector.PID = observed.Connector.PID
		}
		if observed.Tunnel != nil {
			// Update runtime with tunnel info if needed
		}
	}
	c.mu.Unlock()

	return observed, nil
}

// verifyOperationOutcome re-observes the provider and checks that
// the resulting state matches the expected outcome for the given intent.
// This prevents committing terminal success when the provider reports
// a successful operation but the actual observed state does not reflect
// the intended transition (SPEC P0 #12).
func (c *Controller) verifyOperationOutcome(ctx context.Context, intent core.OperationIntent, connID core.ConnectionID) error {
	observed, obsErr := c.Observe(ctx, connID)
	if obsErr != nil {
		return fmt.Errorf("observation failed: %w", obsErr)
	}
	if observed == nil {
		return fmt.Errorf("provider returned nil observation")
	}

	switch intent {
	case core.IntentOpen:
		// For an open operation, the connector must be running.
		if observed.Connector == nil || observed.Connector.Status != "running" {
			return fmt.Errorf("expected connector running, got %q", connectorStatus(observed.Connector))
		}
	case core.IntentClose:
		// For a close operation, the connector must be stopped.
		if observed.Connector == nil || observed.Connector.Status != "stopped" {
			return fmt.Errorf("expected connector stopped, got %q", connectorStatus(observed.Connector))
		}
	case core.IntentRepair:
		// For a repair operation, the connector must be running.
		if observed.Connector == nil || observed.Connector.Status != "running" {
			return fmt.Errorf("expected connector running after repair, got %q", connectorStatus(observed.Connector))
		}
	case core.IntentDelete:
		// For a delete operation, all managed resources must be removed.
		// The store's CommitDeleteSuccess already verifies this, but we
		// also check that the connector is stopped if it was running.
		if observed.Connector != nil && observed.Connector.Status != "stopped" {
			return fmt.Errorf("expected connector stopped after delete, got %q", observed.Connector.Status)
		}
		// Check that no managed resources remain on the provider.
		// This is a best-effort check; the store transaction is authoritative.
		if observed.Tunnel != nil {
			return fmt.Errorf("tunnel %s still present on provider", observed.Tunnel.ID)
		}
		if len(observed.DNSRecords) > 0 {
			return fmt.Errorf("%d DNS records still present on provider", len(observed.DNSRecords))
		}
		if len(observed.AccessApps) > 0 {
			return fmt.Errorf("%d Access apps still present on provider", len(observed.AccessApps))
		}
	}

	return nil
}

// connectorStatus safely extracts the connector status string.
func connectorStatus(c *core.ObservedConnector) string {
	if c == nil {
		return "<nil>"
	}
	return c.Status
}

// Reconcile checks desired vs observed state and returns a suggested action.
// It calls Observe() first to detect drift, then compares desired vs runtime state.
// Returns:
//
//	""                     — no action needed (desired == observed)
//	"open"                 — needs open plan
//	"close"                — needs close plan
//	"repair"               — connector is unhealthy / runtime degraded
//	"remove"               — managed resources need cleanup
func (c *Controller) Reconcile(ctx context.Context, connID core.ConnectionID) (string, error) {
	c.mu.RLock()
	profile, profileOk := c.profiles[connID]
	c.mu.RUnlock()
	if !profileOk {
		return "", fmt.Errorf("profile not found: %s", connID)
	}

	// Step 1: Observe current state from the provider.
	if _, err := c.Observe(ctx, connID); err != nil {
		return "", fmt.Errorf("observe failed: %w", err)
	}

	// Step 2: Compare desired vs runtime state.
	c.mu.RLock()
	rt, rtOk := c.runtimes[connID]
	c.mu.RUnlock()

	desired := profile.Desired
	if !rtOk {
		// No runtime recorded — if desired is open, we need to open.
		if desired == core.DesiredOpen {
			return "open", nil
		}
		return "", nil
	}

	// P0 #11: Desired closed must dominate — do not repair a connection the user wants closed.
	if desired == core.DesiredClosed && rt.State != core.RuntimeClosed {
		return "close", nil
	}

	// Desired open but closed/unknown → open
	if desired == core.DesiredOpen && (rt.State == core.RuntimeClosed || rt.State == core.RuntimeUnknown) {
		return "open", nil
	}

	// Desired open but degraded/open with failed segment → repair
	if desired == core.DesiredOpen && (rt.State == core.RuntimeDegraded || rt.State == core.RuntimeError) {
		return "repair", nil
	}

	// Desired open and runtime open, but connector unhealthy → repair
	if desired == core.DesiredOpen && rt.State == core.RuntimeOpen {
		if rt.Connector.Status != core.ConnectorStatusRunning {
			return "repair", nil
		}
	}

	// If runtime shows managed resources but profile was deleted externally,
	// we should clean them up. (In practice, profile deletion is explicit.)
	if rt.State == core.RuntimeOrphaned {
		return "remove", nil
	}

	return "", nil
}

// --------------- operation execution ---------------

// canStartOperation checks if a new operation can be started.
func (c *Controller) canStartOperation() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	running := 0
	for _, rec := range c.operations {
		snap := rec.Snapshot()
		if snap.State == OperationStateRunning {
			running++
		}
	}
	return running < GlobalOpLimit
}

// HasActiveOperation returns true if the connection has an active operation.
func (c *Controller) HasActiveOperation(connID core.ConnectionID) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	rt, ok := c.runtimes[connID]
	if !ok || rt.ActiveOperation == nil {
		return false
	}
	rec, opOk := c.operations[*rt.ActiveOperation]
	if !opOk {
		return false
	}
	snap := rec.Snapshot()
	return snap.State == OperationStateRunning
}

// GlobalOpLimit returns the global operation concurrency limit.
const GlobalOpLimit = 4

// validatePlan performs all precondition checks and returns nil if the plan
// is safe to apply.
func (c *Controller) validatePlan(ctx context.Context, plan *core.OperationPlan) error {
	c.mu.RLock()
	profile, profileOk := c.profiles[plan.ConnectionID]
	c.mu.RUnlock()

	if !profileOk {
		return fmt.Errorf("profile not found: %s", plan.ConnectionID)
	}
	if plan.ProfileRevision != profile.Revision {
		return fmt.Errorf(
			"profile revision mismatch: plan=%d, current=%d",
			plan.ProfileRevision, profile.Revision,
		)
	}
	if !plan.ExpiresAt.IsZero() && time.Now().UTC().After(plan.ExpiresAt) {
		return fmt.Errorf("plan %s has expired", plan.ID)
	}
	return nil
}

// --------------- hydration (for supervisor restart) ---------------

// RestoreProfile restores a persisted profile into the controller
// without modifying timestamps, revision, or creating a new runtime.
func (c *Controller) RestoreProfile(profile *core.ConnectionProfile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := profile.DeepCopy()
	c.profiles[stored.ID] = stored
}

// RestoreRuntime restores a persisted runtime into the controller
// without triggering provider mutations or creating new operation events.
func (c *Controller) RestoreRuntime(rt *core.ConnectionRuntime) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runtimes[rt.ConnectionID] = rt.DeepCopy()
}

// RestoreResources restores provider resources for a connection.
func (c *Controller) RestoreResources(connID core.ConnectionID, resources []core.ProviderResource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rt, ok := c.runtimes[connID]; ok {
		rt.Provider.Resources = resources
	}
}

// RestorePlan restores a persisted operation plan into the controller.
// Stores a deep copy to prevent external mutation of stored plans.
func (c *Controller) RestorePlan(plan *core.OperationPlan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plans[plan.ID] = plan.DeepCopy()
}

// RestoreFinding restores a persisted diagnostic finding.
func (c *Controller) RestoreFinding(finding *core.DiagnosticFinding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rt, ok := c.runtimes[finding.ConnectionID]; ok {
		rt.Diagnostics = append(rt.Diagnostics, *finding)
	}
}

// Diagnose runs diagnostics for a connection and returns findings.
// For now, returns the current runtime diagnostics.
func (c *Controller) Diagnose(ctx context.Context, connID core.ConnectionID) ([]core.DiagnosticFinding, error) {
	c.mu.RLock()
	rt, ok := c.runtimes[connID]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("runtime not found: %s", connID)
	}
	return rt.Diagnostics, nil
}
