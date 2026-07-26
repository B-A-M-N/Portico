package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
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
// the credential reference alongside the step result. Secret buffers
// are zeroed by the controller after successful storage.
type CredentialMutation struct {
	TunnelID string
	Secret   []byte // opaque secret material
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
	Provider     ProviderID
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
}

// ProviderAccountBinding is an optional capability implemented by providers
// whose adapter instance is configured for one exact provider account. The
// controller uses it to reject a profile that selected a different account
// before planning, observing, or mutating remote infrastructure.
//
// An empty ID means the provider instance is not account-bound (for example,
// a Cloudflare Quick Tunnel adapter).
type ProviderAccountBinding interface {
	ProviderAccountID() ProviderAccountID
}

// AccountScopedProvider supplies an account-bound child adapter for a profile
// that selected one of several authenticated accounts. Controllers resolve the
// child before planning, executing, or observing, so an operation can never
// fall through to whichever account happened to be registered first.
type AccountScopedProvider interface {
	ProviderForAccount(accountID ProviderAccountID) (Provider, error)
}

// ResourceAwareObserver is an optional provider capability for authoritative
// observation after a supervisor restart. The durable resource inventory is
// supplied explicitly instead of relying on provider adapter memory.
type ResourceAwareObserver interface {
	ObserveWithResources(ctx context.Context, id ConnectionID, resources []ProviderResource) (*ObservedConnection, error)
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
	ConnectionID   ConnectionID
	ProviderID     ProviderID
	Tunnel         *ObservedTunnel
	DNSRecords     []ObservedDNSRecord
	AccessApps     []ObservedAccessApp
	AccessPolicies []ObservedAccessPolicy
	Connector      *ObservedConnector
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

// ObservedAccessPolicy is the exact policy state Portico needs to compare
// desired access protection without listing or adopting unrelated policies.
type ObservedAccessPolicy struct {
	ID              string
	AppID           string
	Decision        string
	AllowedEmails   []string
	AllowedDomains  []string
	SessionDuration string
}

// ObservedConnector describes an observed connector
type ObservedConnector struct {
	PID            int
	Status         string
	StartedAt      time.Time
	StartTime      uint64
	ExecutablePath string
	CommandHash    string
	Restarts       int
	LastError      string
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
		ConnectionID     ConnectionID
		ProviderID       ProviderID
		Tunnel           *ObservedTunnel
		Connector        *ObservedConnector
		DNSRecords       []ObservedDNSRecord
		AccessApps       []ObservedAccessApp
		AccessPolicies   []ObservedAccessPolicy
		ResourceStatuses []ObservedResourceStatus
	}
	dnsRecords := append([]ObservedDNSRecord(nil), o.DNSRecords...)
	sort.Slice(dnsRecords, func(i, j int) bool {
		if dnsRecords[i].ID != dnsRecords[j].ID {
			return dnsRecords[i].ID < dnsRecords[j].ID
		}
		return dnsRecords[i].Name < dnsRecords[j].Name
	})
	accessApps := append([]ObservedAccessApp(nil), o.AccessApps...)
	sort.Slice(accessApps, func(i, j int) bool { return accessApps[i].ID < accessApps[j].ID })
	accessPolicies := append([]ObservedAccessPolicy(nil), o.AccessPolicies...)
	for i := range accessPolicies {
		accessPolicies[i].AllowedEmails = append([]string(nil), accessPolicies[i].AllowedEmails...)
		accessPolicies[i].AllowedDomains = append([]string(nil), accessPolicies[i].AllowedDomains...)
		sort.Strings(accessPolicies[i].AllowedEmails)
		sort.Strings(accessPolicies[i].AllowedDomains)
	}
	sort.Slice(accessPolicies, func(i, j int) bool { return accessPolicies[i].ID < accessPolicies[j].ID })
	statuses := append([]ObservedResourceStatus(nil), o.ResourceStatuses...)
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].Type != statuses[j].Type {
			return statuses[i].Type < statuses[j].Type
		}
		return statuses[i].ExternalID < statuses[j].ExternalID
	})

	h := observedHash{
		ConnectionID:     o.ConnectionID,
		ProviderID:       o.ProviderID,
		Tunnel:           o.Tunnel,
		Connector:        o.Connector,
		DNSRecords:       dnsRecords,
		AccessApps:       accessApps,
		AccessPolicies:   accessPolicies,
		ResourceStatuses: statuses,
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
