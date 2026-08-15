# Portico Gateway Integration Design Document

**Status:** Implementation contract  
**Created:** 2026-08-15  

## 1. Purpose

Integrate the Portico Gateway (local HTTP proxy) into the supervisor's connection lifecycle so that connections which need auth/SSE transparency get an automatic gateway that:

- Starts after the transport tunnel is established (connector running)
- Provides a local listen address for clients
- Authenticates clients via Bearer tokens
- Proxies requests transparently through the tunnel
- Stops when the connection is torn down

## 2. Architecture

### 2.1 Gateway lifecycle in supervisor

```
                   ┌──────────────────────────┐
                   │       Supervisor          │
                   │                          │
                   │  ┌────────────────────┐  │
  PlanOpen ──────► │  │    Controller      │  │
                   │  │  execPlan()        │  │
                   │  │    ↓               │  │
                   │  │  StepStartConnector│  │
                   │  │    ↓               │  │
                   │  │  handleProcessEvent│  │
                   │  │  (connector running)│  │
                   │  └────────────────────┘  │
                   │            │              │
                   │            ▼              │
                   │  ┌────────────────────┐  │
                   │  │   Gateway Manager  │  │
                   │  │  StartGateway()    │──┼──► Portico Gateway (HTTP proxy)
                   │  │  StopGateway()     │  │      - Bearer auth
                   │  └────────────────────┘  │      - SSE transparent
                   │            │              │      - upstream = tunnel
                   │            ▼              │
                   │  rt.Gateway.Endpoint     │
                   │  (listen address stored) │
                   └──────────────────────────┘
```

### 2.2 Component responsibilities

| Component | Responsibility |
|-----------|---------------|
| `core.ConnectionRuntime` | Stores `Gateway.Endpoint` (listen URL, tokens) |
| `internal/supervisor/gateway.go` | Manages gateway process lifecycle |
| `internal/supervisor/supervisor.go` | Wires gateway start/stop into connection lifecycle |
| `internal/controller/controller.go` | Passes gateway config through plan execution |

### 2.3 Integration points

1. **Connection established**: After `StepStartConnector` succeeds and the connector reports `running`, the supervisor starts the gateway.
2. **Runtime update**: The gateway's listen address and auth tokens are stored in `ConnectionRuntime.Gateway`.
3. **Connection closing**: Before `StepStopConnector`, the supervisor stops the gateway.

## 3. Data structures

### 3.1 Changes to `core.ConnectionRuntime`

```go
// GatewayRuntime represents the runtime state of the Portico Gateway.
type GatewayRuntime struct {
    Endpoint   string   // local listen URL (http://127.0.0.1:PORT)
    Upstream   string   // tunnel endpoint being proxied
    AuthTokens []string // Bearer tokens accepted by the gateway
    StartedAt  time.Time
}
```

### 3.2 Supervisor gateway manager

```go
// gatewayManager owns gateway instances per connection.
// Each gateway is an HTTP proxy that sits between clients and the tunnel.
type gatewayManager struct {
    mu       sync.Mutex
    gateways map[core.ConnectionID]*gatewayHandle
}

type gatewayHandle struct {
    gateway  *gateway.Gateway
    cancel   context.CancelFunc
}
```

## 4. Lifecycle management

### 4.1 Starting the gateway

The supervisor starts the gateway in response to the connector reaching `running` status:

```go
// In handleProcessEvent:
case process.ProcessStatusRunning:
    rt.Connector.Status = core.ConnectorStatusRunning
    // Start gateway if the connection needs one
    if s.gatewayNeeded(rt) {
        if err := s.startGateway(context.Background(), rt); err != nil {
            slog.Error("failed to start gateway", "connection", event.ConnectionID, "err", err)
        }
    }
```

The gateway is started **after** the connector process event arrives, not inside the step execution. This keeps the gateway lifecycle independent from the connector — a gateway failure does not fail the open operation.

### 4.2 Stopping the gateway

The gateway is stopped when:
1. The connection is closing (before `StepStopConnector`)
2. The supervisor is shutting down
3. The gateway process exits unexpectedly

For close operations, the supervisor adds a compensation-like step that runs **before** the connector stop:

```go
// In supervisor before executing close plan:
if s.gatewayNeeded(rt) {
    if err := s.stopGateway(connID); err != nil {
        slog.Warn("failed to stop gateway during close", "connection", connID, "err", err)
    }
}
```

### 4.3 Determining whether a gateway is needed

A connection needs a gateway when its profile declares an OpenAI-compatible intent (or other profile types that require auth/SSE transparency). This is determined by:

```go
func (s *Supervisor) gatewayNeeded(rt *core.ConnectionRuntime) bool {
    p, ok := s.controller.GetProfile(rt.ConnectionID)
    if !ok {
        return false
    }
    // OpenAI-compatible profiles need the gateway for auth/SSE transparency
    return p.Kind == core.ConnectionClientTunnel && 
           p.Spec.ClientTunnel != nil &&
           p.Spec.ClientTunnel.Client == core.ClientOpenAISecureMCPTunnel
}
```

Initially, the gate is simple: only `client_tunnel` connections with `ClientOpenAISecureMCPTunnel` need the gateway. Future profiles (service exposure with auth, etc.) extend this predicate.

## 5. Plan execution flow

The controller's `executeStep` already dispatches `StepStartConnector` and `StepStopConnector`. The gateway is **not** a provider step — it's a supervisor concern. This means:

- **No provider changes needed**: Providers are unaware of the gateway.
- **No new step kinds**: The gateway lifecycle is driven by process events, not plan steps.
- **Gateway config comes from the profile**: The supervisor reads the profile's gateway spec when starting.

However, the controller needs to pass the tunnel's upstream URL back to the supervisor so it can configure the gateway. This happens via the existing `ConnectionRuntime.Endpoint` (populated by the connector step).

## 6. Testing strategy

1. **Unit tests for gateway manager** (`internal/supervisor/gateway_test.go`):
   - Test starting a gateway returns a valid listen address
   - Test stopping a gateway releases the port
   - Test gateway is reachable after start
   - Test gateway stops after connection close

2. **Integration with supervisor**:
   - Test that handleProcessEvent for running connector triggers gateway start
   - Test that close plan triggers gateway stop
   - Test that gateway address appears in ConnectionRuntime

## 7. Migration path

This implementation is additive:
- `GatewayRuntime` is a new struct field (nil for existing connections)
- Gateway manager is a new field on Supervisor
- No existing provider or step code changes

Existing connections that don't need a gateway simply have `nil` gateway state.