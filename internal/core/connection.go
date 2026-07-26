package core

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ConnectionProfile represents the desired state of a connection.
// A profile does not contain runtime data such as connector PID,
// public address, provider resource IDs, last error, traffic samples,
// current health, or progress state.
type ConnectionProfile struct {
	ID         ConnectionID
	Name       string
	Revision   uint64
	Source     SourceSpec
	Exposure   ExposureSpec
	Protection ProtectionSpec
	Provider   ProviderSelection
	Lifecycle  LifecycleSpec
	Desired    DesiredConnectionState
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// DeepCopy returns a deep copy of the profile.
func (p *ConnectionProfile) DeepCopy() *ConnectionProfile {
	if p == nil {
		return nil
	}
	cp := *p
	if p.Source.Existing != nil {
		existing := *p.Source.Existing
		cp.Source.Existing = &existing
	}
	if p.Source.Directory != nil {
		dir := *p.Source.Directory
		cp.Source.Directory = &dir
	}
	if p.Source.Command != nil {
		cmd := *p.Source.Command
		cp.Source.Command = &cmd
		if len(p.Source.Command.Args) > 0 {
			args := make([]string, len(p.Source.Command.Args))
			copy(args, p.Source.Command.Args)
			cp.Source.Command.Args = args
		}
		if p.Source.Command.Env != nil {
			env := make(map[string]string, len(p.Source.Command.Env))
			for k, v := range p.Source.Command.Env {
				env[k] = v
			}
			cp.Source.Command.Env = env
		}
	}
	if p.Source.MCP != nil {
		mcp := *p.Source.MCP
		cp.Source.MCP = &mcp
		if mcp.Command != nil {
			cmd := *mcp.Command
			if mcp.Command.Args != nil {
				cmd.Args = append([]string(nil), mcp.Command.Args...)
			}
			if mcp.Command.Env != nil {
				cmd.Env = make(map[string]string, len(mcp.Command.Env))
				for key, value := range mcp.Command.Env {
					cmd.Env[key] = value
				}
			}
			cp.Source.MCP.Command = &cmd
		}
	}
	if p.Exposure.RequestedAddress != "" {
		cp.Exposure.RequestedAddress = p.Exposure.RequestedAddress
	}
	if p.Protection.AllowedEmails != nil {
		emails := make([]string, len(p.Protection.AllowedEmails))
		copy(emails, p.Protection.AllowedEmails)
		cp.Protection.AllowedEmails = emails
	}
	if p.Protection.AllowedDomains != nil {
		domains := make([]string, len(p.Protection.AllowedDomains))
		copy(domains, p.Protection.AllowedDomains)
		cp.Protection.AllowedDomains = domains
	}
	if p.Provider.Options != nil {
		options := make(map[string]string, len(p.Provider.Options))
		for k, v := range p.Provider.Options {
			options[k] = v
		}
		cp.Provider.Options = options
	}
	return &cp
}

// Validate checks that the profile is valid and consistent.
// It enforces exactly one active arm in each tagged union.
func (p *ConnectionProfile) Validate() error {
	if p == nil {
		return fmt.Errorf("profile is nil")
	}

	if p.ID == "" {
		return fmt.Errorf("profile ID is required")
	}

	if p.Name == "" {
		return fmt.Errorf("profile name is required")
	}

	// Validate source: exactly one must be set, and Kind must match
	sourceCount := 0
	if p.Source.Existing != nil {
		sourceCount++
		if p.Source.Kind != SourceExisting {
			return fmt.Errorf("source kind %q does not match existing service spec", p.Source.Kind)
		}
		if p.Source.Existing.Address == "" {
			return fmt.Errorf("existing service address is required")
		}
		if p.Source.Existing.Network != "" {
			switch p.Source.Existing.Network {
			case "tcp", "udp":
			default:
				return fmt.Errorf("invalid network %q, must be tcp or udp", p.Source.Existing.Network)
			}
		}
	}
	if p.Source.Directory != nil {
		sourceCount++
		if p.Source.Kind != SourceDirectory {
			return fmt.Errorf("source kind %q does not match directory spec", p.Source.Kind)
		}
		if p.Source.Directory.Path == "" {
			return fmt.Errorf("directory path is required")
		}
		switch p.Source.Directory.Mode {
		case DirectoryModeRead, DirectoryModeWrites, "":
			// valid
		default:
			return fmt.Errorf("invalid directory mode %q", p.Source.Directory.Mode)
		}
		if p.Source.Directory.Mode != DirectoryModeWrites && (p.Source.Directory.AllowUpload || p.Source.Directory.AllowDelete) {
			return fmt.Errorf("directory upload/delete permissions require writes mode")
		}
	}
	if p.Source.Command != nil {
		sourceCount++
		if p.Source.Kind != SourceCommand {
			return fmt.Errorf("source kind %q does not match command spec", p.Source.Kind)
		}
		if p.Source.Command.Executable == "" {
			return fmt.Errorf("command executable is required")
		}
		if p.Source.Command.Port < 1 || p.Source.Command.Port > 65535 {
			return fmt.Errorf("command port must be between 1 and 65535")
		}
		if err := validateCommandEnvironment(p.Source.Command.Env); err != nil {
			return err
		}
		switch p.Source.Command.Protocol {
		case ProtocolHTTP, ProtocolHTTPS, "":
			// valid
		default:
			return fmt.Errorf("invalid command protocol %q", p.Source.Command.Protocol)
		}
	}
	if p.Source.MCP != nil {
		sourceCount++
		if p.Source.Kind != SourceMCP {
			return fmt.Errorf("source kind %q does not match MCP spec", p.Source.Kind)
		}
		if p.Source.MCP.Endpoint == "" && p.Source.MCP.Command == nil {
			return fmt.Errorf("MCP endpoint or command is required")
		}
		if p.Source.MCP.Endpoint != "" && p.Source.MCP.Command != nil {
			return fmt.Errorf("MCP source must use either an endpoint or a command, not both")
		}
		if p.Source.MCP.Endpoint != "" {
			u, err := url.ParseRequestURI(p.Source.MCP.Endpoint)
			if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
				return fmt.Errorf("MCP endpoint must be an absolute HTTP URL without embedded credentials")
			}
		}
		if p.Source.MCP.Command != nil {
			if p.Source.MCP.Command.Executable == "" {
				return fmt.Errorf("MCP command executable is required")
			}
			if p.Source.MCP.Command.Port < 1 || p.Source.MCP.Command.Port > 65535 {
				return fmt.Errorf("MCP command port must be between 1 and 65535")
			}
			if err := validateCommandEnvironment(p.Source.MCP.Command.Env); err != nil {
				return err
			}
			if p.Source.MCP.Command.Protocol != "" && p.Source.MCP.Command.Protocol != ProtocolHTTP {
				return fmt.Errorf("MCP command protocol %q is not supported", p.Source.MCP.Command.Protocol)
			}
		}
		switch p.Source.MCP.Transport {
		case MCPTransportHTTP, MCPTransportStreamable, MCPTransportSSE, "":
			// valid
		default:
			return fmt.Errorf("invalid MCP transport %q", p.Source.MCP.Transport)
		}
	}
	if sourceCount != 1 {
		return fmt.Errorf("exactly one source must be set, got %d", sourceCount)
	}

	// Validate exposure mode
	switch p.Exposure.Mode {
	case ExposureTemporary:
		if p.Exposure.RequestedAddress != "" {
			return fmt.Errorf("temporary exposure does not accept a requested hostname")
		}
	case ExposurePermanent:
		if p.Exposure.RequestedAddress == "" {
			return fmt.Errorf("permanent exposure requires a requested hostname")
		}
	case ExposurePrivate:
		// Private-only exposure is not supported in v0.1
		return fmt.Errorf("private_only exposure is not supported in v0.1")
	case "":
		// Will be set to default later
	default:
		return fmt.Errorf("invalid exposure mode %q", p.Exposure.Mode)
	}

	// Validate protocol
	switch p.Exposure.Protocol {
	case ProtocolHTTP, ProtocolHTTPS, ProtocolTCP, ProtocolUDP, "":
		// valid
	default:
		return fmt.Errorf("invalid exposure protocol %q", p.Exposure.Protocol)
	}

	// Validate desired state
	switch p.Desired {
	case DesiredOpen, DesiredClosed, "":
		// valid
	default:
		return fmt.Errorf("invalid desired state %q", p.Desired)
	}

	// Validate protection
	switch p.Protection.Kind {
	case ProtectionNone, ProtectionEmailOTP, ProtectionIdentity, ProtectionServiceToken, ProtectionPrivateNet, "":
		// valid
	default:
		return fmt.Errorf("invalid protection kind %q", p.Protection.Kind)
	}

	if p.Protection.Kind == ProtectionServiceToken {
		if len(p.Provider.Options) == 0 {
			return fmt.Errorf("service token protection requires provider options")
		}
	}

	if p.Lifecycle.OnDisconnect != "" && p.Lifecycle.OnDisconnect != DisconnectKeepAlive && p.Lifecycle.OnDisconnect != DisconnectClose {
		return fmt.Errorf("invalid disconnect policy %q", p.Lifecycle.OnDisconnect)
	}

	// Validate provider selection
	if p.Provider.ProviderID == "" {
		return fmt.Errorf("provider ID is required")
	}

	// Validate command working directory
	if p.Source.Command != nil {
		if p.Source.Command.WorkingDir != "" {
			// Working directory is validated at use time
		}
	}

	return nil
}

