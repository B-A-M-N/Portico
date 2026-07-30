package core

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// DeepCopy returns a full independent copy of the plan.
func (p *OperationPlan) DeepCopy() *OperationPlan {
	if p == nil {
		return nil
	}
	cp := *p
	// Deep copy steps with all nested fields
	cp.Steps = make([]PlanStep, len(p.Steps))
	for i, step := range p.Steps {
		cp.Steps[i] = step.Clone()
	}
	if p.Warnings != nil {
		cp.Warnings = make([]PlanWarning, len(p.Warnings))
		copy(cp.Warnings, p.Warnings)
	}
	if p.Preconditions != nil {
		cp.Preconditions = make([]Precondition, len(p.Preconditions))
		for i, pc := range p.Preconditions {
			cp.Preconditions[i] = pc.Clone()
		}
	}
	if p.Expected.Resources != nil {
		cp.Expected.Resources = make([]ResourceSummary, len(p.Expected.Resources))
		copy(cp.Expected.Resources, p.Expected.Resources)
	}
	return &cp
}

// Clone returns a deep copy of the plan step.
func (s PlanStep) Clone() PlanStep {
	cp := s
	if s.Technical.Parameters != nil {
		cp.Technical.Parameters = maps.Clone(s.Technical.Parameters)
	}
	if s.Compensation != nil {
		comp := s.Compensation.Clone()
		cp.Compensation = &comp
	}
	return cp
}

// Clone returns a deep copy of the compensation step.
func (c CompensationStep) Clone() CompensationStep {
	cp := c
	if c.Technical.Parameters != nil {
		cp.Technical.Parameters = maps.Clone(c.Technical.Parameters)
	}
	return cp
}

// Clone returns a deep copy of the precondition.
func (p Precondition) Clone() Precondition {
	cp := p
	if p.Checks != nil {
		cp.Checks = slices.Clone(p.Checks)
	}
	if p.Resources != nil {
		cp.Resources = slices.Clone(p.Resources)
	}
	return cp
}

// OperationIntent describes the intent of an operation
type OperationIntent string

const (
	IntentOpen   OperationIntent = "open"
	IntentClose  OperationIntent = "close"
	IntentRepair OperationIntent = "repair"
	IntentDelete OperationIntent = "delete"
	// IntentEdit changes a connection's desired state and reconciles the
	// provider resources the change invalidates, in one operation.
	IntentEdit OperationIntent = "edit"
)

// OperationPlan represents an immutable operation plan
type OperationPlan struct {
	ID                  PlanID
	ConnectionID        ConnectionID
	ProfileRevision     uint64
	Provider            ProviderID
	Intent              OperationIntent
	Steps               []PlanStep
	Warnings            []PlanWarning
	Expected            ExpectedOutcome
	Preconditions       []Precondition
	Fingerprint         string
	ObservedFingerprint string
	CreatedAt           time.Time
	ExpiresAt           time.Time
}

// PlanStep represents a single step in a plan
type PlanStep struct {
	ID           string
	Kind         StepKind
	Summary      string
	Technical    TechnicalOperation
	Destructive  bool
	Irreversible bool
	Sensitive    bool
	Compensation *CompensationStep
	Ownership    ResourceOwnership
}

// StepKind identifies the type of step
type StepKind string

