package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
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
	stepJournal      StepExecutionJournal
	runtimeCommitter RuntimeCommitter
	runtimeSaver     RuntimeSaver
	cleanupRecorder  CleanupRecorder
	eventDispatcher  CommittedEventDispatcher
	originManager    OriginManager
	operationMu      sync.Mutex
	operationWG      sync.WaitGroup
	acceptingOps     bool
}

// OriginManager resolves and owns local source processes/servers. The
// controller depends on this narrow contract so it retains no knowledge of
// concrete origin implementations or process details.
type OriginManager interface {
	Plan(connectionID core.ConnectionID, source core.SourceSpec) (*core.ResolvedOrigin, error)
	Start(ctx context.Context, connectionID core.ConnectionID, source core.SourceSpec, expectedURL string) error
	Stop(ctx context.Context, connectionID core.ConnectionID) error
	// Observe returns the current origin runtime for a connection. The
	// boolean is false when the connection does not own an origin (for
	// example, an external existing-service source).
	Observe(connectionID core.ConnectionID) (core.OriginRuntime, bool)
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
	mu     sync.RWMutex
	oper   Operation
	cancel context.CancelFunc
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
		profiles:     make(map[core.ConnectionID]*core.ConnectionProfile),
		runtimes:     make(map[core.ConnectionID]*core.ConnectionRuntime),
		operations:   make(map[core.OperationID]*operationRecord),
		plans:        make(map[core.PlanID]*core.OperationPlan),
		registry:     registry,
		accounts:     nil,
		journal:      journal,
		acceptingOps: true,
	}
}