func validateCommandEnvironment(environment map[string]string) error {
	for name := range environment {
		upper := strings.ToUpper(name)
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") ||
			strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "CREDENTIAL") ||
			strings.HasSuffix(upper, "_KEY") {
			return fmt.Errorf("command environment variable %q appears to contain a credential; use a provider or OS secret reference instead", name)
		}
	}
	return nil
}

// DesiredConnectionState represents whether the connection should be open or closed.
type DesiredConnectionState string

const (
	DesiredOpen   DesiredConnectionState = "open"
	DesiredClosed DesiredConnectionState = "closed"
)

// --------------- source ---------------

// SourceKind defines the type of source.
type SourceKind string

const (
	SourceExisting  SourceKind = "existing_service"
	SourceDirectory SourceKind = "directory"
	SourceCommand   SourceKind = "command"
	SourceMCP       SourceKind = "mcp_server"
)

// SourceSpec defines what local service to expose.
// Exactly one of the tagged union fields must be populated.
type SourceSpec struct {
	Kind      SourceKind
	Existing  *ExistingServiceSpec
	Directory *DirectorySpec
	Command   *CommandSpec
	MCP       *MCPServiceSpec
}

// ExistingServiceSpec describes an already running service.
type ExistingServiceSpec struct {
	Network  string
	Address  string
	Protocol Protocol
	Health   HealthCheckSpec
}

