package ipc

// DTO types for the IPC transport layer.
// These are versioned transport objects, not direct JSON exports of internal structs.

// --------------- snapshot ---------------

// SnapshotDTO is the full state returned by GET /v1/snapshot.
type SnapshotDTO struct {
	Connections   []ConnectionDTO `json:"connections"`
	Providers     []ProviderDTO   `json:"providers"`
	LastSeq       int64           `json:"last_seq"`
	SupervisorPID int             `json:"supervisor_pid"`
}

// ConnectionDTO is the view of a connection sent over IPC.
type ConnectionDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is what sort of connection this is. Without it a client cannot tell
	// a port forward from a published service, and every list and detail screen
	// has to guess — which they did, always guessing "exposed to the internet".
	Kind              string `json:"kind,omitempty"`
	DesiredState      string `json:"desired_state"`
	RuntimeState      string `json:"runtime_state"`
	UserState         string `json:"user_state"`
	PublicAddress     string `json:"public_address,omitempty"`
	PrivateAddress    string `json:"private_address,omitempty"`
	ProviderID        string `json:"provider_id"`
	ProviderAccountID string `json:"provider_account_id,omitempty"`
	ConnectorPID      int    `json:"connector_pid,omitempty"`
	ConnectorState    string `json:"connector_state"`
	Error             string `json:"error,omitempty"`
	// Repairable reports that the supervisor can produce a repair plan for this
	// connection's kind. It is here so a client offers repair exactly where the
	// backend supports it: the TUI previously kept its own list of repairable
	// kinds, which is a second answer to a question the controller already
	// answers, and the two drifted the moment a kind became repairable.
	Repairable bool `json:"repairable,omitempty"`
}

// OriginOwnershipDTO says what happens to the local service when the connection
// is closed or deleted.
//
// Nothing said whether Portico had started the local service or merely
// connected to one that was already running. That is what a user needs before
// assuming that closing a connection will not stop their server — or, the other
// way, before assuming it will.
type OriginOwnershipDTO struct {
	// Kind is "external" when the service was already running, "portico_managed"
	// when Portico started it, or "client_managed" when a platform's client
	// runs it.
	Kind string `json:"kind"`
	// StartsWithOpen reports that opening the connection starts the service.
	StartsWithOpen bool `json:"starts_with_open"`
	// StopsWithClose and StopsWithDelete report what closing and deleting do
	// to it. They are separate because they are not always the same answer.
	StopsWithClose  bool `json:"stops_with_close"`
	StopsWithDelete bool `json:"stops_with_delete"`
	// Description states the consequence in a sentence.
	Description string `json:"description,omitempty"`
}

// ConnectionDetailDTO is the full detail view of a connection for inspect screens.
type ConnectionDetailDTO struct {
	Summary       ConnectionDTO        `json:"summary"`
	Revision      uint64               `json:"revision"`
	DesiredSpec   ConnectionSpecDTO    `json:"desired_spec"`
	Lifecycle     LifecycleDTO         `json:"lifecycle"`
	Origin        OriginOwnershipDTO   `json:"origin"`
	Driver        DriverSelectionDTO   `json:"driver"`
	Endpoints     []EndpointDTO        `json:"endpoints,omitempty"`
	Segments      []RouteSegmentDTO    `json:"segments,omitempty"`
	Resources     []ManagedResourceDTO `json:"resources,omitempty"`
	Processes     []ProcessDTO         `json:"processes,omitempty"`
	Gateway       *GatewayDTO          `json:"gateway,omitempty"`
	Health        *HealthDTO           `json:"health,omitempty"`
	Findings      []DiagnosticDTO      `json:"findings,omitempty"`
	LastVerified  string               `json:"last_verified,omitempty"`
	LastOperation *OperationSummaryDTO `json:"last_operation,omitempty"`
	CreatedAt     string               `json:"created_at"`
	UpdatedAt     string               `json:"updated_at"`
}

// ConnectionSpecDTO describes the connection specification.
//
// It is a tagged union mirroring core.ConnectionSpec: exactly one arm is
// populated, and Kind says which. It previously carried the service-exposure
// fields flat, so a port forward crossed the wire as a service exposure with an
// empty source, an empty exposure mode and protection "", which readers then
// rendered as an unprotected public service.
type ConnectionSpecDTO struct {
	Kind            string                  `json:"kind"`
	ServiceExposure *ServiceExposureSpecDTO `json:"service_exposure,omitempty"`
	PortForward     *PortForwardDTO         `json:"port_forward,omitempty"`
	PrivateNetwork  *PrivateNetworkSpecDTO  `json:"private_network,omitempty"`
	ClientTunnel    *ClientTunnelSpecDTO    `json:"client_tunnel,omitempty"`
}

// ServiceExposureSpecDTO describes a local service published through a provider.
type ServiceExposureSpecDTO struct {
	Source     SourceDTO     `json:"source"`
	Exposure   ExposureDTO   `json:"exposure"`
	Protection ProtectionDTO `json:"protection"`
}

// PrivateNetworkSpecDTO describes a private network membership.
type PrivateNetworkSpecDTO struct {
	NetworkID   string `json:"network_id"`
	Mode        string `json:"mode,omitempty"` // "join" or "expose"
	ExposeLocal bool   `json:"expose_local,omitempty"`
}

// ClientTunnelSpecDTO describes a client-mediated tunnel with no public address.
type ClientTunnelSpecDTO struct {
	Client   string       `json:"client"`
	TunnelID string       `json:"tunnel_id,omitempty"`
	Profile  string       `json:"profile,omitempty"`
	MCP      MCPSourceDTO `json:"mcp"`
}

