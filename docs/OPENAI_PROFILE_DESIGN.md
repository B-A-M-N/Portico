# OpenAI Profile + Portico Gateway Design Contract

**Status:** Implementation contract
**Created:** 2026-08-15
**Author:** Portico development policy (corrected from user review)

## 1. Architectural correction

**OpenAI tunnel is NOT a provider. It is a connection profile.**

A profile describes *what to expose* (an OpenAI-compatible API).
A transport provider describes *how to expose it* (Cloudflare, ngrok, Tailscale, SSH).

The current `internal/provider/openaitunnel/` is architecturally wrong. It mixes
both concerns. Refactor it into a profile that wraps transport providers.

```
                              PORTICO

Intent                     Profiles              Transport
─────────                  ────────              ──────────
"What do you               OpenAI-compatible     Cloudflare
 want to expose?"          Web service           ngrok
                           TCP service           Tailscale
                           SSH service           SSH service
                           Generic HTTP API
                                │
                                ▼
                        Security/Gateway
                        ┌─────────────────────┐
                        │ Auth / SSE / proxy  │
                        │ (Portico Gateway)   │
                        └─────────────────────┘
                                │
                                ▼
                        Runtime Supervisor
                        (process/state/health)
```

### 1.1 Why the current design is wrong

1. `internal/provider/openaitunnel/` implements `core.Provider`. It has its own
   `Plan()`, `ExecuteStep()`, `Observe()`, etc. This means OpenAI is a
   completely separate code path from Cloudflare. Adding OpenAI-over-ngrok
   requires a whole new provider implementation.

2. The provider abstraction is about *transport* (how to tunnel traffic). OpenAI
   is not a transport — it's a *service profile* (what the traffic means).

3. The provider registry mixes transport providers and service profiles, so the
   wizard cannot distinguish "I want an OpenAI tunnel" (profile) from "I want
   Cloudflare" (transport).

### 1.2 Why the new design is right

1. **Adding OpenAI-over-Cloudflare** = same profile + different transport.
2. **Adding OpenAI-over-ngrok** = same profile + different transport.
3. **Adding OpenAI-over-Tailscale** = same profile + different transport.
4. **The wizard can filter:** "I want OpenAI" → show only OpenAI-compatible transports.

---

## 2. Layer definitions

### 2.1 Connection profile (what to expose)

A profile describes the *intent* of the connection. Profiles are stored in
`internal/profile/`:

```go
// ProfileKind identifies the connection intent.
type ProfileKind string

const (
    ProfileOpenAICompatible ProfileKind = "openai_compatible"
    ProfileWebService       ProfileKind = "web_service"
    ProfileTCPService       ProfileKind = "tcp_service"
    ProfileSSHService       ProfileKind = "ssh_service"
    ProfileGenericHTTP      ProfileKind = "generic_http"
)

// Profile describes what the user wants to expose.
// Profiles are provider-neutral.
type Profile struct {
    ID      ProfileID
    Name    string
    Kind    ProfileKind
    Target  TargetSpec   // local service to expose
    Gateway *GatewaySpec // auth/SSE gateway configuration
    // Provider binding is resolved at plan time, not stored in the profile.
    TransportProviderID ProviderID
}

// TargetSpec describes the local service to expose.
type TargetSpec struct {
    Host     string
    Port     int
    Protocol Protocol
    // For OpenAI-compatible profiles, the base path (usually /v1).
    BasePath string
}

// GatewaySpec configures the Portico Gateway.
type GatewaySpec struct {
    Enabled       bool
    AuthRequired  bool
    AuthTokens    []string // Bearer tokens for client auth
    AllowSSE      bool     // must be true for OpenAI streaming
}
```

### 2.2 Transport provider (how to expose)

Transport providers remain in `internal/provider/`. They implement `core.Provider`:

- Cloudflare (named + quick)
- ngrok
- Tailscale Serve / Funnel
- SSH local/remote forwarding
- (future) SOCKS, HTTP proxy

Transport providers expose capabilities:
```go
// TransportCapabilities declares what a transport provider can do.
type TransportCapabilities struct {
    HTTP             bool
    TCP              bool
    PublicExposure   bool
    PrivateExposure  bool
    StableHostname   bool
    Streaming        bool  // SSE support
    Authentication   bool  // provider-native auth (e.g., Cloudflare Access)
    RemoteForward    bool  // SSH -R
    DynamicForward   bool  // SSH -D
    NoAccount        bool  // works without provider account (quick tunnel, SSH to own host)
}
```

### 2.3 Portico Gateway (security/SSE proxy)

The Gateway is a local HTTP proxy that sits between clients and tunnels.

**Why it exists:** Without a gateway, a user exposes a local llama.cpp server
through Cloudflare and accidentally creates a public inference endpoint. The
gateway fails closed.

**Why it must be transparent to streaming:** OpenAI's Chat Completions and
Responses streaming both use SSE. Buffering the entire response body breaks
streaming. The gateway must flush each chunk immediately.

