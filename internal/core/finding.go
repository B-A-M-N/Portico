// Package core contains pure domain types used across Portico.
// It has no non-standard-library imports except google/uuid.
package core

import (
	"time"

	"github.com/google/uuid"
)

// --------------- ID types ---------------

// StepID identifies a plan step.
type StepID string

// SegmentID identifies a route segment for diagnostics.
type SegmentID = RouteSegmentID

// FindingID is a unique identifier for a diagnostic finding.
type FindingID string

// NewFindingID creates a new finding ID.
func NewFindingID() FindingID {
	return FindingID(uuid.New().String())
}

// --------------- Provider account ---------------

// ProviderAccount represents a stored provider account reference.
// Tokens are never stored here — only credential references.
type ProviderAccount struct {
	ID            ProviderAccountID
	Provider      ProviderID
	Label         string
	CredentialRef string
	Metadata      map[string]string
	Status        ProviderAccountStatus
}

// ProviderAccountStatus describes an account's authentication state.
type ProviderAccountStatus string

const (
	AccountAuthenticated ProviderAccountStatus = "authenticated"
	AccountPending       ProviderAccountStatus = "pending"
	AccountExpired       ProviderAccountStatus = "expired"
	AccountRevoked       ProviderAccountStatus = "revoked"
	// AccountProvisional marks a credential Portico checked only for shape —
	// not against the provider. It exists because some credentials cannot be
	// confirmed without first using them: a client-tunnel runtime key proves
	// itself only when the client reaches the control plane. Recording such a
	// credential as pending would make it permanently unselectable (activation
	// refuses pending accounts), and recording it as authenticated would claim
	// a verification that never happened. Provisional means: usable now,
	// authoritative verdict pending at runtime. The runtime verdict promotes
	// it to authenticated or demotes it to pending, per connection health.
	AccountProvisional ProviderAccountStatus = "provisional"
)

// --------------- Diagnostics types ---------------

// Finding represents a diagnostic finding attached to a route segment.
// This is the primary diagnostic type (SPEC §16.3).
type Finding struct {
	ID            FindingID
	ConnectionID  ConnectionID
	Segment       SegmentID
	Code          string
	Summary       string
	Explanation   string
	Evidence      []Evidence
	Confidence    Confidence
	Repairability Repairability
	SuggestedPlan *RepairPlan
}

// Repairability describes how repairable a finding is.
type Repairability string

const (
	RepairableAuto   Repairability = "auto"
	RepairableManual Repairability = "manual"
	RepairableNone   Repairability = "none"
)

// Severity describes the severity of a diagnostic finding.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// Confidence describes confidence in a diagnostic or discovery result.
type Confidence string

const (
	ConfidenceVeryLikely Confidence = "very_likely"
	ConfidenceLikely     Confidence = "likely"
	ConfidencePossible   Confidence = "possible"
)

// ProbeResult is the result of probing a single route segment (SPEC §16.2).
type ProbeResult struct {
	Segment    SegmentID
	Status     ProbeStatus
	Evidence   []Evidence
	StartedAt  time.Time
	FinishedAt time.Time
	Error      *PorticoError
}

// ProbeStatus describes the status of a segment probe.
type ProbeStatus string

const (
	ProbePass    ProbeStatus = "pass"
	ProbeFail    ProbeStatus = "fail"
	ProbeUnknown ProbeStatus = "unknown"
	ProbeSkipped ProbeStatus = "skipped"
)

// SegmentHealth describes health per route segment.
type SegmentHealth map[SegmentID]ProbeStatus

// --------------- Evidence ---------------

// Evidence supports a diagnostic or discovery finding.
type Evidence struct {
	Type    string
	Source  string
	Message string
	Data    map[string]string
}