// Protocol describes the network protocol.
type Protocol string

const (
	ProtocolHTTP  Protocol = "http"
	ProtocolHTTPS Protocol = "https"
	ProtocolTCP   Protocol = "tcp"
	ProtocolUDP   Protocol = "udp"
)

// HealthCheckSpec describes health check configuration.
type HealthCheckSpec struct {
	Enabled  bool
	Path     string
	Timeout  time.Duration
	Interval time.Duration
}

// DirectorySpec describes a directory to serve.
type DirectorySpec struct {
	Path        string
	Mode        DirectoryMode
	SPAFallback bool
	AllowUpload bool
	AllowDelete bool
}

// DirectoryMode defines how to serve a directory.
type DirectoryMode string

const (
	DirectoryModeRead   DirectoryMode = "read"
	DirectoryModeWrites DirectoryMode = "writes"
)

// CommandSpec describes a command to run.
type CommandSpec struct {
	Executable string
	Args       []string
	WorkingDir string
	Env        map[string]string
	Port       int
	Protocol   Protocol
	UseShell   bool
}

// MCPServiceSpec describes an MCP server.
type MCPServiceSpec struct {
	Transport MCPTransport
	Endpoint  string
	Command   *CommandSpec
}

// MCPTransport describes MCP transport type.
type MCPTransport string

const (
	MCPTransportHTTP       MCPTransport = "http"
	MCPTransportStreamable MCPTransport = "streamable_http"
	MCPTransportSSE        MCPTransport = "sse"
)

// --------------- exposure ---------------

// ExposureSpec defines how the connection should be exposed.
type ExposureSpec struct {
	Mode             ExposureMode
	Protocol         Protocol
	RequestedAddress string
	Expiration       *time.Time
}

// ExposureMode defines the exposure mode.
type ExposureMode string

const (
	ExposureTemporary ExposureMode = "temporary_public"
	ExposurePermanent ExposureMode = "permanent_public"
	ExposurePrivate   ExposureMode = "private_only"
)

// --------------- protection ---------------

// ProtectionSpec defines access control.
type ProtectionSpec struct {
	Kind           ProtectionKind
	AllowedEmails  []string
	AllowedDomains []string
	SessionTTL     time.Duration
}

// DefaultProtectedSessionTTL is used when an authenticated access policy is
// requested without an explicit session lifetime. It avoids serializing 0s to
// provider APIs while retaining a short, secure default for interactive use.
const DefaultProtectedSessionTTL = 30 * time.Minute

// ProtectionKind defines the type of protection.
type ProtectionKind string

const (
	ProtectionNone         ProtectionKind = "none"
	ProtectionEmailOTP     ProtectionKind = "email_otp"
	ProtectionIdentity     ProtectionKind = "identity_provider"
	ProtectionServiceToken ProtectionKind = "service_token"
	ProtectionPrivateNet   ProtectionKind = "private_network"
)

// --------------- provider selection ---------------

// ProviderSelection describes the provider choice.
type ProviderSelection struct {
	ProviderID ProviderID
	AccountID  ProviderAccountID
	Options    map[string]string
}

// ResolveOriginURL extracts the origin URL from a SourceSpec.
// It returns an error for source kinds that cannot provide a URL
// (directory, command, MCP) since they require a running local
// origin that must be prepared separately.
func ResolveOriginURL(source SourceSpec) (string, error) {
	switch source.Kind {
	case SourceExisting:
		if source.Existing == nil || source.Existing.Address == "" {
			return "", fmt.Errorf("existing source has no address")
		}
		return fmt.Sprintf("http://%s", source.Existing.Address), nil
	case SourceDirectory, SourceCommand, SourceMCP:
		return "", fmt.Errorf("source kind %q is not supported for tunnel origin — the origin must be a running service with a reachable address", source.Kind)
	default:
		return "", fmt.Errorf("unknown source kind %q", source.Kind)
	}
}

// --------------- lifecycle ---------------

// LifecycleSpec defines lifecycle behavior.
type LifecycleSpec struct {
	AutoStart    bool
	OnDisconnect DisconnectPolicy
}

// DisconnectPolicy defines what happens when supervisor disconnects.
type DisconnectPolicy string

const (
	DisconnectKeepAlive DisconnectPolicy = "keep_alive"
	DisconnectClose     DisconnectPolicy = "close"
)