```go
// Gateway is a local HTTP proxy that authenticates clients and forwards
// requests to the upstream service through the transport tunnel.
type Gateway struct {
    // Listener address (127.0.0.1:<dynamic>)
    ListenAddr string
    
    // Upstream is the tunnel's local endpoint (e.g., http://127.0.0.1:8080
    // or the Cloudflare quick tunnel URL).
    Upstream string
    
    // Auth configuration
    AuthRequired bool
    ValidTokens  map[string]struct{}
    
    // SSE configuration — must be transparent
    FlushInterval time.Duration // 0 = flush every chunk immediately
}
```

**Gateway behavior:**

1. Client connects to `127.0.0.1:<gateway-port>` with `Authorization: Bearer <token>`
2. Gateway validates the token against `ValidTokens`
3. Gateway forwards the request to the upstream (through the tunnel)
4. For SSE responses, gateway flushes each chunk immediately
5. Gateway redacts tokens from all logs, errors, and diagnostics

### 2.4 Three-state health

Portico distinguishes three health states, not just "PID alive":

```go
// HealthState is the overall connection health.
type HealthState string

const (
    HealthHealthy   HealthState = "healthy"
    HealthDegraded  HealthState = "degraded"
    HealthUnhealthy HealthState = "unhealthy"
    HealthUnknown   HealthState = "unknown"
)

// HealthReport carries the three-state health assessment.
type HealthReport struct {
    ConnectionID ConnectionID
    State        HealthState
    
    // Process health — Is the connector alive?
    Process HealthCheck
    // Transport health — Is the externally reachable tunnel alive?
    Transport HealthCheck
    // Service health — Does the intended application actually work?
    Service HealthCheck
}

type HealthCheck struct {
    OK      bool
    Detail  string
    LastChecked time.Time
}
```

**Health determination logic:**

```go
func (r *HealthReport) Compute() HealthState {
    if !r.Process.OK {
        return HealthUnhealthy
    }
    if !r.Transport.OK {
        return HealthUnhealthy
    }
    if !r.Service.OK {
        return HealthDegraded
    }
    return HealthHealthy
}
```

For an OpenAI connection, the service check probes `/v1/models` through the
tunnel and verifies a streaming request returns SSE chunks.

---

## 3. OpenAI profile discovery

When the user creates an OpenAI-compatible connection:

```
1. Select local address/port (or discover via `ss`)
2. Verify something is listening
3. Probe OpenAI compatibility:
   - GET /v1/models → list of models
   - If streaming supported, test a tiny streaming request
4. Determine supported capabilities:
   - Chat Completions (streaming?)
   - Responses (streaming?)
   - Supported models
5. Select exposure: public / private / specific remote
6. Select transport: Cloudflare / ngrok / Tailscale / SSH
7. Configure auth: gateway token (auto-generated) or upstream auth
8. Establish tunnel + gateway
9. Test through remote endpoint:
   - /v1/models reachable through tunnel + gateway
   - Normal Chat Completions works
   - Streaming Chat Completions works (SSE chunks flush immediately)
10. Display usable client configuration:
    - Base URL: https://endpoint.example/v1
    - API Key: <portico-token>
    - Env vars: OPENAI_BASE_URL, OPENAI_API_KEY
```

### 3.1 OpenAI compatibility probe

```go
// OpenAICompatibility reports what an OpenAI-compatible endpoint supports.
type OpenAICompatibility struct {
    Endpoint        string
    Models          []string
    ChatCompletions struct {
        Supported bool
        Streaming bool
    }
    Responses struct {
        Supported bool
        Streaming bool
    }
    AuthRequired bool
}

// ProbeOpenAICompatibility probes a local endpoint for OpenAI compatibility.
func ProbeOpenAICompatibility(ctx context.Context, baseURL string, authToken string) (*OpenAICompatibility, error) {
    // 1. GET /v1/models
    // 2. POST /v1/chat/completions with stream:false (small request)
    // 3. POST /v1/chat/completions with stream:true (verify SSE chunks)
    // 4. If supported: POST /v1/responses
    // Return structured compatibility report
}
```

### 3.2 Streaming verification

**Critical:** A regular 200 OK test does NOT prove an OpenAI tunnel works.
Streaming must be tested explicitly.

```go
// VerifyStreaming sends a small streaming request and verifies that:
// 1. The response has content-type text/event-stream
// 2. SSE chunks arrive incrementally (not all at once)
// 3. No buffering occurs (chunks flush immediately)
// 4. The stream completes with a [DONE] message
func VerifyStreaming(ctx context.Context, baseURL string, authToken string) error {
    // POST /v1/chat/completions with stream:true, max_tokens:1
    // Read chunks as they arrive
    // Verify chunk timing: first chunk arrives before the full response would
    // Verify [DONE] at end
}
```

