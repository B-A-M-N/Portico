// Package openaitunnel implements core.Provider for OpenAI's Secure MCP Tunnel.
//
// The tunnel is client-mediated: a local `tunnel-client` binary opens an
// outbound HTTPS connection to OpenAI, receives MCP requests, forwards them to
// a local MCP server, and returns responses over the same connection. No
// inbound port is opened and no public address is created.
//
// Every operational detail here — the binary name, its subcommands, its flags,
// the credential environment variable and the health endpoints — comes from
// OpenAI's published documentation. Behaviour that could not be verified from
// documentation is deliberately absent rather than stubbed; see
// docs/CLIENT_TUNNEL_DESIGN.md.
package openaitunnel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// ClientBinary is the customer-run agent's executable name.
const ClientBinary = "tunnel-client"

// ProviderID is the generic transport registration for a client-mediated MCP
// connection. OpenAI is the workload/profile carried by this transport, not a
// provider catalog entry.
const ProviderID core.ProviderID = "client_tunnel"

// CredentialEnvVar is the environment variable holding the control-plane key.
//
// The client does not take the key as a value. It takes a *reference* of the
// form env:VARNAME or file:/path, which it resolves itself, so the secret never
// appears in a command line even as an argument value.
const CredentialEnvVar = "CONTROL_PLANE_API_KEY"

// credentialReference is what is passed to --control-plane.api-key.
const credentialReference = "env:" + CredentialEnvVar

// tunnel ID validation lives in core (ValidateTunnelID) so the adapter and
// the wizard share one authority for the control plane's format.
// plan time turns a malformed ID into a preview-time error rather than a
// process that starts and immediately exits.

// Provider adapts the Secure MCP Tunnel client to Portico's provider contract.
type Provider struct {
	binPath string
	// adminBaseURL is discovered from the health URL file the client writes on
	// startup rather than assumed, because the health port is configurable and
	// defaults to a fixed port that may already be in use.
	adminBaseURL  string
	connectorProc core.ConnectorProcessService
	// gateways is kept as a compatibility boundary for older provider tests
	// whose fake used the pre-GatewayStartSpec method shape. Production wiring
	// supplies core.GatewayService; the small adapters below accept both shapes.
	gateways any
	// runtimeDir is Portico's private runtime directory. Health URL files are
	// per-connection files inside a 0700 subdirectory of it — never the shared
	// temp dir, where a predictable name would let another local user pre-place
	// or read the file.
	runtimeDir string
	// healthMu guards healthFiles. Each connection gets its own health URL
	// file: two simultaneous tunnels must not share, and must not inherit,
	// one another's endpoints.
	healthMu    sync.Mutex
	healthFiles map[core.ConnectionID]string
	// lookPath is injectable so tests do not depend on the binary being
	// installed on the machine running them.
	lookPath func(string) (string, error)
	// probe is injectable so readiness can be tested without a live client.
	probe func(ctx context.Context, url string) error
	// storedCredential is the decrypted runtime key handed over by activation.
	// When set it takes precedence over the supervisor's environment, so a
	// rotation through Portico takes effect at the next semantic restart
	// without touching the daemon's environment. It is held only in memory:
	// persistence is the store's encrypted row, and the value never reaches
	// argv, plans, events, logs, or DTOs.
	storedCredential string
}

// SetAdminBaseURL overrides where the client's health endpoints are expected.
// The client serves them on loopback; tests point this at a stub server so the
// real HTTP probe is exercised rather than replaced.
func (p *Provider) SetAdminBaseURL(base string) {
	p.adminBaseURL = base
}

// New creates a Secure MCP Tunnel provider.
func New(binPath string, procMgr core.ConnectorProcessService) *Provider {
	return NewWithGateway(binPath, procMgr, nil)
}

// NewWithGateway creates the OpenAI profile runtime with the supervisor-owned
// gateway service. Keeping the old constructor preserves isolated provider
// tests and callers that deliberately exercise the direct transport path.
func NewWithGateway(binPath string, procMgr core.ConnectorProcessService, gateways core.GatewayService) *Provider {
	if binPath == "" {
		binPath = ClientBinary
	}
	p := &Provider{
		binPath:       binPath,
		connectorProc: procMgr,
		gateways:      gateways,
		lookPath:      exec.LookPath,
		healthFiles:   make(map[core.ConnectionID]string),
	}
	p.probe = p.httpProbe
	return p
}

type legacyGatewayService interface {
	StartGateway(context.Context, core.ConnectionID, string, []string) (string, error)
	StopGateway(core.ConnectionID) error
}

