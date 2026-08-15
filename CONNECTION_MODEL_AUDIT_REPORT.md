# Portico Connection-Model Layer Audit Report

**Auditor:** Agent A — Connection-model auditor  
**Scope:** `internal/core`, `internal/ipc`, `internal/controller`, `internal/store`  
**Date:** 2026-08-14

---

## Executive Summary

The Portico codebase defines four `ConnectionKind` values but only `service_exposure` and `port_forward` work end-to-end. `private_network` and `client_tunnel` have core types and DTOs but **lack creation arms in the IPC `CreateConnectionRequest`**, and the supervisor explicitly refuses them. Several components still assume service-exposure semantics.

---

## 1. ConnectionKind Trace Through `internal/core/connection.go`

| Kind | Constant | Spec Arm | Validation | DeepCopy | EffectiveKind |
|------|----------|----------|------------|----------|---------------|
| `service_exposure` | Line 15 | `ServiceExposure` (L50) | `validateServiceExposureSpec` (L436) | Lines 269-327 | Works |
| `port_forward` | Line 18 | `PortForward` (L51) | `validatePortForwardSpec` (L636) | Lines 329-332 | Works |
| `private_network` | Line 21 | `PrivateNetwork` (L52) | `validatePrivateNetworkSpec` (L712) | Lines 334-337 | Works |
| `client_tunnel` | Line 28 | `ClientTunnel` (L53) | `validateClientTunnelSpec` (L671) | Lines 339-356 | Works |

**Key functions:**
- `SpecArmKind()` (L144-162): Correctly extracts kind from populated spec arm
- `EffectiveKind()` (L172-183): Falls back to `service_exposure` when spec empty (L174, L182)
- `Validate()` (L372-433): Enforces exactly one arm + Kind match

---

## 2. IPC DTO Gap Analysis (`internal/ipc/dto.go`)

### What Exists
| DTO | Lines | Status |
|-----|-------|--------|
| `ConnectionSpecDTO` | 78-90 | �� All four arms |
| `PrivateNetworkSpecDTO` | 100-104 | �� Complete |
| `ClientTunnelSpecDTO` | 106-112 | �� Complete (but missing MCP field - see below) |
| `PortForwardDTO` | 453-459 | �� Complete |

### Missing in `CreateConnectionRequest` (Lines 438-450)
```go
type CreateConnectionRequest struct {
    Version     int                   `json:"version"`
    Name        string                `json:"name"`
    Kind        string                `json:"kind,omitempty"`       // Only used for service_exposure/port_forward
    Source      SourceDTO             `json:"source"`               // Service-exposure only
    Exposure    ExposureDTO           `json:"exposure"`             // Service-exposure only
    Protection  ProtectionDTO         `json:"protection"`           // Service-exposure only
    Provider    ProviderSelectionDTO  `json:"provider"`
    Lifecycle   LifecycleDTO          `json:"lifecycle"`
    PortForward *PortForwardDTO       `json:"port_forward,omitempty"`  // Only non-service-exposure arm
    // MISSING: PrivateNetwork *PrivateNetworkSpecDTO
    // MISSING: ClientTunnel *ClientTunnelSpecDTO
}
```

### Missing in `UpdateConnectionRequest` (Line 534-540)
```go
type UpdateConnectionRequest struct {
    ExpectedRevision uint64                  `json:"expected_revision,omitempty"`
    Name             *string                 `json:"name,omitempty"`
    Spec             *ServiceExposureSpecDTO `json:"spec,omitempty"`  // ONLY service_exposure
    Driver           *DriverSelectionDTO     `json:"driver,omitempty"`
    Lifecycle        *LifecycleDTO           `json:"lifecycle,omitempty"`
}
```

### ClientTunnelSpecDTO Field Gap
Line 107-112: Missing `MCP` field that exists in core `ClientTunnelSpec` (L68-77):
```go
// DTO - MISSING MCP
type ClientTunnelSpecDTO struct {
    Client   string       `json:"client"`
    TunnelID string       `json:"tunnel_id,omitempty"`
    Profile  string       `json:"profile,omitempty"`
    MCP      MCPSourceDTO `json:"mcp"`  // MISSING
}
```