// DriverSelectionDTO describes the driver/provider choice.
type DriverSelectionDTO struct {
	ProviderID string            `json:"provider_id"`
	AccountID  string            `json:"account_id,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
}

// EndpointDTO describes an observed endpoint.
type EndpointDTO struct {
	Address  string `json:"address"`
	Protocol string `json:"protocol,omitempty"`
	Public   bool   `json:"public"`
}

// RouteSegmentDTO describes one route segment.
type RouteSegmentDTO struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Label  string `json:"label,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ManagedResourceDTO describes a managed provider resource.
type ManagedResourceDTO struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	ExternalID string            `json:"external_id"`
	Ownership  string            `json:"ownership"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// ProcessDTO describes a managed process.
type ProcessDTO struct {
	PID            int    `json:"pid"`
	ConnectionID   string `json:"connection_id"`
	ExecutablePath string `json:"executable_path,omitempty"`
	Status         string `json:"status"`
}

// GatewayDTO describes the Portico Gateway for a connection.
// The gateway is a local HTTP proxy that authenticates clients and forwards
// requests through the transport tunnel. It is nil when the connection
// does not use a gateway.
//
// SECURITY: Plaintext credentials are never sent over IPC.
type GatewayDTO struct {
	Endpoint      string `json:"endpoint"`
	Upstream      string `json:"upstream"`
	AuthEnabled   bool   `json:"auth_enabled"`
	CredentialRef string `json:"credential_ref,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
}

// HealthDTO describes the three-state health assessment for a connection.
// Health is evaluated relative to the connection's desired state and kind.
type HealthDTO struct {
	State      string         `json:"state"`
	Process    HealthCheckDTO `json:"process"`
	Transport  HealthCheckDTO `json:"transport"`
	Service    HealthCheckDTO `json:"service"`
	ComputedAt string         `json:"computed_at,omitempty"`
}

// HealthCheckDTO is a single health check result.
type HealthCheckDTO struct {
	State       string `json:"state"`
	Detail      string `json:"detail,omitempty"`
	LastChecked string `json:"last_checked,omitempty"`
}

// OperationSummaryDTO is a brief operation view for connection detail.
type OperationSummaryDTO struct {
	ID        string `json:"id"`
	Intent    string `json:"intent"`
	State     string `json:"state"`
	StartedAt string `json:"started_at"`
}

// ProviderDTO is the view of a provider sent over IPC.
type ProviderDTO struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DisplayName   string `json:"display_name"`
	Authenticated bool   `json:"authenticated"`
	// Accounts are usable for selection and planning.
	Accounts []ProviderAccountDTO `json:"accounts,omitempty"`
	// PendingAccounts are saved but not usable — never verified, expired or
	// revoked. They are carried separately so a UI can offer to repair them
	// without any selection path treating them as working accounts.
	PendingAccounts []ProviderAccountDTO `json:"pending_accounts,omitempty"`
	// Selectable reports whether a connection can be planned against this
	// provider right now.
	//
	// It is decided once, by the supervisor, because clients were each
	// reinterpreting the availability string and disagreeing: the
	// recommendation engine refused experimental and degraded providers while
	// the wizard's fallback list refused only missing clients, so a failed
	// recommendation offered providers the engine considers unusable. A
	// provider can also be worth showing while not being selectable — one that
	// needs setup, for instance — and one string could not carry both facts.
	Selectable      bool              `json:"selectable"`
	Availability    string            `json:"availability"`        // "ready", "unconfigured", "binary_missing", "degraded"
	Readiness       string            `json:"readiness"`           // "ready", "needs_auth", "needs_config", "error"
	Stability       string            `json:"stability,omitempty"` // "stable", "beta", "experimental"
	Capabilities    *CapabilitySetDTO `json:"capabilities,omitempty"`
	LastError       string            `json:"last_error,omitempty"`
	RestartRequired bool              `json:"restart_required,omitempty"`
	// SetupActions are concrete steps that would make this provider usable.
	// They accompany an unavailable provider so the UI can offer a next step
	// instead of only reporting a gap.
	SetupActions []string `json:"setup_actions,omitempty"`
}

// CapabilitySetDTO describes provider capabilities in a versioned, serializable form.
type CapabilitySetDTO struct {
	Kinds              []string `json:"kinds"`
	TemporaryAddresses bool     `json:"temporary_addresses"`
	CustomHostnames    bool     `json:"custom_hostnames"`
	PrivateExposure    bool     `json:"private_exposure"`
	ManagedDNS         bool     `json:"managed_dns"`
	ProtectionModes    []string `json:"protection_modes"`
	Protocols          []string `json:"protocols"`
	Stability          string   `json:"stability,omitempty"` // "stable", "beta", "experimental"
	TelemetrySupported bool     `json:"telemetry_supported"`
	MaxConnectors      int      `json:"max_connectors"`
	ExpirationMaxSecs  int      `json:"expiration_max_secs,omitempty"`
}

// ProviderAccountDTO is a selectable, non-secret provider account summary.
type ProviderAccountDTO struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Status         string `json:"status"`
	UnusableReason string `json:"unusable_reason,omitempty"`
}

// ConfigureProviderAccountRequest carries one provider credential over the
// authenticated local Unix socket. Callers must obtain Credential from stdin,
// an environment reference, or a protected file descriptor; never a command
// argument. The value is not included in responses or durable events.
type ConfigureProviderAccountRequest struct {
	AccountID  string `json:"account_id"`
	Label      string `json:"label,omitempty"`
	ZoneID     string `json:"zone_id,omitempty"`
	Credential string `json:"credential"`
	// Fields carries values for a provider's declared setup fields, keyed by
	// SetupFieldDTO.ID. It exists so a provider Portico has no built-in
	// knowledge of can still be configured.
	//
	// When empty, the named fields above are mapped onto the reserved IDs
	// instead, which keeps existing callers working.
	Fields map[string]string `json:"fields,omitempty"`
}