// ShutdownOperations prevents new mutations, cancels active operation
// contexts, and waits for their goroutines before the supervisor closes
// persistent state. It is safe to call repeatedly.
func (c *Controller) ShutdownOperations(ctx context.Context) error {
	c.operationMu.Lock()
	c.acceptingOps = false
	c.mu.RLock()
	operations := make([]*operationRecord, 0, len(c.operations))
	for _, record := range c.operations {
		operations = append(operations, record)
	}
	c.mu.RUnlock()
	c.operationMu.Unlock()
	for _, record := range operations {
		record.mu.RLock()
		cancel := record.cancel
		record.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
	}
	done := make(chan struct{})
	go func() {
		c.operationWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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

// SetOriginManager attaches the supervisor-owned local source manager.
func (c *Controller) SetOriginManager(manager OriginManager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.originManager = manager
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
	if profile == nil {
		return nil, nil, core.ErrValidation("profile is required")
	}
	// Assign ID before validation
	if profile.ID == "" {
		profile.ID = core.NewConnectionID()
	}
	// For backward compatibility, use the accessor methods
	provider := profile.GetProvider()
	if provider.AccountID == "" && provider.ProviderID != "" {
		if accounts := c.registry.GetAccounts(provider.ProviderID); len(accounts) == 1 {
			// Make the implicit single-account choice durable. A profile created
			// before a second account is added must continue using the account it
			// was originally planned against.
			profile.Driver.AccountID = accounts[0]
		}
	}
	protection := profile.GetProtection()
	if protection.Kind != "" && protection.Kind != core.ProtectionNone && protection.SessionTTL <= 0 {
		// Update the session TTL in the spec
		if profile.Spec.ServiceExposure != nil {
			profile.Spec.ServiceExposure.Protection.SessionTTL = core.DefaultProtectedSessionTTL
		}
	}

	// Validate before storing
	if err := profile.Validate(); err != nil {
		return nil, nil, core.ErrValidation(err.Error())
	}

	// Validate provider exists
	if _, err := c.providerForProfile(profile); err != nil {
		return nil, nil, err
	}

	// Deep copy before storing
	storedProfile := profile.DeepCopy()
	now := time.Now().UTC()
	storedProfile.CreatedAt = now
	storedProfile.UpdatedAt = now
	storedProfile.Revision = 1

	// Reject duplicate IDs
	if _, exists := c.profiles[storedProfile.ID]; exists {
		return nil, nil, core.ErrConnectionExists(storedProfile.ID)
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
		return core.ErrValidation(err.Error())
	}

	// Validate provider exists
	if _, err := c.providerForProfile(profile); err != nil {
		return err
	}

	// Load existing profile to preserve CreatedAt
	c.mu.RLock()
	existing, ok := c.profiles[profile.ID]
	c.mu.RUnlock()

	if !ok {
		return core.ErrProfileNotFound(profile.ID)
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
			return core.ErrProfileNotFound(profile.ID)
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
	SaveTunnelCredential(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string, token []byte) error
	DeleteTunnelCredentialExact(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string) error
}

// CleanupRecorder persists cleanup obligations for resources that may exist
// remotely but were never successfully inserted into the normal inventory.
type CleanupRecorder interface {
	RecordCleanupItem(ctx context.Context, operationID core.OperationID, connectionID core.ConnectionID, providerID core.ProviderID, resourceType core.ResourceType, externalID, state, lastError string) error
}

// CommittedEventDispatcher delivers rows that have already committed to the
// shared durable event journal. It deliberately cannot create events: the
// store transaction is the source of truth and the dispatcher only wakes SSE
// subscribers after that transaction has succeeded.
type CommittedEventDispatcher interface {
	DispatchCommittedEvents(ctx context.Context) error
}

// SetCommittedEventDispatcher wires the supervisor's SSE broker to the
// controller without giving the controller knowledge of IPC transport types.
func (c *Controller) SetCommittedEventDispatcher(dispatcher CommittedEventDispatcher) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eventDispatcher = dispatcher
}

func (c *Controller) dispatchCommittedEvents(ctx context.Context) {
	c.mu.RLock()
	dispatcher := c.eventDispatcher
	c.mu.RUnlock()
	if dispatcher == nil {
		return
	}
	if err := dispatcher.DispatchCommittedEvents(ctx); err != nil {
		slog.Warn("dispatch committed events", "err", err)
	}
}

// SetCleanupRecorder sets the durable orphan/cleanup ledger.
func (c *Controller) SetCleanupRecorder(recorder CleanupRecorder) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanupRecorder = recorder
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

// StepExecutionJournal durably brackets every normal step execution. Begin
// and terminal outcome commits are each atomic with their corresponding
// operation event, so interrupted operations have a recovery ledger instead
// of relying on best-effort event reconstruction.
type StepExecutionJournal interface {
	BeginStep(ctx context.Context, operationID core.OperationID, connectionID core.ConnectionID, step core.PlanStep) error
	CommitStepOutcome(ctx context.Context, req core.StepCommitRequest) error
}

// CompensationJournal records compensating mutations in the same recovery
// ledger used by normal steps. It is optional for lightweight test wiring.
type CompensationJournal interface {
	BeginCompensation(ctx context.Context, operationID core.OperationID, connectionID core.ConnectionID, step core.PlanStep) error
	CommitCompensationOutcome(ctx context.Context, operationID core.OperationID, connectionID core.ConnectionID, step core.PlanStep, result core.StepResult) error
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
	if journal, ok := s.(StepExecutionJournal); ok {
		c.stepJournal = journal
	}
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

// RuntimeSaver persists observed runtime projections that are not terminal
// operation commits, including the complete process identity used for safe
// restart adoption.
type RuntimeSaver interface {
	SaveRuntime(ctx context.Context, runtime *core.ConnectionRuntime) error
}

func (c *Controller) SetRuntimeSaver(s RuntimeSaver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runtimeSaver = s
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
		return nil, core.ErrProfileNotFound(connID)
	}

	prov, err := c.providerForProfile(profile)
	if err != nil {
		return nil, err
	}

	// New profiles are stored as DesiredClosed; the provider needs an
	// open-intent profile to generate the correct plan.
	openProfile := profile.DeepCopy()
	openProfile.Desired = core.DesiredOpen

	// Prepare/resolve the local origin before calling the provider. Owned
	// origins are represented by an explicit plan step and are started only
	// after the preview is accepted.
	resolvedOrigin, err := c.prepareOriginForConnection(ctx, connID, openProfile.GetSource())
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
	if resolvedOrigin.Owned {
		insertStartOriginStep(plan, resolvedOrigin.URL)
	}

	// Providers compute their own fingerprint, but controller-owned origin
	// steps are part of what will execute and therefore must be rehashed.
	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}

	// Populate the observed fingerprint so that execPlan can verify the
	// provider state hasn't changed since this plan was created.
	if planRequiresProvider(plan) {
		if obs, err := c.Observe(ctx, connID); err == nil {
			if fp, err := obs.ComputeFingerprint(); err == nil {
				plan.ObservedFingerprint = fp
				// Recompute because ObservedFingerprint is part of the fingerprint.
				_ = plan.ComputeFingerprint()
			}
		}
	}

	return plan, nil
}

// prepareOrigin resolves an existing origin or asks the supervisor-owned
// OriginManager to plan a stable owned source URL. Planning never launches a
// process or opens a listener.
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

	case core.SourceDirectory, core.SourceCommand, core.SourceMCP:
		c.mu.RLock()
		manager := c.originManager
		c.mu.RUnlock()
		if manager == nil {
			return nil, fmt.Errorf("local source manager is unavailable")
		}
		// The manager needs the connection ID to derive stable endpoint data;
		// PlanOpen fills it through the surrounding profile, so callers of this
		// helper use a source copy with no runtime mutation.
		return nil, fmt.Errorf("local source resolution requires a connection ID")

	default:
		return nil, fmt.Errorf("unknown source kind %q", source.Kind)
	}
}

