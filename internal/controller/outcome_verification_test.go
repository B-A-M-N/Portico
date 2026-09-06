package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider/mock"
)

// Audit P0-10 failure-injection verticals: a provider mutation that returns
// success while subsequent exact-ID observation reports missing/drifted/
// unclassifiable resources must NOT be committed as terminal success.

// lyingProvider executes every step "successfully" but its observation tells
// the truth configured by the test.
type lyingProvider struct {
	mock        *streamingMock
	statuses    []core.ObservedResourceStatus
	noAccessApp bool
}

func (p *lyingProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "mock", Name: "mock"}
}

func (p *lyingProvider) Capabilities(ctx context.Context) (core.Capabilities, error) {
	return p.mock.Capabilities(ctx)
}

func (p *lyingProvider) ExecuteStep(
	ctx context.Context, connID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	return p.mock.ExecuteStep(ctx, connID, step)
}

func (p *lyingProvider) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	return p.mock.Plan(context.Background(), desired)
}

func (p *lyingProvider) Authenticate(context.Context, core.AuthRequest) error { return nil }

func (p *lyingProvider) Observe(context.Context, core.ConnectionID) (*core.ObservedConnection, error) {
	obs := &core.ObservedConnection{
		Connector:        &core.ObservedConnector{PID: 4242, Status: string(core.ConnectorStatusRunning)},
		ResourceStatuses: p.statuses,
	}
	if !p.noAccessApp {
		obs.AccessApps = []core.ObservedAccessApp{{ID: "app-1"}}
	}
	return obs, nil
}

func outcomeLyingController(t *testing.T, statuses []core.ObservedResourceStatus, noAccess bool) (*Controller, *core.ConnectionProfile) {
	t.Helper()
	current := editProfile("demo", "demo.example.com", core.ProtectionSpec{Kind: core.ProtectionNone})
	c := New(newTestRegistry(&lyingProvider{
		mock:        &streamingMock{Provider: mock.New()},
		statuses:    statuses,
		noAccessApp: noAccess,
	}), newTestJournal())
	c.RestoreProfile(current)
	rt := &core.ConnectionRuntime{ConnectionID: current.ID, State: core.RuntimeClosed}
	c.RestoreRuntime(rt)
	return c, current
}

func TestOutcomeVerificationRefusedWhenResourceMissing(t *testing.T) {
	c, profile := outcomeLyingController(t, []core.ObservedResourceStatus{{
		Type: core.ResourceTunnel, ExternalID: "tun-1", Status: core.ObservationMissing,
	}}, false)

	err := c.verifyOperationOutcome(context.Background(), core.IntentOpen, profile.ID)
	if err == nil {
		t.Fatal("terminal success was granted although observation reports the tunnel missing")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error does not name the defect: %v", err)
	}
}

func TestOutcomeVerificationRefusedWhenResourceDrifted(t *testing.T) {
	c, profile := outcomeLyingController(t, []core.ObservedResourceStatus{{
		Type: core.ResourceDNSRecord, ExternalID: "dns-1", Status: core.ObservationDrifted,
		Detail: "CNAME points elsewhere",
	}}, false)

	err := c.verifyOperationOutcome(context.Background(), core.IntentOpen, profile.ID)
	if err == nil {
		t.Fatal("terminal success was granted although observation reports drift")
	}
}

func TestOutcomeVerificationRefusedWhenLookupUnclassifiable(t *testing.T) {
	c, profile := outcomeLyingController(t, []core.ObservedResourceStatus{{
		Type: core.ResourceAccessApp, ExternalID: "app-1", Status: core.ObservationTransient,
	}}, false)

	err := c.verifyOperationOutcome(context.Background(), core.IntentOpen, profile.ID)
	if err == nil {
		t.Fatal("terminal success was granted although a resource lookup could not be classified")
	}
}

func TestOutcomeVerificationAcceptsPresentResources(t *testing.T) {
	c, profile := outcomeLyingController(t, []core.ObservedResourceStatus{
		{Type: core.ResourceTunnel, ExternalID: "tun-1", Status: core.ObservationPresent},
		{Type: core.ResourceDNSRecord, ExternalID: "dns-1", Status: core.ObservationPresent},
	}, false)

	if err := c.verifyOperationOutcome(context.Background(), core.IntentOpen, profile.ID); err != nil {
		t.Fatalf("a fully present inventory was refused: %v", err)
	}
}

var _ = errors.New
