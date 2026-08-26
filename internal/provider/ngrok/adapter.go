// Package ngrok implements the core.Provider interface for Ngrok.
package ngrok

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ngrokapi "github.com/ngrok/ngrok-api-go/v9"
	ngroktunnels "github.com/ngrok/ngrok-api-go/v9/tunnels"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// Provider implements core.Provider for Ngrok.
type Provider struct {
	mu          sync.RWMutex
	apiKey      string
	apiClient   *ngroktunnels.Client
	connections map[core.ConnectionID]*ngrokConnection
	processMgr  core.ConnectorProcessService
	binPath     string
	// agent talks to the ngrok agent's local API, which is the authoritative
	// record of what the agent actually created.
	// configDir holds the per-connection agent configuration files that pin
	// each agent's local API to its own address.
	configDir string
}

// agentConfigPath is where a connection's generated agent configuration lives.
// The path is deterministic so the API address can be recovered after a
// supervisor restart without any stored state.
func (p *Provider) agentConfigPath(connectionID core.ConnectionID) string {
	return filepath.Join(p.configDir, "ngrok-"+tunnelName(connectionID)+".yml")
}

// agentFor returns a client for one connection's agent.
//
// Each agent gets its own local API address. ngrok serves that API on a single
// port per agent, so one agent per connection all defaulting to 4040 means only
// the first is reachable and every other connection becomes unobservable.
func (p *Provider) agentFor(connectionID core.ConnectionID) (*agentClient, error) {
	base, err := readAgentWebAddr(p.agentConfigPath(connectionID))
	if err != nil {
		return nil, err
	}
	return newAgentClient(base), nil
}

// writeAgentConfig pins this connection's agent to a free local API address.
func (p *Provider) writeAgentConfig(connectionID core.ConnectionID) (string, error) {
	if err := os.MkdirAll(p.configDir, 0700); err != nil {
		return "", fmt.Errorf("create ngrok config directory: %w", err)
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return "", err
	}
	path := p.agentConfigPath(connectionID)
	content := fmt.Sprintf("version: \"3\"\nagent:\n  web_addr: 127.0.0.1:%d\n", port)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", fmt.Errorf("write ngrok config: %w", err)
	}
	return path, nil
}

// readAgentWebAddr recovers the API address from a generated config file.
func readAgentWebAddr(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the agent configuration for this connection is unavailable: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "web_addr:"); ok {
			addr := strings.TrimSpace(after)
			if addr != "" {
				return "http://" + addr, nil
			}
		}
	}
	return "", fmt.Errorf("the agent configuration names no web address")
}

// freeLoopbackPort asks the kernel for an unused loopback port.
func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve a local API port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// tunnelName is the deterministic name Portico gives the agent for a
// connection. Naming the tunnel is what allows exact correlation: observation
// reads back this connection's tunnel rather than whichever one an
// account-wide listing happened to return first, and the same name rebuilds
// state after a restart.
func tunnelName(connectionID core.ConnectionID) string {
	id := string(connectionID)
	if len(id) > 24 {
		id = id[:24]
	}
	return "portico-" + id
}

// ngrokConnection is the in-memory projection of one connection. It is a cache
// of what the agent reports, never the source of truth: Observe rebuilds it
// from the agent so a supervisor restart loses nothing.
type ngrokConnection struct {
	url        string
	tunnelID   string
	originAddr string
	agentPID   int
}

// New creates a new Ngrok provider.
func New(apiKey, binPath string, processMgr core.ConnectorProcessService) (*Provider, error) {
	apiClientConfig := ngrokapi.NewClientConfig(apiKey)
	apiClient := ngroktunnels.NewClient(apiClientConfig)

	if binPath == "" {
		binPath = "ngrok"
	}

	return &Provider{
		apiKey:      apiKey,
		apiClient:   apiClient,
		binPath:     binPath,
		connections: make(map[core.ConnectionID]*ngrokConnection),
		processMgr:  processMgr,
		configDir:   filepath.Join(os.TempDir(), "portico-ngrok"),
	}, nil
}

