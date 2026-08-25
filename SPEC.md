# Portico — Full Product and Implementation Specification

**Status:** Architecture lock / implementation contract  
**Initial target:** Linux desktop, single-user, local-first  
**Primary binary:** `portico`  
**Implementation language:** Go  
**TUI stack:** Bubble Tea v2, Bubbles v2, Lip Gloss v2  
**First real provider:** Cloudflare  
**Reference donor:** Flare CLI  
**Primary objective:** Build the correct architecture once, then add providers and polish without repeatedly reshaping the system.

---

## 0. What this document is, and what is built

This is the design contract. It describes the architecture Portico is meant to
have, including parts not yet built. **It is not a description of the current
implementation**, and a section here is not evidence that its subject exists.

For what is actually implemented and proven:

- `docs/ACCEPTANCE_MATRIX.md` and `docs/AUDIT_ACCEPTANCE_MATRIX.md` map each
  requirement to the test that holds it and the command that runs it.
- `make acceptance` executes every one of those commands and fails if a cited
  test does not exist.
- `docs/REMAINING_WORK.md` is the backlog of what is known to be missing.
- `AGENTS.md` carries the provider status table.

Reviewed against `62903ed`. Where this document and the code disagree, the code
is what ships; treat the disagreement as a defect in one of them and say which.

### Implemented providers, in summary

| Provider | Adapter | Default |
|---|---|---|
| Cloudflare | yes | enabled |
| Port forward (local) | yes | enabled |
| ngrok | yes | disabled — experimental opt-in |
| OpenAI Secure MCP Tunnel | yes | disabled — experimental opt-in |
| Tailscale, zrok | none | catalog entries only, so the UI can explain the gap |

Sections below describing Tailscale or zrok behaviour are design, not
implementation.

---

## 1. Product definition

Portico is a standalone, terminal-native connection manager that discovers local services, creates and owns provider-backed connections, keeps them alive after the TUI closes, shows the path from local service to public or private endpoint, diagnoses failures by segment, and proposes the smallest safe repair in ordinary language.

The normal user command is:

```bash
portico
```

The TUI is the default interface. Expert CLI and JSON interfaces exist, but they are alternate clients of the same supervisor API. They do not contain separate lifecycle logic.

### 1.1 Product promise

A user should be able to answer three questions:

1. What should be reachable?
2. How should it be reachable?
3. Who, if anyone, should be allowed to reach it?

Portico determines the provider-specific operations required to produce that outcome.

### 1.2 Non-goals for v0.1

Portico v0.1 will not include:

- Kubernetes integration.
- A web dashboard.
- A remote multi-host control plane.
- A provider plugin marketplace.
- Arbitrary YAML ingestion.
- A generic reverse-proxy platform.
- A stdio-to-network MCP transport bridge.
- Public UDP exposure.
- Cross-platform support beyond Linux.
- Automatic migration between providers.
- Multi-user authorization for the local supervisor.
- Billing optimization or live price comparison.

These are explicit deferrals, not missing implementation.

---

## 2. Non-negotiable architecture

```text
┌─────────────────────────────────────────────────────────────┐
│ Clients                                                     │
│                                                             │
│  Portico TUI     Portico CLI     JSON/agent client          │
└──────────────┬──────────────┬──────────────┬────────────────┘
               │              │              │
               └──────────────┴──────────────┘
                              │
                    Unix socket HTTP/SSE
                              │
┌─────────────────────────────▼───────────────────────────────┐
│ Local supervisor                                            │
│                                                             │
│  API server     event broker     operation runner           │
│  reconciler     process manager  telemetry sampler          │
│  discovery      diagnostics      credential resolver        │
└─────────────────────────────┬───────────────────────────────┘
                              │
┌─────────────────────────────▼───────────────────────────────┐
│ Provider-neutral controller                                 │
│                                                             │
│  desired state   observed state   plans   repairs           │
│  capability matching          ownership and rollback         │
└───────────┬─────────────────┬─────────────────┬──────────────┘
            │                 │                 │
     Cloudflare adapter   ngrok adapter   Tailscale adapter
            │                 │                 │
      cloudflared            ngrok           tailscaled
```

### 2.1 Hard boundaries

1. **The supervisor is the only process allowed to mutate connection state.**
2. **The supervisor is the only process allowed to open the Portico database.**
3. **The TUI never calls a provider, connector binary, or database directly.**
4. **The CLI never duplicates controller logic.**
5. **Every mutation of a connection is represented by an immutable operation
   plan before it is applied.** A plan is the boundary for mutations that
   change provider-visible state, execute as an ordered sequence that can stop
   between steps, and therefore need compensation and a re-observed outcome.
   The subject is a connection because a connection is what owns provider
   resources and can be left internally inconsistent.

   Mutations of local durable state that touch no provider, run as a single
   transaction, and have no intermediate state are outside this boundary —
   configuring a provider account, and removing one, are both examples. They
   are not exempt from the *properties* a plan provides: what a caller confirms
   must be computed from the same evidence the mutation decides on, must bind
   the mutation by fingerprint, and must leave a durable record. See
   `docs/ACCOUNT_REMOVAL_DESIGN.md`, which records why forcing such a mutation
   through connection-scoped plan machinery was declined.
6. **Every provider-specific behavior is confined to its adapter package.**
7. **Closing the TUI only disconnects the client. It never implies closing a connection.**
8. **Profiles express desired state. Runtime records express observed state. They are never merged into one mutable object.**
9. **Rendering is pure. `View()` performs no I/O, reads no clock, and mutates
   nothing** — including through a pointer it holds into live model state.
   `View()` has a value receiver, so anything it writes to the model is written
   to a copy and discarded; anything it writes *through a pointer* reaches the
   real model. Both have happened. Scroll state written during render was lost,
   leaving a feature that could not work; the inspect model was assigned
   through a live pointer, so drawing a screen changed the program. State is
   written in `Update` and read in `View`.
10. **Provider capabilities are structured constraints, not a loose collection of optimistic booleans.**

Any code change that violates one of these boundaries is rejected even if it appears to make a feature easier.

---

## 3. Flare donor strategy

Flare already contains useful Cloudflare-specific machinery: six origin implementations, Cloudflare Tunnel, DNS and Access provisioning, rollback behavior, persisted sessions, JSON output, and a lifecycle pipeline. Its current structure is command-centered and Cloudflare-specific, so Portico must extract capabilities from it without preserving its product architecture.

### 3.1 Repository strategy

Create Portico from Flare's Git history rather than beginning with an unrelated blank repository. Perform one mechanical project rename in a dedicated commit, then stop renaming.

Recommended sequence:

```bash
git clone https://github.com/paoloanzn/flare-cli portico
cd portico
git remote rename origin flare-upstream
git remote add origin <PORTICO_REPOSITORY>
```

The first commit changes only:

- Go module path.
- Binary name.
- package imports affected by the module path.
- README title.

Do not combine architectural changes with this rename.

### 3.2 Legacy command path removed

The legacy Flare command subtree under `cmd/` was removed at commit `0c53e4f`
after a capability parity inventory confirmed every legacy command — `serve`,
`list`, `close`, `status`, `update`, `logs`, `doctor`, `init`, `auth login`,
`auth whoami`, `auth logout`, `auth print-login-url`, `auth rotate-mtls`,
`config get`, `config set`, and `version` — has an equivalent in the new
`internal/cli/` path that talks to the supervisor over IPC. Tests and
documentation no longer reference `portico legacy`.

### 3.3 Extraction order

Move code in this order:

1. Subprocess helpers.
2. Local source/origin implementations.
3. Cloudflare API clients.
4. Cloudflare connector management.
5. DNS and Access operations.
6. Rollback primitives.
7. Legacy session state conversion.
8. Legacy Cobra orchestration removal.

Do not start by moving files into aesthetically pleasing directories. Move behavior only when its destination interface and tests already exist.

---

## 4. Locked terminology

| Portico term | Internal meaning |
|---|---|
| Connection | Saved desired configuration |
| Local service | Source/origin reachable on the local machine |
| Provider | Cloudflare, ngrok, Tailscale, zrok, or another implementation |
| Public address | Internet-reachable endpoint |
| Private address | Endpoint restricted to a provider-defined private network |
| Protection | Authentication or access policy |
| Connector | Local provider process or embedded transport |
| Plan | Immutable preview of exact operations |
| Operation | Execution of a plan |
| Repair | Reconciliation plan intended to restore desired state |
| Profile | Persisted desired state of a connection |
| Runtime | Current observed state and provider resources |
| Finding | Evidence-backed diagnostic result |

Avoid architectural metaphors in user-facing copy. Portico's name affects geometry, spacing, and visual identity, not vocabulary.

---

## 5. Repository structure