// ConfigureProviderAccountResponse tells clients whether a supervisor restart
// is needed before newly persisted account adapters become available.
type ConfigureProviderAccountResponse struct {
	RestartRequired bool `json:"restart_required"`
	// Validated reports that the credential was confirmed against the provider
	// before the account was saved, rather than accepted on faith.
	Validated bool `json:"validated,omitempty"`
	// CapabilityLevel names what the saved account can actually do, since a
	// zone is required only for DNS and custom hostnames.
	CapabilityLevel string `json:"capability_level,omitempty"`
	// Zones the credential can see, so a zone can be chosen rather than copied
	// by hand.
	Zones []ZoneDTO `json:"zones,omitempty"`
	// MissingPermissions names the specific permissions the token lacks.
	MissingPermissions []string `json:"missing_permissions,omitempty"`
	// Status is the account status actually recorded.
	Status string `json:"status,omitempty"`
	// VerificationUnavailable explains why the credential could not be
	// checked, when it could not. An account saved this way is recorded as
	// pending rather than authenticated, and callers must say so rather than
	// showing it as ready — an unchecked credential presented as a working one
	// is how Portico came to advertise providers that could not perform a
	// single operation.
	VerificationUnavailable string `json:"verification_unavailable,omitempty"`
}

// ZoneDTO is a DNS zone visible to a provider credential.
type ZoneDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ReverifyProviderAccountRequest re-verifies an existing account's credential
// without changing it. The credential is never exposed to the caller.
type ReverifyProviderAccountRequest struct {
	AccountID string `json:"account_id"`
}

// ReverifyProviderAccountResponse reports the outcome of re-verification.
type ReverifyProviderAccountResponse struct {
	// Validated reports that the credential was confirmed against the provider
	// before the account was saved, rather than accepted on faith.
	Validated bool   `json:"validated,omitempty"`
	Status    string `json:"status,omitempty"`
	// VerificationUnavailable explains why the credential could not be checked,
	// when it could not.
	VerificationUnavailable string `json:"verification_unavailable,omitempty"`
}

// ReplaceCredentialRequest rotates the secret behind an existing account.
//
// The account identity (provider_id, account_id) is not part of the body: it is
// in the path, and it is deliberately not changeable here. Every connection
// stores that identity, so replacing a credential must keep it — otherwise the
// connections would still point at the account whose credential was replaced.
type ReplaceCredentialRequest struct {
	// Credential is the new secret. Like every other credential field it must
	// come from stdin, an environment reference, or a protected descriptor —
	// never a command argument — and it is never echoed back.
	Credential string `json:"credential"`
}

// ReplaceCredentialResponse reports the outcome of a rotation.
//
// It carries no credential and no derivative of one: only whether the new
// secret was validated, what the account's status is now, and whether the
// provider could be reloaded in place.
type ReplaceCredentialResponse struct {
	AccountID string `json:"account_id,omitempty"`
	// Validated reports that the new credential was confirmed against the
	// provider before it replaced the old one.
	Validated bool   `json:"validated,omitempty"`
	Status    string `json:"status,omitempty"`
	// VerificationUnavailable explains why the new credential could not be
	// checked, when Portico has no way to check it for this provider.
	VerificationUnavailable string `json:"verification_unavailable,omitempty"`
	// RestartRequired reports that the credential is stored but the running
	// provider could not be reloaded, so it is not yet in force.
	RestartRequired bool `json:"restart_required,omitempty"`
}

// --------------- events ---------------