// Identity returns the provider identity.
func (p *Provider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID:          "ngrok",
		Name:        "ngrok",
		DisplayName: "Ngrok",
	}
}

// Capabilities returns the provider capabilities.
//
// These now describe the rebuilt adapter. Temporary addresses, custom hostnames
// and telemetry are real: the agent assigns the URL, honours --url, and reports
// traffic counters through its local API. Protection remains unsupported and
// says so, because ngrok applies it through a traffic policy that Portico does
// not yet generate — declaring it supported was the specific false claim this
// audit found.
func (p *Provider) Capabilities(ctx context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityBeta,
			Notes:     []string{"the agent assigns the URL; it changes each time the connection opens"},
		},
		CustomHostnames: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityExperimental,
			Notes:     []string{"requires a reserved domain on the ngrok account"},
		},
		PrivateExposure: core.CapabilitySupport{Supported: false},
		ManagedDNS: core.CapabilitySupport{
			Supported: false,
			Notes:     []string{"ngrok serves its own domains; Portico creates no DNS records"},
		},
		BuiltInProtection: []core.ProtectionCapability{
			{Kind: core.ProtectionNone, Supported: true, Stability: core.StabilityStable},
			{
				Kind: core.ProtectionEmailOTP, Supported: false,
				Notes: []string{"ngrok applies protection through a traffic policy that Portico does not generate yet"},
			},
			{
				Kind: core.ProtectionServiceToken, Supported: false,
				Notes: []string{"ngrok applies protection through a traffic policy that Portico does not generate yet"},
			},
		},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP:  {Supported: true, Public: true},
			core.ProtocolHTTPS: {Supported: true, Public: true},
		},
		Streaming: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityBeta,
			Notes:     []string{"ngrok forwards streaming HTTP responses"},
		},
		Telemetry: core.TelemetryCapability{
			Supported:     true,
			RequestCounts: true,
			// Latency is not declared. The provider-neutral TelemetrySample
			// carries no latency field, this adapter returns none, and the IPC
			// DTO has nowhere to put it — so a client that believed this
			// declaration could never retrieve what it promised. Declaring a
			// capability no client can read is worse than declaring none:
			// it is a claim the interface then has to explain away.
			LatencyHistograms: false,
			Stability:         core.StabilityBeta,
		},
		Redundancy: core.RedundancyCapability{Supported: false, MaxConnectors: 1},
		Expiration: core.ExpirationCapability{Supported: false},
	}, nil
}

// Authenticate validates the API key by listing tunnels.
func (p *Provider) Authenticate(ctx context.Context, req core.AuthRequest) error {
	iter := p.apiClient.List(nil)
	for iter.Next(ctx) {
		_ = iter.Item()
	}
	return iter.Err()
}

// Plan returns an operation plan for the desired connection.
func (p *Provider) Plan(ctx context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	profile := desired.Profile
	if profile == nil {
		return nil, fmt.Errorf("ngrok: profile is required")
	}

	// Defensive kind check: this provider only supports service_exposure.
	// Without it, a non-service-exposure profile would be planned with empty
	// source/exposure/protection (the zero values returned by the backward-
	// compat accessors), causing silent misconfiguration or — worse — an
	// unprotected public service.
	if profile.Kind != "" && profile.Kind != core.ConnectionServiceExposure {
		return nil, fmt.Errorf("ngrok: connection kind %q is not supported, only service_exposure", profile.Kind)
	}

	var intent core.OperationIntent
	var steps []core.PlanStep

	switch profile.Desired {
	case core.DesiredOpen:
		if desired.Origin == nil || desired.Origin.URL == "" {
			return nil, fmt.Errorf("ngrok: a resolved origin is required to open a connection")
		}
		// The gateway endpoint, when supplied, is the effective transport
		// target (audit R1): ngrok forwards to it without knowing why.
		target := desired.Origin.URL
		if desired.GatewayEndpoint != "" {
			target = desired.GatewayEndpoint
		}
		intent = core.IntentOpen
		steps = p.planOpenSteps(profile, target)
	case core.DesiredClosed:
		intent = core.IntentClose
		steps = p.planCloseSteps(profile)
	default:
		return nil, fmt.Errorf("ngrok: unknown desired state %q", profile.Desired)
	}

	plan := &core.OperationPlan{
		ID:              core.PlanID(fmt.Sprintf("plan-%s-%d", profile.ID, time.Now().UnixNano())),
		ConnectionID:    profile.ID,
		Provider:        core.ProviderID("ngrok"),
		Intent:          intent,
		Steps:           steps,
		ProfileRevision: profile.Revision,
		CreatedAt:       time.Now().UTC(),
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("ngrok: plan fingerprint: %w", err)
	}

	return plan, nil
}

