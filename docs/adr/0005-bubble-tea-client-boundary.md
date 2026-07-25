# ADR 0005: Bubble Tea Client Boundary

## Status

Accepted

## Context

The TUI must not contain business logic, database access, or provider SDK calls.

## Decision

Bubble Tea is a projection layer only:
- Handles keyboard/mouse, window size, navigation
- Manages presentation state
- Renders views
- Controls animation frames

**NOT owned by Bubble Tea:**
- Connection lifecycle
- Provider state
- Persistent profiles
- Connector processes
- Reconciliation
- Long-running operation truth

**Update() message rules:**
- No anonymous maps or arbitrary strings
- Use typed messages only:
  - `SnapshotLoadedMsg`
  - `EventReceivedMsg`
  - `PlanCreatedMsg`
  - `OperationUpdatedMsg`
  - `SupervisorUnavailableMsg`

**View() rules:**
- Pure function - no I/O, no clock, no mutation
- Receives interpreted view models
- Returns terminal cells

## Consequences

- Clear separation of concerns
- Testable rendering logic
- Animation isolated to `internal/tui/animation`