// EventDTO is an event delivered via SSE.
type EventDTO struct {
	Sequence     int64  `json:"seq"`
	OperationID  string `json:"operation_id,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
	Type         string `json:"type"`
	Stage        string `json:"stage,omitempty"`
	Timestamp    string `json:"timestamp"`

	// Typed payloads — at most one will be non-nil per event.
	Operation  *OperationEventDTO  `json:"operation,omitempty"`
	Connection *ConnectionEventDTO `json:"connection,omitempty"`
	Provider   *ProviderEventDTO   `json:"provider,omitempty"`
	Diagnostic *DiagnosticEventDTO `json:"diagnostic,omitempty"`

	// Legacy untyped data for backward compatibility.
	// Deprecated: use typed fields above.
	Data interface{} `json:"data,omitempty"`
}

// OperationEventDTO describes an operation lifecycle event.
type OperationEventDTO struct {
	OperationID string `json:"operation_id"`
	Intent      string `json:"intent,omitempty"`
	State       string `json:"state,omitempty"`
	StepID      string `json:"step_id,omitempty"`
	StepSummary string `json:"step_summary,omitempty"`
	Error       string `json:"error,omitempty"`
}

// ConnectionEventDTO describes a connection state change event.
type ConnectionEventDTO struct {
	ConnectionID string `json:"connection_id"`
	DesiredState string `json:"desired_state,omitempty"`
	RuntimeState string `json:"runtime_state,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ProviderEventDTO describes a provider state change event.
type ProviderEventDTO struct {
	ProviderID string `json:"provider_id"`
	AccountID  string `json:"account_id,omitempty"`
	Available  bool   `json:"available"`
	LastError  string `json:"last_error,omitempty"`
}

// DiagnosticEventDTO describes a diagnostic finding or resolution.
type DiagnosticEventDTO struct {
	ConnectionID string `json:"connection_id"`
	FindingID    string `json:"finding_id"`
	Segment      string `json:"segment,omitempty"`
	Severity     string `json:"severity,omitempty"`
	Summary      string `json:"summary,omitempty"`
	Resolved     bool   `json:"resolved"`
}

// --------------- plans ---------------

// PlanDTO is the plan preview sent over IPC.
type PlanDTO struct {
	ID           string    `json:"id"`
	ConnectionID string    `json:"connection_id"`
	Intent       string    `json:"intent"`
	Provider     string    `json:"provider"`
	Steps        []StepDTO `json:"steps"`
	Warnings     []string  `json:"warnings,omitempty"`
	Fingerprint  string    `json:"fingerprint"`
	Noop         bool      `json:"noop,omitempty"`

	// A preview must state consequences before implementation terminology.
	// These describe, in plain language, what the plan will achieve and what it
	// will change; the step list remains available as the exact technical plan.

	// Outcome is what the user will be able to do once the plan succeeds.
	Outcome string `json:"outcome,omitempty"`
	// Access states who will be able to reach the service.
	Access string `json:"access,omitempty"`
	// LocalChanges are the changes Portico will make on this machine.
	LocalChanges []string `json:"local_changes,omitempty"`
	// ProviderChanges are the resources Portico will create or remove at the
	// provider.
	ProviderChanges []string `json:"provider_changes,omitempty"`
	// Reversibility explains what closing or deleting will and will not undo.
	Reversibility []string `json:"reversibility,omitempty"`
}

// StepDTO is a step in a plan preview.
type StepDTO struct {
	ID           string `json:"id"`
	Kind         string `json:"kind,omitempty"`
	Summary      string `json:"summary"`
	Destructive  bool   `json:"destructive"`
	Irreversible bool   `json:"irreversible"`
	// Execution state (populated during operation execution)
	State       string `json:"state,omitempty"` // pending, running, succeeded, failed, compensated, skipped
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
	Error       string `json:"error,omitempty"`
}

// --------------- operations ---------------

// OperationDTO is an operation state sent over IPC.
type OperationDTO struct {
	ID           string    `json:"id"`
	PlanID       string    `json:"plan_id"`
	ConnectionID string    `json:"connection_id"`
	State        string    `json:"state"`
	Steps        []StepDTO `json:"steps,omitempty"`
	StartedAt    string    `json:"started_at"`
	CompletedAt  string    `json:"completed_at,omitempty"`
	Error        string    `json:"error,omitempty"`

	// Identity of the plan this operation executed. These live on the plan
	// rather than the operation row, so a caller that only has an operation
	// cannot otherwise say what the operation was doing.
	Intent          string `json:"intent,omitempty"`
	ProviderID      string `json:"provider_id,omitempty"`
	Fingerprint     string `json:"fingerprint,omitempty"`
	ProfileRevision uint64 `json:"profile_revision,omitempty"`
}

// --------------- errors ---------------

// APIError is a structured error response.
type APIError struct {
	Version          int              `json:"version"`
	Code             string           `json:"code"`
	Summary          string           `json:"summary"`
	Explanation      string           `json:"explanation,omitempty"`
	Retryable        bool             `json:"retryable"`
	ConnectionID     string           `json:"connection_id,omitempty"`
	Segment          string           `json:"segment,omitempty"`
	ProviderID       string           `json:"provider_id,omitempty"`
	RecoveryActions  []RecoveryAction `json:"recovery_actions,omitempty"`
	TechnicalDetails string           `json:"technical_details,omitempty"`

	// Typed details for refusals a caller has to present as more than a
	// sentence. Encoding domain data into recovery-action strings would make
	// every consumer parse prose back into structure.
	AccountDependencies []AccountDependencyDTO     `json:"account_dependencies,omitempty"`
	ProviderValidation  *ProviderValidationDetails `json:"provider_validation,omitempty"`
	// AccountRemovalPreview is the current state of an account whose removal
	// was refused as stale, so the caller can re-confirm against what is now
	// true instead of being told only that its preview expired.
	AccountRemovalPreview *AccountRemovalPreviewDTO `json:"account_removal_preview,omitempty"`
}

// ProviderValidationDetails explains why a credential was rejected.
//
// A caller needs to say which permissions are missing, not merely that the
// token was refused — the user cannot act on "validation failed".
type ProviderValidationDetails struct {
	MissingPermissions []string  `json:"missing_permissions,omitempty"`
	AvailableZones     []ZoneDTO `json:"available_zones,omitempty"`
	AccountAccessible  bool      `json:"account_accessible"`
	VerificationState  string    `json:"verification_state,omitempty"`
}

// RecoveryAction describes a recovery action.
type RecoveryAction struct {
	Label   string `json:"label"`
	Action  string `json:"action"`
	Primary bool   `json:"primary"`
}

// --------------- connections ---------------

// CreateConnectionRequest is the request body for POST /v1/connections.
// It uses a versioned tagged union so that new source, exposure, protection,
// provider, and lifecycle fields can be added without breaking the contract.
//
// Exactly one spec arm must be populated for kinds other than service_exposure.
// The tagged union is validated server-side so a malformed request is refused
// with a reason rather than being silently coerced into a service_exposure
// connection.
type CreateConnectionRequest struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	// Kind selects the connection kind. It defaults to service exposure, so
	// existing callers are unaffected.
	Kind string `json:"kind,omitempty"`
	// ServiceExposure is the only kind that carries flat source/exposure/protection
	// fields for backward compatibility. Use the corresponding spec arm for
	// other kinds.
	Source      SourceDTO            `json:"source"`
	Exposure    ExposureDTO          `json:"exposure"`
	Protection  ProtectionDTO        `json:"protection"`
	Provider    ProviderSelectionDTO `json:"provider"`
	Lifecycle   LifecycleDTO         `json:"lifecycle"`
	PortForward *PortForwardDTO      `json:"port_forward,omitempty"`
	// PrivateNetwork carries the specification for a private network connection.
	// It is required when Kind is "private_network" and must be empty otherwise.
	PrivateNetwork *PrivateNetworkSpecDTO `json:"private_network,omitempty"`
	// ClientTunnel carries the specification for a client-mediated tunnel connection.
	// It is required when Kind is "client_tunnel" and must be empty otherwise.
	ClientTunnel *ClientTunnelSpecDTO `json:"client_tunnel,omitempty"`
}

