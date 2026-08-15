package core

import (
	"context"
	"maps"
	"slices"
	"time"
)

// Clone returns a deep copy of the provider resource.
func (r ProviderResource) Clone() ProviderResource {
	cp := r
	if r.Metadata != nil {
		cp.Metadata = maps.Clone(r.Metadata)
	}
	return cp
}

// IsLive reports whether Portico still considers a provider resource to have
// a remote representation. A resource confirmed deleted by Portico or by an
// authoritative external observation must not be observed or deleted again.
func (r ProviderResource) IsLive() bool {
	return r.Lifecycle != LifecycleRemoved && r.Lifecycle != LifecycleExternallyRemoved
}

// RuntimeCommitResult carries the exact values committed by a terminal
// operation transaction so the controller installs committed state into
// memory instead of fabricating its own revision and timestamp.
type RuntimeCommitResult struct {
	ProfileRevision uint64
	DesiredState    DesiredConnectionState
	RuntimeState    RuntimeState
	LastTransition  time.Time
}

// ConnectionRuntime represents the observed runtime state of a connection.
// It is owned by the supervisor. Provider observation must never overwrite
// the user's desired profile.
type ConnectionRuntime struct {
	ConnectionID     ConnectionID
	RuntimeRevision  uint64 // persisted CAS clock; bumped on every successful write
	ObservedRevision uint64 // in-memory observation counter; not persisted
	State            RuntimeState
	Origin           OriginRuntime
	Connector        ConnectorRuntime
	Provider         ProviderRuntime
	Endpoint         EndpointRuntime
	Gateway          *GatewayRuntime
	Health           *HealthReport
	Diagnostics      []DiagnosticFinding
	ActiveOperation  *OperationID
	LastObservedAt   time.Time
	LastTransition   time.Time
	Error            *PorticoError
}

// DeepCopy returns a deep copy of the runtime.
func (r *ConnectionRuntime) DeepCopy() *ConnectionRuntime {
	if r == nil {
		return nil
	}
	cr := *r
	cr.Origin = r.Origin.DeepCopy()
	if r.Diagnostics != nil {
		diags := make([]DiagnosticFinding, len(r.Diagnostics))
		for i := range r.Diagnostics {
			diags[i] = r.Diagnostics[i].Clone()
		}
		cr.Diagnostics = diags
	}
	if r.Provider.Resources != nil {
		resources := make([]ProviderResource, len(r.Provider.Resources))
		for i := range r.Provider.Resources {
			resources[i] = r.Provider.Resources[i].Clone()
		}
		cr.Provider.Resources = resources
	}
	if r.Provider.AccountInfo != nil {
		cr.Provider.AccountInfo = maps.Clone(r.Provider.AccountInfo)
	}
	if r.ActiveOperation != nil {
		opID := *r.ActiveOperation
		cr.ActiveOperation = &opID
	}
	if r.Error != nil {
		errCopy := *r.Error
		cr.Error = &errCopy
	}
	if r.Gateway != nil {
		gwCopy := *r.Gateway
		if r.Gateway.AuthTokens != nil {
			gwCopy.AuthTokens = append([]string(nil), r.Gateway.AuthTokens...)
		}
		cr.Gateway = &gwCopy
	}
	if r.Health != nil {
		hcCopy := *r.Health
		cr.Health = &hcCopy
	}
	return &cr
}

// RuntimeState represents the observed state of a connection.
type RuntimeState string

const (
	RuntimeUnknown   RuntimeState = "unknown"
	RuntimePlanning  RuntimeState = "planning"
	RuntimeOpening   RuntimeState = "opening"
	RuntimeOpen      RuntimeState = "open"
	RuntimeDegraded  RuntimeState = "degraded"
	RuntimeRepairing RuntimeState = "repairing"
	RuntimeClosing   RuntimeState = "closing"
	RuntimeClosed    RuntimeState = "closed"
	RuntimeError     RuntimeState = "error"
	RuntimeOrphaned  RuntimeState = "orphaned"
)