---

## 4. Result output

When an OpenAI connection is established, Portico displays:

```
OpenAI-Compatible Endpoint

Status:        Healthy
Transport:     Cloudflare
Local origin:  http://127.0.0.1:8000
Remote base:   https://llm.example.com/v1
Auth:          Portico Bearer Token
Streaming:     Verified
Latency:       42 ms

[c] Copy base URL    [k] Copy API token    [e] Copy env vars
[t] Test connection  [l] Show logs         [r] Restart    [s] Stop
```

Copy env vars produces:
```bash
export OPENAI_BASE_URL="https://llm.example.com/v1"
export OPENAI_API_KEY="<portico-token>"
```

---

## 5. Refactoring plan

### Phase 1: Profile abstraction

1. Create `internal/profile/` package
2. Define `Profile`, `ProfileKind`, `TargetSpec`, `GatewaySpec` types
3. Add profile kind to `ConnectionProfile` (or derive from kind)
4. Wire profile kind through `internal/ipc/dto.go`

### Phase 2: Gateway implementation

1. Create `internal/gateway/` package
2. Implement HTTP proxy with Bearer token auth
3. Implement SSE transparency (no buffering, immediate flush)
4. Add gateway process to runtime supervisor

### Phase 3: OpenAI profile

1. Move OpenAI-specific code from `internal/provider/openaitunnel/` to `internal/profile/openai/`
2. Implement `ProbeOpenAICompatibility`
3. Implement `VerifyStreaming`
4. Profile wraps a transport provider (Cloudflare first)
5. Delete `internal/provider/openaitunnel/` after migration

### Phase 4: Three-state health

1. Add `HealthReport` to `internal/core/`
2. Implement process check (connector alive?)
3. Implement transport check (tunnel reachable?)
4. Implement service check (application works?)
5. Wire into supervisor reconciliation loop

### Phase 5: Wizard integration

1. Add profile kind selection to wizard
2. For OpenAI profile: show local address probe, compatibility probe, transport selection
3. For web service profile: show source/exposure/protection selection
4. Gateway configuration screen (auto-generate token, show copyable env vars)

---

## 6. Tests

For the OpenAI profile, every transport must pass:

```
OpenAI-over-Cloudflare:
  - /v1/models reachable through tunnel
  - normal Chat Completions works
  - streaming Chat Completions works (SSE chunks flush immediately)
  - 401 with bad Portico token
  - success with correct token
  - long-running generation
  - client cancellation propagates

OpenAI-over-ngrok:
  - (same test suite, no OpenAI-specific new code)

OpenAI-over-Tailscale:
  - (same test suite, no OpenAI-specific new code)
```

**The key test:** Adding OpenAI-over-ngrok should require almost no
OpenAI-specific new code. It should be: same profile + different transport.

---

## 7. Constraints

- Portico Gateway must never buffer streaming responses
- Gateway auth tokens must never appear in logs, errors, or diagnostics
- Gateway must fail closed: no token = no access
- Three-state health must distinguish transport failure from service failure
- Cloudflare Quick Tunnels must reject OpenAI profiles (no SSE support)
- OpenAI profile must reject incompatible transports before execution

---

## 8. Files to create/modify

### Create
- `internal/profile/profile.go` — Profile, ProfileKind, TargetSpec, GatewaySpec
- `internal/profile/openai/probe.go` — OpenAICompatibility, ProbeOpenAICompatibility
- `internal/profile/openai/streaming.go` — VerifyStreaming
- `internal/profile/openai/profile.go` — OpenAI profile orchestration
- `internal/gateway/gateway.go` — Gateway HTTP proxy with auth + SSE transparency
- `internal/gateway/gateway_test.go` — Gateway tests including streaming transparency

### Modify
- `internal/core/connection.go` — add profile kind (or derive), add HealthReport
- `internal/ipc/dto.go` — add profile kind to DTOs
- `internal/tui/screens/wizard.go` — add profile kind selection
- `internal/tui/screens/inspect.go` — show three-state health
- `internal/provider/registry.go` — distinguish profile vs transport
- `internal/provider/cloudflare/adapter.go` — wire into OpenAI profile as transport

### Delete
- `internal/provider/openaitunnel/` — after migration to profile

---

## 9. Implementation order

1. **Profile types** (`internal/profile/profile.go`)
2. **Gateway** (`internal/gateway/gateway.go`) — auth + SSE proxy
3. **Health report** (`internal/core/` + supervisor)
4. **OpenAI probe** (`internal/profile/openai/probe.go`)
5. **OpenAI profile** (`internal/profile/openai/profile.go`)
6. **Cloudflare as OpenAI transport** (wire Cloudflare adapter into OpenAI profile)
7. **Wizard integration** (profile kind selection, compatibility probe UI)
8. **Streaming verification** (`VerifyStreaming` test)
9. **Cleanup** (delete `internal/provider/openaitunnel/`)