// PortForwardDTO describes a port forward connection.
type PortForwardDTO struct {
	LocalPort  int    `json:"local_port"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
	Protocol   string `json:"protocol,omitempty"`
	Direction  string `json:"direction,omitempty"`
}

// SourceDTO describes the local service to expose.
// Exactly one tagged union arm must be populated.
type SourceDTO struct {
	Kind      string              `json:"kind"` // "existing_service", "directory", "command", "mcp_server"
	Existing  *ExistingSourceDTO  `json:"existing,omitempty"`
	Directory *DirectorySourceDTO `json:"directory,omitempty"`
	Command   *CommandSourceDTO   `json:"command,omitempty"`
	MCP       *MCPSourceDTO       `json:"mcp,omitempty"`
}

// ExistingSourceDTO describes an already running service.
type ExistingSourceDTO struct {
	Network  string `json:"network,omitempty"` // "tcp" or "udp"
	Address  string `json:"address"`
	Protocol string `json:"protocol,omitempty"` // "http", "https", "tcp", "udp"
}

// DirectorySourceDTO describes a directory to serve.
type DirectorySourceDTO struct {
	Path        string `json:"path"`
	Mode        string `json:"mode,omitempty"` // "read" or "writes"
	SPAFallback bool   `json:"spa_fallback,omitempty"`
	AllowUpload bool   `json:"allow_upload,omitempty"`
	AllowDelete bool   `json:"allow_delete,omitempty"`
}

// CommandSourceDTO describes a command to run.
type CommandSourceDTO struct {
	Executable string            `json:"executable"`
	Args       []string          `json:"args,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Port       int               `json:"port,omitempty"`
	Protocol   string            `json:"protocol,omitempty"` // "http", "https"
	UseShell   bool              `json:"use_shell,omitempty"`
}

// MCPSourceDTO describes an MCP server.
type MCPSourceDTO struct {
	Transport string            `json:"transport"` // "http", "streamable_http", "sse"
	Endpoint  string            `json:"endpoint,omitempty"`
	Command   *CommandSourceDTO `json:"command,omitempty"`
}

// ExposureDTO defines how the connection should be exposed.
type ExposureDTO struct {
	Mode             string `json:"mode"`               // "temporary_public", "permanent_public", "private_only"
	Protocol         string `json:"protocol,omitempty"` // "http", "https", "tcp", "udp"
	RequestedAddress string `json:"requested_address,omitempty"`
}

// ProtectionDTO defines access control.
type ProtectionDTO struct {
	Kind           string   `json:"kind"` // "none", "email_otp", "identity_provider", "service_token", "private_network"
	AllowedEmails  []string `json:"allowed_emails,omitempty"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
}

// ProviderSelectionDTO describes the provider choice.
type ProviderSelectionDTO struct {
	ProviderID string            `json:"provider_id"`
	AccountID  string            `json:"account_id,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
}

// LifecycleDTO defines lifecycle behavior.
type LifecycleDTO struct {
	AutoStart    bool   `json:"auto_start,omitempty"`
	OnDisconnect string `json:"on_disconnect,omitempty"` // "keep_alive" or "close"
}

// UpdateConnectionRequest is the request body for PATCH /v1/connections/{id}.
// State transitions must exclusively use plan open/close endpoints.
type UpdateConnectionRequest struct {
	ExpectedRevision uint64                  `json:"expected_revision,omitempty"`
	Name             *string                 `json:"name,omitempty"`
	Spec             *ServiceExposureSpecDTO `json:"spec,omitempty"`
	Driver           *DriverSelectionDTO     `json:"driver,omitempty"`
	Lifecycle        *LifecycleDTO           `json:"lifecycle,omitempty"`
}

// ProviderRecommendationRequest asks the supervisor to recommend a driver.
type ProviderRecommendationRequest struct {
	ConnectionKind   string `json:"connection_kind,omitempty"`
	SourceKind       string `json:"source_kind,omitempty"`
	MCPTransport     string `json:"mcp_transport,omitempty"`
	ExposureMode     string `json:"exposure_mode,omitempty"`
	Protocol         string `json:"protocol,omitempty"`
	ProtectionKind   string `json:"protection_kind,omitempty"`
	RequestedAddress string `json:"requested_address,omitempty"`
	PreferredAccount string `json:"preferred_account,omitempty"`
	// PreferredProvider owns PreferredAccount. Account identity is the pair, so
	// a preference carrying only the account ID matches any provider that
	// happens to have an account of that name.
	PreferredProvider string `json:"preferred_provider,omitempty"`
}

// ProviderRecommendationResponse contains the recommendation result.
type ProviderRecommendationResponse struct {
	Recommended  *ProviderChoiceDTO  `json:"recommended,omitempty"`
	Alternatives []ProviderChoiceDTO `json:"alternatives,omitempty"`
	Filtered     []FilteredChoiceDTO `json:"filtered,omitempty"`
	// Summary explains the outcome in plain language, including the case where
	// no provider is eligible.
	Summary string `json:"summary,omitempty"`
}