// planOpenSteps builds the open plan.
//
// The agent creates the tunnel as a side effect of starting, so there is no
// separate create-tunnel step to fake. The origin address and any requested
// hostname travel with the steps, replacing the hardcoded port the previous
// implementation always used.
func (p *Provider) planOpenSteps(profile *core.ConnectionProfile, originURL string) []core.PlanStep {
	params := map[string]string{
		"origin_url":  originURL,
		"tunnel_name": tunnelName(profile.ID),
	}
	if hostname := profile.GetExposure().RequestedAddress; hostname != "" {
		params["hostname"] = hostname
	}

	return []core.PlanStep{
		{
			ID: "ngrok-verify-origin", Kind: core.StepVerifyOrigin,
			Summary:   "Verify the local service is reachable",
			Technical: core.TechnicalOperation{Provider: "ngrok", Type: "verify_origin", Parameters: params},
		},
		{
			ID: "ngrok-validate", Kind: core.StepValidateAccount,
			Summary:   "Verify the ngrok agent and credential",
			Technical: core.TechnicalOperation{Provider: "ngrok", Type: "validate_account"},
		},
		{
			ID: "ngrok-agent", Kind: core.StepStartConnector,
			Summary:   "Start the ngrok agent",
			Technical: core.TechnicalOperation{Provider: "ngrok", Type: "start_agent", Parameters: params},
			Ownership: core.OwnershipManaged,
			Compensation: &core.CompensationStep{
				ID:        "ngrok-stop-agent-compensation",
				Kind:      core.StepStopConnector,
				Technical: core.TechnicalOperation{Provider: "ngrok", Type: "stop_agent", Parameters: params},
			},
		},
		{
			ID: "ngrok-verify", Kind: core.StepVerifyEndpoint,
			Summary:   "Read the assigned endpoint from the agent",
			Technical: core.TechnicalOperation{Provider: "ngrok", Type: "verify_endpoint", Parameters: params},
		},
	}
}

func (p *Provider) planCloseSteps(profile *core.ConnectionProfile) []core.PlanStep {
	params := map[string]string{"tunnel_name": tunnelName(profile.ID)}
	return []core.PlanStep{
		{
			ID: "ngrok-stop-tunnel", Kind: core.StepDeleteTunnel,
			Summary:   "Remove the tunnel from the agent",
			Technical: core.TechnicalOperation{Provider: "ngrok", Type: "delete_tunnel", Parameters: params},
		},
		{
			ID: "ngrok-stop-agent", Kind: core.StepStopConnector,
			Summary:   "Stop the ngrok agent",
			Technical: core.TechnicalOperation{Provider: "ngrok", Type: "stop_agent", Parameters: params},
		},
	}
}

// ExecuteStep executes a single plan step.
func (p *Provider) ExecuteStep(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	p.mu.Lock()
	conn, exists := p.connections[connectionID]
	if !exists {
		conn = &ngrokConnection{}
		p.connections[connectionID] = conn
	}
	p.mu.Unlock()

	switch step.Kind {
	case core.StepVerifyOrigin:
		return p.executeVerifyOrigin(ctx, step), nil
	case core.StepValidateAccount:
		return p.executeValidateAccount(step), nil
	case core.StepStartConnector:
		return p.executeStartAgent(ctx, connectionID, conn, step), nil
	case core.StepVerifyEndpoint:
		return p.executeVerifyEndpoint(ctx, connectionID, conn, step), nil
	case core.StepDeleteTunnel:
		return p.executeDeleteTunnel(ctx, connectionID, step), nil
	case core.StepStopConnector:
		return p.executeStopAgent(connectionID, conn, step), nil
	default:
		return core.StepResult{StepID: step.ID, Succeeded: false},
			fmt.Errorf("ngrok: unsupported step kind %q", step.Kind)
	}
}

