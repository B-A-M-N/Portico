package core

import "fmt"

// PorticoError represents a structured error with a stable error code.
// User-facing messages are generated from codes plus evidence, not raw provider errors.
type PorticoError struct {
	Code      string
	Message   string
	Technical string
	Retryable bool
	Segment   RouteSegmentID
	Provider  ProviderID
	Cause     error
}

// Error implements the error interface.
func (e *PorticoError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying cause.
func (e *PorticoError) Unwrap() error {
	return e.Cause
}

// NewPorticoError creates a new Portico error with a code and message.
func NewPorticoError(code, message string) *PorticoError {
	return &PorticoError{
		Code:    code,
		Message: message,
	}
}

// RouteSegmentID identifies a segment in the connection route graph.
type RouteSegmentID string

const (
	SegmentLocalService RouteSegmentID = "local_service"
	SegmentLocalRoute   RouteSegmentID = "local_route"
	SegmentConnector    RouteSegmentID = "connector"
	SegmentProviderEdge RouteSegmentID = "provider_edge"
	SegmentAddress      RouteSegmentID = "address"
	SegmentProtection   RouteSegmentID = "protection"
	SegmentEndpoint     RouteSegmentID = "endpoint"
)

// Error code families (SPEC §30.2)
const (
	ErrCorePrefix  = "PTO-CORE-"
	ErrIPCPrefix   = "PTO-IPC-"
	ErrStorePrefix = "PTO-STORE-"
	ErrProcPrefix  = "PTO-PROC-"
	ErrDiscPrefix  = "PTO-DISC-"
	ErrDiagPrefix  = "PTO-DIAG-"
	ErrCFPrefix    = "PTO-CF-"
	ErrNgrokPrefix = "PTO-NGROK-"
	ErrTSPrefix    = "PTO-TS-"
	ErrZrokPrefix  = "PTO-ZROK-"
)

// Common error constructors
func ErrProfileNotFound(id ConnectionID) *PorticoError {
	return &PorticoError{
		Code:    ErrCorePrefix + "001",
		Message: fmt.Sprintf("Profile not found: %s", id),
	}
}

func ErrProviderNotFound(id ProviderID) *PorticoError {
	return &PorticoError{
		Code:    ErrCorePrefix + "002",
		Message: fmt.Sprintf("Provider not found: %s", id),
	}
}

func ErrPlanNotFound(id PlanID) *PorticoError {
	return &PorticoError{
		Code:    ErrCorePrefix + "003",
		Message: fmt.Sprintf("Plan not found: %s", id),
	}
}

func ErrRevisionMismatch(plan, current uint64) *PorticoError {
	return &PorticoError{
		Code:      ErrCorePrefix + "004",
		Message:   fmt.Sprintf("Profile revision mismatch: plan=%d, current=%d", plan, current),
		Retryable: true,
	}
}

func ErrOperationNotFound(id OperationID) *PorticoError {
	return &PorticoError{
		Code:    ErrCorePrefix + "005",
		Message: fmt.Sprintf("Operation not found: %s", id),
	}
}

func ErrConnectionExists(id ConnectionID) *PorticoError {
	return &PorticoError{
		Code:    ErrCorePrefix + "006",
		Message: fmt.Sprintf("Connection already exists: %s", id),
	}
}

func ErrInvalidState(id ConnectionID, state RuntimeState) *PorticoError {
	return &PorticoError{
		Code:    ErrCorePrefix + "007",
		Message: fmt.Sprintf("Invalid state for operation: connection=%s state=%s", id, state),
	}
}

func ErrStalePlan(id PlanID, message string) *PorticoError {
	return &PorticoError{
		Code:      ErrCorePrefix + "008",
		Message:   fmt.Sprintf("Plan %s is stale: %s", id, message),
		Retryable: true,
	}
}

// ErrValidation reports input that cannot form a valid Portico domain object.
// Keeping this typed lets IPC return a stable client error without inspecting
// implementation-specific validation text.
func ErrValidation(message string) *PorticoError {
	return &PorticoError{
		Code:    ErrCorePrefix + "009",
		Message: message,
	}
}

// ErrProviderAccountUnavailable reports a profile selecting an account that
// the registered provider instance cannot safely serve.
func ErrProviderAccountUnavailable(provider ProviderID, account ProviderAccountID) *PorticoError {
	return &PorticoError{
		Code:     ErrCorePrefix + "010",
		Message:  fmt.Sprintf("Provider account is unavailable: provider=%s account=%s", provider, account),
		Provider: provider,
	}
}
