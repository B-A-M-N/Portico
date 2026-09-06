package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/store"
)

// targetCapturingProvider records the effective origin_url each plan step
// carries, so the vertical can assert the transport was pointed at the
// gateway rather than the raw origin.
type targetCapturingProvider struct {
	mu       sync.Mutex
	targets  []string
	failOpen bool
}

func (p *targetCapturingProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "gw-target", Name: "gw-target"}
}

func (p *targetCapturingProvider) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Public: true},
		},
		Streaming: core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
	}, nil
}

func (*targetCapturingProvider) Authenticate(context.Context, core.AuthRequest) error { return nil }

func (p *targetCapturingProvider) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if desired.Origin != nil {
		p.targets = append(p.targets, desired.Origin.URL)
	}
	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    desired.Profile.ID,
		ProfileRevision: desired.Profile.Revision,
		Provider:        "gw-target",
		Intent:          core.IntentOpen,
		Steps: []core.PlanStep{{
			ID: "start", Kind: core.StepStartConnector, Summary: "Start connector",
			Technical: core.TechnicalOperation{Parameters: map[string]string{
				"origin_url": func() string {
					if desired.GatewayEndpoint != "" {
						return desired.GatewayEndpoint
					}
					if desired.Origin != nil {
						return desired.Origin.URL
					}
					return ""
				}(),
			}},
		}},
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := plan.ComputeFingerprint(); err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *targetCapturingProvider) ExecuteStep(_ context.Context, connID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch step.Kind {
	case core.StepStartConnector:
		if p.failOpen {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: errors.New("injected start failure")}, nil
		}
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	case core.StepStopConnector:
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	default:
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	}
}

func (p *targetCapturingProvider) Observe(context.Context, core.ConnectionID) (*core.ObservedConnection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	observed := &core.ObservedConnection{}
	status := core.ConnectorStatusStopped
	if !p.failOpen {
		observed.Connector = &core.ObservedConnector{PID: 4242, Status: string(core.ConnectorStatusRunning)}
		status = core.ConnectorStatusRunning
	}
	_ = status
	return observed, nil
}

// failingApplyProvider always fails its single open step.
type failingApplyProvider struct {
	mu sync.Mutex
}

func (p *failingApplyProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "gw-target", Name: "gw-target"}
}

func (p *failingApplyProvider) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Public: true},
		},
		Streaming: core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
	}, nil
}

func (*failingApplyProvider) Authenticate(context.Context, core.AuthRequest) error { return nil }

func (p *failingApplyProvider) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    desired.Profile.ID,
		ProfileRevision: desired.Profile.Revision,
		Provider:        "gw-target",
		Intent:          core.IntentOpen,
		Steps:           []core.PlanStep{{ID: "boom", Kind: core.StepStartConnector, Summary: "Fails on purpose"}},
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
	}
	if err := plan.ComputeFingerprint(); err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *failingApplyProvider) ExecuteStep(_ context.Context, _ core.ConnectionID, _ core.PlanStep) (core.StepResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return core.StepResult{Succeeded: false, Error: errors.New("injected failure")}, nil
}

func (p *failingApplyProvider) Observe(context.Context, core.ConnectionID) (*core.ObservedConnection, error) {
	return &core.ObservedConnection{}, nil
}

// testSupervisorWithController builds a real supervisor around one provider so
// verticals can drive planning and apply through production paths.
func testSupervisorWithController(t *testing.T, prov core.Provider) *Supervisor {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry := provider.NewRegistry()
	ctrl := controller.New(registry, st)
	sup := &Supervisor{
		store: st, controller: ctrl, registry: registry, mutating: true,
		gatewayMgr: newGatewayManager(),
	}
	sup.SetProviderDefinitions([]provider.Definition{}, provider.RuntimeServices{})
	sup.activation = &activationCoordinator{definitions: []provider.Definition{}, services: provider.RuntimeServices{}}
	if err := registry.Add(prov); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	return sup
}

var (
	_ = fmt.Sprintf
	_ = ipc.ConnectionDTO{}
	_ = provider.Definition(nil)
)
