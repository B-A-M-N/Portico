package clienttunnel

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

type fakeProcess struct {
	started core.ProcessSpec
	stopped bool
	running bool
	// startedByConnection records every start by connection ID, so tests
	// driving more than one connection can tell the specs apart.
	startedByConnection map[core.ConnectionID]core.ProcessSpec
}

type fakeGateway struct {
	upstream string
	started  bool
	stopped  bool
}

func (g *fakeGateway) StartGateway(_ context.Context, _ core.ConnectionID, upstream string, _ []string) (string, error) {
	g.upstream = upstream
	g.started = true
	return "http://127.0.0.1:49152", nil
}

func (g *fakeGateway) StopGateway(core.ConnectionID) error {
	g.stopped = true
	return nil
}

func (f *fakeProcess) Start(_ context.Context, cfg core.ProcessConfig) (core.ConnectorHandle, error) {
	f.started = cfg.Spec
	f.running = true
	f.startedByConnection[cfg.ConnectionID] = cfg.Spec
	return core.ConnectorHandle{PID: 4242}, nil
}

// startedFor returns the spec started for one connection, for tests that run
// more than one.
func (f *fakeProcess) startedFor(id core.ConnectionID) core.ProcessSpec {
	return f.startedByConnection[id]
}

// writeFile records the health URL a client would have reported.
func writeFile(t *testing.T, path, url string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(url), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
func (f *fakeProcess) Stop(core.ConnectionID, time.Duration) error { f.stopped = true; return nil }
func (f *fakeProcess) Observe(core.ConnectionID) (core.ConnectorHandle, bool) {
	if !f.running {
		return core.ConnectorHandle{}, false
	}
	return core.ConnectorHandle{PID: 4242}, true
}

func testProvider(t *testing.T) (*Provider, *fakeProcess) {
	t.Helper()
	proc := &fakeProcess{}
	p := New("tunnel-client", proc)
	p.lookPath = func(string) (string, error) { return "/usr/local/bin/tunnel-client", nil }
	p.probe = func(context.Context, string) error { return nil }
	p.SetRuntimeDir(t.TempDir())
	proc.startedByConnection = make(map[core.ConnectionID]core.ProcessSpec)
	return p, proc
}

func tunnelProfile(desired core.DesiredConnectionState) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID:   "conn-mcp",
		Name: "mcp",
		Kind: core.ConnectionClientTunnel,
		Spec: core.ConnectionSpec{
			ClientTunnel: &core.ClientTunnelSpec{
				Client:   core.ClientOpenAISecureMCPTunnel,
				MCP:      core.MCPServiceSpec{Endpoint: "http://127.0.0.1:8787/mcp"},
				TunnelID: "tunnel_0123456789abcdef0123456789abcdef",
				Profile:  "local-http",
			},
		},
		Driver:  core.DriverSelection{ProviderID: "openai_tunnel"},
		Desired: desired,
	}
}

// TestTunnelNeverPlansAPublicAddress pins the core invariant of audit item 5:
// a connection asking for private client-mediated access must never be
// satisfied by publishing the service.
func TestTunnelNeverPlansAPublicAddress(t *testing.T) {
	p, _ := testProvider(t)
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: tunnelProfile(core.DesiredOpen)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if plan.Expected.PublicAddress != "" {
		t.Fatalf("plan expects a public address: %q", plan.Expected.PublicAddress)
	}
	forbidden := map[core.StepKind]bool{
		core.StepCreateDNSRecord: true,
		core.StepUpdateDNSRecord: true,
		core.StepConfigureRoute:  true,
		core.StepCreateAccessApp: true,
	}
	for _, step := range plan.Steps {
		if forbidden[step.Kind] {
			t.Fatalf("private tunnel plan contains public-exposure step %s", step.Kind)
		}
	}
}

// TestCapabilitiesForbidPublicExposure pins the hard constraint the
// recommendation engine relies on.
func TestCapabilitiesForbidPublicExposure(t *testing.T) {
	p, _ := testProvider(t)
	caps, err := p.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.TemporaryAddresses.Supported || caps.CustomHostnames.Supported || caps.ManagedDNS.Supported {
		t.Fatal("a private tunnel declares a public-exposure capability")
	}
	if !caps.PrivateExposure.Supported {
		t.Fatal("private exposure is not declared supported")
	}
	if caps.PrivateExposure.Stability != core.StabilityExperimental {
		t.Fatalf("stability = %q; the adapter has not been run against a live tunnel", caps.PrivateExposure.Stability)
	}
	if pc := caps.Protocols[core.ProtocolHTTP]; pc.Public {
		t.Fatal("the HTTP protocol capability is marked public")
	}
}

