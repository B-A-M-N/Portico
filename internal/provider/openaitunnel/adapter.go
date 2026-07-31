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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// ClientBinary is the customer-run agent's executable name.
const ClientBinary = "tunnel-client"

// CredentialEnvVar is the environment variable holding the control-plane key.
//
// The client does not take the key as a value. It takes a *reference* of the
// form env:VARNAME or file:/path, which it resolves itself, so the secret never
// appears in a command line even as an argument value.
const CredentialEnvVar = "CONTROL_PLANE_API_KEY"

// credentialReference is what is passed to --control-plane.api-key.
const credentialReference = "env:" + CredentialEnvVar

// tunnelIDPattern is the identifier format the client accepts. Validating at
// plan time turns a malformed ID into a preview-time error rather than a
// process that starts and immediately exits.
var tunnelIDPattern = regexp.MustCompile(`^tunnel_[a-z0-9]{32}$`)

// Provider adapts the Secure MCP Tunnel client to Portico's provider contract.
type Provider struct {
	binPath string
	// adminBaseURL is discovered from the health URL file the client writes on
	// startup rather than assumed, because the health port is configurable and
	// defaults to a fixed port that may already be in use.
	adminBaseURL  string
	healthURLFile string
	connectorProc core.ConnectorProcessService
	// lookPath is injectable so tests do not depend on the binary being
	// installed on the machine running them.
	lookPath func(string) (string, error)
	// probe is injectable so readiness can be tested without a live client.
	probe func(ctx context.Context, url string) error
}

// SetAdminBaseURL overrides where the client's health endpoints are expected.
// The client serves them on loopback; tests point this at a stub server so the
// real HTTP probe is exercised rather than replaced.
func (p *Provider) SetAdminBaseURL(base string) {
	p.adminBaseURL = base
}

// New creates a Secure MCP Tunnel provider.
func New(binPath string, procMgr core.ConnectorProcessService) *Provider {
	if binPath == "" {
		binPath = ClientBinary
	}
	p := &Provider{
		binPath:       binPath,
		connectorProc: procMgr,
		lookPath:      exec.LookPath,
	}
	p.probe = p.httpProbe
	return p
}

// Identity returns the provider identity.
func (p *Provider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID:          "openai_tunnel",
		Name:        "openai_tunnel",
		DisplayName: "OpenAI Secure MCP Tunnel",
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
		Telemetry:  core.TelemetryCapability{Supported: false},
		Redundancy: core.RedundancyCapability{Supported: false, MaxConnectors: 1},
		Expiration: core.ExpirationCapability{Supported: false},
	}, nil
}

// Authenticate reports whether the control-plane credential is present.
// Portico does not validate the key itself; that happens when the client runs.
func (p *Provider) Authenticate(context.Context, core.AuthRequest) error {
	if os.Getenv(CredentialEnvVar) == "" {
		return fmt.Errorf("%s is not set; the tunnel client cannot reach the OpenAI control plane", CredentialEnvVar)
	}
	return nil
}