// UserFacingState maps internal runtime states to a smaller vocabulary.
func (s RuntimeState) UserFacingState() string {
	switch s {
	case RuntimeOpen:
		return "Open"
	case RuntimeDegraded:
		return "Unstable"
	case RuntimeClosed:
		return "Closed"
	case RuntimeOpening, RuntimeRepairing, RuntimeClosing:
		return "Changing"
	case RuntimeError, RuntimeOrphaned:
		return "Needs attention"
	case RuntimeUnknown, RuntimePlanning:
		return "Checking"
	default:
		return "Unknown"
	}
}

// OriginRuntime represents the runtime state of a Portico-owned local origin.
type OriginRuntime struct {
	Ownership   OriginOwnership
	URL         string
	Status      OriginStatus
	PID         int
	StartTime   uint64
	Executable  string
	CommandHash string
	LastError   string
}

// DeepCopy returns a deep copy of the origin runtime.
func (o OriginRuntime) DeepCopy() OriginRuntime {
	return o
}

// OriginStatus describes the observed lifecycle status of an owned origin.
type OriginStatus string

const (
	OriginStatusRunning OriginStatus = "running"
	OriginStatusStopped OriginStatus = "stopped"
	OriginStatusCrashed OriginStatus = "crashed"
	OriginStatusUnknown OriginStatus = "unknown"
)

// OriginOwnership describes who owns the origin process.
type OriginOwnership string

const (
	OriginOwnershipOwned    OriginOwnership = "owned"
	OriginOwnershipExternal OriginOwnership = "external"
)

// ConnectorRuntime represents the runtime state of a connector process.
type ConnectorRuntime struct {
	PID         int
	StartTime   uint64
	Executable  string
	CommandHash string
	Status      ConnectorStatus
	Restarts    int
	LastError   string
}

// ConnectorStatus describes the connector lifecycle status.
type ConnectorStatus string

const (
	ConnectorStatusRunning  ConnectorStatus = "running"
	ConnectorStatusStopped  ConnectorStatus = "stopped"
	ConnectorStatusCrashed  ConnectorStatus = "crashed"
	ConnectorStatusStarting ConnectorStatus = "starting"
	// ConnectorStatusUnknown means the process identity could not be
	// verified. Unknown processes must never be signaled; repair is required.
	ConnectorStatusUnknown ConnectorStatus = "unknown"
	// ConnectorStatusUnstable means Portico exhausted the connector's restart
	// budget. It is distinct from crashed because automatic recovery stopped.
	ConnectorStatusUnstable ConnectorStatus = "unstable"
)

// ProviderRuntime represents provider-specific runtime info.
type ProviderRuntime struct {
	ProviderID  ProviderID
	AccountID   ProviderAccountID
	Resources   []ProviderResource
	AccountInfo map[string]string
}

// EndpointRuntime represents the runtime endpoint state.
type EndpointRuntime struct {
	PublicAddress  string
	PrivateAddress string
	Hostname       string
	Port           int
	Verified       bool
	LastVerifiedAt time.Time
}

// GatewayRuntime represents the runtime state of the Portico Gateway.
// The gateway is a local HTTP proxy that authenticates clients and forwards
// requests through the transport tunnel. It is nil when the connection
// does not use a gateway.
type GatewayRuntime struct {
	// Endpoint is the local URL the gateway listens on (e.g., http://127.0.0.1:PORT).
	Endpoint string `json:"endpoint"`
	// Upstream is the tunnel endpoint URL being proxied.
	Upstream string `json:"upstream"`
	// AuthTokens are the Bearer tokens accepted by the gateway.
	// Stored for runtime use; the actual tokens are also persisted durably.
	AuthTokens []string `json:"auth_tokens,omitempty"`
	// StartedAt records when the gateway was started.
	StartedAt time.Time `json:"started_at"`
}

// ProviderResource represents a provider-managed resource.
type ProviderResource struct {
	ID           string
	ConnectionID ConnectionID
	ProviderID   ProviderID
	Type         ResourceType
	ExternalID   string
	Ownership    ResourceOwnership
	Lifecycle    ResourceLifecycle
	SpecHash     string
	Metadata     map[string]string
}