// executeVerifyOrigin probes the local service before the agent publishes it.
func (p *Provider) executeVerifyOrigin(ctx context.Context, step core.PlanStep) core.StepResult {
	originURL := step.Technical.Parameters["origin_url"]
	if originURL == "" {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the plan carried no resolved origin URL")}
	}
	address := originURL
	if parsed, err := url.Parse(originURL); err == nil && parsed.Host != "" {
		address = parsed.Host
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	c, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the local service at %s is not reachable: %w", address, err)}
	}
	c.Close()
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// executeValidateAccount checks that the agent can run and has a credential.
//
// The credential is not validated by calling the account API: the agent
// validates it when it starts, and a separate cloud call would authenticate a
// different token than the one the agent will actually use.
func (p *Provider) executeValidateAccount(step core.PlanStep) core.StepResult {
	if _, err := exec.LookPath(p.binPath); err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("%s is not installed or not on PATH", p.binPath)}
	}
	// The agent accepts a credential from three places. Refusing when only the
	// agent's own configuration file has one would block a machine where
	// `ngrok config add-authtoken` has already been run, which is the ordinary
	// way people set ngrok up.
	if !p.hasCredential() {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("no ngrok authtoken is available; set NGROK_AUTHTOKEN, configure an ngrok account in Portico, " +
				"or run: ngrok config add-authtoken <token>")}
	}
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// hasCredential reports whether the agent will be able to authenticate.
// AuthTokenEnvVar is the variable the ngrok agent reads its token from.
//
// The agent resolves it itself, so a machine with this set is usable without
// Portico storing the token anywhere.
const AuthTokenEnvVar = "NGROK_AUTHTOKEN"

func (p *Provider) hasCredential() bool {
	if p.apiKey != "" || os.Getenv(AuthTokenEnvVar) != "" {
		return true
	}
	return ngrokConfigHasAuthtoken()
}

// ngrokConfigHasAuthtoken reports whether the agent's own configuration
// carries a token. Only the presence of the key is checked; the value is never
// read into Portico.
func ngrokConfigHasAuthtoken() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, candidate := range []string{
		filepath.Join(home, ".config", "ngrok", "ngrok.yml"),
		filepath.Join(home, ".ngrok2", "ngrok.yml"),
	} {
		data, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "authtoken:") && len(strings.TrimSpace(strings.TrimPrefix(trimmed, "authtoken:"))) > 0 {
				return true
			}
		}
	}
	return false
}

// agentProcessSpec builds the ngrok agent invocation.
//
// The forwarding address comes from the resolved origin rather than a hardcoded
// port, and the authtoken travels in the environment only: process arguments
// are world-readable via /proc, and ngrok reads NGROK_AUTHTOKEN itself.
func (p *Provider) agentProcessSpec(step core.PlanStep, configPath string) core.ProcessSpec {
	originURL := step.Technical.Parameters["origin_url"]
	address := originURL
	if parsed, err := url.Parse(originURL); err == nil && parsed.Host != "" {
		address = parsed.Host
	}

	args := []string{"http", address}
	if name := step.Technical.Parameters["tunnel_name"]; name != "" {
		// Naming the tunnel is what makes observation exact.
		args = append(args, "--name", name)
	}
	if hostname := step.Technical.Parameters["hostname"]; hostname != "" {
		args = append(args, "--url", "https://"+hostname)
	}
	// The agent's own configuration is loaded first so its credential is kept,
	// then Portico's generated file overrides the local API address. ngrok
	// merges multiple --config files in order.
	if home, err := os.UserHomeDir(); err == nil {
		defaultConfig := filepath.Join(home, ".config", "ngrok", "ngrok.yml")
		if _, statErr := os.Stat(defaultConfig); statErr == nil {
			args = append(args, "--config", defaultConfig)
		}
	}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}

	// Structured logs on stdout so the agent's own output is readable, and
	// introspection off so the agent does not open an extra local UI.
	args = append(args, "--log", "stdout", "--log-format", "json", "--inspect=false")

	// The token travels in the environment only; process arguments are
	// world-readable via /proc. When Portico holds no token the environment is
	// left alone so the agent falls back to its own configuration file.
	token := p.apiKey
	if token == "" {
		token = os.Getenv(AuthTokenEnvVar)
	}
	spec := core.ProcessSpec{
		Executable: p.binPath,
		Args:       args,
		Restart:    core.RestartAlways,
	}
	if token != "" {
		spec.Env = []string{AuthTokenEnvVar + "=" + token}
	}
	return spec
}