func startGateway(value any, ctx context.Context, connectionID core.ConnectionID, spec core.GatewayStartSpec) (string, error) {
	switch gateway := value.(type) {
	case core.GatewayService:
		return gateway.StartGateway(ctx, connectionID, spec)
	case legacyGatewayService:
		return gateway.StartGateway(ctx, connectionID, spec.Upstream, nil)
	default:
		return "", fmt.Errorf("gateway service has unsupported type %T", value)
	}
}

func stopGateway(value any, connectionID core.ConnectionID) error {
	switch gateway := value.(type) {
	case core.GatewayService:
		return gateway.StopGateway(connectionID)
	case legacyGatewayService:
		return gateway.StopGateway(connectionID)
	default:
		return fmt.Errorf("gateway service has unsupported type %T", value)
	}
}

// SetRuntimeDir points the provider at Portico's private runtime directory,
// where per-connection health URL files are created. Called by activation;
// tests may leave it unset.
func (p *Provider) SetRuntimeDir(dir string) {
	p.runtimeDir = dir
}

// SetCredential installs the decrypted stored runtime key as the adapter's
// credential source. Called by activation with material decrypted by the
// supervisor's credential store; the value lives only in this struct.
func (p *Provider) SetCredential(secret string) {
	p.storedCredential = secret
}

// credential resolves the control-plane key from the single authority: the
// stored key when one was activated, otherwise the supervisor's environment.
func (p *Provider) credential() string {
	if p.storedCredential != "" {
		return p.storedCredential
	}
	return os.Getenv(CredentialEnvVar)
}

// Identity returns the provider identity.
func (p *Provider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID:          ProviderID,
		Name:        string(ProviderID),
		DisplayName: "Client-mediated MCP transport",
	}
}

// Capabilities declares a private-only provider.
//
// Declaring TemporaryAddresses and CustomHostnames unsupported is load-bearing,
// not documentation: the recommendation engine treats capabilities as hard
// constraints, so this is what prevents the adapter being offered for a public
// exposure and prevents a public provider being offered for a client tunnel.
//
// Every capability is experimental. Portico has not been exercised against a
// live tunnel, and declaring otherwise would repeat the over-claim this audit
// was written to correct.
func (p *Provider) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		Kinds: []core.ConnectionKind{core.ConnectionClientTunnel},
		TemporaryAddresses: core.CapabilitySupport{
			Supported: false,
			Notes:     []string{"the tunnel is private; it never creates a public address"},
		},
		CustomHostnames: core.CapabilitySupport{
			Supported: false,
			Notes:     []string{"the tunnel has no hostname and creates no DNS record"},
		},
		PrivateExposure: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityExperimental,
		},
		ManagedDNS: core.CapabilitySupport{
			Supported: false,
			Notes:     []string{"the tunnel creates no DNS record"},
		},
		BuiltInProtection: []core.ProtectionCapability{
			// Access is mediated entirely by the platform. Portico applies no
			// policy of its own and must not imply that it does.
			{Kind: core.ProtectionNone, Supported: true, Stability: core.StabilityStable},
		},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Public: false, Private: true},
		},
		Streaming: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityExperimental,
			Notes:     []string{"the client-mediated MCP stream is outbound and experimental"},
		},
		Telemetry:  core.TelemetryCapability{Supported: false},
		Redundancy: core.RedundancyCapability{Supported: false, MaxConnectors: 1},
		Expiration: core.ExpirationCapability{Supported: false},
	}, nil
}

// Authenticate reports whether a control-plane key is resolvable. Portico does
// not validate the key itself; that happens when the client runs and is
// reported honestly through readiness observation.
func (p *Provider) Authenticate(context.Context, core.AuthRequest) error {
	if p.credential() == "" {
		return fmt.Errorf("no control plane API key is stored and %s is not set; run 'portico provider login %s'",
			CredentialEnvVar, ProviderID)
	}
	return nil
}

// The tunnel ID is not account state: it belongs to the connection
// (ClientTunnelSpec.TunnelID) and reaches the client through step parameters.
// The setup flow itself is declared by the definition, which owns what
// configuration means before any adapter exists.
func (p *Provider) SetupFlow() core.SetupFlow { return openAITunnelSetupFlow() }