func (c *Controller) prepareOriginForConnection(ctx context.Context, connectionID core.ConnectionID, source core.SourceSpec) (*core.ResolvedOrigin, error) {
	if source.Kind == core.SourceExisting {
		return c.prepareOrigin(ctx, source)
	}
	c.mu.RLock()
	manager := c.originManager
	c.mu.RUnlock()
	if manager == nil {
		return nil, fmt.Errorf("local source manager is unavailable")
	}
	return manager.Plan(connectionID, source)
}

func insertStartOriginStep(plan *core.OperationPlan, originURL string) {
	step := core.PlanStep{
		ID:      "origin-start",
		Kind:    core.StepStartOrigin,
		Summary: "Start local source",
		Technical: core.TechnicalOperation{Type: "start_origin", Parameters: map[string]string{
			"origin_url": originURL,
		}},
		Compensation: &core.CompensationStep{
			ID:        "origin-stop-compensation",
			Kind:      core.StepStopOrigin,
			Technical: core.TechnicalOperation{Type: "stop_origin"},
		},
	}
	for i, existing := range plan.Steps {
		if existing.Kind == core.StepStartConnector {
			plan.Steps = append(plan.Steps, core.PlanStep{})
			copy(plan.Steps[i+1:], plan.Steps[i:])
			plan.Steps[i] = step
			return
		}
	}
	// A provider opening a connection must start a connector. Retaining the
	// step at the end makes malformed provider plans visible during execution
	// instead of performing an invisible side effect during preview.
	plan.Steps = append(plan.Steps, step)
}

func appendStopOriginStep(plan *core.OperationPlan) {
	plan.Steps = append(plan.Steps, core.PlanStep{
		ID:        "origin-stop",
		Kind:      core.StepStopOrigin,
		Summary:   "Stop local source",
		Technical: core.TechnicalOperation{Type: "stop_origin"},
	})
}