```text
portico/
├── internal/
│   ├── app/
│   │   ├── bootstrap.go
│   │   └── paths.go
│   ├── core/
│   │   ├── connection.go
│   │   ├── source.go
│   │   ├── exposure.go
│   │   ├── protection.go
│   │   ├── provider.go
│   │   ├── capability.go
│   │   ├── plan.go
│   │   ├── operation.go
│   │   ├── event.go
│   │   ├── finding.go
│   │   └── errors.go
│   ├── controller/
│   │   ├── controller.go
│   │   ├── planner.go
│   │   ├── executor.go
│   │   ├── reconciler.go
│   │   ├── repair.go
│   │   └── recommendation.go
│   ├── supervisor/
│   │   ├── supervisor.go
│   │   ├── api.go
│   │   ├── eventbroker.go
│   │   ├── startup.go
│   │   └── shutdown.go
│   ├── process/
│   │   ├── manager.go
│   │   ├── identity_linux.go
│   │   ├── logs.go
│   │   └── restart.go
│   ├── discovery/
│   │   ├── discovery.go
│   │   ├── listeners_linux.go
│   │   ├── process_linux.go
│   │   ├── probe_http.go
│   │   ├── probe_tls.go
│   │   ├── classify.go
│   │   └── confidence.go
│   ├── diagnostics/
│   │   ├── engine.go
│   │   ├── graph.go
│   │   ├── probes.go
│   │   └── explain.go
│   ├── provider/
│   │   ├── registry.go
│   │   ├── mock/
│   │   ├── cloudflare/
│   │   │   ├── adapter.go
│   │   │   ├── capabilities.go
│   │   │   ├── auth.go
│   │   │   ├── plan.go
│   │   │   ├── apply.go
│   │   │   ├── observe.go
│   │   │   ├── repair.go
│   │   │   ├── remove.go
│   │   │   ├── connector.go
│   │   │   ├── telemetry.go
│   │   │   └── internal/
│   │   ├── ngrok/
│   │   ├── tailscale/
│   │   └── zrok/
│   ├── source/
│   │   ├── existing.go
│   │   ├── command.go
│   │   ├── directory.go
│   │   ├── fileserver.go
│   │   └── health.go
│   ├── store/
│   │   ├── store.go
│   │   ├── sqlite.go
│   │   ├── migrations/
│   │   └── queries/
│   ├── credentials/
│   │   ├── resolver.go
│   │   ├── env.go
│   │   ├── keyring.go
│   │   └── memory.go
│   ├── ipc/
│   │   ├── client.go
│   │   ├── server.go
│   │   ├── routes.go
│   │   ├── sse.go
│   │   └── dto.go
│   ├── tui/
│   │   ├── app.go
│   │   ├── update.go
│   │   ├── view.go
│   │   ├── messages.go
│   │   ├── commands.go
│   │   ├── keymap.go
│   │   ├── theme.go
│   │   ├── layout.go
│   │   ├── screens/
│   │   ├── components/
│   │   ├── route/
│   │   ├── animation/
│   │   └── testdata/
│   └── cli/
│       ├── root.go
│       ├── connection.go
│       ├── provider.go
│       ├── supervisor.go
│       └── json.go
├── migrations/
├── docs/
│   ├── architecture/
│   ├── adr/
│   ├── providers/
│   └── ui/
├── test/
│   ├── integration/
│   ├── e2e/
│   └── fixtures/
├── Makefile
├── go.mod
└── go.sum
```

### 5.1 Dependency direction

Allowed imports:

```text
core <- provider adapters
core <- controller
core <- store DTO conversion
core <- supervisor
core <- TUI presenters

controller <- provider interfaces
controller <- store interfaces
supervisor <- controller
IPC server <- supervisor
TUI/CLI <- IPC client
```

Forbidden imports:

```text
core -> provider/cloudflare
core -> Bubble Tea
TUI -> store
TUI -> provider/*
CLI -> provider/*
provider/* -> TUI
store -> Bubble Tea
```

Add an import-boundary test or linter rule so these constraints fail CI.

---

## 6. Core domain model

### 6.1 Connection profile: desired state

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
    CreatedAt   time.Time
    UpdatedAt   time.Time
}
```

```go
type DesiredConnectionState string

const (
    DesiredOpen   DesiredConnectionState = "open"
    DesiredClosed DesiredConnectionState = "closed"
)
```

A profile does not contain:

- Connector PID.
- Current public address.
- Provider resource IDs.
- Last error.
- Traffic samples.
- Current health.
- Progress state.

Those belong to runtime or operation records.

### 6.2 Source specification

```go
type SourceKind string

const (
    SourceExisting  SourceKind = "existing_service"
    SourceDirectory SourceKind = "directory"
    SourceCommand   SourceKind = "command"
    SourceMCP       SourceKind = "mcp_server"
)

type SourceSpec struct {
    Kind      SourceKind
    Existing  *ExistingServiceSpec
    Directory *DirectorySpec
    Command   *CommandSpec
    MCP       *MCPServiceSpec
}
```

Every tagged union must validate that exactly one matching field is populated.

```go
type ExistingServiceSpec struct {
    Network   string
    Address   string
    Protocol  Protocol
    Health    HealthCheckSpec
}

type CommandSpec struct {
    Executable string
    Args       []string
    WorkingDir string
    Env        map[string]string
    Port       int
    Protocol   Protocol
    UseShell   bool
}

type DirectorySpec struct {
    Path          string
    Mode          DirectoryMode
    SPAFallback   bool
    AllowUpload   bool
    AllowDelete   bool
}

type MCPServiceSpec struct {
    Transport MCPTransport
    Endpoint  string
    Command   *CommandSpec
}
```

v0.1 supports MCP servers that already expose Streamable HTTP, HTTP, or SSE. A stdio-only MCP server is not automatically bridged. The wizard must say this plainly rather than silently creating a separate transport system.

### 6.3 Exposure specification

```go
type ExposureMode string

const (
    ExposureTemporary ExposureMode = "temporary_public"
    ExposurePermanent ExposureMode = "permanent_public"
    ExposurePrivate   ExposureMode = "private_only"
)

type ExposureSpec struct {
    Mode             ExposureMode
    Protocol         Protocol
    RequestedAddress string
    Expiration       *time.Time
}
```

### 6.4 Protection specification

```go
type ProtectionKind string

const (
    ProtectionNone         ProtectionKind = "none"
    ProtectionEmailOTP     ProtectionKind = "email_otp"
    ProtectionIdentity     ProtectionKind = "identity_provider"
    ProtectionServiceToken ProtectionKind = "service_token"
    ProtectionPrivateNet   ProtectionKind = "private_network"
)

type ProtectionSpec struct {
    Kind           ProtectionKind
    AllowedEmails  []string
    AllowedDomains []string
    SessionTTL     time.Duration
}
```

Protection is an outcome. Provider adapters map it to Cloudflare Access, ngrok Traffic Policy, Tailscale ACL behavior, zrok private sharing, or an unsupported-capability result.

### 6.5 Runtime: observed state

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
    LastObservation  time.Time
    LastTransition   time.Time
    ActiveOperation  *OperationID
    Error            *PorticoError
}
```

```go
type RuntimeState string

const (
    RuntimeUnknown     RuntimeState = "unknown"
    RuntimePlanning    RuntimeState = "planning"
    RuntimeOpening     RuntimeState = "opening"
    RuntimeOpen        RuntimeState = "open"
    RuntimeDegraded    RuntimeState = "degraded"
    RuntimeRepairing   RuntimeState = "repairing"
    RuntimeClosing     RuntimeState = "closing"
    RuntimeClosed      RuntimeState = "closed"
    RuntimeError       RuntimeState = "error"
    RuntimeOrphaned    RuntimeState = "orphaned"
)
```

### 6.6 User-facing status mapping

Internal runtime states map to a smaller vocabulary:

| Runtime state | User-facing state |
|---|---|
| `open` | Open |
| `degraded` | Unstable |
| `closed` | Closed |
| `opening`, `repairing`, `closing` | Changing |
| `error`, `orphaned` | Needs attention |
| `unknown`, `planning` | Checking |

Never expose a synthetic numeric health score.

---

## 7. Provider contract

Use the provider surface below as the stable public boundary:

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

### 7.1 Capabilities must be structured

Do not stop at:

```go
SupportsCustomHostname bool
```

Use:

```go
type Capabilities struct {
    TemporaryAddresses CapabilitySupport
    CustomHostnames    CapabilitySupport
    PrivateExposure    CapabilitySupport
    ManagedDNS         CapabilitySupport
    BuiltInProtection  []ProtectionCapability
    Protocols          map[Protocol]ProtocolCapability
    Telemetry          TelemetryCapability
    Redundancy         RedundancyCapability
    RemoteConfig       CapabilitySupport
    Expiration         ExpirationCapability
    Constraints        []CapabilityConstraint
}
```

```go
type CapabilitySupport struct {
    Supported bool
    Stability Stability
    Requires  []Requirement
    Notes     []string
}

type ProtocolCapability struct {
    Supported   bool
    Public      bool
    Private     bool
    Constraints []CapabilityConstraint
}
```

A provider may support HTTP temporary addresses but reject SSE in that mode. It may support TCP only on selected ports. It may support a generated provider hostname but not a user-owned custom hostname. The capability model must express these differences.

### 7.2 Provider registry

```go
type Registry interface {
    Get(id ProviderID) (Provider, bool)
    List() []Provider
}
```

v0.1 compiles providers into the binary. Do not build a runtime plugin loader.

### 7.3 Provider account and authentication state

```go
type ProviderAccount struct {
    ID            ProviderAccountID
    Provider      ProviderID
    Label         string
    CredentialRef string
    Metadata      map[string]string
    Status        ProviderAccountStatus
}
```