// ProviderChoiceDTO describes one viable provider choice.
type ProviderChoiceDTO struct {
	ProviderID  string   `json:"provider_id"`
	DisplayName string   `json:"display_name,omitempty"`
	AccountID   string   `json:"account_id,omitempty"`
	Reasons     []string `json:"reasons,omitempty"`
	// Tradeoffs state the consequences of this choice in plain language, so a
	// recommendation explains what will happen rather than only naming a
	// provider.
	Tradeoffs    []string `json:"tradeoffs,omitempty"`
	SetupActions []string `json:"setup_actions,omitempty"`
	Score        int      `json:"score,omitempty"`
}

// FilteredChoiceDTO describes a provider that was filtered out and why.
type FilteredChoiceDTO struct {
	ProviderID  string   `json:"provider_id"`
	DisplayName string   `json:"display_name,omitempty"`
	Reason      string   `json:"reason"`
	Reasons     []string `json:"reasons,omitempty"`
	// SetupActions are the steps that would make this provider eligible.
	//
	// The engine computes them and they were dropped here, so a refused
	// provider could say why it was refused and not what to do about it —
	// which is the half the user can act on.
	SetupActions []string `json:"setup_actions,omitempty"`
}

// OperationHistoryDTO contains a list of past operations.
//
// Available distinguishes "history was read and there is none" from "history
// could not be read". An empty list with Available=false must never be
// presented as an authoritative statement that no work has occurred.
type OperationHistoryDTO struct {
	Operations  []OperationDTO `json:"operations"`
	Available   bool           `json:"available"`
	Unavailable string         `json:"unavailable,omitempty"`
	// Limit is how many operations were asked for, and Truncated reports that
	// there are older ones beyond them. A capped list with no way to tell it
	// was capped reads as the complete history, which is the one thing a
	// history must not be wrong about.
	Limit     int  `json:"limit,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

// --------------- discovery ---------------

// DiscoveryDTO is the result of service discovery.
type DiscoveryDTO struct {
	Services []DiscoveredServiceDTO `json:"services"`
}

// DiscoveredServiceDTO describes a discovered local service.
type DiscoveredServiceDTO struct {
	Address    string `json:"address"`
	Port       int    `json:"port"`
	Protocol   string `json:"protocol"`
	Framework  string `json:"framework,omitempty"`
	Confidence string `json:"confidence"`
	PID        int    `json:"pid,omitempty"`
	Process    string `json:"process,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
}

// --------------- diagnostics ---------------

// DiagnosticDTO is a diagnostic finding sent over IPC.
type DiagnosticDTO struct {
	ID          string `json:"id"`
	Segment     string `json:"segment"`
	Severity    string `json:"severity"`
	Summary     string `json:"summary"`
	Explanation string `json:"explanation"`
}

// CloneConnectionRequest asks the supervisor to copy a connection's desired
// state into a new connection. Cloning never mutates the source connection or
// its provider resources.
type CloneConnectionRequest struct {
	// Name for the clone. Defaults to the original's name with a suffix.
	Name string `json:"name,omitempty"`
	// RequestedAddress is required when the source uses a permanent hostname,
	// since a hostname cannot be shared by two connections.
	RequestedAddress string `json:"requested_address,omitempty"`
}

// SetupFlowDTO is a provider's declarative setup description, rendered by the
// UI without provider-specific knowledge.
type SetupFlowDTO struct {
	ProviderID string `json:"provider_id"`
	// Kind is "account" when submitting the flow stores something, or
	// "guidance" when Portico cannot hold this provider's credential and the
	// fields are instructions for the user to act on elsewhere.
	Kind            string          `json:"kind,omitempty"`
	Summary         string          `json:"summary,omitempty"`
	Fields          []SetupFieldDTO `json:"fields,omitempty"`
	CapabilityNotes []string        `json:"capability_notes,omitempty"`
	// GuidanceReason states why Portico cannot store the credential, so a
	// read-only flow does not look like a missing feature.
	GuidanceReason string `json:"guidance_reason,omitempty"`
}

// StoresAccount reports whether submitting this flow persists anything.
func (f SetupFlowDTO) StoresAccount() bool {
	return f.Kind == "" || f.Kind == "account"
}

// SetupFieldDTO is one input in a provider setup flow.
type SetupFieldDTO struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	// EnvVars name the environment variables a non-interactive caller may
	// supply this field through. Portico never accepts a secret as a command
	// argument — it would be in the shell history and in the process list — so
	// this is how the CLI collects one.
	EnvVars []string `json:"env_vars,omitempty"`
}

// RemoveProviderAccountResponse reports whether an account was removed and, if
// not, which connections still depend on it.
type RemoveProviderAccountResponse struct {
	Removed         bool `json:"removed"`
	RestartRequired bool `json:"restart_required,omitempty"`
	// DependentConnections is kept for callers that only report connections.
	DependentConnections []string `json:"dependent_connections,omitempty"`
	// Dependencies is everything in the way, including provider resources that
	// still need removing with this account's credential — those are not
	// connections, and reporting only connections hid them entirely.
	Dependencies []AccountDependencyDTO `json:"dependencies,omitempty"`
	// Preview is the current state of the account when a removal was refused
	// as stale, so the caller can re-confirm against what is true now.
	Preview *AccountRemovalPreviewDTO `json:"preview,omitempty"`
}

// AccountRemovalPreviewDTO is what removing an account would do.
//
// The supervisor composes it, so every client says the same true thing about
// the same account. Clients used to write their own description, and the
// confirmation screen promised to forget a credential Portico might not hold.
type AccountRemovalPreviewDTO struct {
	ProviderID       string                 `json:"provider_id"`
	AccountID        string                 `json:"account_id"`
	Label            string                 `json:"label,omitempty"`
	Removable        bool                   `json:"removable"`
	CredentialStored bool                   `json:"credential_stored"`
	Dependencies     []AccountDependencyDTO `json:"dependencies,omitempty"`
	// Consequences are the sentences a client shows, composed from what is
	// actually true of this account rather than from what is usually true.
	Consequences []string `json:"consequences,omitempty"`
	// Fingerprint binds this preview to the removal it describes.
	Fingerprint string `json:"fingerprint"`
}