// ResourceType identifies the type of provider resource.
type ResourceType string

const (
	ResourceTunnel       ResourceType = "tunnel"
	ResourceDNSRecord    ResourceType = "dns_record"
	ResourceAccessApp    ResourceType = "access_application"
	ResourceAccessPolicy ResourceType = "access_policy"
	ResourceConnector    ResourceType = "connector"
)

// DiagnosticFinding represents a diagnostic result attached to a route segment.
type DiagnosticFinding struct {
	ID            FindingID
	ConnectionID  ConnectionID
	Segment       RouteSegmentID
	Severity      Severity
	Summary       string
	Explanation   string
	Evidence      []Evidence
	RepairOptions []RepairOption
	ObservedAt    time.Time
	ResolvedAt    *time.Time
}

// RepairOption describes one possible repair.
type RepairOption struct {
	Summary     string
	Explanation string
	Steps       []PlanStep
	IsSafe      bool
}

// Clone returns a deep copy of the diagnostic finding.
func (f DiagnosticFinding) Clone() DiagnosticFinding {
	cp := f
	if f.Evidence != nil {
		cp.Evidence = slices.Clone(f.Evidence)
	}
	if f.RepairOptions != nil {
		cp.RepairOptions = make([]RepairOption, len(f.RepairOptions))
		for i, ro := range f.RepairOptions {
			cp.RepairOptions[i] = ro.Clone()
		}
	}
	if f.ResolvedAt != nil {
		t := *f.ResolvedAt
		cp.ResolvedAt = &t
	}
	return cp
}

// Clone returns a deep copy of the repair option.
func (r RepairOption) Clone() RepairOption {
	cp := r
	if r.Steps != nil {
		cp.Steps = make([]PlanStep, len(r.Steps))
		for i, s := range r.Steps {
			cp.Steps[i] = s.Clone()
		}
	}
	return cp
}

// ProcessIdentity represents a managed process identity.
// Never trust a PID alone. Adoption requires every field to be populated.
type ProcessIdentity struct {
	PID            int
	StartTime      uint64
	ExecutablePath string
	CommandHash    string
}

// Complete reports whether the identity has all four fields populated.
func (p ProcessIdentity) Complete() bool {
	return p.PID > 0 && p.StartTime != 0 && p.ExecutablePath != "" && p.CommandHash != ""
}

// ProcessSpec specifies how to start a connector process.
type ProcessSpec struct {
	Executable string
	Args       []string
	Env        []string
	Dir        string
	StdoutPath string
	StderrPath string
	Restart    RestartPolicy
	Redactions []string
}

// RestartPolicy defines when to restart a process.
type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"
	RestartOnFailure RestartPolicy = "on_failure"
	RestartNever     RestartPolicy = "never"
)

// ConnectorProcessService manages connector subprocesses with
// identity verification, log persistence, and lifecycle control.
// All connector processes must go through this service — never
// directly through exec.Cmd or tunnel.ProcessConnector.
type ConnectorProcessService interface {
	// Start launches a connector process and returns a handle
	// for observation and control. The service takes ownership
	// of the process lifecycle.
	Start(ctx context.Context, cfg ProcessConfig) (ConnectorHandle, error)
	// Stop gracefully terminates a connector process by connection ID.
	// It sends SIGTERM, waits up to timeout, then SIGKILL if needed.
	Stop(connectionID ConnectionID, timeout time.Duration) error
	// Observe returns the current status of a managed connector process.
	// Returns nil, false if no process is tracked for the connection.
	Observe(connectionID ConnectionID) (ConnectorHandle, bool)
}

// ProcessConfig is the configuration for starting a managed process.
type ProcessConfig struct {
	ConnectionID ConnectionID
	Spec         ProcessSpec
}

// ConnectorHandle is a reference to a running connector process
// returned by ConnectorProcessService.Start.
type ConnectorHandle struct {
	ConnectionID ConnectionID
	Identity     ProcessIdentity
	PID          int
}