Credentials are resolved by reference at operation time. Tokens are never serialized into plans, events, logs, runtime records, or UI snapshots.

---

## 8. Plans are the mutation boundary

### 8.1 Operation plan

```go
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
```

```go
type PlanStep struct {
    ID             StepID
    Kind           StepKind
    Summary        string
    Technical      TechnicalOperation
    Destructive    bool
    Irreversible   bool
    Sensitive      bool
    Compensation   *CompensationStep
    Ownership      ResourceOwnership
}
```

### 8.2 Exactness requirement

A plan is not a vague preview. Applying a plan must execute the serialized steps in order.

At apply time:

1. Confirm the plan has not expired.
2. Confirm the profile revision still matches.
3. Re-observe the relevant provider state.
4. Confirm the observed fingerprint still matches.
5. Confirm required credentials remain available.
6. Execute the exact steps.
7. Record every step event.
8. Run compensations for newly created resources if a later step fails.
9. Re-observe and compare with the expected outcome.

The adapter may resolve explicitly declared output placeholders, such as a provider-generated temporary URL, but it may not silently add unplanned mutations.

### 8.3 Canonical fingerprint

Canonicalize the plan JSON and hash it with SHA-256. The fingerprint is displayed in technical details and stored with the operation. It prevents a previewed plan from being replaced with a different plan during apply.

### 8.4 Ownership

```go
type ResourceOwnership string

const (
    OwnershipManaged ResourceOwnership = "managed"
    OwnershipAdopted ResourceOwnership = "adopted"
    OwnershipExternal ResourceOwnership = "external"
)
```

Portico automatically deletes only resources it created and owns. Adopted or external resources require explicit confirmation and a plan that names the resource.

---

## 9. Controller behavior

### 9.1 Controller responsibilities

The controller:

- Validates desired state.
- Selects or validates a provider.
- Requests a provider plan.
- Persists the plan.
- Applies plans through a serialized operation runner.
- Observes current state.
- Computes drift.
- Produces repair plans.
- Maps provider events into normalized events.
- Updates runtime snapshots.
- Enforces ownership and idempotency.

The controller does not:

- Render UI.
- Read keyboard input.
- Store credentials directly.
- Parse provider log lines in generic code.
- Know Cloudflare API types.
- Spawn arbitrary processes outside the process manager.

### 9.2 Concurrency

- One active mutation per connection.
- Four active operations globally by default.
- Observation may run concurrently with operations only when it cannot conflict.
- Provider-wide rate limits are enforced by adapter-specific limiters.
- Duplicate requests with the same idempotency key return the existing plan or operation.

### 9.3 Open, close, delete

**Open** reconciles the connection to `DesiredOpen`.

**Close** changes desired state to `DesiredClosed`, stops active exposure, and preserves the profile. Reusable provider resources may remain when that is the provider's safe, normal behavior.

**Delete** is a separate destructive workflow. It previews removal of managed provider resources and deletes the local profile only after the provider removal plan succeeds or the user explicitly accepts an orphaned local deletion.

Do not use Space to delete. Space only opens or closes.

---

## 10. Supervisor

### 10.1 One binary, separate process mode

The same executable supports:

```bash
portico
portico supervisor run
portico supervisor start
portico supervisor stop
portico supervisor status
portico supervisor logs
```

`portico` performs:

1. Try the Unix socket.
2. If unavailable, acquire a startup lock.
3. Re-exec itself as `portico supervisor run`.
4. Detach the child with a new session.
5. Redirect stdout and stderr to supervisor logs.
6. Wait for a ready handshake.
7. Start the TUI client.

The TUI process is not the supervisor's parent in any lifecycle-sensitive sense. Closing the terminal must not send termination to managed connectors.

### 10.2 Linux paths

Use XDG paths:

```text
Socket:
  $XDG_RUNTIME_DIR/portico/portico.sock
Fallback:
  /tmp/portico-$UID/portico.sock

Database:
  $XDG_DATA_HOME/portico/portico.db
Fallback:
  ~/.local/share/portico/portico.db

Configuration:
  $XDG_CONFIG_HOME/portico/config.toml
Fallback:
  ~/.config/portico/config.toml

Logs and state:
  $XDG_STATE_HOME/portico/
Fallback:
  ~/.local/state/portico/
```

Permissions:

- Runtime, data, config, and state directories: `0700`.
- Unix socket: `0600`.
- Credential references and config: `0600`.
- Connector logs: `0600`.

Where Linux peer credentials are available, reject socket peers whose UID does not match the supervisor UID.

### 10.3 Startup sequence

```text
Acquire supervisor lock
Open structured log
Open SQLite
Run migrations
Create provider registry
Resolve provider account metadata
Load connection profiles
Recover incomplete operations
Observe managed resources
Validate recorded connector identities
Restart connections whose desired state is open
Start API and event broker
Start reconciliation scheduler
Signal ready
```

### 10.4 Shutdown sequence

A normal supervisor shutdown does not delete provider resources.

```text
Stop accepting mutations
Complete or cancel active operations according to safety rules
Flush events
Stop connectors only when shutdown policy requests it
Close API socket
Close database
Release lock
```

`portico supervisor stop` asks whether open connections should remain running only when the process manager cannot transfer ownership safely. In v0.1, stopping the supervisor also stops its child connectors, but does not delete remote resources or profiles. Ordinary TUI exit does not stop the supervisor.

### 10.5 Reconciliation schedule

Use event-driven reconciliation plus bounded periodic checks:

- Immediately after any operation.
- Immediately after a connector exit.
- Every 15 seconds for selected or changing connections.
- Every 60 seconds for other open connections.
- Every 5 minutes for closed connections with retained remote resources.
- Add jitter to provider observations.

Do not poll provider APIs every animation frame or every second.

---

## 11. Process manager

### 11.1 Connector identity

Never trust a PID alone.

```go
type ProcessIdentity struct {
    PID            int
    StartTime      uint64
    ExecutablePath string
    CommandHash    string
}
```

Before signaling a recorded process, verify all available identity fields. If identity does not match, mark the connector as unknown and do not kill it.

### 11.2 Process specification

```go
type ProcessSpec struct {
    Executable string
    Args       []string
    Env        []string
    Dir        string
    StdoutPath string
    StderrPath string
    Restart    RestartPolicy
    Redactions []string
}
```

Avoid `sh -c` unless the user explicitly created a shell command source. Provider connector commands use executable plus argument arrays.

### 11.3 Restart policy

Default connector restart policy:

```text
Attempt 1: 1 second
Attempt 2: 2 seconds
Attempt 3: 5 seconds
Attempt 4: 10 seconds
Attempt 5: 30 seconds
Then: mark unstable and require repair
Window: 10 minutes
```

Reset the restart count after ten minutes of stable operation.

### 11.4 Logs

- Rotate at 10 MiB.
- Keep five files per connector.
- Redact credential values and authorization headers.
- Store structured supervisor logs separately from raw connector logs.
- UI log streaming is tail-based and bounded.

---

## 12. Persistence

### 12.1 Database ownership

Use SQLite with WAL mode. Only the supervisor opens the database. TUI and CLI access all state through IPC.

### 12.2 Tables

Minimum schema:

```text
schema_migrations
connection_profiles
connection_runtime
provider_accounts
provider_resources
operation_plans
operations
operation_events
diagnostic_findings
traffic_samples
idempotency_keys
```

### 12.3 Important columns

```sql
CREATE TABLE connection_profiles (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    revision INTEGER NOT NULL,
    source_json BLOB NOT NULL,
    exposure_json BLOB NOT NULL,
    protection_json BLOB NOT NULL,
    provider_json BLOB NOT NULL,
    lifecycle_json BLOB NOT NULL,
    desired_state TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE connection_runtime (
    connection_id TEXT PRIMARY KEY,
    runtime_state TEXT NOT NULL,
    provider_id TEXT,
    public_address TEXT,
    private_address TEXT,
    connector_json BLOB,
    segment_health_json BLOB,
    active_operation_id TEXT,
    error_json BLOB,
    last_observation TEXT,
    last_transition TEXT
);

CREATE TABLE provider_resources (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    connection_id TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    external_id TEXT NOT NULL,
    ownership TEXT NOT NULL,
    spec_hash TEXT,
    metadata_json BLOB,
    UNIQUE(provider_id, resource_type, external_id)
);

CREATE TABLE operation_events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id TEXT,
    connection_id TEXT,
    occurred_at TEXT NOT NULL,
    event_type TEXT NOT NULL,
    stage TEXT,
    payload_json BLOB NOT NULL
);
```

### 12.4 Migration rules

- Migrations are embedded and strictly ordered.
- Never rewrite an already released migration.
- Every migration has an upgrade test from the previous release fixture.
- Back up the database before any destructive migration.
- The supervisor refuses to start on a database created by a newer unsupported schema.

### 12.5 Retention

- Operation events: retain 30 days or 100,000 rows, whichever is larger.
- Traffic samples: one-second data for one hour, one-minute aggregates for seven days.
- Findings: retain until resolved plus 30 days.
- Connector logs: file rotation policy above.

---

## 13. IPC contract