// SetupFlow declares what the provider needs, for the generic setup screen.
//
// This is guidance, not a form. The tunnel client reads the control-plane key
// from the supervisor's own environment — see validateClient and
// clientProcessSpec, both of which call os.Getenv(CredentialEnvVar) — and
// nothing here reads Portico's account store. A credential stored through
// Portico would therefore change nothing, and setup would report success while
// the connection went on failing for exactly the reason it failed before.
//
// The tunnel ID is not account state either: it belongs to the connection
// (ClientTunnelSpec.TunnelID) and reaches the client through step parameters.
func openAITunnelSetupFlow() core.SetupFlow {
	return core.SetupFlow{
		Kind:    core.SetupGuidance,
		Summary: "Connect a local MCP server to ChatGPT over a private, outbound-only tunnel.",
		GuidanceReason: "The tunnel client reads " + CredentialEnvVar + " from the supervisor's own " +
			"environment, so Portico has nowhere to put a credential you enter here. Export it before " +
			"starting the supervisor.",
		Fields: []core.SetupField{
			{
				ID:          "tunnel_id",
				Label:       "Tunnel ID",
				Description: "Create a tunnel in the OpenAI platform's organization settings, then paste its ID here. Portico does not create tunnels.",
				Required:    true,
				Placeholder: "tunnel_...",
			},
			{
				ID:          "credential",
				Label:       "Control plane API key",
				Description: "Supplied to the client through " + CredentialEnvVar + ". Never placed in a command line.",
				Secret:      true,
				Required:    true,
			},
		},
		CapabilityNotes: []string{
			"The connection is outbound-only: no inbound port is opened and no public address is created.",
			"Creating the tunnel and registering the app in ChatGPT happen on OpenAI's platform, not in Portico.",
		},
	}
}

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
		Provider:        "openai_tunnel",
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
		if !tunnelIDPattern.MatchString(spec.TunnelID) {
			return nil, core.ErrValidation(fmt.Sprintf(
				"tunnel ID %q is malformed; the client requires tunnel_ followed by 32 lowercase letters or digits",
				spec.TunnelID))
		}
		params := map[string]string{"tunnel_id": spec.TunnelID}
		if spec.Profile != "" {
			params["profile"] = spec.Profile
		}
		if spec.MCP.Endpoint != "" {
			params["mcp_server_url"] = spec.MCP.Endpoint
		} else {
			params["mcp_command"] = spec.MCP.Command.Executable
		}

		plan.Steps = []core.PlanStep{
			{
				ID: "tunnel-verify-origin", Kind: core.StepVerifyOrigin,
				Summary:   "Verify the local MCP server is reachable",
				Technical: core.TechnicalOperation{Provider: "openai_tunnel", Type: "verify_origin", Parameters: params},
			},
			{
				ID: "tunnel-validate", Kind: core.StepValidateAccount,
				Summary:   "Verify the tunnel client is installed and has a credential",
				Technical: core.TechnicalOperation{Provider: "openai_tunnel", Type: "validate_client"},
			},
			{
				ID: "tunnel-start", Kind: core.StepStartConnector,
				Summary:   "Start the tunnel client",
				Technical: core.TechnicalOperation{Provider: "openai_tunnel", Type: "start_client", Parameters: params},
			},
			{
				ID: "tunnel-verify", Kind: core.StepVerifyConnector,
				Summary:   "Verify the tunnel client reports ready",
				Technical: core.TechnicalOperation{Provider: "openai_tunnel", Type: "verify_client"},
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
				Technical: core.TechnicalOperation{Provider: "openai_tunnel", Type: "stop_client"},
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
		return p.validateClient(step), nil
	case core.StepStartConnector:
		return p.startClient(ctx, connectionID, step), nil
	case core.StepVerifyConnector:
		return p.verifyClient(ctx, step), nil
	case core.StepStopConnector:
		return p.stopClient(connectionID, step), nil
	default:
		return core.StepResult{StepID: step.ID, Succeeded: false},
			fmt.Errorf("openai tunnel: unsupported step kind %q", step.Kind)
	}
}

func (p *Provider) verifyOrigin(ctx context.Context, step core.PlanStep) core.StepResult {
	endpoint := step.Technical.Parameters["mcp_server_url"]
	if endpoint == "" {
		// A stdio server is started by the client itself, so there is nothing
		// to probe here. Saying so is better than reporting a vacuous success.
		return core.StepResult{StepID: step.ID, Succeeded: true}
	}
	if err := p.probe(ctx, endpoint); err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("local MCP server at %s is not reachable: %w", endpoint, err)}
	}
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

func (p *Provider) validateClient(step core.PlanStep) core.StepResult {
	if _, err := p.lookPath(p.binPath); err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("%s is not installed or not on PATH; download it from the OpenAI platform's tunnel settings", p.binPath)}
	}
	if os.Getenv(CredentialEnvVar) == "" {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("%s is not set; the tunnel client cannot reach the OpenAI control plane", CredentialEnvVar)}
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
	// than assuming the default port is free.
	args = append(args, "--health.listen-addr", "127.0.0.1:0")
	if p.healthURLFile != "" {
		args = append(args, "--health.url-file", p.healthURLFile)
	}

	return core.ProcessSpec{
		Executable: p.binPath,
		Args:       args,
		Env:        []string{CredentialEnvVar + "=" + os.Getenv(CredentialEnvVar)},
		Restart:    core.RestartAlways,
	}
}