---

## 3. Exact Tagged-Union Changes Required for `CreateConnectionRequest`

### File: `internal/ipc/dto.go`

**Add to `CreateConnectionRequest` (after line 449):**
```go
// Add these two fields:
PrivateNetwork *PrivateNetworkSpecDTO `json:"private_network,omitempty"`
ClientTunnel   *ClientTunnelSpecDTO   `json:"client_tunnel,omitempty"`
```

**Fix `ClientTunnelSpecDTO` (line 107-112):**
```go
type ClientTunnelSpecDTO struct {
    Client   string       `json:"client"`
    TunnelID string       `json:"tunnel_id,omitempty"`
    Profile  string       `json:"profile,omitempty"`
    MCP      MCPSourceDTO `json:"mcp"`  // ADD THIS
}
```

**Update `UpdateConnectionRequest` (line 537):**
```go
// Replace single Spec with tagged union:
Spec *ConnectionSpecDTO `json:"spec,omitempty"`
```

---

## 4. Persistence/Reconstruction Verification (`internal/store/sqlite.go`)

| Operation | Function | Lines | Status |
|-----------|----------|-------|--------|
| Encode | `encodeProfileSpec` | 1419-1439 | �� Uses `SpecArmKind()` |
| Decode | `decodeProfileSpec` | 1443-1460 | �� Validates kind matches arm |
| Create | `CreateConnection` | 1290-1402 | �� Calls `encodeProfileSpec` |
| Save | `SaveProfile` | 1463-1499 | �� Calls `encodeProfileSpec` |
| Load | `LoadProfile` / `loadProfileLocked` | 1502-1634 | �� Calls `decodeProfileSpec` |
| List | `ListProfiles` | 1637-1669 | �� Uses `loadProfileLocked` |

**Verdict:** Persistence layer **correctly handles all four kinds** — no gaps.

---

## 5. Service-Exposure Assumptions (Hardcoded/Implicit)

### A. Core Layer (`internal/core/`)

| File | Line | Issue |
|------|------|-------|
| `capability.go` | 234 | `Executes()` defaults to `service_exposure` when provider declares no kinds |
| `connection.go` | 174, 182 | `EffectiveKind()` falls back to `service_exposure` twice |
| `connection.go` | 192-211 | `GetSource()`, `GetExposure()`, `GetProtection()` return zero values for non-service-exposure |
| `connection.go` | 234-238 | `IsProtected()` assumes `ServiceExposure` exists |
| `connection.go` | 250-258 | `ExpectsPublicAddress()` assumes `ServiceExposure` exists |

### B. Supervisor (`internal/supervisor/supervisor.go`)

| File | Line | Issue |
|------|------|-------|
| `supervisor.go` | 805 | `HandleCreateConnection` handles `""` and `service_exposure` together |
| `supervisor.go` | 809-812 | `private_network` → explicit "not implemented" error |
| `supervisor.go` | 813-815 | `client_tunnel` → "created through provider's setup flow" error |
| `supervisor.go` | 1225-1235 | `HandleCloneConnection` assumes `ServiceExposure` for permanent hostname check |
| `supervisor.go` | 1344-1346 | `applyEditRequest` refuses edits for non-service-exposure: `"only service exposure connections can have their spec edited"` |

### C. Controller (`internal/controller/`)

| File | Line | Issue |
|------|------|-------|
| `recommendation.go` | 138-154 | Exposure mode constraints assume service_exposure semantics (temporary/permanent/private) |
| `recommendation.go` | 187-192 | SSE + temporary address constraint assumes service_exposure source types |

### D. TUI Wizard (`internal/tui/screens/wizard.go`)

| File | Line | Issue |
|------|------|-------|
| `wizard.go` | 969 | `buildRequest()` hardcodes `Kind: "service_exposure"` |