### 13.1 Transport

Use HTTP/1.1 over a Unix domain socket with JSON request/response bodies and Server-Sent Events for event streaming.

Reasons:

- Standard library implementation.
- Easy CLI and agent reuse.
- Easy inspection with local tooling.
- SSE maps cleanly to the supervisor's monotonic event stream.
- No generated RPC code is required in v0.1.

### 13.2 Versioned endpoints

```text
GET    /v1/health
GET    /v1/snapshot
GET    /v1/events
GET    /v1/connections
POST   /v1/connections
GET    /v1/connections/{id}
PATCH  /v1/connections/{id}
POST   /v1/connections/{id}/plan/open
POST   /v1/connections/{id}/plan/close
POST   /v1/connections/{id}/plan/repair
POST   /v1/connections/{id}/plan/delete
POST   /v1/plans/{id}/apply
GET    /v1/operations/{id}
GET    /v1/operations/{id}/events
GET    /v1/discovery
POST   /v1/discovery/refresh
GET    /v1/providers
POST   /v1/providers/{id}/authenticate
GET    /v1/diagnostics/{connection_id}
```

### 13.3 Event stream

SSE events include:

```text
id: <monotonic sequence>
event: connection.state_changed
data: <JSON>
```

The client sends `Last-Event-ID` when reconnecting.

If the requested sequence is no longer retained, the server returns a resync event. The client reloads `/v1/snapshot` and resumes from the latest sequence.

### 13.4 Normalized event types

```text
supervisor.ready
snapshot.changed
connection.created
connection.updated
connection.state_changed
connector.started
connector.exited
operation.created
operation.started
operation.step_started
operation.step_succeeded
operation.step_failed
operation.rolled_back
operation.completed
operation.failed
diagnostic.finding
diagnostic.resolved
traffic.sample
provider.auth_required
provider.capabilities_changed
log.line
```

### 13.5 DTO isolation

IPC DTOs are versioned transport objects. Do not JSON-encode internal structs directly. Conversion functions sit in `internal/ipc/dto.go`.

---

## 14. Local service discovery

### 14.1 Scope

Discovery inspects only the local machine. It never scans the LAN or internet.

Linux v0.1 discovery:

1. Enumerate listening TCP and UDP sockets.
2. Associate sockets with processes where permissions allow.
3. Read executable name, command line, working directory, and owning UID.
4. Probe likely HTTP/TLS services.
5. Classify likely frameworks and MCP endpoints.
6. Return candidates with evidence and confidence.

### 14.2 Listener enumeration

Use `ss` as the initial Linux implementation:

```bash
ss -H -lntup
```

Wrap it behind:

```go
type ListenerEnumerator interface {
    List(context.Context) ([]ListeningSocket, error)
}
```

This keeps `/proc` parsing available as a later replacement without changing the controller or TUI.

### 14.3 Probe limits

- Probe loopback and explicitly local bind addresses only.
- Maximum 128 candidates per refresh.
- Maximum 16 concurrent probes.
- Per-probe timeout: 250 milliseconds.
- No more than one redirect.
- No TLS verification bypass except for local classification, and classification records that verification failed.
- Never send credentials.
- Prefer `HEAD /`; fall back to bounded `GET /`.
- Read at most 16 KiB of response body.

### 14.4 Confidence

```text
Very likely:
  listening socket
  + successful protocol probe
  + process metadata consistent with classification

Likely:
  listening socket
  + successful protocol probe

Possible:
  listening socket only, or incomplete permissions
```

Every label has evidence. Do not infer an application name solely from a port number.

### 14.5 MCP detection

MCP classification evidence may include:

- Process command contains an MCP server package or explicit MCP argument.
- Streamable HTTP endpoint responds with an MCP-compatible content type or handshake behavior.
- SSE endpoint shape is detected.
- User explicitly labels the command as MCP.

MCP detection is advisory. Portico must never claim certainty based only on a process name containing `mcp`.

---

## 15. Provider recommendation

### 15.1 Two-stage algorithm

1. **Hard filter:** Remove providers that cannot satisfy the requested protocol, exposure mode, hostname, protection, or account prerequisites.
2. **Deterministic score:** Rank remaining providers and show the reasons.

### 15.2 Default score

```text
Already authenticated                 +40
Explicit user preference              +25
Own-domain managed DNS support        +20
Required protection is built in       +20
No extra local prerequisite           +15
Useful traffic telemetry              +10
Supports reversible close             +10
Generated temporary address           +10
Experimental/beta constraint          -15
Unknown account entitlement           -15
Known paid-only required capability   -20
```

Do not include hidden commercial preferences. The score is internal; the UI shows reasons, not a numeric provider score.

### 15.3 Recommendation copy

```text
Recommended: Cloudflare

Why:
  Works with your domain
  Supports the protection you selected
  Already connected on this machine
  Can keep the same address when the connector restarts

[ Use Cloudflare ]   [ Compare providers ]
```

If no provider satisfies the requirement, explain the exact conflicting requirement and offer valid changes.

---

## 16. Diagnostics and repair

### 16.1 Segment graph

Every connection is represented as normalized segments:

```text
Local service
Local route
Connector
Provider edge
DNS or provider address
Protection
Public/private probe
```

```go
type SegmentID string

const (
    SegmentLocalService SegmentID = "local_service"
    SegmentLocalRoute   SegmentID = "local_route"
    SegmentConnector    SegmentID = "connector"
    SegmentProviderEdge SegmentID = "provider_edge"
    SegmentAddress      SegmentID = "address"
    SegmentProtection   SegmentID = "protection"
    SegmentEndpoint     SegmentID = "endpoint"
)
```

### 16.2 Probe result

```go
type ProbeResult struct {
    Segment    SegmentID
    Status     ProbeStatus
    Evidence   []Evidence
    StartedAt  time.Time
    FinishedAt time.Time
    Error      *PorticoError
}
```

### 16.3 Finding

```go
type Finding struct {
    ID             FindingID
    ConnectionID   ConnectionID
    Segment        SegmentID
    Code           string
    Summary        string
    Explanation    string
    Evidence       []Evidence
    Confidence     Confidence
    Repairability  Repairability
    SuggestedPlan  *RepairPlan
}
```

### 16.4 Repair policy

A repair plan must:

- Target the first failing causal segment, not every unhealthy symptom.
- Prefer restart or reconciliation over recreation.
- Preserve public addresses where possible.
- Avoid deleting healthy resources.
- State what will and will not change.
- Require explicit confirmation for destructive steps.
- Re-run the relevant probes after completion.

Example:

```text
The local service is responding, but the Cloudflare connector process exited.

Portico can restart the existing connector. The tunnel, DNS record,
Access policy, and public address will not be recreated.

[ Restart connector ]   [ Use another provider ]   [ Details ]
```

### 16.5 Drift examples

| Drift | Smallest repair |
|---|---|
| Connector process stopped | Restart connector |
| Connector token invalid | Re-authenticate, then restart |
| DNS points to wrong tunnel | Correct DNS record only |
| Access policy differs | Update policy only |
| Local service moved ports | Update local route after confirmation |
| Provider tunnel deleted | Recreate tunnel and dependent routing |
| Unknown PID occupies recorded PID | Do not signal; start verified connector |
| Public probe fails but all internal segments pass | Mark provider-edge/endpoint finding and retry observation |

---

## 17. Bubble Tea implementation

### 17.1 Version lock

Initialize with the current v2 module paths and pin exact versions in `go.mod`:

```text
charm.land/bubbletea/v2
charm.land/bubbles/v2
charm.land/lipgloss/v2
github.com/charmbracelet/harmonica
```

Do not mix Bubble Tea v1 and v2 imports.

### 17.2 Bubble Tea's role

Bubble Tea owns:

- Keyboard and mouse messages.
- Window size.
- Client-side navigation.
- Temporary form input.
- Presentation snapshots.
- Animation frame state.
- Rendering.

Bubble Tea does not own:

- Connection lifecycle.
- Provider state.
- Persistent profiles.
- Connector processes.
- Reconciliation.
- Long-running operation truth.

### 17.3 Root model

```go
type Model struct {
    width       int
    height      int
    ready       bool

    screen      ScreenID
    previous    ScreenID
    focus       FocusTarget
    selectedID  core.ConnectionID

    snapshot    SnapshotVM
    providers   []ProviderVM
    discovery   DiscoveryVM

    wizard      WizardModel
    inspect     InspectModel
    plan        PlanModel
    operation   OperationModel
    overlay     OverlayModel
    toast       ToastModel

    keys        KeyMap
    theme       Theme
    layout      Layout
    animation   animation.State

    client      *ipc.Client
    stream      *ipc.EventStream
    lastEventID int64
    err         error
}
```

The model contains view models, not database entities or provider SDK objects.

### 17.4 Initialization

```go
func (m Model) Init() tea.Cmd {
    return tea.Batch(
        requestSnapshotCmd(m.client),
        connectEventStreamCmd(m.client, m.lastEventID),
        tea.RequestBackgroundColor,
    )
}
```

### 17.5 Message types

