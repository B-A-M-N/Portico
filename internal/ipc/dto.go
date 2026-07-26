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
	ID                string `json:"id"`
	Name              string `json:"name"`
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
}

// ProviderDTO is the view of a provider sent over IPC.
type ProviderDTO struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	DisplayName   string               `json:"display_name"`
	Authenticated bool                 `json:"authenticated"`
	Accounts      []ProviderAccountDTO `json:"accounts,omitempty"`
}

// ProviderAccountDTO is a selectable, non-secret provider account summary.
type ProviderAccountDTO struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
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
}

// ConfigureProviderAccountResponse tells clients whether a supervisor restart
// is needed before newly persisted account adapters become available.
type ConfigureProviderAccountResponse struct {
	RestartRequired bool `json:"restart_required"`
}

// --------------- events ---------------

// EventDTO is an event delivered via SSE.
type EventDTO struct {
	Sequence     int64       `json:"seq"`
	OperationID  string      `json:"operation_id,omitempty"`
	ConnectionID string      `json:"connection_id,omitempty"`
	Type         string      `json:"type"`
	Stage        string      `json:"stage,omitempty"`
	Timestamp    string      `json:"timestamp"`
	Data         interface{} `json:"data"`
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
}

// StepDTO is a step in a plan preview.
type StepDTO struct {
	ID           string `json:"id"`
	Summary      string `json:"summary"`
	Destructive  bool   `json:"destructive"`
	Irreversible bool   `json:"irreversible"`
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
type CreateConnectionRequest struct {
	Version    int                  `json:"version"`
	Name       string               `json:"name"`
	Source     SourceDTO            `json:"source"`
	Exposure   ExposureDTO          `json:"exposure"`
	Protection ProtectionDTO        `json:"protection"`
	Provider   ProviderSelectionDTO `json:"provider"`
	Lifecycle  LifecycleDTO         `json:"lifecycle"`
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
type UpdateConnectionRequest struct {
	Name         *string `json:"name,omitempty"`
	DesiredState *string `json:"desired_state,omitempty"`
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