const (
	StepValidateAccount    StepKind = "validate_account"
	StepCreateTunnel       StepKind = "create_tunnel"
	StepConfigureRoute     StepKind = "configure_route"
	StepCreateDNSRecord    StepKind = "create_dns_record"
	StepCreateAccessApp    StepKind = "create_access_application"
	StepCreateAccessPolicy StepKind = "create_access_policy"
	StepStartConnector     StepKind = "start_connector"
	// StepStartOrigin starts a local service owned by Portico before the
	// provider connector is started. It is deliberately separate from the
	// connector so plans show both lifecycles and can compensate safely.
	StepStartOrigin StepKind = "start_origin"
	// StepVerifyOrigin probes the resolved local origin before any provider
	// resource is created. Provider mutations are externally visible and
	// outlive a failed operation, so a dead origin must stop the plan before
	// a tunnel, DNS record or Access policy is published for it.
	StepVerifyOrigin          StepKind = "verify_origin"
	StepVerifyConnector       StepKind = "verify_connector"
	StepVerifyEndpoint        StepKind = "verify_endpoint"
	StepStopConnector         StepKind = "stop_connector"
	StepStopOrigin            StepKind = "stop_origin"
	StepDeleteTunnel          StepKind = "delete_tunnel"
	StepDeleteDNSRecord       StepKind = "delete_dns_record"
	StepDeleteAccessApp       StepKind = "delete_access_application"
	StepDeleteAccessPolicy    StepKind = "delete_access_policy"
	StepRestartConnector      StepKind = "restart_connector"
	StepUpdateRoute           StepKind = "update_route"
	StepUpdateDNSRecord       StepKind = "update_dns_record"
	StepUpdateAccessApp       StepKind = "update_access_application"
	StepUpdateAccessPolicy    StepKind = "update_access_policy"
	StepRecreateTunnel        StepKind = "recreate_tunnel"
	StepFinalizeLocalDeletion StepKind = "finalize_local_deletion"
	// StepApplyProfile commits an edited profile. It is the commit boundary of
	// an edit: every step before it operates on the previous profile, which
	// stays readable until this step succeeds.
	StepApplyProfile StepKind = "apply_profile"
)

// TechnicalOperation describes the technical operation details
type TechnicalOperation struct {
	Provider   ProviderID
	Type       string
	Parameters map[string]string
	ResourceID string
}

// CompensationStep describes how to roll back a step.
// It is inline executable — no dangling references.
type CompensationStep struct {
	ID        string
	Kind      StepKind
	Technical TechnicalOperation
}

// ResourceOwnership describes who owns the resource.
type ResourceOwnership string

const (
	OwnershipManaged  ResourceOwnership = "managed"
	OwnershipAdopted  ResourceOwnership = "adopted"
	OwnershipExternal ResourceOwnership = "external"
)

// ResourceLifecycle describes whether a resource is present or has been removed.
type ResourceLifecycle string

const (
	LifecyclePresent        ResourceLifecycle = "present"
	LifecycleRemovalPending ResourceLifecycle = "removal_pending"
	LifecycleRemoved        ResourceLifecycle = "removed"
	LifecycleRemovalFailed  ResourceLifecycle = "removal_failed"
	LifecycleOrphaned       ResourceLifecycle = "orphaned"
	// LifecycleExternallyRemoved records a managed resource that an
	// authoritative provider lookup confirmed was deleted outside Portico.
	// It is distinct from LifecycleRemoved, which records a Portico-owned
	// deletion operation. Both are terminal remote-absence states.
	LifecycleExternallyRemoved ResourceLifecycle = "externally_removed"
)

// PlanWarning represents a warning about the plan
type PlanWarning struct {
	Code    string
	Message string
}

// ExpectedOutcome describes the expected result of the plan
type ExpectedOutcome struct {
	State          RuntimeState
	PublicAddress  string
	PrivateAddress string
	Resources      []ResourceSummary
}

// ResourceSummary summarizes a managed resource
type ResourceSummary struct {
	Type       string
	ExternalID string
	Ownership  ResourceOwnership
	Lifecycle  ResourceLifecycle
}

// Precondition describes a precondition for the plan
type Precondition struct {
	Type      string
	Checks    []string
	Resources []string
}

// ComputeFingerprint computes the canonical fingerprint of the plan.
// Includes every field that changes what will execute or what success means.
// ComputeFingerprint computes and stores the fingerprint.
// Excludes only the plan ID and fingerprint itself.
// Steps are hashed in their original execution order — reordering steps
// produces a different fingerprint.
func (p *OperationPlan) ComputeFingerprint() error {
	computed, err := p.computeFingerprint()
	if err != nil {
		return err
	}
	p.Fingerprint = computed
	return nil
}

// computeFingerprint computes the fingerprint without storing it.
func (p *OperationPlan) computeFingerprint() (string, error) {
	type planHash struct {
		ConnectionID        ConnectionID
		ProfileRevision     uint64
		Provider            ProviderID
		Intent              OperationIntent
		Steps               []PlanStep
		Warnings            []PlanWarning
		Expected            ExpectedOutcome
		Preconditions       []Precondition
		ObservedFingerprint string
		ExpiresAt           time.Time
	}

	h := planHash{
		ConnectionID:        p.ConnectionID,
		ProfileRevision:     p.ProfileRevision,
		Provider:            p.Provider,
		Intent:              p.Intent,
		Steps:               p.Steps,
		Warnings:            p.Warnings,
		Expected:            p.Expected,
		Preconditions:       p.Preconditions,
		ObservedFingerprint: p.ObservedFingerprint,
		ExpiresAt:           p.ExpiresAt,
	}

	data, err := json.Marshal(h)
	if err != nil {
		return "", err
	}

	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash), nil
}