```go
type snapshotMsg struct{ Snapshot ipc.SnapshotDTO }
type eventStreamConnectedMsg struct{ Stream *ipc.EventStream }
type supervisorEventMsg struct{ Event ipc.EventDTO }
type supervisorEventErrMsg struct{ Err error }
type frameMsg struct{ At time.Time }
type discoveryMsg struct{ Result ipc.DiscoveryDTO }
type planCreatedMsg struct{ Plan ipc.PlanDTO }
type operationStartedMsg struct{ Operation ipc.OperationDTO }
type operationLoadedMsg struct{ Operation ipc.OperationDTO }
type rpcErrMsg struct{ Err error }
```

No anonymous `map[string]any` messages.

### 17.6 Long-lived event subscription

Use a command that waits for one event and returns it:

```go
func waitForEventCmd(stream *ipc.EventStream) tea.Cmd {
    return func() tea.Msg {
        event, err := stream.Next()
        if err != nil {
            return supervisorEventErrMsg{Err: err}
        }
        return supervisorEventMsg{Event: event}
    }
}
```

When `supervisorEventMsg` is handled, schedule `waitForEventCmd` again.

Do not create unmanaged goroutines that call `Program.Send` from component packages.

### 17.7 Update order

```text
1. Handle global terminal messages.
2. Handle snapshot and supervisor events.
3. Handle active operation messages.
4. Handle overlays.
5. Handle current screen keys.
6. Recompute animation demand.
7. Return batched commands.
```

Global quit keys are ignored while a confirmation field is actively editing only when necessary. `ctrl+c` always exits the TUI client, never closes connections.

### 17.8 View

```go
func (m Model) View() tea.View {
    content := renderApp(m)

    v := tea.NewView(content)
    v.AltScreen = true
    v.WindowTitle = "Portico"
    v.ReportFocus = true

    if m.layout.MouseEnabled {
        v.MouseMode = tea.MouseModeCellMotion
    }

    return v
}
```

`View()` must be deterministic for the same model value. It must not call `time.Now()`, open files, query the supervisor, or mutate animation state.

### 17.9 Bubbles usage

Use Bubbles selectively:

- `textinput` for manual addresses, names, hostnames, email rules, and command fields.
- `viewport` for logs, technical details, and long plan output.
- `help` and `key` for context-sensitive footer help.
- `spinner` only while a real operation is waiting and no step progress is available.
- `list` only for provider comparison or discovery if its default chrome is stripped.
- Do not use `table` for the main connection list.
- Do not use progress bars for connection health.

Build the main wizard and connection list as custom components so Portico does not inherit a generic form/dashboard appearance.

---

## 18. TUI screen state machine

```text
Boot
  ├─ supervisor unavailable -> Recovery
  └─ snapshot loaded -> Home

Home
  ├─ N -> NewConnection
  ├─ Enter -> Inspect
  ├─ Space -> PlanOpenOrClose
  ├─ R -> Repair
  └─ P -> Providers

NewConnection
  SourceIntent
    -> DiscoverySelection
    -> ReachabilityOutcome
    -> AddressAndProtection
    -> ProviderRecommendation
    -> PlanPreview
    -> OperationProgress
    -> Completion

Inspect
  ├─ Overview
  ├─ Route
  ├─ Activity
  ├─ Technical
  └─ Logs

Repair
  Diagnose
    -> Finding
    -> RepairPlanPreview
    -> OperationProgress
    -> Verification
```

Back navigation returns to the previous completed wizard step without discarding answers. Editing an earlier answer invalidates downstream recommendations and plans.

---

## 19. Responsive layout

### 19.1 Breakpoints

```text
Wide:       width >= 110
Standard:   width 80–109
Compact:    width 60–79
Emergency:  width < 60
```

### 19.2 Wide home layout

```text
Header: 1 row
Gap: 1 row

Left:
  30 columns
  connection list

Right:
  remaining width
  selected connection title
  route visualization
  measurements
  traffic sparkline
  contextual explanation

Footer: 1–2 rows
```

### 19.3 Standard layout

- Connection list at top, maximum five rows.
- Selected route below.
- One-line footer.
- Technical details move to Inspect.

### 19.4 Compact layout

- Selected connection summary.
- Single-line route.
- One measurement row.
- No sparkline if width is insufficient.
- Discovery and provider lists become full-screen.

### 19.5 Emergency layout

```text
Portico needs at least 60 columns.
y

Selected: TopHat MCP
State: Unstable
Problem: Connector stopped

Resize the terminal or press R to repair.
```

Never panic or emit broken ANSI sequences at small sizes.

---

## 20. Visual system

### 20.1 Palette

Default semantic palette:

```text
Background:       near-black
Primary text:     warm ivory
Structure:        oxidized copper
Stable emphasis:  muted teal
Attention:        amber
Intervention:     red
Muted:            low-contrast warm gray
```

Colors are theme tokens, not embedded literals in components.

```go
type Theme struct {
    Background   color.Color
    Text         color.Color
    Muted        color.Color
    Structure    color.Color
    Stable       color.Color
    Attention    color.Color
    Intervention color.Color
}
```

Every status remains understandable in monochrome.

### 20.2 Shape language

| Meaning | Glyph |
|---|---|
| Open endpoint | `●` |
| Unstable endpoint | `◐` |
| Closed endpoint | `○` |
| Unknown endpoint | `◌` |
| Provider gateway | `◈` |
| Protection checkpoint | `◇` |
| Active route | `━` |
| Inactive route | `─` |
| Planned route | `┄` |
| Degraded route | `┅` |
| Break | `╳` |
| Traffic particle | `•` |

ASCII fallback:

```text
open      *
unstable  o
closed    O
unknown   .
gateway   #
check     +
route     =
inactive  -
planned   .
break     X
particle  >
```

### 20.3 Color rules

- Red is used only for a state requiring intervention.
- Amber indicates attention or degraded operation.
- Muted teal indicates established stable structure, not generic success everywhere.
- Provider identity uses text, not a giant logo or permanent brand color.
- Selection is shown with position, weight, or underline in addition to color.

---

## 21. Route renderer

### 21.1 Pure renderer

```go
func RenderRoute(vm RouteVM, width int, frame animation.Frame) string
```

The renderer accepts normalized route data and returns a string. It has no Bubble Tea dependency except shared color types if necessary.

### 21.2 Route view model

```go
type RouteVM struct {
    LocalLabel       string
    EndpointLabel    string
    ProviderLabel    string
    State            RouteState
    Segments         []RouteSegmentVM
    RedundantPaths   int
    Protection       *CheckpointVM
    DNSDrift         bool
    Measurements     MeasurementVM
    Traffic          TrafficVM
    ActiveFinding    *FindingVM
}
```

### 21.3 Cell canvas

Implement a small cell canvas:

```go
type Cell struct {
    Rune  rune
    Style StyleID
}

type Canvas struct {
    Width  int
    Height int
    Cells  []Cell
}
```

Core operations:

```go
Set(x, y int, r rune, style StyleID)
Text(x, y int, s string, style StyleID)
HLine(x1, x2, y int, r rune, style StyleID)
VLine(x, y1, y2 int, r rune, style StyleID)
Polyline(points []Point, style RouteLineStyle)
Render(theme Theme, profile ColorProfile) string
```

Coordinate glyphs used in the canvas must occupy one terminal cell. Labels are rendered separately and clipped using ANSI-aware width measurement.

### 21.4 Geometry

For the dominant route:

```text
local node x     = 0
gateway center x = width / 2
public node x    = width - 1
center y         = 2 or 3 depending on route height
```

A single route:

```text
●━━━━━━━━━━╮          ╭━━━━━━━━━━●
           ╰━━━ ◈ ━━━╯
```

Redundant routes:

```text
             ╭━━━━━━━━━━━━╮
●━━━━━━━━━━━━┤     ◈      ├━━━━━━━━━━━━●
             ╰━━━━━━━━━━━━╯
```

A failed segment physically stops before the next node:

```text
●━━━━━━━━━━╮
           ╳          ◌ Edge       ○ Public
```

DNS drift offsets the public endpoint vertically and draws a dotted displaced association rather than leaving the route visually intact.

### 21.5 Segment-to-space mapping

```text
Local service  -> local node
Local route    -> first span
Connector      -> gateway entry
Provider edge  -> gateway body
Address/DNS    -> gateway exit to endpoint
Protection     -> checkpoint near endpoint
Endpoint probe -> public/private node
```

A finding always highlights one of these normalized regions.

### 21.6 Traffic encoding

Traffic is illustrative but data-backed:

- Request rate controls particle frequency and count.
- End-to-end latency controls particle travel duration.
- Error rate changes particle shape or terminates particles at the failing segment.
- Maximum visible particles: five.
- Zero recent requests: no particles.
- Unsupported telemetry: show `Traffic unavailable`, never `0/min`.

Do not imply that each particle is one literal request.

### 21.7 Sparkline

Use a fixed one-cell glyph set:

```text
▁▂▃▄▅▆▇█
```

Keep a 60-sample ring buffer. Select the number of samples that fit the available width. Missing samples render as spaces, not zero.

---

## 22. Animation

### 22.1 Animation policy

No global permanent ticker.

Request a frame only when at least one of these is true:

- A route transition is in progress.
- Recent traffic should produce particles.
- An operation step has an actual indeterminate wait.
- A toast is expiring.
- A cursor or component explicitly needs animation.