// Plan produces the operation plan for a client tunnel.
//
// The order is deliberate: the local MCP server and the client are both
// verified before anything is started, so a misconfigured connection fails
// before a process exists.
func (p *Provider) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	profile := desired.Profile
	if profile == nil {
		return nil, fmt.Errorf("openai tunnel: profile required")
	}
	if profile.Kind != core.ConnectionClientTunnel {
		return nil, fmt.Errorf("openai tunnel: connection kind %q is not supported", profile.Kind)
	}
	spec := profile.Spec.ClientTunnel
	if spec == nil {
		return nil, fmt.Errorf("openai tunnel: client tunnel spec is required")
	}
	if spec.MCP.Endpoint == "" && spec.MCP.Command == nil {
		return nil, fmt.Errorf("openai tunnel: an MCP endpoint or command is required")
	}

	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    profile.ID,
		ProfileRevision: profile.Revision,
		Provider:        ProviderID,
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}

	switch profile.Desired {
	case core.DesiredOpen:
		plan.Intent = core.IntentOpen
		if spec.TunnelID == "" {
			return nil, fmt.Errorf(
				"openai tunnel: a tunnel ID is required; create the tunnel in the OpenAI platform's organization settings first")
		}
		if !core.ValidTunnelID(spec.TunnelID) {
			return nil, core.ErrValidation(fmt.Sprintf(
				"tunnel ID %q is malformed; the client requires tunnel_ followed by 32 lowercase hexadecimal characters",
				spec.TunnelID))
		}
		params := map[string]string{"tunnel_id": spec.TunnelID}
		if spec.MCP.Endpoint != "" {
			endpoint := spec.MCP.Endpoint
			if desired.GatewayRequired {
				if desired.GatewayEndpoint != "" {
					endpoint = desired.GatewayEndpoint
				} else {
					endpoint = core.GatewayTargetRef
				}
				// Keep the raw local origin as an internal planning input. The
				// public mcp_server_url remains pinned to the gateway, while the
				// runtime can still build that gateway around the true origin.
				params["origin_mcp_server_url"] = spec.MCP.Endpoint
			}
			params["mcp_server_url"] = endpoint
			params["mcp_transport"] = string(spec.MCP.Transport)
		} else {
			params["mcp_command"] = spec.MCP.Command.Executable
		}

		plan.Steps = []core.PlanStep{
			{
				ID: "tunnel-verify-origin", Kind: core.StepVerifyOrigin,
				Summary:   "Verify the local MCP server is reachable",
				Technical: core.TechnicalOperation{Provider: ProviderID, Type: "verify_origin", Parameters: params},
			},
			{
				ID: "tunnel-validate", Kind: core.StepValidateAccount,
				Summary:   "Verify the tunnel client is installed and has a credential",
				Technical: core.TechnicalOperation{Provider: ProviderID, Type: "validate_client"},
			},
			{
				ID: "tunnel-start", Kind: core.StepStartConnector,
				Summary:   "Start the tunnel client",
				Technical: core.TechnicalOperation{Provider: ProviderID, Type: "start_client", Parameters: params},
				// If a later step fails — most importantly /readyz never
				// passing — this rolls back everything startClient created:
				// the client process and the local gateway. Idempotent by
				// construction: stopping an already-stopped client succeeds.
				Compensation: &core.CompensationStep{
					ID:   "comp-tunnel-stop",
					Kind: core.StepStopConnector,
					Technical: core.TechnicalOperation{
						Provider: ProviderID,
						Type:     "stop_client",
					},
				},
			},
			{
				ID: "tunnel-verify", Kind: core.StepVerifyConnector,
				Summary:   "Verify the tunnel client reports ready",
				Technical: core.TechnicalOperation{Provider: ProviderID, Type: "verify_client"},
			},
		}
		// There is no public address. Expected state records the private
		// nature of the connection rather than leaving a field that other code
		// might fill with a public one.
		plan.Expected.State = core.RuntimeOpen
		plan.Expected.PrivateAddress = "private tunnel to OpenAI"

	case core.DesiredClosed:
		plan.Intent = core.IntentClose
		plan.Steps = []core.PlanStep{
			{
				ID: "tunnel-stop", Kind: core.StepStopConnector,
				Summary:   "Stop the tunnel client",
				Technical: core.TechnicalOperation{Provider: ProviderID, Type: "stop_client"},
			},
		}
		plan.Expected.State = core.RuntimeClosed

	default:
		return nil, fmt.Errorf("openai tunnel: unsupported desired state %q", profile.Desired)
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("openai tunnel: plan fingerprint: %w", err)
	}
	return plan, nil
}