// VerifyFingerprint recomputes the fingerprint and compares it with the stored value.
// This is a pure check - it does not mutate the plan's fingerprint.
func (p *OperationPlan) VerifyFingerprint() error {
	saved := p.Fingerprint
	computed, err := p.computeFingerprint()
	if err != nil {
		return err
	}
	if computed != saved {
		return fmt.Errorf("fingerprint mismatch: plan has been modified")
	}
	return nil
}

// Validate checks that the plan is valid and can be applied.
func (p *OperationPlan) Validate() error {
	if p == nil {
		return fmt.Errorf("plan is nil")
	}

	if p.ID == "" {
		return fmt.Errorf("plan ID is required")
	}

	if p.ConnectionID == "" {
		return fmt.Errorf("connection ID is required")
	}

	if p.Provider == "" {
		return fmt.Errorf("provider is required")
	}

	if len(p.Steps) == 0 {
		return fmt.Errorf("plan must have at least one step")
	}

	// Validate fingerprint
	if p.Fingerprint == "" {
		return fmt.Errorf("plan fingerprint is required")
	}

	// Validate steps
	stepIDs := make(map[string]bool, len(p.Steps))
	for i, step := range p.Steps {
		if step.ID == "" {
			return fmt.Errorf("step %d has no ID", i)
		}
		if stepIDs[step.ID] {
			return fmt.Errorf("duplicate step ID: %s", step.ID)
		}
		stepIDs[step.ID] = true
		if step.Kind == "" {
			return fmt.Errorf("step %s has no kind", step.ID)
		}
		// Verify technical provider matches plan provider
		if step.Technical.Provider != "" && step.Technical.Provider != p.Provider {
			return fmt.Errorf("step %s technical provider %q does not match plan provider %q", step.ID, step.Technical.Provider, p.Provider)
		}
		// Verify compensation has an ID
		if step.Compensation != nil {
			if step.Compensation.ID == "" {
				return fmt.Errorf("step %s compensation has no ID", step.ID)
			}
			if step.Compensation.Kind == "" {
				return fmt.Errorf("step %s compensation has no kind", step.ID)
			}
		}
		// Check for secrets in technical parameters
		for key := range step.Technical.Parameters {
			if isSecretParameter(key) {
				return fmt.Errorf("step %s contains secret parameter %s (use references instead)", step.ID, key)
			}
		}
	}

	// Verify expected state matches intent
	switch p.Intent {
	case IntentOpen:
		if p.Expected.State != RuntimeOpen && p.Expected.State != "" {
			return fmt.Errorf("open plan expected state %q does not match intent", p.Expected.State)
		}
	case IntentClose:
		if p.Expected.State != RuntimeClosed && p.Expected.State != "" {
			return fmt.Errorf("close plan expected state %q does not match intent", p.Expected.State)
		}
	}

	// Validate expiry
	if !p.ExpiresAt.IsZero() && p.ExpiresAt.Before(p.CreatedAt) {
		return fmt.Errorf("plan expires before it was created")
	}

	// Validate preconditions
	for _, pc := range p.Preconditions {
		if pc.Type == "" {
			return fmt.Errorf("precondition has no type")
		}
	}

	return nil
}

// isSecretParameter returns true if the parameter name suggests a secret.
// Uses substring matching only for high-specificity patterns to avoid
// false positives on Cloudflare parameters like "auth_mode".
func isSecretParameter(name string) bool {
	lower := strings.ToLower(name)
	// Only match exact or suffix patterns for "key", "secret", "token"
	// to avoid flagging "auth_mode", "public_key_id", etc.
	if strings.HasSuffix(lower, "key") && lower != "public_key_id" && lower != "idempotency_key" {
		return true
	}
	if strings.HasSuffix(lower, "secret") {
		return true
	}
	if strings.HasSuffix(lower, "token") {
		return true
	}
	if strings.HasSuffix(lower, "password") {
		return true
	}
	if strings.Contains(lower, "credential") {
		return true
	}
	return false
}