// TestCredentialNeverEntersArgv pins the same rule applied to ngrok: process
// arguments are world-readable.
func TestCredentialNeverEntersArgv(t *testing.T) {
	const secret = "sk-control-plane-DO-NOT-LEAK"
	t.Setenv(CredentialEnvVar, secret)

	p, _ := testProvider(t)
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: tunnelProfile(core.DesiredOpen)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var start core.PlanStep
	for _, step := range plan.Steps {
		if step.Kind == core.StepStartConnector {
			start = step
		}
	}

	spec := p.clientProcessSpec(start)
	for i, arg := range spec.Args {
		if strings.Contains(arg, secret) {
			t.Fatalf("credential leaked into argv at Args[%d] = %q", i, arg)
		}
	}
	// It must still reach the client, or this test would pass for the wrong
	// reason.
	var delivered bool
	for _, env := range spec.Env {
		if env == CredentialEnvVar+"="+secret {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("credential was not supplied through %s", CredentialEnvVar)
	}
	// The documented flags must be used.
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{
		"run", "--control-plane.tunnel-id", "--control-plane.api-key",
		"env:" + CredentialEnvVar, "--mcp.server-url",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv %q missing documented flag %q", joined, want)
		}
	}
}

// TestLegacyClientProfileNeverReachesPlansOrArgv pins the P0-03 correction:
// persisted Profile remains readable for compatibility, but it is not an
// effective setting and no native profile reaches the launched client.
func TestLegacyClientProfileNeverReachesPlansOrArgv(t *testing.T) {
	p, _ := testProvider(t)
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: tunnelProfile(core.DesiredOpen)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var start core.PlanStep
	for _, step := range plan.Steps {
		if step.Kind == core.StepStartConnector {
			start = step
		}
	}
	if _, ok := start.Technical.Parameters["profile"]; ok {
		t.Fatal("a native client profile reached plan parameters")
	}
	for _, arg := range p.clientProcessSpec(start).Args {
		if arg == "profile" || strings.Contains(arg, "profile=") {
			t.Fatalf("a native client profile reached argv: %q", arg)
		}
	}
}

func TestClientUsesGatewayAsMCPOrigin(t *testing.T) {
	t.Setenv(CredentialEnvVar, "sk-control-plane")
	p, proc := testProvider(t)
	gateway := &fakeGateway{}
	p.gateways = gateway

	step := core.PlanStep{ID: "start", Kind: core.StepStartConnector, Technical: core.TechnicalOperation{
		Parameters: map[string]string{
			"tunnel_id":      "tunnel_0123456789abcdef0123456789abcdef",
			"mcp_server_url": "http://127.0.0.1:8787/mcp",
		},
	}}
	result := p.startClient(context.Background(), "conn-mcp", step)
	if !result.Succeeded {
		t.Fatalf("start result = %#v", result)
	}
	if !gateway.started || gateway.upstream != "http://127.0.0.1:8787/mcp" {
		t.Fatalf("gateway start = %#v", gateway)
	}
	joined := strings.Join(proc.started.Args, " ")
	if !strings.Contains(joined, "url=http://127.0.0.1:49152") {
		t.Fatalf("client was not pointed at the gateway: %q", joined)
	}
	if strings.Contains(joined, "url=http://127.0.0.1:8787/mcp") {
		t.Fatalf("client still points directly at the MCP origin: %q", joined)
	}

	stop := p.stopClient("conn-mcp", core.PlanStep{ID: "stop"})
	if !stop.Succeeded || !gateway.stopped {
		t.Fatalf("stop result = %#v, gateway = %#v", stop, gateway)
	}
}

