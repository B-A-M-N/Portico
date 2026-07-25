package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

// StepResult describes the outcome of executing a single plan step.
type StepResult struct {
	StepID              string
	Succeeded           bool
	Error               error
	Resources           []ProviderResource   // resources created/modified by this step
	CredentialMutations []CredentialMutation // credentials created/modified by this step
}

// CredentialMutation describes a credential that should be persisted
// after a successful step. The controller transactionally persists
// the credential reference alongside the step result.
type CredentialMutation struct {
	TunnelID string
	Token    string // opaque secret material
}

// StepExecutionResult tracks the execution state of a single step for recovery.
type StepExecutionResult struct {
	StepID            string
	StepKind          StepKind
	Status            string
	ProviderRequestID string
	Result            StepResult
	RecoveryRequired  bool
}
type LifecycleMark struct {
	ProviderID   ProviderID
	ResourceType ResourceType
	ExternalID   string
	NewLifecycle ResourceLifecycle
}

// StepCommitRequest describes the full result of executing a step
// that must be persisted atomically. (SPEC P0 atomic step commit)
type StepCommitRequest struct {
	OperationID  OperationID
	ConnectionID ConnectionID
	Step         PlanStep
	Result       StepResult
	Lifecycle    []LifecycleMark
	// RemovedAccessApps lists Access application external IDs whose
	// associated policies must be marked removed in the same transaction
	// (policy deletion cascades from application deletion).
	RemovedAccessApps []string
}

// Provider is the interface all providers must implement
type Provider interface {
	Identity() ProviderIdentity
	Capabilities(ctx context.Context) (Capabilities, error)

	Authenticate(ctx context.Context, req AuthRequest) error
	Plan(ctx context.Context, conn DesiredConnection) (*OperationPlan, error)

	// ExecuteStep executes a single plan step. The controller handles
	// sequencing, journaling, compensation, and terminal verification.
	ExecuteStep(ctx context.Context, connectionID ConnectionID, step PlanStep) (StepResult, error)

	Observe(ctx context.Context, id ConnectionID) (*ObservedConnection, error)

	// Legacy async execution — deprecated, to be removed after migration.
	Apply(ctx context.Context, plan OperationPlan) (<-chan Event, error)
	Repair(ctx context.Context, plan RepairPlan) (<-chan Event, error)
	Remove(ctx context.Context, plan RemovePlan) (<-chan Event, error)
}

// ResolvedOrigin describes a prepared local origin ready for provider consumption.
type ResolvedOrigin struct {
	URL      string
	Protocol Protocol
	Owned    bool
}

// DesiredConnection describes the desired connection state
type DesiredConnection struct {
	Profile *ConnectionProfile
	Runtime *ConnectionRuntime
	Origin  *ResolvedOrigin
}

// AuthRequest describes an authentication request
type AuthRequest struct {
	ProviderID    ProviderID
	AccountID     ProviderAccountID
	CredentialRef string
}

// ObservedConnection describes observed provider state
type ObservedConnection struct {
	ConnectionID ConnectionID
	ProviderID   ProviderID
	Tunnel       *ObservedTunnel
	DNSRecords   []ObservedDNSRecord
	AccessApps   []ObservedAccessApp
	Connector    *ObservedConnector
	// ResourceStatuses records the per-resource observation
	// classification for each persisted provider resource that was
	// queried by exact external ID. Observation never assigns ownership.
	ResourceStatuses []ObservedResourceStatus
}

// ObservationStatus classifies the authoritative result of observing a
// single provider resource by its exact external ID.
type ObservationStatus string

const (
	// ObservationPresent means the resource exists on the provider.
	ObservationPresent ObservationStatus = "present"
	// ObservationMissing means the provider authoritatively reported
	// the resource as not found (404).
	ObservationMissing ObservationStatus = "missing"
	// ObservationUnauthorized means the provider rejected the lookup
	// with 401/403 — resource existence could not be determined.
	ObservationUnauthorized ObservationStatus = "unauthorized"
	// ObservationTransient means the lookup failed for a retryable
	// reason (5xx, network, timeout). Never treated as missing.
	ObservationTransient ObservationStatus = "transient"
	// ObservationRateLimited means the provider rate-limited the
	// lookup (429). Never treated as missing.
	ObservationRateLimited ObservationStatus = "rate_limited"
)

// ObservedResourceStatus records the observation classification for a
// persisted provider resource.
type ObservedResourceStatus struct {
	Type       ResourceType
	ExternalID string
	Status     ObservationStatus
	Detail     string
}

// ObservedTunnel describes an observed tunnel
type ObservedTunnel struct {
	ID        string
	Name      string
	State     string
	CreatedAt time.Time
}

// ObservedDNSRecord describes an observed DNS record
type ObservedDNSRecord struct {
	ID     string
	Type   string
	Name   string
	Target string
}

// ObservedAccessApp describes an observed Access application
type ObservedAccessApp struct {
	ID       string
	Name     string
	Domain   string
	AuthMode string
}

// ObservedConnector describes an observed connector
type ObservedConnector struct {
	PID       int
	Status    string
	StartedAt time.Time
}

// ComputeFingerprint computes a canonical fingerprint of the observed
// provider state. This fingerprint is compared against the plan's
// ObservedFingerprint to detect stale plans when provider state has
// changed since the plan was previewed.
func (o *ObservedConnection) ComputeFingerprint() (string, error) {
	if o == nil {
		return "", nil
	}
	type observedHash struct {
		ConnectionID    ConnectionID
		ProviderID      ProviderID
		TunnelState     string
		ConnectorStatus string
		ConnectorPID    int
		DNSCount        int
		DNSNames        []string
		AccessAppCount  int
	}

	tunnelState := ""
	if o.Tunnel != nil {
		tunnelState = o.Tunnel.State
	}

	connectorStatus := ""
	connectorPID := 0
	if o.Connector != nil {
		connectorStatus = o.Connector.Status
		connectorPID = o.Connector.PID
	}

	dnsNames := make([]string, 0, len(o.DNSRecords))
	for _, r := range o.DNSRecords {
		dnsNames = append(dnsNames, r.Name)
	}

	h := observedHash{
		ConnectionID:    o.ConnectionID,
		ProviderID:      o.ProviderID,
		TunnelState:     tunnelState,
		ConnectorStatus: connectorStatus,
		ConnectorPID:    connectorPID,
		DNSCount:        len(o.DNSRecords),
		DNSNames:        dnsNames,
		AccessAppCount:  len(o.AccessApps),
	}

	data, err := json.Marshal(h)
	if err != nil {
		return "", err
	}

	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash), nil
}

// RepairPlan describes a repair plan
type RepairPlan struct {
	ID              PlanID
	ConnectionID    ConnectionID
	ProfileRevision uint64
	Provider        ProviderID
	RootCause       string
	Steps           []PlanStep
	Fingerprint     string
}

// RemovePlan describes a removal plan
type RemovePlan struct {
	ID           PlanID
	ConnectionID ConnectionID
	Provider     ProviderID
	Resources    []ResourceOwnership
	Steps        []PlanStep
	Fingerprint  string
}
