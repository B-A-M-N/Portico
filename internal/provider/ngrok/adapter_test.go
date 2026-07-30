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
	// Every ngrok capability is experimental: the adapter synthesises tunnel
	// identifiers, targets a hardcoded port and cannot rebuild observed state
	// after a restart. Declaring temporary addresses stable contradicted both
	// the rest of this descriptor and the documentation.
	if caps.TemporaryAddresses.Stability != core.StabilityExperimental {
		t.Fatalf("TemporaryAddresses.Stability = %q, want %q", caps.TemporaryAddresses.Stability, core.StabilityExperimental)
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
	if !caps.ManagedDNS.Supported {
		t.Fatal("ManagedDNS should be supported")
	}
	// ManagedDNS is experimental because observation after restart is not implemented.
	if caps.ManagedDNS.Stability != core.StabilityExperimental {
		t.Fatalf("ManagedDNS.Stability = %q, want %q", caps.ManagedDNS.Stability, core.StabilityExperimental)
	}
	if len(caps.BuiltInProtection) != 2 {
		t.Fatalf("BuiltInProtection count = %d, want 2", len(caps.BuiltInProtection))
	}
	if caps.BuiltInProtection[0].Kind != core.ProtectionNone {
		t.Fatalf("BuiltInProtection[0].Kind = %q, want %q", caps.BuiltInProtection[0].Kind, core.ProtectionNone)
	}
	if !caps.BuiltInProtection[0].Supported {
		t.Fatal("ProtectionNone should be supported")
	}
	if caps.BuiltInProtection[1].Kind != core.ProtectionServiceToken {
		t.Fatalf("BuiltInProtection[1].Kind = %q, want %q", caps.BuiltInProtection[1].Kind, core.ProtectionServiceToken)
	}
	// ProtectionServiceToken is not yet implemented.
	if caps.BuiltInProtection[1].Supported {
		t.Fatal("ProtectionServiceToken should not be supported (not implemented)")
	}
	for _, proto := range []core.Protocol{core.ProtocolHTTP, core.ProtocolHTTPS, core.ProtocolTCP} {
		pc, ok := caps.Protocols[proto]
		if !ok || !pc.Supported || !pc.Public {
			t.Fatalf("protocol %q should be supported and public", proto)
		}
	}
	// Telemetry is not yet implemented.
	if caps.Telemetry.Supported {
		t.Fatal("Telemetry should not be supported (not implemented)")
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

	spec := p.agentProcessSpec()

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

func TestExecuteStepStartAgent(t *testing.T) {
	// executeStartAgent calls p.apiClient.List() which requires a real API client.
	// This test verifies the step dispatch works but cannot test the full flow
	// without a live ngrok API. The real integration is tested via e2e tests.
	t.Skip("executeStartAgent requires a real ngrok API client; tested via integration tests")
}

func TestExecuteStepVerifyEndpointWithPresetURL(t *testing.T) {
	p := newTestProvider(nil)
	p.mu.Lock()
	p.connections["conn-1"] = &ngrokConnection{url: "https://abc123.ngrok-free.app"}
	p.mu.Unlock()

	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID:   "verify-endpoint",
		Kind: core.StepVerifyEndpoint,
	})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatal("step should have succeeded")
	}
	if len(result.Resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(result.Resources))
	}
	if result.Resources[0].ExternalID != "https://abc123.ngrok-free.app" {
		t.Fatalf("resource ExternalID = %q, want tunnel URL", result.Resources[0].ExternalID)
	}
	if result.Resources[0].Ownership != core.OwnershipManaged {
		t.Fatalf("resource Ownership = %q, want %q", result.Resources[0].Ownership, core.OwnershipManaged)
	}
}

func TestExecuteStepVerifyEndpointNoTunnel(t *testing.T) {
	// executeVerifyEndpoint calls p.apiClient.List() when URL is empty,
	// which requires a real API client. Without a URL set and without an
	// API client, this would panic. Tested via integration tests.
	t.Skip("executeVerifyEndpoint with no URL requires a real ngrok API client; tested via integration tests")
}

func TestExecuteStepStopAgent(t *testing.T) {
	cancelled := false
	cancelFn := func() { cancelled = true }

	p := newTestProvider(nil)
	p.mu.Lock()
	p.connections["conn-1"] = &ngrokConnection{agentCancel: cancelFn}
	p.mu.Unlock()

	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID:   "stop-agent",
		Kind: core.StepStopConnector,
	})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatal("step should have succeeded")
	}
	if !cancelled {
		t.Fatal("agent cancel function should have been called")
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

func TestExecuteStepCreateProtection(t *testing.T) {
	p := newTestProvider(nil)
	p.mu.Lock()
	p.connections["conn-1"] = &ngrokConnection{}
	p.mu.Unlock()

	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID:   "create-protection",
		Kind: core.StepCreateAccessPolicy,
	})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatal("create protection step should succeed (stub)")
	}
}

func TestExecuteStepDeleteTunnel(t *testing.T) {
	p := newTestProvider(nil)
	p.mu.Lock()
	p.connections["conn-1"] = &ngrokConnection{}
	p.mu.Unlock()

	result, err := p.ExecuteStep(context.Background(), "conn-1", core.PlanStep{
		ID:   "delete-tunnel",
		Kind: core.StepDeleteTunnel,
	})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatal("delete tunnel step should succeed (stub)")
	}
}

func TestObserveActiveConnection(t *testing.T) {
	p := newTestProvider(nil)
	p.mu.Lock()
	p.connections["conn-1"] = &ngrokConnection{
		url:      "https://abc123.ngrok-free.app",
		agentPID: 99999,
	}
	p.mu.Unlock()

	obs, err := p.Observe(context.Background(), "conn-1")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.ConnectionID != "conn-1" {
		t.Fatalf("obs.ConnectionID = %q, want %q", obs.ConnectionID, "conn-1")
	}
	if obs.ProviderID != "ngrok" {
		t.Fatalf("obs.ProviderID = %q, want %q", obs.ProviderID, "ngrok")
	}
	if obs.Tunnel == nil {
		t.Fatal("obs.Tunnel should not be nil for active connection")
	}
	if obs.Tunnel.State != "active" {
		t.Fatalf("obs.Tunnel.State = %q, want %q", obs.Tunnel.State, "active")
	}
	if obs.Connector == nil {
		t.Fatal("obs.Connector should not be nil for active connection")
	}
	if obs.Connector.PID != 99999 {
		t.Fatalf("obs.Connector.PID = %d, want %d", obs.Connector.PID, 99999)
	}
	if obs.Connector.Status != "running" {
		t.Fatalf("obs.Connector.Status = %q, want %q", obs.Connector.Status, "running")
	}
}

func TestObserveConnectionWithoutURL(t *testing.T) {
	p := newTestProvider(nil)
	p.mu.Lock()
	p.connections["conn-1"] = &ngrokConnection{}
	p.mu.Unlock()

	obs, err := p.Observe(context.Background(), "conn-1")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Tunnel != nil {
		t.Fatal("obs.Tunnel should be nil when no URL is set")
	}
	if obs.Connector != nil {
		t.Fatal("obs.Connector should be nil when no URL is set")
	}
}

func TestObserveUnknownConnection(t *testing.T) {
	p := newTestProvider(nil)
	_, err := p.Observe(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("Observe for unknown connection should return error")
	}
}