// TestUnreachableMCPServerFailsBeforeAnythingStarts ensures a misconfigured
// connection fails before a process exists.
func TestUnreachableMCPServerFailsBeforeAnythingStarts(t *testing.T) {
	p, proc := testProvider(t)
	p.probe = func(_ context.Context, url string) error {
		if strings.Contains(url, "8787") {
			return errors.New("connection refused")
		}
		return nil
	}

	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: tunnelProfile(core.DesiredOpen)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// The origin check must be the first step, so failure precedes any start.
	if plan.Steps[0].Kind != core.StepVerifyOrigin {
		t.Fatalf("first step is %s, want verify_origin", plan.Steps[0].Kind)
	}

	res, err := p.ExecuteStep(context.Background(), "conn-mcp", plan.Steps[0])
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if res.Succeeded {
		t.Fatal("verification succeeded against an unreachable MCP server")
	}
	if proc.running {
		t.Fatal("the tunnel client was started despite a failed origin check")
	}
}

// TestMissingClientOrCredentialIsReportedSpecifically ensures setup gaps are
// actionable rather than generic.
func TestMissingClientOrCredentialIsReportedSpecifically(t *testing.T) {
	t.Run("client not installed", func(t *testing.T) {
		t.Setenv(CredentialEnvVar, "sk-present")
		p, _ := testProvider(t)
		p.lookPath = func(string) (string, error) { return "", errors.New("not found") }

		res, _ := p.ExecuteStep(context.Background(), "conn-mcp", core.PlanStep{
			ID: "v", Kind: core.StepValidateAccount,
		})
		if res.Succeeded {
			t.Fatal("validation passed with no client installed")
		}
		if !strings.Contains(res.Error.Error(), "tunnel-client") {
			t.Fatalf("error does not name the missing client: %v", res.Error)
		}
	})

	t.Run("credential missing", func(t *testing.T) {
		t.Setenv(CredentialEnvVar, "")
		p, _ := testProvider(t)

		res, _ := p.ExecuteStep(context.Background(), "conn-mcp", core.PlanStep{
			ID: "v", Kind: core.StepValidateAccount,
		})
		if res.Succeeded {
			t.Fatal("validation passed with no credential")
		}
		if !strings.Contains(res.Error.Error(), CredentialEnvVar) {
			t.Fatalf("error does not name the credential variable: %v", res.Error)
		}
	})
}

// TestPlanRequiresATunnelCreatedOnThePlatform ensures Portico does not imply it
// creates tunnels.
func TestPlanRequiresATunnelCreatedOnThePlatform(t *testing.T) {
	p, _ := testProvider(t)
	profile := tunnelProfile(core.DesiredOpen)
	profile.Spec.ClientTunnel.TunnelID = ""

	_, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err == nil {
		t.Fatal("planning succeeded with no tunnel ID")
	}
	if !strings.Contains(err.Error(), "OpenAI platform") {
		t.Fatalf("error does not say where the tunnel is created: %v", err)
	}
}

// TestTunnelResourceIsAdoptedNotManaged ensures Portico never deletes a tunnel
// it did not create.
func TestTunnelResourceIsAdoptedNotManaged(t *testing.T) {
	t.Setenv(CredentialEnvVar, "sk-present")
	p, _ := testProvider(t)
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: tunnelProfile(core.DesiredOpen)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var start core.PlanStep
	for _, step := range plan.Steps {
		if step.Kind == core.StepStartConnector {
			start = step
		}
	}
	res, err := p.ExecuteStep(context.Background(), "conn-mcp", start)
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if len(res.Resources) != 1 {
		t.Fatalf("resources = %#v", res.Resources)
	}
	if res.Resources[0].Ownership != core.OwnershipAdopted {
		t.Fatalf("ownership = %q; Portico must not claim to own a tunnel it did not create",
			res.Resources[0].Ownership)
	}
}

// TestProfileValidation covers the union arm's rules.
func TestProfileValidation(t *testing.T) {
	valid := tunnelProfile(core.DesiredOpen)
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid client tunnel profile was rejected: %v", err)
	}

	both := tunnelProfile(core.DesiredOpen)
	both.Spec.ClientTunnel.MCP.Command = &core.CommandSpec{Executable: "python"}
	if err := both.Validate(); err == nil {
		t.Fatal("a profile with both an endpoint and a command was accepted")
	}

	neither := tunnelProfile(core.DesiredOpen)
	neither.Spec.ClientTunnel.MCP = core.MCPServiceSpec{}
	if err := neither.Validate(); err == nil {
		t.Fatal("a profile with neither an endpoint nor a command was accepted")
	}

	mismatched := tunnelProfile(core.DesiredOpen)
	mismatched.Kind = core.ConnectionServiceExposure
	if err := mismatched.Validate(); err == nil {
		t.Fatal("a profile whose kind disagrees with its arm was accepted")
	}
}