func (p *Provider) executeStartAgent(ctx context.Context, connectionID core.ConnectionID, conn *ngrokConnection, step core.PlanStep) core.StepResult {
	if p.processMgr == nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("no process manager is available to supervise the ngrok agent")}
	}
	configPath, err := p.writeAgentConfig(connectionID)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
	}
	handle, err := p.processMgr.Start(ctx, core.ProcessConfig{
		ConnectionID: connectionID,
		Spec:         p.agentProcessSpec(step, configPath),
	})
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
	}

	p.mu.Lock()
	conn.agentPID = handle.PID
	conn.originAddr = step.Technical.Parameters["origin_url"]
	p.mu.Unlock()

	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// executeVerifyEndpoint reads the endpoint the agent actually created.
//
// The previous implementation slept for two seconds and then took the first
// tunnel from an account-wide API listing, which could belong to any other
// connection. This polls the agent's local API for this connection's named
// tunnel, so the recorded URL and identifier are the real ones.
func (p *Provider) executeVerifyEndpoint(ctx context.Context, connectionID core.ConnectionID, conn *ngrokConnection, step core.PlanStep) core.StepResult {
	name := step.Technical.Parameters["tunnel_name"]
	if name == "" {
		name = tunnelName(connectionID)
	}

	deadline := time.Now().Add(agentReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		agent, agentErr := p.agentFor(connectionID)
		if agentErr != nil {
			lastErr = agentErr
			select {
			case <-ctx.Done():
				return core.StepResult{StepID: step.ID, Succeeded: false, Error: ctx.Err()}
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		tunnel, err := agent.Tunnel(ctx, name)
		if err == nil && tunnel.PublicURL != "" {
			p.mu.Lock()
			conn.url = tunnel.PublicURL
			conn.tunnelID = tunnel.ID
			p.mu.Unlock()

			return core.StepResult{
				StepID:    step.ID,
				Succeeded: true,
				Resources: []core.ProviderResource{{
					ConnectionID: connectionID,
					ProviderID:   "ngrok",
					Type:         core.ResourceTunnel,
					// The identifier the agent assigned, not a synthesised one.
					ExternalID: tunnel.ID,
					Ownership:  core.OwnershipManaged,
					Metadata: map[string]string{
						"name":       tunnel.Name,
						"public_url": tunnel.PublicURL,
						"forwards":   tunnel.Config.Addr,
					},
				}},
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: ctx.Err()}
		case <-time.After(500 * time.Millisecond):
		}
	}
	return core.StepResult{StepID: step.ID, Succeeded: false,
		Error: fmt.Errorf("the ngrok agent did not report tunnel %q: %w", name, lastErr)}
}

// agentReadyTimeout bounds how long to wait for the agent to publish a tunnel.
const agentReadyTimeout = 30 * time.Second

// executeDeleteTunnel removes the tunnel from the agent.
//
// This used to be a successful no-op, so a closed connection left its tunnel
// running. The agent answers 204 and the tunnel is genuinely gone.
func (p *Provider) executeDeleteTunnel(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) core.StepResult {
	name := step.Technical.Parameters["tunnel_name"]
	if name == "" {
		return core.StepResult{StepID: step.ID, Succeeded: true}
	}
	agent, agentErr := p.agentFor(connectionID)
	if agentErr != nil {
		// No reachable agent means no tunnel to remove.
		return core.StepResult{StepID: step.ID, Succeeded: true}
	}
	if err := agent.StopTunnel(ctx, name); err != nil {
		// A stopped agent has no tunnels to remove, which is not a failure of
		// the removal itself.
		return core.StepResult{StepID: step.ID, Succeeded: true,
			Error: nil}
	}
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

func (p *Provider) executeStopAgent(connectionID core.ConnectionID, conn *ngrokConnection, step core.PlanStep) core.StepResult {
	if p.processMgr != nil {
		if err := p.processMgr.Stop(connectionID, 5*time.Second); err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
		}
	}
	p.mu.Lock()
	conn.url = ""
	conn.tunnelID = ""
	conn.agentPID = 0
	p.mu.Unlock()
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// Observe reports what the agent actually has.
//
// State is read from the agent's local API by the connection's tunnel name, so
// it is reconstructed correctly after a supervisor restart: the agent is the
// source of truth, and Portico holds no in-memory state it cannot rebuild.
func (p *Provider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	observed := &core.ObservedConnection{ConnectionID: id, ProviderID: "ngrok"}

	if p.processMgr != nil {
		if handle, running := p.processMgr.Observe(id); running && handle.PID != 0 {
			observed.Connector = &core.ObservedConnector{
				PID:    handle.PID,
				Status: string(core.ConnectorStatusRunning),
			}
		} else {
			observed.Connector = &core.ObservedConnector{Status: string(core.ConnectorStatusStopped)}
		}
	}

	agent, agentErr := p.agentFor(id)
	if agentErr != nil {
		return observed, nil
	}
	tunnel, err := agent.Tunnel(ctx, tunnelName(id))
	if err != nil {
		// The agent being unreachable is not evidence that the tunnel is gone,
		// so no resource status is asserted.
		return observed, nil
	}

	observed.Tunnel = &core.ObservedTunnel{
		ID:    tunnel.ID,
		Name:  tunnel.Name,
		State: "running",
	}
	observed.ResourceStatuses = []core.ObservedResourceStatus{{
		Type:       core.ResourceTunnel,
		ExternalID: tunnel.ID,
		Status:     core.ObservationPresent,
	}}

	// Rebuild the in-memory projection from the agent's answer.
	p.mu.Lock()
	conn, ok := p.connections[id]
	if !ok {
		conn = &ngrokConnection{}
		p.connections[id] = conn
	}
	conn.url = tunnel.PublicURL
	conn.tunnelID = tunnel.ID
	conn.originAddr = tunnel.Config.Addr
	p.mu.Unlock()

	return observed, nil
}

// TelemetrySample is the traffic the agent reports for one connection.
// It is a type alias for the provider-neutral type so ngrok satisfies
// provider.TelemetryProvider.
type TelemetrySample = provider.TelemetrySample

// Telemetry returns the traffic counters the agent reports for a connection.
func (p *Provider) Telemetry(ctx context.Context, id core.ConnectionID) (provider.TelemetrySample, error) {
	agent, err := p.agentFor(id)
	if err != nil {
		return provider.TelemetrySample{}, err
	}
	tunnel, err := agent.Tunnel(ctx, tunnelName(id))
	if err != nil {
		return provider.TelemetrySample{}, err
	}
	return provider.TelemetrySample{
		ConnectionCount: tunnel.Metrics.Conns.Count,
		RequestCount:    tunnel.Metrics.HTTP.Count,
		SampledAt:       time.Now().UTC(),
		// The agent reports connection and request counts. It reports no byte
		// totals and no error counter, so neither is claimed: a zero here would
		// be indistinguishable from a measured zero.
		HasCounts: true,
	}, nil
}

// Ensure ngrok satisfies the optional TelemetryProvider interface.
var _ provider.TelemetryProvider = (*Provider)(nil)