// PlanClose creates a close plan.
func (c *Controller) PlanClose(ctx context.Context, connID core.ConnectionID) (*core.OperationPlan, error) {
	c.mu.RLock()
	profile, ok := c.profiles[connID]
	c.mu.RUnlock()

	if !ok {
		return nil, core.ErrProfileNotFound(connID)
	}

	// Derive a closed-profile view for the provider.
	closedProfile := *profile
	closedProfile.Desired = core.DesiredClosed

	prov, err := c.providerForProfile(profile)
	if err != nil {
		return nil, err
	}

	desired := core.DesiredConnection{
		Profile: &closedProfile,
	}

	plan, err := prov.Plan(ctx, desired)
	if err != nil {
		return nil, err
	}

	if sourceOwnsOrigin(profile.GetSource()) {
		appendStopOriginStep(plan)
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}

	// Populate observed fingerprint for stale-plan detection.
	if planRequiresProvider(plan) {
		if obs, err := c.Observe(ctx, connID); err == nil {
			if fp, err := obs.ComputeFingerprint(); err == nil {
				plan.ObservedFingerprint = fp
				// Recompute because ObservedFingerprint is part of the fingerprint.
				_ = plan.ComputeFingerprint()
			}
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
		return nil, core.ErrProfileNotFound(connID)
	}

	// Gather managed resources from runtime to inform the delete plan.
	// Deep-copy resources to avoid holding a reference to the shared
	// runtime slice after releasing the lock.
	c.mu.RLock()
	rt, rtOk := c.runtimes[connID]
	var resources []core.ProviderResource
	var connectorStatus core.ConnectorStatus
	if rtOk {
		if rt.Provider.Resources != nil {
			resources = make([]core.ProviderResource, len(rt.Provider.Resources))
			for i, r := range rt.Provider.Resources {
				resources[i] = r.Clone()
			}
		}
		connectorStatus = rt.Connector.Status
	}
	c.mu.RUnlock()

	steps := []core.PlanStep{}

	// Step 1: Stop connector if it may still be running.
	// We stop on any status that indicates a process might exist — running,
	// crashed, unknown, or unstable — to prevent leaking a connector
	// process during deletion.
	if rtOk && connectorStatus != core.ConnectorStatusStopped {
		steps = append(steps, core.PlanStep{
			ID:      "delete-stop-connector",
			Kind:    core.StepStopConnector,
			Summary: "Stop connector process",
			Technical: core.TechnicalOperation{
				Provider: profile.GetProvider().ProviderID,
				Type:     "stop_connector",
			},
			Destructive:  true,
			Irreversible: false,
		})
	}
	if sourceOwnsOrigin(profile.GetSource()) {
		steps = append(steps, core.PlanStep{
			ID:          "delete-stop-origin",
			Kind:        core.StepStopOrigin,
			Summary:     "Stop local source",
			Technical:   core.TechnicalOperation{Type: "stop_origin"},
			Destructive: true,
		})
	}

	// Step 2: Remove managed remote resources.
	// Create a step for each managed resource with its exact external ID.
	// Note: Access policy resources are deleted as part of Access application
	// deletion (cascading), so we don't generate separate steps for them.
	for _, res := range resources {
		if res.Ownership != core.OwnershipManaged || !res.IsLive() {
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
				Provider:   profile.GetProvider().ProviderID,
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
			Provider: profile.GetProvider().ProviderID,
			Type:     "finalize_local_deletion",
		},
		Destructive:  true,
		Irreversible: false,
	})

	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    connID,
		ProfileRevision: profile.Revision,
		Provider:        profile.GetProvider().ProviderID,
		Intent:          core.IntentDelete,
		Steps:           steps,
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}

	// Populate observed fingerprint for stale-plan detection.
	if planRequiresProvider(plan) {
		if obs, err := c.Observe(ctx, connID); err == nil {
			if fp, err := obs.ComputeFingerprint(); err == nil {
				plan.ObservedFingerprint = fp
				// Recompute because ObservedFingerprint is part of the fingerprint.
				_ = plan.ComputeFingerprint()
			}
		}
	}

	return plan, nil
}