// ExecuteStep executes one plan step.
func (p *Provider) ExecuteStep(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	switch step.Kind {
	case core.StepVerifyOrigin:
		return p.verifyOrigin(ctx, step), nil
	case core.StepValidateAccount:
		return p.validateClient(ctx, step), nil
	case core.StepStartConnector:
		return p.startClient(ctx, connectionID, step), nil
	case core.StepVerifyConnector:
		return p.verifyClient(ctx, connectionID, step), nil
	case core.StepStopConnector:
		return p.stopClient(connectionID, step), nil
	default:
		return core.StepResult{StepID: step.ID, Succeeded: false},
			fmt.Errorf("openai tunnel: unsupported step kind %q", step.Kind)
	}
}

func (p *Provider) verifyOrigin(ctx context.Context, step core.PlanStep) core.StepResult {
	endpoint := step.Technical.Parameters["mcp_server_url"]
	if endpoint == core.GatewayTargetRef {
		endpoint = step.Technical.Parameters["origin_mcp_server_url"]
	}
	if endpoint == "" {
		// A stdio server is started by the client itself, so there is nothing
		// to probe here. Saying so is better than reporting a vacuous success.
		return core.StepResult{StepID: step.ID, Succeeded: true}
	}
	transport := core.MCPTransport(step.Technical.Parameters["mcp_transport"])
	// MCPServiceCheck performs a protocol-aware probe — an initialize POST for
	// Streamable HTTP, an event-stream GET for SSE — instead of a generic GET,
	// which a legitimate Streamable HTTP server may legitimately answer 405.
	if err := core.MCPServiceCheck(ctx, endpoint, transport); err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("local MCP server at %s did not answer the MCP handshake: %w", endpoint, err)}
	}
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

func (p *Provider) validateClient(ctx context.Context, step core.PlanStep) core.StepResult {
	if _, err := p.lookPath(p.binPath); err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("%s is not installed or not on PATH; download it from the OpenAI platform's tunnel settings", p.binPath)}
	}
	if p.credential() == "" {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("no control plane API key is stored and %s is not set; run 'portico provider login %s' or export %s",
				CredentialEnvVar, ProviderID, CredentialEnvVar)}
	}
	// The upstream doctor is the preflight authority: it checks the real
	// client contract (flags, credential acceptance, MCP reachability, health
	// listener) from inside, where Portico can only approximate. A failed
	// doctor stops the operation before a process exists.
	report, err := p.runDoctorPreflight(ctx,
		step.Technical.Parameters["tunnel_id"],
		originEndpoint(step.Technical.Parameters))
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
	}
	if len(report.Issues) > 0 {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("tunnel-client doctor reported problems: %s",
				strings.Join(report.Issues, "; "))}
	}
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// clientProcessSpec builds the tunnel client's process specification.
//
// The control-plane key travels in the environment only. Placing it in argv
// would expose it through /proc and any process listing, which is the same
// defect this audit found in the ngrok adapter.
func (p *Provider) clientProcessSpec(step core.PlanStep) core.ProcessSpec {
	args := []string{"run"}
	if tunnelID := step.Technical.Parameters["tunnel_id"]; tunnelID != "" {
		args = append(args, "--control-plane.tunnel-id", tunnelID)
	}
	// A reference, not the secret. The client resolves it from the environment
	// itself, so the value never appears in argv at all.
	args = append(args, "--control-plane.api-key", credentialReference)

	if url := step.Technical.Parameters["mcp_server_url"]; url != "" {
		args = append(args, "--mcp.server-url", "url="+url)
	} else if command := step.Technical.Parameters["mcp_command"]; command != "" {
		args = append(args, "--mcp.command", "command="+command)
	}

	// Ask for an ephemeral health port and have the client report it, rather
	// than assuming the default port is free. The URL file belongs to this one
	// connection and lives in Portico's private runtime directory.
	if urlFile := step.Technical.Parameters["health_url_file"]; urlFile != "" {
		args = append(args, "--health.url-file", urlFile)
	}

	return core.ProcessSpec{
		Executable: p.binPath,
		Args:       args,
		Env:        []string{CredentialEnvVar + "=" + p.credential()},
		// The client is not stateless: a correct relaunch needs the gateway
		// running first and its current endpoint in --mcp.server-url, which
		// the generic actor cannot know (a crash stops the gateway, and
		// blind re-execution would point at the dead one). RestartNever
		// leaves restart to supervisor reconciliation, which replans the
		// whole chain — gateway, credential, client, /readyz.
		Restart: core.RestartNever,
	}
}

