package core

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ConnectionKind identifies the type of connection.
type ConnectionKind string

const (
	// ConnectionServiceExposure exposes a local service through a tunnel provider.
	ConnectionServiceExposure ConnectionKind = "service_exposure"

	// ConnectionPortForward forwards a local port to a remote endpoint.
	ConnectionPortForward ConnectionKind = "port_forward"

	// ConnectionPrivateNetwork joins or exposes through a private network.
	ConnectionPrivateNetwork ConnectionKind = "private_network"

	// ConnectionClientTunnel reaches a local service through a client-mediated
	// tunnel operated by the consuming platform, with no public address and no
	// inbound port. It is a distinct kind rather than an exposure mode because
	// it has no hostname, no DNS record and no public endpoint, and must never
	// be satisfied by publishing the service instead.
	ConnectionClientTunnel ConnectionKind = "client_tunnel"
)

// ConnectionProfile represents the desired state of a connection.
// A profile does not contain runtime data such as connector PID,
// public address, provider resource IDs, last error, traffic samples,
// current health, or progress state.
type ConnectionProfile struct {
	ID        ConnectionID           `json:"id"`
	Name      string                 `json:"name"`
	Revision  uint64                 `json:"revision"`
	Kind      ConnectionKind         `json:"kind"`
	Spec      ConnectionSpec         `json:"spec"`
	Driver    DriverSelection        `json:"driver"`
	Lifecycle LifecycleSpec          `json:"lifecycle"`
	Desired   DesiredConnectionState `json:"desired"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// ConnectionSpec is a tagged union containing kind-specific specifications.
type ConnectionSpec struct {
	ServiceExposure *ServiceExposureSpec `json:"service_exposure,omitempty"`
	PortForward     *PortForwardSpec     `json:"port_forward,omitempty"`
	PrivateNetwork  *PrivateNetworkSpec  `json:"private_network,omitempty"`
	ClientTunnel    *ClientTunnelSpec    `json:"client_tunnel,omitempty"`
}

// ClientKind identifies the platform whose client mediates the tunnel.
type ClientKind string

const (
	// ClientOpenAISecureMCPTunnel is OpenAI's Secure MCP Tunnel, run locally as
	// the tunnel-client binary. It opens an outbound HTTPS connection to
	// OpenAI and forwards MCP requests to a local server; no inbound port is
	// opened and no public address is created.
	ClientOpenAISecureMCPTunnel ClientKind = "openai_secure_mcp_tunnel"
)

// ClientTunnelSpec describes a client-mediated private connection.
type ClientTunnelSpec struct {
	Client ClientKind     `json:"client"`
	MCP    MCPServiceSpec `json:"mcp"`
	// TunnelID identifies the tunnel created in the platform's own settings.
	// Portico does not create it: tunnel creation happens on the platform, and
	// claiming otherwise would report an unverified step as done.
	TunnelID string `json:"tunnel_id,omitempty"`
	// Profile names the local client profile to run.
	Profile string `json:"profile,omitempty"`
}

// ServiceExposureSpec describes a service exposure connection.
// This is the original connection type that exposes a local service through a tunnel.
type ServiceExposureSpec struct {
	Source     SourceSpec     `json:"source"`
	Exposure   ExposureSpec   `json:"exposure"`
	Protection ProtectionSpec `json:"protection"`
}

// PortForwardSpec describes a port forward connection.
type PortForwardSpec struct {
	LocalPort  int                  `json:"local_port"`
	RemoteHost string               `json:"remote_host"`
	RemotePort int                  `json:"remote_port"`
	Protocol   Protocol             `json:"protocol"`
	Direction  PortForwardDirection `json:"direction"`
}

// PortForwardDirection indicates the direction of port forwarding.
type PortForwardDirection string

const (
	// PortForwardLocal forwards a local port to a remote endpoint.
	PortForwardLocal PortForwardDirection = "local"

	// PortForwardRemote forwards a remote port to a local endpoint.
	PortForwardRemote PortForwardDirection = "remote"
)

// PrivateNetworkSpec describes a private network connection.
type PrivateNetworkSpec struct {
	NetworkID   string             `json:"network_id"`
	Mode        PrivateNetworkMode `json:"mode"`
	ExposeLocal bool               `json:"expose_local"`
}

// PrivateNetworkMode indicates how the connection interacts with the private network.
type PrivateNetworkMode string

const (
	// PrivateNetworkJoin joins an existing private network.
	PrivateNetworkJoin PrivateNetworkMode = "join"

	// PrivateNetworkExpose exposes local services to the private network.
	PrivateNetworkExpose PrivateNetworkMode = "expose"
)

// DriverSelection describes the driver choice for a connection.
type DriverSelection struct {
	DriverID   DriverID          `json:"driver_id"`
	ProviderID ProviderID        `json:"provider_id"`
	AccountID  ProviderAccountID `json:"account_id"`
	Options    map[string]string `json:"options,omitempty"`
}

// DriverID identifies a connection driver.
type DriverID string

// Backward compatibility accessors for the old flat profile structure.
// These allow existing code to continue working while we migrate to the new structure.
// Note: renamed to GetSource, GetExposure, etc. to avoid JSON serialization conflicts.

// GetSource returns the source spec for service exposure connections.
// Returns a zero-value SourceSpec for non-service-exposure connections.
func (p *ConnectionProfile) GetSource() SourceSpec {
	if p == nil || p.Spec.ServiceExposure == nil {
		return SourceSpec{}
	}
	return p.Spec.ServiceExposure.Source
}

// GetExposure returns the exposure spec for service exposure connections.
func (p *ConnectionProfile) GetExposure() ExposureSpec {
	if p == nil || p.Spec.ServiceExposure == nil {
		return ExposureSpec{}
	}
	return p.Spec.ServiceExposure.Exposure
}

// GetProtection returns the protection spec for service exposure connections.
func (p *ConnectionProfile) GetProtection() ProtectionSpec {
	if p == nil || p.Spec.ServiceExposure == nil {
		return ProtectionSpec{}
	}
	return p.Spec.ServiceExposure.Protection
}

// GetProvider returns the provider selection for service exposure connections.
// For backward compatibility, this maps Driver to ProviderSelection.
func (p *ConnectionProfile) GetProvider() ProviderSelection {
	if p == nil {
		return ProviderSelection{}
	}
	return ProviderSelection{
		ProviderID: p.Driver.ProviderID,
		AccountID:  p.Driver.AccountID,
		Options:    p.Driver.Options,
	}
}

// DeepCopy returns a deep copy of the profile.
func (p *ConnectionProfile) DeepCopy() *ConnectionProfile {
	if p == nil {
		return nil
	}
	cp := *p

	// Deep copy the spec
	if p.Spec.ServiceExposure != nil {
		se := *p.Spec.ServiceExposure
		cp.Spec.ServiceExposure = &se

		if p.Spec.ServiceExposure.Source.Existing != nil {
			existing := *p.Spec.ServiceExposure.Source.Existing
			cp.Spec.ServiceExposure.Source.Existing = &existing
		}
		if p.Spec.ServiceExposure.Source.Directory != nil {
			dir := *p.Spec.ServiceExposure.Source.Directory
			cp.Spec.ServiceExposure.Source.Directory = &dir
		}
		if p.Spec.ServiceExposure.Source.Command != nil {
			cmd := *p.Spec.ServiceExposure.Source.Command
			cp.Spec.ServiceExposure.Source.Command = &cmd
			if len(p.Spec.ServiceExposure.Source.Command.Args) > 0 {
				args := make([]string, len(p.Spec.ServiceExposure.Source.Command.Args))
				copy(args, p.Spec.ServiceExposure.Source.Command.Args)
				cp.Spec.ServiceExposure.Source.Command.Args = args
			}
			if p.Spec.ServiceExposure.Source.Command.Env != nil {
				env := make(map[string]string, len(p.Spec.ServiceExposure.Source.Command.Env))
				for k, v := range p.Spec.ServiceExposure.Source.Command.Env {
					env[k] = v
				}
				cp.Spec.ServiceExposure.Source.Command.Env = env
			}
		}
		if p.Spec.ServiceExposure.Source.MCP != nil {
			mcp := *p.Spec.ServiceExposure.Source.MCP
			cp.Spec.ServiceExposure.Source.MCP = &mcp
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
				cp.Spec.ServiceExposure.Source.MCP.Command = &cmd
			}
		}
		if p.Spec.ServiceExposure.Exposure.RequestedAddress != "" {
			cp.Spec.ServiceExposure.Exposure.RequestedAddress = p.Spec.ServiceExposure.Exposure.RequestedAddress
		}
		if p.Spec.ServiceExposure.Protection.AllowedEmails != nil {
			emails := make([]string, len(p.Spec.ServiceExposure.Protection.AllowedEmails))
			copy(emails, p.Spec.ServiceExposure.Protection.AllowedEmails)
			cp.Spec.ServiceExposure.Protection.AllowedEmails = emails
		}
		if p.Spec.ServiceExposure.Protection.AllowedDomains != nil {
			domains := make([]string, len(p.Spec.ServiceExposure.Protection.AllowedDomains))
			copy(domains, p.Spec.ServiceExposure.Protection.AllowedDomains)
			cp.Spec.ServiceExposure.Protection.AllowedDomains = domains
		}
	}

	if p.Spec.PortForward != nil {
		pf := *p.Spec.PortForward
		cp.Spec.PortForward = &pf
	}

	if p.Spec.PrivateNetwork != nil {
		pn := *p.Spec.PrivateNetwork
		cp.Spec.PrivateNetwork = &pn
	}

	if p.Spec.ClientTunnel != nil {
		ct := *p.Spec.ClientTunnel
		cp.Spec.ClientTunnel = &ct
		if p.Spec.ClientTunnel.MCP.Command != nil {
			cmd := *p.Spec.ClientTunnel.MCP.Command
			if cmd.Args != nil {
				cmd.Args = append([]string(nil), cmd.Args...)
			}
			if cmd.Env != nil {
				env := make(map[string]string, len(cmd.Env))
				for k, v := range cmd.Env {
					env[k] = v
				}
				cmd.Env = env
			}
			cp.Spec.ClientTunnel.MCP.Command = &cmd
		}
	}

	// Deep copy driver options
	if p.Driver.Options != nil {
		options := make(map[string]string, len(p.Driver.Options))
		for k, v := range p.Driver.Options {
			options[k] = v
		}
		cp.Driver.Options = options
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

	// Validate Kind is set
	if p.Kind == "" {
		return fmt.Errorf("connection kind is required")
	}

	// Validate exactly one spec is set and matches the Kind
	specCount := 0
	if p.Spec.ServiceExposure != nil {
		specCount++
		if p.Kind != ConnectionServiceExposure {
			return fmt.Errorf("kind %q does not match service exposure spec", p.Kind)
		}
		if err := validateServiceExposureSpec(p, p.Spec.ServiceExposure); err != nil {
			return err
		}
	}
	if p.Spec.PortForward != nil {
		specCount++
		if p.Kind != ConnectionPortForward {
			return fmt.Errorf("kind %q does not match port forward spec", p.Kind)
		}
		if err := validatePortForwardSpec(p.Spec.PortForward); err != nil {
			return err
		}
	}
	if p.Spec.PrivateNetwork != nil {
		specCount++
		if p.Kind != ConnectionPrivateNetwork {
			return fmt.Errorf("kind %q does not match private network spec", p.Kind)
		}
		if err := validatePrivateNetworkSpec(p.Spec.PrivateNetwork); err != nil {
			return err
		}
	}
	if p.Spec.ClientTunnel != nil {
		specCount++
		if p.Kind != ConnectionClientTunnel {
			return fmt.Errorf("kind %q does not match client tunnel spec", p.Kind)
		}
		if err := validateClientTunnelSpec(p, p.Spec.ClientTunnel); err != nil {
			return err
		}
	}
	if specCount != 1 {
		return fmt.Errorf("exactly one connection spec must be set, got %d", specCount)
	}

	return nil
}

// validateServiceExposureSpec validates a service exposure specification.
func validateServiceExposureSpec(p *ConnectionProfile, spec *ServiceExposureSpec) error {
	if spec == nil {
		return fmt.Errorf("service exposure spec is nil")
	}

	// Validate source: exactly one must be set, and Kind must match
	sourceCount := 0
	if spec.Source.Existing != nil {
		sourceCount++
		if spec.Source.Kind != SourceExisting {
			return fmt.Errorf("source kind %q does not match existing service spec", spec.Source.Kind)
		}
		if spec.Source.Existing.Address == "" {
			return fmt.Errorf("existing service address is required")
		}
		if spec.Source.Existing.Network != "" {
			switch spec.Source.Existing.Network {
			case "tcp", "udp":
			default:
				return fmt.Errorf("invalid network %q, must be tcp or udp", spec.Source.Existing.Network)
			}
		}
	}
	if spec.Source.Directory != nil {
		sourceCount++
		if spec.Source.Kind != SourceDirectory {
			return fmt.Errorf("source kind %q does not match directory spec", spec.Source.Kind)
		}
		if spec.Source.Directory.Path == "" {
			return fmt.Errorf("directory path is required")
		}
		switch spec.Source.Directory.Mode {
		case DirectoryModeRead, DirectoryModeWrites, "":
			// valid
		default:
			return fmt.Errorf("invalid directory mode %q", spec.Source.Directory.Mode)
		}
		if spec.Source.Directory.Mode != DirectoryModeWrites && (spec.Source.Directory.AllowUpload || spec.Source.Directory.AllowDelete) {
			return fmt.Errorf("directory upload/delete permissions require writes mode")
		}
	}
	if spec.Source.Command != nil {
		sourceCount++
		if spec.Source.Kind != SourceCommand {
			return fmt.Errorf("source kind %q does not match command spec", spec.Source.Kind)
		}
		if spec.Source.Command.Executable == "" {
			return fmt.Errorf("command executable is required")
		}
		if spec.Source.Command.Port < 1 || spec.Source.Command.Port > 65535 {
			return fmt.Errorf("command port must be between 1 and 65535")
		}
		if err := validateCommandEnvironment(spec.Source.Command.Env); err != nil {
			return err
		}
		switch spec.Source.Command.Protocol {
		case ProtocolHTTP, ProtocolHTTPS, "":
			// valid
		default:
			return fmt.Errorf("invalid command protocol %q", spec.Source.Command.Protocol)
		}
	}
	if spec.Source.MCP != nil {
		sourceCount++
		if spec.Source.Kind != SourceMCP {
			return fmt.Errorf("source kind %q does not match MCP spec", spec.Source.Kind)
		}
		if spec.Source.MCP.Endpoint == "" && spec.Source.MCP.Command == nil {
			return fmt.Errorf("MCP endpoint or command is required")
		}
		if spec.Source.MCP.Endpoint != "" && spec.Source.MCP.Command != nil {
			return fmt.Errorf("MCP source must use either an endpoint or a command, not both")
		}
		if spec.Source.MCP.Endpoint != "" {
			u, err := url.ParseRequestURI(spec.Source.MCP.Endpoint)
			if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
				return fmt.Errorf("MCP endpoint must be an absolute HTTP URL without embedded credentials")
			}
		}
		if spec.Source.MCP.Command != nil {
			if spec.Source.MCP.Command.Executable == "" {
				return fmt.Errorf("MCP command executable is required")
			}
			if spec.Source.MCP.Command.Port < 1 || spec.Source.MCP.Command.Port > 65535 {
				return fmt.Errorf("MCP command port must be between 1 and 65535")
			}
			if err := validateCommandEnvironment(spec.Source.MCP.Command.Env); err != nil {
				return err
			}
			if spec.Source.MCP.Command.Protocol != "" && spec.Source.MCP.Command.Protocol != ProtocolHTTP {
				return fmt.Errorf("MCP command protocol %q is not supported", spec.Source.MCP.Command.Protocol)
			}
		}
		switch spec.Source.MCP.Transport {
		case MCPTransportHTTP, MCPTransportStreamable, MCPTransportSSE, "":
			// valid
		default:
			return fmt.Errorf("invalid MCP transport %q", spec.Source.MCP.Transport)
		}
	}
	if sourceCount != 1 {
		return fmt.Errorf("exactly one source must be set, got %d", sourceCount)
	}

	// Validate exposure mode
	switch spec.Exposure.Mode {
	case ExposureTemporary:
		if spec.Exposure.RequestedAddress != "" {
			return fmt.Errorf("temporary exposure does not accept a requested hostname")
		}
	case ExposurePermanent:
		if spec.Exposure.RequestedAddress == "" {
			return fmt.Errorf("permanent exposure requires a requested hostname")
		}
	case ExposurePrivate:
		// Private-only exposure is not supported in v0.1
		return fmt.Errorf("private_only exposure is not supported in v0.1")
	case "":
		// Will be set to default later
	default:
		return fmt.Errorf("invalid exposure mode %q", spec.Exposure.Mode)
	}

	// Validate protocol
	switch spec.Exposure.Protocol {
	case ProtocolHTTP, ProtocolHTTPS, ProtocolTCP, ProtocolUDP, "":
		// valid
	default:
		return fmt.Errorf("invalid exposure protocol %q", spec.Exposure.Protocol)
	}

	// Validate desired state
	switch p.Desired {
	case DesiredOpen, DesiredClosed, "":
		// valid
	default:
		return fmt.Errorf("invalid desired state %q", p.Desired)
	}

	// Validate protection
	switch spec.Protection.Kind {
	case ProtectionNone, ProtectionEmailOTP, ProtectionIdentity, ProtectionServiceToken, ProtectionPrivateNet, "":
		// valid
	default:
		return fmt.Errorf("invalid protection kind %q", spec.Protection.Kind)
	}

	if spec.Protection.Kind == ProtectionServiceToken {
		if len(p.Driver.Options) == 0 {
			return fmt.Errorf("service token protection requires driver options")
		}
	}

	// Access protection needs an address that does not move. A temporary
	// address changes every time the connector restarts, so a policy bound to
	// it stops applying without anything reporting that it stopped.
	//
	// This lived only as a condition in the setup wizard, which meant it was
	// enforced for people using that screen and by nothing else: the
	// recommendation engine checks whether a provider lists the protection
	// kind, never whether the exposure can carry it, so the combination was
	// accepted and failed when the plan was applied — after the connection had
	// been saved. It belongs here, where every path reaches it.
	// Private-network protection is exempt: it is not a policy bound to a
	// public hostname, so a changing address does not detach it.
	if spec.Protection.Kind != "" && spec.Protection.Kind != ProtectionNone &&
		spec.Protection.Kind != ProtectionPrivateNet &&
		spec.Exposure.Mode == ExposureTemporary {
		return fmt.Errorf(
			"%s protection needs a permanent address: a temporary address changes when the "+
				"connector restarts, so the protection would stop applying", spec.Protection.Kind)
	}

	// Write-enabled directories must not be exposed without protection.
	// A publicly reachable upload endpoint is a severe security risk.
	if spec.Source.Directory != nil && spec.Source.Directory.Mode == DirectoryModeWrites &&
		spec.Source.Directory.AllowUpload && spec.Protection.Kind == ProtectionNone {
		return fmt.Errorf("write-enabled directory with upload requires protection; use email_otp, identity_provider, service_token, or private_network")
	}

	if p.Lifecycle.OnDisconnect != "" && p.Lifecycle.OnDisconnect != DisconnectKeepAlive && p.Lifecycle.OnDisconnect != DisconnectClose {
		return fmt.Errorf("invalid disconnect policy %q", p.Lifecycle.OnDisconnect)
	}

	// Validate driver selection
	if p.Driver.ProviderID == "" {
		return fmt.Errorf("provider ID is required")
	}

	// Validate command working directory
	if spec.Source.Command != nil {
		if spec.Source.Command.WorkingDir != "" {
			// Working directory is validated at use time
		}
	}

	return nil
}

// validatePortForwardSpec validates a port forward specification.
func validatePortForwardSpec(spec *PortForwardSpec) error {
	if spec == nil {
		return fmt.Errorf("port forward spec is nil")
	}

	if spec.LocalPort < 1 || spec.LocalPort > 65535 {
		return fmt.Errorf("local port must be between 1 and 65535")
	}

	if spec.RemotePort < 1 || spec.RemotePort > 65535 {
		return fmt.Errorf("remote port must be between 1 and 65535")
	}

	if spec.RemoteHost == "" {
		return fmt.Errorf("remote host is required")
	}

	switch spec.Protocol {
	case ProtocolTCP, ProtocolUDP, "":
		// valid
	default:
		return fmt.Errorf("invalid protocol %q, must be tcp or udp", spec.Protocol)
	}

	switch spec.Direction {
	case PortForwardLocal, PortForwardRemote, "":
		// valid
	default:
		return fmt.Errorf("invalid direction %q, must be local or remote", spec.Direction)
	}

	return nil
}

// validateClientTunnelSpec validates a client-mediated tunnel specification.
func validateClientTunnelSpec(p *ConnectionProfile, spec *ClientTunnelSpec) error {
	if spec == nil {
		return fmt.Errorf("client tunnel spec is nil")
	}
	if spec.Client != ClientOpenAISecureMCPTunnel {
		return fmt.Errorf("unsupported tunnel client %q", spec.Client)
	}
	// The client forwards to exactly one local MCP server, supplied either as a
	// URL for an HTTP server or a command for a stdio server.
	if spec.MCP.Endpoint == "" && spec.MCP.Command == nil {
		return fmt.Errorf("a client tunnel requires an MCP endpoint or command")
	}
	if spec.MCP.Endpoint != "" && spec.MCP.Command != nil {
		return fmt.Errorf("a client tunnel must use either an MCP endpoint or a command, not both")
	}
	if spec.MCP.Endpoint != "" {
		u, err := url.ParseRequestURI(spec.MCP.Endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return fmt.Errorf("MCP endpoint must be an absolute HTTP URL without embedded credentials")
		}
	}
	if spec.MCP.Command != nil {
		if spec.MCP.Command.Executable == "" {
			return fmt.Errorf("MCP command executable is required")
		}
		if err := validateCommandEnvironment(spec.MCP.Command.Env); err != nil {
			return err
		}
	}
	if p.Driver.ProviderID == "" {
		return fmt.Errorf("provider ID is required")
	}
	switch p.Desired {
	case DesiredOpen, DesiredClosed, "":
	default:
		return fmt.Errorf("invalid desired state %q", p.Desired)
	}
	return nil
}

// validatePrivateNetworkSpec validates a private network specification.
func validatePrivateNetworkSpec(spec *PrivateNetworkSpec) error {
	if spec == nil {
		return fmt.Errorf("private network spec is nil")
	}

	if spec.NetworkID == "" {
		return fmt.Errorf("network ID is required")
	}

	switch spec.Mode {
	case PrivateNetworkJoin, PrivateNetworkExpose, "":
		// valid
	default:
		return fmt.Errorf("invalid mode %q, must be join or expose", spec.Mode)
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

// ValidateForOpen reports whether this profile could be opened as it stands.
//
// It validates a copy with open intent rather than the profile itself, so a
// stored connection is never mutated by being checked, and a rule that tightens
// after a connection was saved refuses the attempt to open it rather than
// making the connection disappear.
//
// Every path that tries to realise an open state must call this: opening,
// repairing, and automatic reconciliation. Closing and deleting must not — a
// connection that can no longer open must still be closable and removable, or
// a tightened rule would strand it with no way out.
func (p *ConnectionProfile) ValidateForOpen() error {
	candidate := p.DeepCopy()
	candidate.Desired = DesiredOpen
	return candidate.Validate()
}