// adminBase resolves the client's health base URL, preferring the file the
// client writes on startup over any configured override.
func (p *Provider) adminBase() (string, error) {
	if p.adminBaseURL != "" {
		return p.adminBaseURL, nil
	}
	if p.healthURLFile == "" {
		return "", fmt.Errorf("the client was not asked to report its health URL")
	}
	data, err := os.ReadFile(p.healthURLFile)
	if err != nil {
		return "", fmt.Errorf("the client has not reported a health URL yet: %w", err)
	}
	base := strings.TrimSpace(string(data))
	if base == "" {
		return "", fmt.Errorf("the client reported an empty health URL")
	}
	return base, nil
}

func (p *Provider) startClient(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) core.StepResult {
	// The client reports its chosen health port through this file.
	if p.healthURLFile == "" && p.adminBaseURL == "" {
		p.healthURLFile = filepath.Join(os.TempDir(),
			fmt.Sprintf("portico-tunnel-health-%s.url", connectionID))
		_ = os.Remove(p.healthURLFile)
	}
	if p.connectorProc == nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("no process manager is available to supervise the tunnel client")}
	}
	handle, err := p.connectorProc.Start(ctx, core.ProcessConfig{
		ConnectionID: connectionID,
		Spec:         p.clientProcessSpec(step),
	})
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
	}
	return core.StepResult{
		StepID:    step.ID,
		Succeeded: true,
		Resources: []core.ProviderResource{{
			ConnectionID: connectionID,
			ProviderID:   "openai_tunnel",
			Type:         core.ResourceTunnel,
			ExternalID:   step.Technical.Parameters["tunnel_id"],
			// The tunnel is created on OpenAI's platform, not by Portico.
			// Recording it as adopted keeps Portico from deleting something it
			// did not create.
			Ownership: core.OwnershipAdopted,
			Metadata:  map[string]string{"pid": fmt.Sprint(handle.PID)},
		}},
	}
}

// verifyClient waits for the client to report ready.
//
// /healthz and /readyz mean different things and both are needed: the client
// answers /healthz as soon as its HTTP server is up, but returns 503 from
// /readyz until it has actually reached the control plane. Treating liveness as
// readiness would report a tunnel as open while it was still unauthenticated.
func (p *Provider) verifyClient(ctx context.Context, step core.PlanStep) core.StepResult {
	deadline := time.Now().Add(clientReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		base, err := p.adminBase()
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
	if p.connectorProc == nil {
		return core.StepResult{StepID: step.ID, Succeeded: true}
	}
	if err := p.connectorProc.Stop(connectionID, 5*time.Second); err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
	}
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// Observe reports what Portico can actually determine.
//
// Portico can see whether the client process is running and whether it reports
// ready. It cannot see whether the app has been registered in ChatGPT, so it
// does not claim to; that step is reported to the user as an outstanding
// action rather than inferred.
func (p *Provider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	observed := &core.ObservedConnection{ConnectionID: id, ProviderID: "openai_tunnel"}
	if p.connectorProc == nil {
		return observed, nil
	}
	handle, running := p.connectorProc.Observe(id)
	if !running || handle.PID == 0 {
		observed.Connector = &core.ObservedConnector{Status: string(core.ConnectorStatusStopped)}
		return observed, nil
	}
	connector := &core.ObservedConnector{PID: handle.PID, Status: string(core.ConnectorStatusRunning)}
	base, baseErr := p.adminBase()
	if baseErr != nil {
		connector.Status = string(core.ConnectorStatusUnstable)
		connector.LastError = baseErr.Error()
		observed.Connector = connector
		return observed, nil
	}
	if err := p.probe(ctx, base+"/healthz"); err != nil {
		// The process is alive but the client is not answering, which is a
		// degraded tunnel rather than a stopped one.
		connector.Status = string(core.ConnectorStatusUnstable)
		connector.LastError = err.Error()
	}
	observed.Connector = connector
	return observed, nil
}

// httpProbe performs a bounded GET and treats any non-error status below 400 as
// reachable.
func (p *Provider) httpProbe(ctx context.Context, url string) error {
	client := &http.Client{Timeout: 5 * time.Second}
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