Idle connections render static.

### 22.2 Frame rates

```text
Route reconstruction: 20 FPS until settled, maximum 1.2 seconds
Traffic particles:      8 FPS for 1.5 seconds after a traffic sample
Indeterminate wait:    10 FPS
Toast expiration:       4 FPS
Idle:                   0 FPS
```

### 22.3 Harmonica use

Use Harmonica only inside `internal/tui/animation` for:

- Segment reveal length.
- Endpoint displacement returning to alignment.
- Gateway reconstruction after connector restart.

Use critically damped or slightly over-damped springs. Do not add bouncing motion to routine state changes.

### 22.4 Scheduler

```go
type Demand struct {
    RouteTransition bool
    TrafficUntil    time.Time
    Spinner         bool
    ToastUntil      time.Time
}

func (s State) NextFrame(now time.Time) (time.Duration, bool)
```

The Update loop schedules exactly one next frame command. Duplicate frame commands are coalesced.

---

## 23. Home screen

```text
┌─ PORTICO ────────────────────────────────────────────────────────┐
│                                                                 │
│  CONNECTIONS                                      3 open        │
│                                                                 │
│  ● TopHat MCP          flowing     84/min                       │
│  ◐ Jekyl Dashboard    unstable      3/min                       │
│  ○ Project Files      closed                                    │
│                                                                 │
│  TOPHAT MCP                                                     │
│                                                                 │
│  localhost:6767                                                 │
│        ●━━━━━━━━━━╮          ╭━━━━━━━━━━●  tophat.example.com   │
│                   ╰━━━ ◈ ━━━╯                                  │
│                       Cloudflare                                │
│                                                                 │
│       3ms local       4 paths       21ms edge       38ms public │
│                                                                 │
│       Traffic     ▁▂▂▃▅▇▅▄▃▂▂▁▃▆█▅▃▂                          │
│                                                                 │
│  N New     Enter Inspect     Space Open/Close     R Repair      │
└─────────────────────────────────────────────────────────────────┘
```

### 23.1 Connection list row

Each row contains only:

- Shape status.
- Name.
- Plain-language state.
- Recent request rate when supported.

No CPU, memory, provider logo, health percentage, or decorative metadata.

### 23.2 Selection

Selection uses:

- A leading structural marker.
- Slight text weight increase.
- Optional muted background strip.
- Color only as a secondary cue.

---

## 24. New connection wizard

### 24.1 Step 1: intent

```text
What should be reachable?

  ● A service already running on this computer
    A folder of files
    A command or application
    An MCP server
    Something else
```

### 24.2 Step 2: discovery

```text
Portico found these services:

  ● localhost:6767    TopHat MCP          Very likely
    localhost:3000    Vite application    Very likely
    localhost:8080    Web service         Possible
    Enter an address manually
```

Pressing `?` shows the evidence used for the confidence label.

### 24.3 Step 3: outcome

```text
How should it be reachable?

  ● Temporarily, with a generated address
    Permanently, with my own hostname
    Privately, only from my devices
```

### 24.4 Step 4: protection

Only show protection options supported by at least one currently viable provider.

```text
Who should be able to reach it?

  ● Anyone with the address
    People who verify their email
    Specific email addresses or domains
    Only devices in my private network
```

### 24.5 Step 5: recommendation

The recommendation is derived after all hard requirements are known.

### 24.6 Step 6: exact plan preview

```text
Portico will:

  1. Verify localhost:6767 is responding
  2. Create Cloudflare tunnel "tophat-mcp"
  3. Route tophat.example.com to localhost:6767
  4. Create the DNS record
  5. Create an email OTP protection policy
  6. Start one cloudflared connector
  7. Verify the public address

Nothing will be deleted.
Estimated retained resources after close:
  Tunnel, DNS record, and Access policy

[ Open connection ]   [ Back ]   [ Technical details ]
```

The button label is the intended outcome, not `Apply`.

---

## 25. Inspect screen

Tabs:

```text
Overview   Route   Activity   Technical   Logs
```

### Overview

- Local service.
- Public/private address.
- Provider.
- Protection.
- Open/closed desired state.
- Current runtime state.
- Last verified time.
- Primary action.

### Route

- Larger route visualization.
- Segment list with evidence.
- Redundant paths.
- Latency measurements.
- Active finding.

### Activity

- Request rate.
- Error count.
- Latency history.
- Operation history.
- Explicit `Unavailable from this provider` states.

### Technical

- Profile ID and revision.
- Provider resource IDs.
- Connector identity.
- Plan fingerprint.
- Exact normalized capability constraints.
- Redacted provider responses where useful.

### Logs

- Supervisor events.
- Connector stdout/stderr.
- Filter by source and severity.
- Bounded viewport.
- No secrets.

---

## 26. CLI contract

The CLI is an expert and automation surface over the supervisor.

```text
portico
portico list [--json]
portico inspect <connection> [--json]
portico open <connection> [--yes] [--json]
portico close <connection> [--yes] [--json]
portico repair <connection> [--yes] [--json]
portico delete <connection> [--yes] [--json]
portico plan open|close|repair|delete <connection> [--json]
portico apply <plan-id> [--yes] [--json]
portico discover [--json]
portico provider list [--json]
portico provider login <provider>
portico doctor [--json]
portico supervisor start|stop|status|logs
```

Rules:

- `--json` produces stable versioned DTOs.
- Mutating commands preview unless `--yes` is provided.
- Even with `--yes`, a plan is created and recorded first.
- CLI commands never bypass the supervisor.
- Prefix matching is allowed only when unambiguous.
- Exit codes are documented and stable.

---

## 27. Cloudflare adapter v0.1

### 27.1 Supported v0.1 outcomes

- Existing local HTTP/HTTPS service.
- Directory served by Portico's built-in local file server.
- Command that launches an HTTP/HTTPS service.
- MCP over Streamable HTTP or HTTP.
- Temporary public HTTP address using Quick Tunnel, subject to capability constraints.
- Permanent custom hostname using a managed Cloudflare Tunnel.
- Cloudflare Access email OTP, allowed emails, allowed domains, IdP, and service token where the donor implementation supports them.
- Managed DNS.
- Connector process ownership.
- Connector and tunnel observation.
- Cloudflared Prometheus telemetry.
- Connector restart repair.
- DNS, route, Access, and tunnel drift repair.
- Optional additional connector replicas on the same machine only after the single-connector path is stable.

### 27.2 Explicit constraints

- Quick Tunnel is development-oriented.
- Quick Tunnel capability must reject workloads requiring SSE.
- v0.1 does not claim raw public TCP or UDP equivalence.
- v0.1 private-only Cloudflare networking is deferred.
- A Cloudflare account and domain are required for permanent custom hostnames.
- Portico-local expiration may close a connection; it does not imply that the provider has a native expiration primitive.

### 27.3 Cloudflare plan steps

Typed step kinds:

```text
cloudflare.validate_account
cloudflare.create_tunnel
cloudflare.configure_route
cloudflare.create_dns_record
cloudflare.create_access_application
cloudflare.create_access_policy
cloudflare.start_connector
cloudflare.verify_connector
cloudflare.verify_endpoint
```

Repairs use targeted variants:

```text
cloudflare.restart_connector
cloudflare.update_route
cloudflare.update_dns_record
cloudflare.update_access_policy
cloudflare.recreate_tunnel
```

### 27.4 Adoption

If a matching Cloudflare resource exists:

- Default behavior is to show it as external.
- Offer adoption only when identity and configuration can be verified.
- Adoption is its own plan.
- Portico never deletes an external resource merely because its name matches.

---

## 28. Planned provider capability view

This table defines adapter intent, not a promise that every feature ships in v0.1.

| Capability | Cloudflare | ngrok | Tailscale | zrok |
|---|---|---|---|---|
| Generated public address | Yes, Quick Tunnel constraints | Yes | Funnel provider hostname | Yes, public share |
| Own custom hostname | Yes | Yes, entitlement-dependent | No for Serve/Funnel | Not generally; provider/self-host dependent |
| Private-only exposure | Future adapter scope | Limited/provider-specific | Yes, Serve/tailnet | Yes, private share |
| HTTP/HTTPS | Yes | Yes | Yes | Yes |
| Raw TCP | Specialized/future | Yes | Yes with constraints | Private share |
| UDP | Future/private scope | Provider-specific | No standard Funnel UDP | Private share |
| Built-in protection | Access | Traffic Policy | Tailnet ACLs for private | Private-share access model |
| Traffic telemetry | cloudflared metrics | Agent/cloud telemetry | Limited normalized telemetry | Adapter-dependent |
| Redundancy | Tunnel connections/replicas | Pooling where available | Not normalized initially | Adapter-dependent |
| Managed DNS | Yes | Domain/API dependent | No custom DNS workflow | No generic managed DNS |
| Persistent provider address | Named tunnel hostname | Reserved domain/endpoint | Device tailnet hostname | Reserved share |
| Self-hostable provider | No | No | Control-plane alternatives outside scope | Yes |

Provider adapters must report account-specific entitlements at runtime rather than hard-coding plan assumptions.

---

## 29. Security requirements

### 29.1 Credentials

Resolution order:

1. Explicit environment variable named by provider account metadata.
2. OS keyring reference.
3. Session-only credential entered interactively.
4. Migration-only legacy credential import with explicit user action.

Do not persist new provider tokens in plaintext.

### 29.2 Redaction

Central redactor handles:

- Authorization headers.
- API tokens.
- Tunnel tokens.
- Service tokens.
- Cookie values.
- User-configured secret environment variables.
- Provider response fields marked sensitive.

Redaction occurs before logs and events are persisted.

### 29.3 Local command safety

- Store executable and args separately.
- Show the exact command before first launch.
- Shell execution is opt-in and visibly marked.
- Filter inherited environment variables.
- Never request `sudo`.
- Run as the current user.
- Validate working directory existence.
- On deletion, never delete a user source directory.

### 29.4 Built-in file serving

- Resolve and pin the root path.
- Reject traversal outside the root.
- Define symlink behavior explicitly; default is no symlink escape.
- Upload and delete are off by default.
- Show a warning before public exposure with upload or delete enabled.
- Apply body and file size limits.

### 29.5 IPC

- Unix socket only by default.
- No TCP listener.
- Same-UID peer validation.
- `0700` parent directory.
- Bounded request bodies.
- Request deadlines.
- Idempotency keys on mutations.

---

## 30. Observability

### 30.1 Structured logging

Use `log/slog`.

Required fields:

```text
component
connection_id
provider_id
operation_id
plan_id
step_id
event_type
error_code
duration_ms
```

### 30.2 Error model

```go
type PorticoError struct {
    Code        string
    Message     string
    Technical   string
    Retryable   bool
    Segment     SegmentID
    Provider    ProviderID
    Cause       error
}
```

Stable error code families:

```text
PTO-CORE-*
PTO-IPC-*
PTO-STORE-*
PTO-PROC-*
PTO-DISC-*
PTO-DIAG-*
PTO-CF-*
PTO-NGROK-*
PTO-TS-*
PTO-ZROK-*
```

User-facing messages are generated from codes plus evidence, not raw provider errors alone.

### 30.3 Performance targets

Release targets:

- Idle supervisor CPU: effectively idle, under 1% on the target machine.
- Idle TUI: no animation tick.
- Typical TUI render: under 8 ms at 120×40.
- Snapshot payload: under 1 MiB.
- Traffic event rate to TUI: at most 1 Hz per visible connection.
- Operation and state events: never coalesced.
- Discovery refresh: bounded and cancelable.
- No unbounded goroutine, channel, event, or log growth.

---

## 31. Testing strategy

### 31.1 Unit tests

- Domain validation.
- Capability matching.
- Provider scoring.
- State transition validation.
- Plan fingerprinting.
- Ownership rules.
- Restart backoff.
- Discovery parsing.
- Probe classification.
- Diagnostic causal selection.
- Route geometry.
- ANSI width clipping.
- Animation scheduler.
- DTO conversion.
- Redaction.

### 31.2 Golden rendering tests

Golden snapshots for:

```text
80x24 open
80x24 unstable connector
80x24 closed
120x35 redundant route
120x35 DNS drift
60x20 compact
50x15 emergency
monochrome
ASCII fallback
operation progress
repair finding
provider recommendation
```

Normalize terminal control sequences before comparison where necessary. Store fixtures in `internal/tui/testdata`.

### 31.3 Controller contract tests

Run the same suite against the mock provider and Cloudflare adapter test doubles:

```text
plan does not mutate
apply follows plan
failed step compensates managed resources
duplicate apply is idempotent
observe returns normalized state
repair touches only failing segment
remove preserves external resources
```

### 31.4 Integration tests

- Supervisor starts and serves Unix socket.
- TUI client reconnects after event-stream interruption.
- Closing TUI leaves connection open.
- Supervisor restart reconstructs runtime.
- Connector crash triggers finding.
- Restart repair preserves public address.
- Database migration from previous fixture.
- Concurrent operations serialize per connection.
- JSON CLI output remains schema-compatible.

### 31.5 End-to-end tests

Use a fake connector and local HTTP service first. Cloudflare live tests are opt-in and use a dedicated test account.

Required E2E:

1. Discover local HTTP service.
2. Create profile.
3. Preview plan.
4. Apply.
5. Observe open.
6. Generate traffic.
7. Render traffic.
8. Kill connector.
9. Diagnose connector failure.
10. Repair.
11. Confirm endpoint and address unchanged.
12. Close TUI.
13. Confirm supervisor and connection remain.
14. Reopen TUI.
15. Close connection.
16. Delete connection.

### 31.6 Quality commands

```bash
go test ./...
go test -race ./...
go vet ./...
staticcheck ./...
govulncheck ./...
```

Add goroutine leak checks to supervisor and event-stream tests.

---

## 32. Implementation phases

No phase begins until the preceding gate passes.

### Phase 0 — Architecture lock

Deliver:

- This specification in the repository.
- ADRs for process model, IPC, persistence, provider contract, and TUI boundary.
- Import-boundary test.
- Go module and CI skeleton.

Gate:

```text
All locked terms and boundaries documented.
No production feature implementation.
CI runs a placeholder test successfully.
```

### Phase 1 — Donor characterization

Allowed scope:

- Tests around existing Flare behavior.
- Legacy command namespace.
- No new TUI.

Deliver:

- Characterization tests for origin startup.
- Cloudflare plan step inventory.
- Rollback tests.
- Session-state fixture set.
- Legacy `portico legacy` path.

Gate:

```text
Legacy Cloudflare path still works.
Current behavior is captured by tests.
No Cloudflare feature has been silently lost.
```

### Phase 2 — Core and mock provider

Allowed scope:

- `core`, `controller`, `provider/mock`.
- In-memory store only.
- No SQLite, real Cloudflare, or TUI animation.

Deliver:

- Profile/runtime split.
- Capability model.
- Plan/apply/observe/repair/remove.
- Operation event stream.
- Mock provider vertical slice.

Gate:

```text
A mock connection can be planned, opened, observed, broken,
repaired, closed, and deleted through controller tests.
```

### Phase 3 — Store and supervisor

Allowed scope:

- SQLite.
- Unix socket API.
- SSE.
- Process manager with fake connector.

Deliver:

- Migrations.
- Supervisor startup/recovery.
- HTTP/SSE client.
- CLI JSON smoke path.

Gate:

```text
A client can disconnect and reconnect without changing connection state.
Supervisor restart reconstructs the fake connection.
Only supervisor opens the database.
```

### Phase 4 — Static TUI vertical slice

Allowed scope:

- Bubble Tea root model.
- Home screen.
- Static route renderer.
- Mock provider data.
- No real Cloudflare.
- No animation.

Deliver:

- Responsive layout.
- Monochrome and ASCII mode.
- Golden snapshots.
- Key map.
- Inspect and plan preview skeletons.

Gate:

```text
All target terminal sizes render correctly.
The UI is understandable without color.
No generic card grid exists.
```

### Phase 5 — TUI connected to supervisor

Allowed scope:

- IPC commands.
- Event subscription.
- Wizard using mock provider.
- Operation progress.

Deliver:

- End-to-end mock workflow entirely inside `portico`.
- TUI exit leaves mock connector open.
- Reconnect resync.
- Exact plan preview.

Gate:

```text
The complete user journey works without a provider SDK in the TUI.
```

### Phase 6 — Cloudflare adapter extraction

Allowed scope:

- Move donor Cloudflare behavior behind provider interface.
- No ngrok, Tailscale, or zrok implementation.

Deliver:

- Permanent hostname flow.
- Access protection.
- DNS.
- Connector ownership.
- Observation.
- Rollback.
- Temporary Quick Tunnel with constraints.

Gate:

```text
Cloudflare path reaches feature parity with the retained donor behavior.
Legacy path and new path produce equivalent managed outcomes.
```

### Phase 7 — Discovery and recommendation

Deliver:

- Linux listener discovery.
- HTTP/TLS probes.
- confidence/evidence.
- Intent-first wizard.
- Deterministic provider recommendation.

Gate:

```text
A running local service can be selected without typing its port.
Manual entry remains available.
Unsupported combinations are filtered before planning.
```

### Phase 8 — Diagnostics and repair

Deliver:

- Segment graph.
- Connector failure.
- DNS drift.
- Access drift.
- Local service failure.
- Plain-language findings.
- Targeted repair plans.

Gate:

```text
Killing cloudflared produces a connector-segment failure.
Repair restarts only the connector and preserves address/resources.
```

### Phase 9 — Motion and telemetry

Deliver:

- Cloudflared metrics normalization.
- traffic ring buffer.
- particles.
- Harmonica reconstruction.
- zero-tick idle scheduler.

Gate:

```text
Traffic creates bounded motion.
Idle creates no frame loop.
Animations do not alter layout dimensions.
```

### Phase 10 — Hardening and release

Deliver:

- Credential migration.
- redaction audit.
- upgrade tests.
- packaging.
- man pages.
- shell completion.
- VHS demos.
- full E2E.

Gate:

```text
All Definition of Done items pass.
Legacy path is removed only after parity is proven.
```

---

## 33. Anti-thrash rules

### 33.1 Locked decisions

The following cannot change without an ADR:

