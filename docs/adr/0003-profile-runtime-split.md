# ADR 0003: Profile/Runtime Separation

## Status

Accepted

## Context

Desired configuration and observed runtime state must remain separate.

## Decision

**Profile** (desired state):
```go
type ConnectionProfile struct {
    ID          ConnectionID
    Name        string
    Revision    uint64
    Source      SourceSpec
    Exposure    ExposureSpec
    Protection  ProtectionSpec
    Provider    ProviderSelection
    Lifecycle   LifecycleSpec
    Desired     DesiredConnectionState
}
```

**Runtime** (observed state):
```go
type ConnectionRuntime struct {
    ConnectionID     ConnectionID
    State            RuntimeState
    ProviderID       ProviderID
    PublicAddress    string
    PrivateAddress   string
    Connector        ConnectorRuntime
    Resources        []ProviderResource
    SegmentHealth    SegmentHealth
    Error            *PorticoError
}
```

A profile can exist while closed. Runtime can disappear and be reconstructed. Provider observation must never overwrite user's desired profile.

## Consequences

- Clear ownership of what user controls vs what system observes
- Enables repair without destroying user intent
- Supports "close but remember" pattern