// RemoveProviderAccountRequest carries the fingerprint of the preview the
// caller confirmed, so applying cannot describe one removal and perform another.
type RemoveProviderAccountRequest struct {
	Fingerprint string `json:"fingerprint"`
}

// AccountDependencyDTO is one thing standing in the way of removing an account.
type AccountDependencyDTO struct {
	// Kind is "connection" or "cleanup_item".
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Explanation says why this blocks removal, in terms of consequence.
	Explanation string `json:"explanation,omitempty"`
}

// SupportExportDTO is a redacted diagnostic report intended to be attached to a
// bug report. It carries no credentials, authorization headers, cookies,
// private keys, command environments or full process environments.
type SupportExportDTO struct {
	GeneratedAt     string `json:"generated_at"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	GoVersion       string `json:"go_version"`
	SchemaVersion   int    `json:"schema_version,omitempty"`
	SocketPath      string `json:"socket_path,omitempty"`
	DatabasePath    string `json:"database_path,omitempty"`
	SupervisorReady bool   `json:"supervisor_ready"`
	// Reviewed records whether a human has checked the report before sharing.
	// It is always false when generated.
	Reviewed bool `json:"reviewed"`

	Providers        []SupportProviderDTO   `json:"providers,omitempty"`
	Connections      []SupportConnectionDTO `json:"connections,omitempty"`
	RecentOperations []SupportOperationDTO  `json:"recent_operations,omitempty"`
	Notes            []string               `json:"notes,omitempty"`
}

// SupportProviderDTO is a provider's state in a support export.
type SupportProviderDTO struct {
	ID           string `json:"id"`
	Availability string `json:"availability"`
	Reason       string `json:"reason,omitempty"`
	Accounts     int    `json:"accounts"`
}

// SupportConnectionDTO is a connection's configuration and observed state,
// without secrets. Allowed identities are reported as a count because they are
// personal data.
type SupportConnectionDTO struct {
	ID                   string               `json:"id"`
	Kind                 string               `json:"kind"`
	Revision             uint64               `json:"revision"`
	DesiredState         string               `json:"desired_state"`
	RuntimeState         string               `json:"runtime_state,omitempty"`
	ProviderID           string               `json:"provider_id,omitempty"`
	AccountID            string               `json:"account_id,omitempty"`
	SourceKind           string               `json:"source_kind,omitempty"`
	ExposureMode         string               `json:"exposure_mode,omitempty"`
	RequestedAddress     string               `json:"requested_address,omitempty"`
	ProtectionKind       string               `json:"protection_kind,omitempty"`
	AllowedIdentityCount int                  `json:"allowed_identity_count,omitempty"`
	ConnectorState       string               `json:"connector_state,omitempty"`
	ConnectorPID         int                  `json:"connector_pid,omitempty"`
	Restarts             int                  `json:"restarts,omitempty"`
	LastError            string               `json:"last_error,omitempty"`
	Resources            []SupportResourceDTO `json:"resources,omitempty"`
	Findings             []string             `json:"findings,omitempty"`
}

// SupportResourceDTO is a provider resource in a support export.
type SupportResourceDTO struct {
	Type       string            `json:"type"`
	ExternalID string            `json:"external_id"`
	Ownership  string            `json:"ownership"`
	Lifecycle  string            `json:"lifecycle,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// SupportOperationDTO is a recent operation in a support export.
type SupportOperationDTO struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connection_id"`
	Intent       string `json:"intent,omitempty"`
	State        string `json:"state"`
	StartedAt    string `json:"started_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ConnectionLogsDTO is a bounded, redacted tail of a connection's connector
// output.
//
// Available distinguishes "logs were read and there are none" from "logs could
// not be read", so an empty list is never presented as an authoritative
// statement that the connector produced no output.
type ConnectionLogsDTO struct {
	ConnectionID string       `json:"connection_id"`
	Lines        []LogLineDTO `json:"lines,omitempty"`
	Available    bool         `json:"available"`
	Unavailable  string       `json:"unavailable,omitempty"`
	Truncated    bool         `json:"truncated,omitempty"`
}

// LogLineDTO is one line of connector output.
type LogLineDTO struct {
	Stream string `json:"stream"`
	Text   string `json:"text"`
	// Timestamp and Severity are populated when the connector's own output
	// carries them in a form the supervisor recognises, and left empty when it
	// does not.
	//
	// They are not invented. Portico reads rotating log files a connector wrote;
	// the time a line was read is not the time the event happened, and stamping
	// lines with it would put a fabricated ordering in front of the user. So a
	// line whose prefix parses gets its own timestamp, and one that does not gets
	// none — which the client renders as an unadorned line rather than a blank
	// column.
	Timestamp string `json:"timestamp,omitempty"`
	Severity  string `json:"severity,omitempty"`
}

// TelemetryDTO is a provider-neutral traffic snapshot for a connection.
type TelemetryDTO struct {
	ConnectionCount int64  `json:"connection_count"`
	RequestCount    int64  `json:"request_count"`
	BytesIn         int64  `json:"bytes_in"`
	BytesOut        int64  `json:"bytes_out"`
	ProviderErrors  int64  `json:"provider_errors"`
	SampledAt       string `json:"sampled_at"`
	Available       bool   `json:"available"`
	Unavailable     string `json:"unavailable,omitempty"`

	// HasCounts, HasBytes and HasErrors report which counters the provider
	// actually measured. A provider that does not report byte totals must not
	// have "0 bytes" displayed against it: an unmeasured counter and a measured
	// zero are different facts, and a client cannot tell them apart from the
	// value alone.
	HasCounts bool `json:"has_counts,omitempty"`
	HasBytes  bool `json:"has_bytes,omitempty"`
	HasErrors bool `json:"has_errors,omitempty"`
}

// ReadinessDTO answers "what does Portico need, and what is already satisfied?"
// in one response, so setup does not require assembling the answer from several
// screens.
type ReadinessDTO struct {
	// Summary states the overall position in one sentence.
	Summary string `json:"summary"`
	// LaunchMode is "manual" or "auto".
	LaunchMode string `json:"launch_mode"`
	// LaunchModePinned reports that an environment override is deciding the
	// launch mode. The screen must say so, because otherwise a toggle that
	// cannot take effect looks broken rather than overridden.
	LaunchModePinned bool `json:"launch_mode_pinned,omitempty"`
	// LaunchModePinnedBy names the override to unset.
	LaunchModePinnedBy string                   `json:"launch_mode_pinned_by,omitempty"`
	Providers          []ProviderReadinessDTO   `json:"providers,omitempty"`
	Connections        []ConnectionReadinessDTO `json:"connections,omitempty"`
}

// LaunchModeRequest asks the supervisor to change the startup gate.
type LaunchModeRequest struct {
	// Mode is "manual" or "auto". Anything else is rejected rather than
	// coerced, so a typo cannot silently arm every connection.
	Mode string `json:"mode"`
}

// LaunchModeDTO reports the mode actually in effect after a change.
//
// It is not simply an echo of the request. PORTICO_LAUNCH_MODE overrides the
// stored value, so a request to change the mode can legitimately have no
// effect; reporting the requested mode back would tell the caller something
// untrue about the machine it is running on.
type LaunchModeDTO struct {
	// Mode is the mode now in effect.
	Mode string `json:"mode"`
	// Pinned reports that an environment override is deciding the mode, so the
	// stored value is not being consulted.
	Pinned bool `json:"pinned,omitempty"`
	// PinnedBy names the override, so the caller can say what to unset.
	PinnedBy string `json:"pinned_by,omitempty"`
	// Persistent reports whether the mode survives a supervisor restart.
	// A stored mode does; a mode pinned by the environment is not stored at
	// all, and reporting either as merely temporary would understate what the
	// user's choice did.
	Persistent bool `json:"persistent"`
}

// SettingsDTO is the operational configuration the supervisor owns.
//
// These are the choices that outlive a session and are not properties of any
// one connection: whether marked connections open at startup, and what a newly
// created connection defaults to. The TUI reads and writes them over IPC — it
// never touches the config file, which only the supervisor opens.
type SettingsDTO struct {
	// LaunchMode is "manual" or "auto".
	LaunchMode string `json:"launch_mode"`
	// LaunchModePinned reports that an environment override decides the mode,
	// so the stored value is not consulted.
	LaunchModePinned bool `json:"launch_mode_pinned,omitempty"`
	// LaunchModePinnedBy names the override to unset.
	LaunchModePinnedBy string `json:"launch_mode_pinned_by,omitempty"`

	// DefaultAutoStart is what a new connection's AutoStart is set to when the
	// user does not choose otherwise. The wizard hardcoded true, which armed
	// every connection ever created without asking.
	DefaultAutoStart bool `json:"default_auto_start"`
	// DefaultOnDisconnect is "keep_alive" or "close": what happens to a
	// connection when the client that created it goes away. The wizard
	// hardcoded keep_alive.
	DefaultOnDisconnect string `json:"default_on_disconnect"`
}

// SettingsRequest changes operational settings.
//
// Every field is a pointer so an absent one means "leave this alone". Sending
// a whole settings object back would make every write a full overwrite, and two
// clients changing different settings would clobber each other.
type SettingsRequest struct {
	LaunchMode          *string `json:"launch_mode,omitempty"`
	DefaultAutoStart    *bool   `json:"default_auto_start,omitempty"`
	DefaultOnDisconnect *string `json:"default_on_disconnect,omitempty"`
}

// ProviderReadinessDTO is one provider's position, with the credential sources
// Portico found for it.
type ProviderReadinessDTO struct {
	ID           string                `json:"id"`
	DisplayName  string                `json:"display_name"`
	Availability string                `json:"availability"`
	Summary      string                `json:"summary"`
	Blocked      bool                  `json:"blocked"`
	Reason       string                `json:"reason,omitempty"`
	SetupActions []string              `json:"setup_actions,omitempty"`
	Accounts     int                   `json:"accounts"`
	Credentials  []CredentialSourceDTO `json:"credentials,omitempty"`
}

// CredentialSourceDTO is one place a credential can come from. It reports
// presence and location only; the value is never read.
type CredentialSourceDTO struct {
	Kind     string `json:"kind"`
	Location string `json:"location"`
	// Searched lists every place this source was looked for, so a negative
	// result can say where Portico looked rather than implying the credential
	// does not exist anywhere.
	Searched    []string `json:"searched,omitempty"`
	Present     bool     `json:"present"`
	Description string   `json:"description,omitempty"`
	Action      string   `json:"action,omitempty"`
}

// ConnectionReadinessDTO reports whether a connection can open, and what stands
// in the way if not.
type ConnectionReadinessDTO struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Provider  string   `json:"provider"`
	Desired   string   `json:"desired"`
	AutoStart bool     `json:"auto_start"`
	Ready     bool     `json:"ready"`
	Blockers  []string `json:"blockers,omitempty"`
}
