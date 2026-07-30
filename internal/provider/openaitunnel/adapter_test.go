package openaitunnel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

type fakeProcess struct {
	started core.ProcessSpec
	stopped bool
	running bool
}

func (f *fakeProcess) Start(_ context.Context, cfg core.ProcessConfig) (core.ConnectorHandle, error) {
	f.started = cfg.Spec
	f.running = true
	return core.ConnectorHandle{PID: 4242}, nil
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
				TunnelID: "tunnel_0123456789abcdef",
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
	for _, want := range []string{"run", "--tunnel-id", "--mcp-server-url", "--profile"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv %q missing documented flag %q", joined, want)
		}
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