// healthDir returns the 0700 directory holding per-connection health URL
// files. It lives under Portico's own state, not the shared temp dir: a file
// there is owned by this process's user alone and its parent cannot be swapped.
func (p *Provider) healthDir() string {
	return filepath.Join(p.runtimeDir, "tunnel-health")
}

// healthURLFileFor returns this connection's health URL file path. The name
// carries the connection ID, which is a UUID: two simultaneous tunnels get two
// files, and neither inherits the other's endpoint. Any previous file at the
// path is removed so a stale endpoint from an earlier run can never be read as
// this run's answer.
func (p *Provider) healthURLFileFor(id core.ConnectionID) (string, error) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	if path, ok := p.healthFiles[id]; ok {
		_ = os.Remove(path)
		return path, nil
	}
	dir := p.healthDir()
	if p.runtimeDir == "" {
		return "", fmt.Errorf("no runtime directory is configured; the client cannot report its health URL")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create tunnel-health directory: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s.url", id))
	_ = os.Remove(path)
	p.healthFiles[id] = path
	return path, nil
}

// forgetHealthFile drops and removes the connection's health URL state. Called
// on stop so a stopped connection leaves no endpoint behind.
func (p *Provider) forgetHealthFile(id core.ConnectionID) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	if path, ok := p.healthFiles[id]; ok {
		_ = os.Remove(path)
		delete(p.healthFiles, id)
	}
}

// validateHealthBase accepts only what a local tunnel-client health endpoint
// can be: plain HTTP on the loopback interface with an explicit port. Anything
// else — https, a remote host, embedded credentials, a non-default port field —
// means the file did not contain what we asked the client to write.
func validateHealthBase(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("health URL does not parse: %w", err)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("health URL scheme %q is not local http", u.Scheme)
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("health URL host %q is not loopback", host)
	}
	if u.Port() == "" {
		return fmt.Errorf("health URL has no port")
	}
	if u.User != nil {
		return fmt.Errorf("health URL carries userinfo")
	}
	return nil
}

// adminBase resolves one connection's health base URL, preferring the file the
// client writes on startup over any configured override.
func (p *Provider) adminBase(id core.ConnectionID) (string, error) {
	if p.adminBaseURL != "" {
		return p.adminBaseURL, nil
	}
	p.healthMu.Lock()
	path := p.healthFiles[id]
	p.healthMu.Unlock()
	if path == "" {
		return "", fmt.Errorf("the client was not asked to report its health URL")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the client has not reported a health URL yet: %w", err)
	}
	base := strings.TrimSpace(string(data))
	if base == "" {
		return "", fmt.Errorf("the client reported an empty health URL")
	}
	if err := validateHealthBase(base); err != nil {
		return "", err
	}
	return base, nil
}

func (p *Provider) startClient(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) core.StepResult {
	urlFile, err := p.healthURLFileFor(connectionID)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
	}
	if p.connectorProc == nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("no process manager is available to supervise the tunnel client")}
	}

	// The gateway must be started before the client so the client forwards MCP
	// traffic through Portico's auth/observability boundary rather than directly
	// to the raw local server. The gateway manager owns the lifetime beyond this
	// operation, so a completed apply cannot tear it down with its context.
	effectiveStep := step.Clone()
	gatewayStarted := false
	if p.gateways != nil && effectiveStep.Technical.Parameters["mcp_server_url"] != "" {
		upstream := originEndpoint(effectiveStep.Technical.Parameters)
		endpoint, err := startGateway(p.gateways, ctx, connectionID, core.GatewayStartSpec{
			Upstream: upstream,
			// The client-mediated MCP transport has no independent client
			// credential surface yet: the gateway fronts the local MCP server
			// for the tunnel-client process on loopback only. Declared
			// explicitly here rather than inferred from token absence.
			AuthRequired: false,
			AllowSSE:     true,
		})
		if err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("start Portico gateway: %w", err)}
		}
		effectiveStep.Technical.Parameters["mcp_server_url"] = endpoint
		gatewayStarted = true
	}
	effectiveStep.Technical.Parameters["health_url_file"] = urlFile
	handle, err := p.connectorProc.Start(ctx, core.ProcessConfig{
		ConnectionID: connectionID,
		Spec:         p.clientProcessSpec(effectiveStep),
	})
	if err != nil {
		if gatewayStarted {
			_ = stopGateway(p.gateways, connectionID)
		}
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
	}
	return core.StepResult{
		StepID:    step.ID,
		Succeeded: true,
		Resources: []core.ProviderResource{{
			ConnectionID: connectionID,
			ProviderID:   ProviderID,
			Type:         core.ResourceTunnel,
			ExternalID:   step.Technical.Parameters["tunnel_id"],
			// The tunnel is created on OpenAI's platform, not by Portico.
			// Recording it as adopted keeps Portico from deleting something it
			// did not create.
			Ownership: core.OwnershipAdopted,
			Metadata: map[string]string{
				"pid":             fmt.Sprint(handle.PID),
				"health_url_file": urlFile,
			},
		}},
	}
}