- One binary with a local supervisor mode.
- Unix socket HTTP/SSE IPC for v0.1.
- SQLite owned only by the supervisor.
- Profile/runtime separation.
- Immutable plans before mutations.
- Provider adapter boundary.
- Linux-first release scope.
- Bubble Tea v2.
- Plain user vocabulary.
- Route visualization as the dominant UI.
- No generic plugin loader.
- No YAML-first workflow.

### 33.2 Change procedure

A proposed change to a locked decision must include:

1. Current problem demonstrated by a failing test or concrete blocked requirement.
2. Alternatives considered.
3. Affected packages and public types.
4. Data migration impact.
5. IPC compatibility impact.
6. UI impact.
7. Rollback plan.
8. Explicit acceptance.

No architecture change is made merely because a coding agent prefers a different abstraction.

### 33.3 Vertical-slice rule

At all times, preserve one working path:

```text
local HTTP service
-> saved profile
-> plan preview
-> mock or Cloudflare apply
-> supervisor-owned connector
-> open route in TUI
-> close
```

Do not break the slice for more than one commit.

### 33.4 Interface rule

Do not introduce an interface until:

- There are two implementations, or
- It is one of the locked boundaries in this specification.

The provider, store, process, discovery, and IPC client boundaries are pre-approved. Generic screen, component, animation, and repository interfaces are not.

### 33.5 Renaming rule

After Phase 0:

- No broad package renames.
- No terminology renames without migration.
- No aesthetic type renames mixed with feature work.
- One mechanical rename per commit at most.

### 33.6 UI rule

Build in this order:

1. Monochrome static layout.
2. Responsive behavior.
3. Golden tests.
4. Semantic colors.
5. Traffic data.
6. Animation.

Do not animate an unstable layout.

### 33.7 Provider rule

Implement only:

```text
mock
cloudflare
```

until the provider-neutral controller passes all contract tests. ngrok, Tailscale, and zrok begin as capability documents and empty adapter packages only.

### 33.8 No speculative systems

Do not add:

- Dependency injection containers.
- Event sourcing as a product architecture.
- A plugin runtime.
- A generic workflow DSL.
- A custom terminal graphics engine beyond the small route canvas.
- Remote daemon support.
- Embedded web UI.
- Provider migration orchestration.

unless a released requirement makes it necessary.

### 33.9 Agent work rule

For coding agents:

- One agent or worker owns mutations to a package at a time.
- Every task names allowed files.
- Every task has a test gate.
- Reviewers do not rewrite architecture; they identify contract violations.
- Failed gates return to the same phase.
- No parallel work on shared core types.
- UI and provider implementation may proceed in parallel only after DTO and interface locks.

---

## 34. Definition of Done

Portico v0.1 is complete only when all items pass.

### Product behavior

- `portico` launches the TUI with no required flags.
- A local HTTP service is discovered.
- A user can create a permanent Cloudflare connection through intent-first steps.
- A user can create a supported temporary connection.
- A user can configure protection without knowing Cloudflare terminology.
- Closing the TUI leaves open connections running.
- Reopening the TUI restores the same state.
- Open, close, repair, and delete are distinct.
- Every mutation has a previewed plan.
- Technical details remain available.
- Unsupported provider semantics are not shown as valid options.

### Supervisor

- Single instance lock works.
- Unix socket permissions are correct.
- Database migrations are automatic and tested.
- Supervisor restart reconstructs runtime.
- Connector identity prevents signaling unrelated processes.
- Connector logs rotate.
- Events resume or resync correctly.
- Per-connection operation serialization works.
- Idempotency works.

### Cloudflare

- Account validation.
- Named tunnel creation.
- Route configuration.
- DNS creation/update.
- Access application and policy.
- Connector start/stop/restart.
- Observation.
- Rollback.
- Quick Tunnel constraints.
- Connector operational telemetry (reachability, restart count, state);
  cloudflared traffic metrics are a later-phase item, not v0.1 DoD.
- Drift detection.
- Managed-resource ownership.
- Address-preserving connector repair.

### Diagnostics

- Local service stopped.
- Connector stopped.
- Provider auth invalid.
- DNS drift.
- Access drift.
- Provider resource missing.
- Public verification failure.
- Plain-language explanation.
- Smallest safe repair.
- Post-repair verification.

### TUI

- 120×35, 80×24, 60-column compact, and under-60 emergency modes.
- Monochrome mode.
- ASCII fallback.
- No generic card grid.
- No permanent spinner.
- No idle animation loop.
- Status distinguishable without color.
- Route breaks at the correct segment.
- DNS drift is spatially visible.
- Protection checkpoint is visible.
- Traffic motion is bounded.
- Help reflects the current screen.
- `ctrl+c` exits only the client.

### Security

- No plaintext new credentials.
- No token in logs, plans, events, snapshots, or errors.
- No arbitrary shell use for provider commands.
- No process killing by PID alone.
- No source directory deletion.
- Unix socket is same-user only.
- Built-in file server blocks traversal.

### Quality

- Unit, integration, E2E, golden, race, static analysis, and vulnerability checks pass.
- No known goroutine leaks.
- No unbounded queues.
- No provider-specific imports in core, TUI, or CLI.
- No TUI direct database access.
- Documentation matches behavior.
- Legacy Flare path removed only after new-path parity.

---

## 35. Exact first implementation tickets

Execute these in order.

### Ticket 001 — Freeze documentation

Files:

```text
docs/architecture/PORTICO_SPEC.md
docs/adr/0001-local-supervisor.md
docs/adr/0002-http-sse-unix-socket.md
docs/adr/0003-profile-runtime-split.md
docs/adr/0004-provider-plan-contract.md
docs/adr/0005-bubble-tea-client-boundary.md
```

Gate: docs review only.

### Ticket 002 — Mechanical rename

Files:

```text
go.mod
cmd/*
main.go
README.md
```

Gate: existing tests pass; no behavior changes.

### Ticket 003 — Preserve legacy command

Move current Cobra tree beneath `portico legacy`.

Gate: legacy commands behave identically.

### Ticket 004 — Core types

Implement tagged unions, validation, states, errors, IDs.

Gate: unit tests and JSON round trips.

### Ticket 005 — Capability model

Implement constraints and matching.

Gate: table-driven tests including SSE Quick Tunnel rejection.

### Ticket 006 — Plans

Implement immutable plans, canonical hashing, ownership, compensations.

Gate: mutation-free planning test.

### Ticket 007 — Mock provider

Implement all provider methods.

Gate: full controller lifecycle test.

### Ticket 008 — Controller

Implement planning, apply, observe, repair, remove, idempotency.

Gate: mock vertical slice.

### Ticket 009 — SQLite store

Implement migrations and repositories.

Gate: restart and migration fixture tests.

### Ticket 010 — Supervisor and IPC

Implement socket, API, SSE, startup lock, re-exec.

Gate: client disconnect/reconnect test.

### Ticket 011 — Fake process manager

Implement process identity and fake connector.

Gate: crash and restart tests.

### Ticket 012 — Static TUI shell

Implement Bubble Tea root model and responsive layout.

Gate: golden tests, no supervisor mutation yet.

### Ticket 013 — Route canvas

Implement monochrome open/closed/degraded/broken routes.

Gate: route golden tests.

### Ticket 014 — TUI IPC integration

Implement snapshot, event stream, selection, open/close plan.

Gate: full mock flow in TUI.

### Ticket 015 — Cloudflare adapter extraction

Move donor code behind adapter one operation type at a time.

Gate: parity contract suite.

### Ticket 016 — Discovery

Implement `ss`, probes, classification, evidence.

Gate: fixture and local integration tests.

### Ticket 017 — Wizard

Implement intent, discovery, outcome, protection, recommendation, plan.

Gate: beginner E2E.

### Ticket 018 — Diagnostics

Implement segment probes and connector/DNS/Access findings.

Gate: fault injection suite.

### Ticket 019 — Telemetry

Implement Cloudflare metrics and ring buffers.

Gate: bounded one-Hz normalized samples.

### Ticket 020 — Motion

Implement scheduler, particles, and reconstruction.

Gate: zero CPU frame loop while idle; animation goldens or deterministic frame tests.

### Ticket 021 — Hardening

Credential migration, redaction, packaging, docs, completion.

Gate: complete Definition of Done.

---

## 36. Coding-agent execution header

Use this at the top of every implementation task:

```text
You are implementing one bounded Portico ticket.

Authority:
- docs/architecture/PORTICO_SPEC.md
- applicable ADRs
- existing tests

Rules:
1. Modify only the allowed files named in the ticket.
2. Do not rename public types or packages outside the ticket.
3. Do not introduce a new abstraction unless the spec requires it.
4. Do not move provider logic into core, TUI, CLI, or IPC DTOs.
5. Do not let TUI or CLI access the database or provider directly.
6. Preserve the working vertical slice.
7. Add or update tests for every behavior change.
8. Run the ticket gate commands.
9. Report exact files changed, tests run, and remaining failures.
10. Stop rather than broadening scope when the ticket cannot be completed
    without changing a locked decision.
```

This is the anti-edit-thrash operating contract. The implementation should feel boring in the best way: each phase establishes one layer, proves it, and then stops changing its shape.
