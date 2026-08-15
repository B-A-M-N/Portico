package tailscale

import (
	"context"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// fakeProcessManager is a minimal ConnectorProcessService for tests.
type fakeProcessManager struct{}

func (f *fakeProcessManager) Start(ctx context.Context, cfg core.ProcessConfig) (core.ConnectorHandle, error) {
	return core.ConnectorHandle{}, nil
}

func (f *fakeProcessManager) Stop(connID core.ConnectionID, timeout time.Duration) error {
	return nil
}

func (f *fakeProcessManager) Observe(connID core.ConnectionID) (core.ConnectorHandle, bool) {
	return core.ConnectorHandle{}, false
}

func newTestProvider() *Provider {
	return New("tailscale", &fakeProcessManager{})
}

func TestTailscaleIdentity(t *testing.T) {
	p := newTestProvider()
	ident := p.Identity()
	if ident.ID != "tailscale" {
		t.Fatalf("ID = %q, want tailscale", ident.ID)
	}
}

func TestTailscaleCapabilities(t *testing.T) {
	p := newTestProvider()
	caps, err := p.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	found := false
	for _, k := range caps.Kinds {
		if k == core.ConnectionPrivateNetwork {
			found = true
		}
	}
	if !found {
		t.Fatalf("Capabilities should support ConnectionPrivateNetwork, got %v", caps.Kinds)
	}
	if caps.TemporaryAddresses.Supported {
		t.Fatal("TemporaryAddresses should not be supported")
	}
	if !caps.PrivateExposure.Supported {
		t.Fatal("PrivateExposure should be supported")
	}
}

func TestTailscalePlanRequiresProfile(t *testing.T) {
	p := newTestProvider()
	_, err := p.Plan(context.Background(), core.DesiredConnection{})
	if err == nil {
		t.Fatal("Plan should require a profile")
	}
}

func TestTailscalePlanWrongKind(t *testing.T) {
	p := newTestProvider()
	_, err := p.Plan(context.Background(), core.DesiredConnection{
		Profile: &core.ConnectionProfile{Kind: core.ConnectionServiceExposure},
	})
	if err == nil {
		t.Fatal("Plan should reject wrong kind")
	}
}

func TestTailscalePlanOpen(t *testing.T) {
	p := newTestProvider()
	profile := &core.ConnectionProfile{
		ID:      "conn-test",
		Kind:    core.ConnectionPrivateNetwork,
		Desired: core.DesiredOpen,
		Spec:    core.ConnectionSpec{PrivateNetwork: &core.PrivateNetworkSpec{NetworkID: "test-net", Mode: core.PrivateNetworkJoin}},
		Driver:  core.DriverSelection{ProviderID: "tailscale"},
	}
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Intent != core.IntentOpen {
		t.Fatalf("Intent = %q, want open", plan.Intent)
	}
	if len(plan.Steps) != 3 {
		t.Fatalf("Steps = %d, want 3", len(plan.Steps))
	}
	if plan.Steps[0].Kind != core.StepValidateAccount {
		t.Fatalf("Step 0 kind = %q, want validate_account", plan.Steps[0].Kind)
	}
}

func TestTailscalePlanClose(t *testing.T) {
	p := newTestProvider()
	profile := &core.ConnectionProfile{
		ID:      "conn-test",
		Kind:    core.ConnectionPrivateNetwork,
		Desired: core.DesiredClosed,
		Spec:    core.ConnectionSpec{PrivateNetwork: &core.PrivateNetworkSpec{NetworkID: "test-net", Mode: core.PrivateNetworkJoin}},
		Driver:  core.DriverSelection{ProviderID: "tailscale"},
	}
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Intent != core.IntentClose {
		t.Fatalf("Intent = %q, want close", plan.Intent)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(plan.Steps))
	}
}

func TestTailscaleExecuteVerifyConnection(t *testing.T) {
	p := newTestProvider()
	result := p.executeVerifyConnection()
	// SCAFFOLD: verify_connection is not implemented and returns failure.
	if result.Succeeded {
		t.Fatal("executeVerifyConnection should fail for scaffold")
	}
}

func TestTailscaleExecuteExpose(t *testing.T) {
	p := newTestProvider()
	step := core.PlanStep{ID: "ts-expose"}
	result := p.executeExpose("conn-test", step)
	// SCAFFOLD: expose is not implemented and returns failure.
	if result.Succeeded {
		t.Fatal("executeExpose should fail for scaffold")
	}
}

func TestTailscaleObserve(t *testing.T) {
	p := newTestProvider()
	observed, err := p.Observe(context.Background(), "conn-test")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observed.ProviderID != "tailscale" {
		t.Fatalf("ProviderID = %q, want tailscale", observed.ProviderID)
	}
	if observed.Connector.Status != string(core.ConnectorStatusStopped) {
		t.Fatalf("Connector.Status = %q, want stopped", observed.Connector.Status)
	}
}

func TestDefinitionActivate(t *testing.T) {
	d := NewDefinition(DefinitionConfig{Bin: "tailscale"})
	inst, err := d.Activate(context.Background(), provider.ActivationRequest{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// SCAFFOLD: Activate refuses to build an adapter for an unimplemented provider.
	if inst.Provider != nil {
		t.Fatal("Activate should return nil provider for scaffold")
	}
	if inst.Catalog.Availability != provider.AvailabilityNotImplemented {
		t.Fatalf("Availability = %q, want not_implemented", inst.Catalog.Availability)
	}
}

func TestDefinitionCatalogEntry(t *testing.T) {
	d := NewDefinition(DefinitionConfig{Bin: "tailscale"})
	entry := d.CatalogEntry()
	if entry.ID != "tailscale" {
		t.Fatalf("ID = %q, want tailscale", entry.ID)
	}
	if entry.Availability != provider.AvailabilityNotImplemented {
		t.Fatalf("Availability = %q, want not_implemented", entry.Availability)
	}
}

func TestDefinitionRequiredBinary(t *testing.T) {
	d := NewDefinition(DefinitionConfig{Bin: "tailscale"})
	if d.RequiredBinary() != "tailscale" {
		t.Fatalf("RequiredBinary = %q, want tailscale", d.RequiredBinary())
	}
}

func TestTailscaleImplementsProvider(t *testing.T) {
	var _ core.Provider = (*Provider)(nil)
}

func TestDefinitionImplementsProviderDefinition(t *testing.T) {
	var _ provider.Definition = (*Definition)(nil)
}
