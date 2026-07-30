package ngrok

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// fakeConnectorProcessService is a test double for core.ConnectorProcessService.
type fakeConnectorProcessService struct {
	startCalled bool
	stopCalled  bool
}

func (f *fakeConnectorProcessService) Start(_ context.Context, _ core.ProcessConfig) (core.ConnectorHandle, error) {
	f.startCalled = true
	return core.ConnectorHandle{PID: 12345}, nil
}

func (f *fakeConnectorProcessService) Stop(_ core.ConnectionID, _ time.Duration) error {
	f.stopCalled = true
	return nil
}

func (f *fakeConnectorProcessService) Observe(_ core.ConnectionID) (core.ConnectorHandle, bool) {
	return core.ConnectorHandle{}, false
}

// newTestProvider creates a Provider with injected dependencies for testing.
// It bypasses the real ngrok API client by directly constructing the struct.
func newTestProvider(processMgr core.ConnectorProcessService) *Provider {
	if processMgr == nil {
		processMgr = &fakeConnectorProcessService{}
	}
	return &Provider{
		apiKey:      "test-api-key",
		binPath:     "ngrok",
		connections: make(map[core.ConnectionID]*ngrokConnection),
		processMgr:  processMgr,
	}
}

func TestProviderIdentity(t *testing.T) {
	p := newTestProvider(nil)
	ident := p.Identity()
	if ident.ID != "ngrok" {
		t.Fatalf("Identity().ID = %q, want %q", ident.ID, "ngrok")
	}
	if ident.Name != "ngrok" {
		t.Fatalf("Identity().Name = %q, want %q", ident.Name, "ngrok")
	}
	if ident.DisplayName != "Ngrok" {
		t.Fatalf("Identity().DisplayName = %q, want %q", ident.DisplayName, "Ngrok")
	}
}

func TestProviderCapabilities(t *testing.T) {
	p := newTestProvider(nil)
	caps, err := p.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.TemporaryAddresses.Supported {
		t.Fatal("TemporaryAddresses should be supported")
	}
	// The rebuilt adapter creates real tunnels through the agent, so temporary
	// addresses are no longer experimental.
	if caps.TemporaryAddresses.Stability != core.StabilityBeta {
		t.Fatalf("TemporaryAddresses.Stability = %q, want %q", caps.TemporaryAddresses.Stability, core.StabilityBeta)
	}
	if !caps.CustomHostnames.Supported {
		t.Fatal("CustomHostnames should be supported")
	}
	// CustomHostnames is experimental because lifecycle is not complete.
	if caps.CustomHostnames.Stability != core.StabilityExperimental {
		t.Fatalf("CustomHostnames.Stability = %q, want %q", caps.CustomHostnames.Stability, core.StabilityExperimental)
	}
	if caps.PrivateExposure.Supported {
		t.Fatal("PrivateExposure should not be supported")
	}
	// ngrok serves its own domains; Portico creates no DNS records for it.
	if caps.ManagedDNS.Supported {
		t.Fatal("ManagedDNS should not be supported: Portico creates no DNS records for ngrok")
	}
	if len(caps.BuiltInProtection) != 3 {
		t.Fatalf("BuiltInProtection count = %d, want 3", len(caps.BuiltInProtection))
	}
	if caps.BuiltInProtection[0].Kind != core.ProtectionNone {
		t.Fatalf("BuiltInProtection[0].Kind = %q, want %q", caps.BuiltInProtection[0].Kind, core.ProtectionNone)
	}
	if !caps.BuiltInProtection[0].Supported {
		t.Fatal("ProtectionNone should be supported")
	}
	// Protection remains unsupported: ngrok applies it through a traffic policy
	// that Portico does not generate. Declaring it supported was the false
	// claim this audit found.
	for _, prot := range caps.BuiltInProtection {
		if prot.Kind == core.ProtectionNone {
			continue
		}
		if prot.Supported {
			t.Fatalf("protection %q is declared supported but Portico applies no traffic policy", prot.Kind)
		}
	}
	for _, proto := range []core.Protocol{core.ProtocolHTTP, core.ProtocolHTTPS} {
		pc, ok := caps.Protocols[proto]
		if !ok || !pc.Supported || !pc.Public {
			t.Fatalf("protocol %q should be supported and public", proto)
		}
	}
	// The agent reports traffic counters through its local API, so telemetry is
	// now a real capability rather than an absent one.
	if !caps.Telemetry.Supported {
		t.Fatal("Telemetry should be supported: the agent reports request counts")
	}
	// Redundancy is not yet implemented.
	if caps.Redundancy.Supported {
		t.Fatal("Redundancy should not be supported (not implemented)")
	}
	// Expiration is not yet implemented.
	if caps.Expiration.Supported {
		t.Fatal("Expiration should not be supported (not implemented)")
	}
}