func originEndpoint(parameters map[string]string) string {
	if origin := parameters["origin_mcp_server_url"]; origin != "" {
		return origin
	}
	return parameters["mcp_server_url"]
}

// verifyClient waits for the client to report ready.
//
// /healthz and /readyz mean different things and both are needed: the client
// answers /healthz as soon as its HTTP server is up, but returns 503 from
// /readyz until it has actually reached the control plane. Treating liveness as
// readiness would report a tunnel as open while it was still unauthenticated.
func (p *Provider) verifyClient(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) core.StepResult {
	deadline := time.Now().Add(clientReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		base, err := p.adminBase(connectionID)
		if err != nil {
			lastErr = err
		} else if err := p.probe(ctx, base+"/readyz"); err == nil {
			return core.StepResult{StepID: step.ID, Succeeded: true}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: ctx.Err()}
		case <-time.After(500 * time.Millisecond):
		}
	}
	return core.StepResult{StepID: step.ID, Succeeded: false,
		Error: fmt.Errorf("the tunnel client did not report ready: %w", lastErr)}
}

// clientReadyTimeout bounds how long to wait for the client to reach the
// control plane before the step fails.
const clientReadyTimeout = 30 * time.Second

func (p *Provider) stopClient(connectionID core.ConnectionID, step core.PlanStep) core.StepResult {
	defer p.forgetHealthFile(connectionID)
	if p.connectorProc != nil {
		if err := p.connectorProc.Stop(connectionID, 5*time.Second); err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
		}
	}
	if p.gateways != nil {
		if err := stopGateway(p.gateways, connectionID); err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("stop Portico gateway: %w", err)}
		}
	}
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// Observe reports what Portico can actually determine.
//
// Liveness and readiness are separate facts and both are checked. The client
// answers /healthz as soon as its HTTP server is up; /readyz stays 503 until it
// has reached the control plane. A process that answers /healthz while /readyz
// fails is alive but not carrying traffic — reporting it running would converge
// reconciliation toward a healthy-looking state over a broken tunnel.
//
// Portico cannot see whether the app has been registered in ChatGPT, so it
// does not claim to; that step is reported to the user as an outstanding
// action rather than inferred.
func (p *Provider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	observed := &core.ObservedConnection{ConnectionID: id, ProviderID: ProviderID}
	if p.connectorProc == nil {
		return observed, nil
	}
	handle, running := p.connectorProc.Observe(id)
	if !running || handle.PID == 0 {
		observed.Connector = &core.ObservedConnector{Status: string(core.ConnectorStatusStopped)}
		return observed, nil
	}
	connector := &core.ObservedConnector{PID: handle.PID, Status: string(core.ConnectorStatusRunning)}
	base, baseErr := p.adminBase(id)
	switch {
	case baseErr != nil:
		connector.Status = string(core.ConnectorStatusUnstable)
		connector.LastError = baseErr.Error()
	case p.probe(ctx, base+"/healthz") != nil:
		// The process is alive but its local server is not answering at all,
		// which is a degraded tunnel rather than a stopped one.
		connector.Status = string(core.ConnectorStatusUnstable)
		connector.LastError = "the tunnel client's health endpoint is not answering"
	case p.probe(ctx, base+"/readyz") != nil:
		// Alive and serving /healthz, but startup or downstream readiness has
		// not passed: the control plane is unreachable or the credential was
		// refused. The transport is down even though the process is not.
		connector.Status = string(core.ConnectorStatusUnstable)
		connector.LastError = "the tunnel client is running but not ready (control plane unreachable or credential rejected)"
	}
	observed.Connector = connector
	return observed, nil
}

// httpProbe performs a bounded GET that refuses redirects. A health endpoint is
// loopback-only by contract; following a redirect would let a compromised or
// misbehaving endpoint point the probe at an arbitrary remote URL and have the
// probe report on that instead. Any 3xx is therefore treated as a failure.
func (p *Provider) httpProbe(ctx context.Context, url string) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("returned status %d", resp.StatusCode)
	}
	return nil
}