func sourceOwnsOrigin(source core.SourceSpec) bool {
	return source.Kind == core.SourceDirectory || source.Kind == core.SourceCommand ||
		(source.Kind == core.SourceMCP && source.MCP != nil && source.MCP.Command != nil)
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
		return nil, core.ErrProfileNotFound(connID)
	}

	if _, err := c.providerForProfile(profile); err != nil {
		return nil, err
	}

	// Get current findings to determine repair actions.
	c.mu.RLock()
	rt, rtOk := c.runtimes[connID]
	c.mu.RUnlock()

	// Build a repair plan based on origin and connector status. Causal order:
	// origin must be healthy before the connector can succeed.
	steps := []core.PlanStep{}
	if rtOk {
		originNeedsRestart := rt.Origin.Ownership == core.OriginOwnershipOwned &&
			rt.Origin.Status != core.OriginStatusRunning
		connectorDown := rt.Connector.Status == core.ConnectorStatusCrashed ||
			rt.Connector.Status == core.ConnectorStatusStopped ||
			rt.Connector.Status == core.ConnectorStatusUnknown ||
			rt.Connector.Status == core.ConnectorStatusUnstable

		if originNeedsRestart || connectorDown {
			resolvedOrigin, err := c.prepareOriginForConnection(ctx, connID, profile.GetSource())
			if err != nil {
				return nil, fmt.Errorf("origin preparation: %w", err)
			}
			mode := "permanent"
			if profile.GetExposure().Mode == core.ExposureTemporary {
				mode = "quick"
			}
			// Owned origin must come first so the connector has a healthy
			// upstream to bind to.  Only repair what needs repair.
			if resolvedOrigin.Owned {
				if originNeedsRestart {
					planStub := &core.OperationPlan{Steps: steps}
					insertStartOriginStep(planStub, resolvedOrigin.URL)
					steps = planStub.Steps
				}
			}
			if connectorDown {
				steps = append(steps, core.PlanStep{
					ID:      "repair-restart-connector",
					Kind:    core.StepStartConnector,
					Summary: "Restart connector",
					Technical: core.TechnicalOperation{
						Provider:   profile.GetProvider().ProviderID,
						Type:       "start_connector",
						Parameters: map[string]string{"mode": mode, "origin_url": resolvedOrigin.URL},
					},
				})
			}
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
		Provider:        profile.GetProvider().ProviderID,
		Intent:          core.IntentRepair,
		Steps:           steps,
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}

	// Populate observed fingerprint for stale-plan detection. Only
	// meaningful for plans that interact with a provider — local-only
	// plans (delete with no tracked resources) have no provider state
	// to verify.
	if planRequiresProvider(plan) {
		if obs, err := c.Observe(ctx, connID); err == nil {
			if fp, err := obs.ComputeFingerprint(); err == nil {
				plan.ObservedFingerprint = fp
				// Recompute because ObservedFingerprint is part of the fingerprint.
				_ = plan.ComputeFingerprint()
			}
		}
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
		return Operation{}, core.ErrPlanNotFound(planID)
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

	// A delete plan may consist solely of controller-owned cleanup (for
	// example, removing an old profile whose optional development provider is
	// no longer installed). Do not make that recoverable local action depend
	// on loading a provider. Any plan containing a provider-owned step still
	// fails closed when its provider is unavailable.
	var prov core.Provider
	if planRequiresProvider(plan) {
		c.mu.RLock()
		profile := c.profiles[plan.ConnectionID]
		c.mu.RUnlock()
		if profile == nil {
			return Operation{}, core.ErrProfileNotFound(plan.ConnectionID)
		}
		var err error
		prov, err = c.providerForProfile(profile)
		if err != nil {
			return Operation{}, err
		}
		if prov.Identity().ID != plan.Provider {
			return Operation{}, core.ErrProviderNotFound(plan.Provider)
		}
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

// planRequiresProvider reports whether executing a plan can mutate or query
// provider-owned infrastructure. Origin lifecycle and final local deletion
// are controller-owned, so an otherwise local cleanup plan remains usable
// after a provider plugin has been removed. Keep this deliberately allowlist
// based: new step kinds require a provider unless they are explicitly proven
// controller-local here.
func planRequiresProvider(plan *core.OperationPlan) bool {
	if plan == nil {
		return true
	}
	for _, step := range plan.Steps {
		switch step.Kind {
		case core.StepStartOrigin, core.StepStopOrigin, core.StepFinalizeLocalDeletion:
			continue
		default:
			return true
		}
	}
	return false
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
	var resources []core.ProviderResource
	if runtime := c.runtimes[connID]; runtime != nil {
		for _, resource := range runtime.Provider.Resources {
			if resource.IsLive() {
				resources = append(resources, resource)
			}
		}
	}
	c.mu.RUnlock()
	if !ok {
		return nil, core.ErrProfileNotFound(connID)
	}

	prov, err := c.providerForProfile(profile)
	if err != nil {
		return nil, err
	}

	var observed *core.ObservedConnection
	if aware, ok := prov.(core.ResourceAwareObserver); ok {
		observed, err = aware.ObserveWithResources(ctx, connID, resources)
	} else {
		observed, err = prov.Observe(ctx, connID)
	}
	if err != nil {
		return nil, err
	}

	// Update runtime with observed state and increment ObservedRevision.
	c.mu.Lock()
	if rt, ok := c.runtimes[connID]; ok {
		rt.ObservedRevision++
		rt.LastObservedAt = time.Now().UTC()
		// Always derive origin ownership from the profile.
		ownsOrigin := sourceOwnsOrigin(profile.GetSource())
		if ownsOrigin && c.originManager != nil {
			if ort, hasOrigin := c.originManager.Observe(connID); hasOrigin {
				rt.Origin = ort
			} else {
				// Manager has no entry: mark as stopped (not leave stale state).
				rt.Origin = core.OriginRuntime{
					Ownership: core.OriginOwnershipOwned,
					Status:    core.OriginStatusStopped,
				}
			}
		}
		if observed.Connector != nil {
			rt.Connector.Status = core.ConnectorStatus(observed.Connector.Status)
			rt.Connector.PID = observed.Connector.PID
			rt.Connector.StartTime = observed.Connector.StartTime
			rt.Connector.Executable = observed.Connector.ExecutablePath
			rt.Connector.CommandHash = observed.Connector.CommandHash
			rt.Connector.Restarts = observed.Connector.Restarts
			rt.Connector.LastError = observed.Connector.LastError
		}
		if observed.Tunnel != nil {
			// Update runtime with tunnel info if needed
		}
		if c.runtimeSaver != nil && rt.ActiveOperation == nil {
			snapshot := rt.DeepCopy()
			c.mu.Unlock()
			if err := c.runtimeSaver.SaveRuntime(ctx, snapshot); err != nil {
				slog.Warn("persist observed runtime", "connection", connID, "err", err)
			}
			return observed, nil
		}
		c.mu.Unlock()
		return observed, nil
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
		return "", core.ErrProfileNotFound(connID)
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
		return core.ErrProfileNotFound(plan.ConnectionID)
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

// ReplaceDiagnostics installs the durable diagnostic projection into memory.
// Persistence is deliberately owned by Store.SyncFindings so a diagnostic
// run never has a separately committed runtime and finding set.
func (c *Controller) ReplaceDiagnostics(connID core.ConnectionID, findings []core.DiagnosticFinding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rt := c.runtimes[connID]
	if rt == nil {
		return
	}
	rt.Diagnostics = make([]core.DiagnosticFinding, len(findings))
	for i := range findings {
		rt.Diagnostics[i] = findings[i].Clone()
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