### E. CLI (`internal/cli/handler.go`)

| File | Line | Issue |
|------|------|-------|
| `handler.go` | 914-924 | `createConnection()` doesn't set `Kind` at all (relies on supervisor default) |

### F. Provider Adapters (Multiple)

| File | Line | Issue |
|------|------|-------|
| `ngrok/adapter_test.go` | 152 | Test profiles use `ConnectionServiceExposure` |
| `cloudflare/adapter_test.go` | 489 | Test profiles use `ConnectionServiceExposure` |
| `openaitunnel/adapter_test.go` | 285 | Test mutates Kind to `ConnectionServiceExposure` |

---

## 6. Required Changes Summary

### Priority 1: Enable PrivateNetwork & ClientTunnel Creation

1. **`internal/ipc/dto.go`** — Add `PrivateNetwork` and `ClientTunnel` arms to `CreateConnectionRequest`
2. **`internal/ipc/dto.go`** — Add `MCP` field to `ClientTunnelSpecDTO`
3. **`internal/ipc/dto.go`** — Change `UpdateConnectionRequest.Spec` to `*ConnectionSpecDTO`
4. **`internal/supervisor/supervisor.go`** — Implement `createPrivateNetwork()` and `createClientTunnel()` handlers
5. **`internal/tui/screens/wizard.go`** — Add wizard steps for new kinds, remove hardcoded Kind
6. **`internal/cli/handler.go`** — Add flags for new kinds

### Priority 2: Remove Service-Exposure Assumptions

1. **`internal/core/connection.go`** — Fix `EffectiveKind()` to not default to service_exposure
2. **`internal/core/connection.go`** — Make `GetSource/Exposure/Protection` panic or return error for wrong kind
3. **`internal/core/capability.go`** — Remove service_exposure default in `Executes()`
4. **`internal/supervisor/supervisor.go`** — Fix `HandleCloneConnection` and `applyEditRequest` to handle all kinds
5. **`internal/controller/recommendation.go`** — Make exposure-mode constraints kind-aware

### Priority 3: Testing

- Add round-trip tests for all four kinds in `sqlite_test.go`
- Add `TestClientTunnelProfileRoundTrips` (referenced in AUDIT_ACCEPTANCE_MATRIX.md:29)

---

## 7. File:Line Citation Index

| Finding | File | Line(s) |
|---------|------|---------|
| ConnectionKind constants | `internal/core/connection.go` | 13-29 |
| ConnectionSpec union | `internal/core/connection.go` | 49-54 |
| SpecArmKind() | `internal/core/connection.go` | 144-162 |
| EffectiveKind() default | `internal/core/connection.go` | 174, 182 |
| GetSource/Exposure/Protection | `internal/core/connection.go` | 192-211 |
| IsProtected/ExpectsPublicAddress | `internal/core/connection.go` | 234-258 |
| CreateConnectionRequest | `internal/ipc/dto.go` | 438-450 |
| PrivateNetworkSpecDTO | `internal/ipc/dto.go` | 100-104 |
| ClientTunnelSpecDTO (missing MCP) | `internal/ipc/dto.go` | 107-112 |
| UpdateConnectionRequest | `internal/ipc/dto.go` | 534-540 |
| HandleCreateConnection switch | `internal/supervisor/supervisor.go` | 804-818 |
| createPortForward | `internal/supervisor/supervisor.go` | 1135-1189 |
| applyEditRequest restriction | `internal/supervisor/supervisor.go` | 1344-1346 |
| HandleCloneConnection assumption | `internal/supervisor/supervisor.go` | 1225-1235 |
| wizard buildRequest hardcode | `internal/tui/screens/wizard.go` | 969 |
| CLI createConnection no Kind | `internal/cli/handler.go` | 914-924 |
| capability.go default | `internal/core/capability.go` | 234 |
| Persistence encode/decode | `internal/store/sqlite.go` | 1419-1460 |