func TestNewDefaultsBinPath(t *testing.T) {
	p := newTestProvider(nil)
	if p.binPath != "ngrok" {
		t.Fatalf("binPath = %q, want %q", p.binPath, "ngrok")
	}
}

func TestPlanRequiresProfile(t *testing.T) {
	p := newTestProvider(nil)
	_, err := p.Plan(context.Background(), core.DesiredConnection{})
	if err == nil {
		t.Fatal("Plan with nil profile should return error")
	}
}

// newServiceExposureProfile builds a service-exposure ConnectionProfile using
// the tagged connection union. Tests must construct the union explicitly so
// that unsupported connection kinds fail at compile time rather than silently
// falling through zero-value compatibility accessors.
func newServiceExposureProfile(desired core.DesiredConnectionState) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID:   "test-conn",
		Name: "test",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: "localhost:8080"},
				},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary, Protocol: core.ProtocolHTTP},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver:  core.DriverSelection{ProviderID: "ngrok"},
		Desired: desired,
	}
}

// TestAgentProcessSpecNeverPlacesTokenInArgv enforces the release requirement
// that no secret appears in process arguments. argv is world-readable through
// /proc and is captured by process listings, diagnostics and crash reports, so
// the token may travel in the environment only.
func TestAgentProcessSpecNeverPlacesTokenInArgv(t *testing.T) {
	const token = "super-secret-ngrok-token"
	p := newTestProvider(nil)
	p.apiKey = token

	spec := p.agentProcessSpec(core.PlanStep{
		Technical: core.TechnicalOperation{Parameters: map[string]string{
			"origin_url":  "http://127.0.0.1:3000",
			"tunnel_name": "portico-test",
		}},
	}, "")

	for i, arg := range spec.Args {
		if strings.Contains(arg, token) {
			t.Fatalf("auth token leaked into argv at Args[%d] = %q", i, arg)
		}
		if arg == "--authtoken" {
			t.Fatalf("Args[%d] passes --authtoken; the token must come from the environment", i)
		}
	}

	// The token must still reach the agent, otherwise the connection cannot
	// authenticate and this test would pass for the wrong reason.
	var delivered bool
	for _, env := range spec.Env {
		if env == "NGROK_AUTHTOKEN="+token {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("auth token was not supplied to the agent through NGROK_AUTHTOKEN")
	}
}

func TestPlanTemporaryExposure(t *testing.T) {
	p := newTestProvider(nil)
	plan, err := p.Plan(context.Background(), core.DesiredConnection{
		Profile: newServiceExposureProfile(core.DesiredOpen),
		Origin:  &core.ResolvedOrigin{URL: "http://localhost:8080"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if plan.Provider != "ngrok" {
		t.Fatalf("plan.Provider = %q, want %q", plan.Provider, "ngrok")
	}
	if plan.Intent != core.IntentOpen {
		t.Fatalf("plan.Intent = %q, want %q", plan.Intent, core.IntentOpen)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("plan should have at least one step")
	}
	stepKinds := make(map[core.StepKind]bool)
	for _, step := range plan.Steps {
		stepKinds[step.Kind] = true
	}
	if !stepKinds[core.StepStartConnector] {
		t.Fatal("plan should include start_connector step")
	}
	if !stepKinds[core.StepVerifyEndpoint] {
		t.Fatal("plan should include verify_endpoint step")
	}
}

func TestPlanClose(t *testing.T) {
	p := newTestProvider(nil)
	plan, err := p.Plan(context.Background(), core.DesiredConnection{
		Profile: newServiceExposureProfile(core.DesiredClosed),
	})
	if err != nil {
		t.Fatalf("Plan close: %v", err)
	}
	if plan.Intent != core.IntentClose {
		t.Fatalf("plan.Intent = %q, want %q", plan.Intent, core.IntentClose)
	}
}

func TestExecuteStepStopAgentNilConnection(t *testing.T) {
	p := newTestProvider(nil)
	result, err := p.ExecuteStep(context.Background(), "conn-missing", core.PlanStep{
		ID:   "stop-agent",
		Kind: core.StepStopConnector,
	})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatal("step should have succeeded even with no connection")
	}
}

func TestExecuteStepUnsupportedKind(t *testing.T) {
	p := newTestProvider(nil)
	p.mu.Lock()
	p.connections["conn-1"] = &ngrokConnection{}
	p.mu.Unlock()

	_, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID:   "unknown",
		Kind: core.StepKind("unsupported_step"),
	})
	if err == nil {
		t.Fatal("unsupported step kind should return error")
	}
}
