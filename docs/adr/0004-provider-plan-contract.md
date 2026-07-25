# ADR 0004: Provider Interface Contract

## Status

Accepted

## Context

Multiple providers must be supported without scattering provider-name conditionals throughout the codebase.

## Decision

Provider interface:
```go
type Provider interface {
    Identity() ProviderIdentity
    Capabilities(context.Context) (Capabilities, error)

    Authenticate(context.Context, AuthRequest) error
    Plan(context.Context, DesiredConnection) (*OperationPlan, error)
    Apply(context.Context, OperationPlan) (<-chan Event, error)
    Observe(context.Context, ConnectionID) (*ObservedConnection, error)
    Repair(context.Context, RepairPlan) (<-chan Event, error)
    Remove(context.Context, RemovePlan) (<-chan Event, error)
}
```

Capabilities are structured, not booleans:
```go
type Capability[T any] struct {
    Supported   bool
    Mode       SupportMode
    Constraints T
    Reason     string
}
```

Provider adapters live in `internal/provider/<name>/`.

## Consequences

- Provider-specific logic isolated in adapters
- Capability-based decisions, not provider-name checks
- TUI never imports provider packages
