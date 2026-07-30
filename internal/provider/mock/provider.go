package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// Provider is a mock provider for testing the controller vertical slice.
// It simulates a real provider without external dependencies.
type Provider struct {
	mu          sync.RWMutex
	connections map[core.ConnectionID]*mockConnection
	nextSeq     int64
}

type mockConnection struct {
	runtime *core.ConnectionRuntime
}

// New creates a new mock provider.
func New() *Provider {
	return &Provider{
		connections: make(map[core.ConnectionID]*mockConnection),
	}
}

// Identity returns the provider identity.
func (p *Provider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID:          "mock",
		Name:        "mock",
		DisplayName: "Mock Provider",
	}
}

// Capabilities returns the provider capabilities.
func (p *Provider) Capabilities(ctx context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityStable,
		},
		CustomHostnames: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityStable,
		},
		PrivateExposure: core.CapabilitySupport{
			Supported: false,
			Stability: core.StabilityExperimental,
		},
		ManagedDNS: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityStable,
		},
		BuiltInProtection: []core.ProtectionCapability{
			{Kind: core.ProtectionNone, Supported: true, Stability: core.StabilityStable},
			{Kind: core.ProtectionEmailOTP, Supported: true, Stability: core.StabilityStable},
		},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP:  {Supported: true, Public: true},
			core.ProtocolHTTPS: {Supported: true, Public: true},
		},
		Telemetry: core.TelemetryCapability{
			Supported:     false,
			RequestCounts: false,
			Stability:     core.StabilityExperimental,
		},
		Redundancy: core.RedundancyCapability{
			Supported:     true,
			MaxConnectors: 5,
			Stability:     core.StabilityBeta,
		},
		Expiration: core.ExpirationCapability{
			Supported:   true,
			MaxDuration: 24 * time.Hour,
			Stability:   core.StabilityStable,
		},
	}, nil
}

// Authenticate does nothing for the mock provider.
func (p *Provider) Authenticate(ctx context.Context, req core.AuthRequest) error {
	return nil
}

// Plan returns an immutable operation plan for the desired connection.
func (p *Provider) Plan(ctx context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	if desired.Profile == nil {
		return nil, fmt.Errorf("mock: profile is required")
	}

	profile := desired.Profile

	var intent core.OperationIntent
	var steps []core.PlanStep

	switch profile.Desired {
	case core.DesiredOpen:
		intent = core.IntentOpen
		steps = []core.PlanStep{
			{
				ID:      "mock-validate-1",
				Kind:    core.StepValidateAccount,
				Summary: "Validate account",
			},
			{
				ID:      "mock-tunnel-2",
				Kind:    core.StepCreateTunnel,
				Summary: "Create mock tunnel",
			},
			{
				ID:      "mock-connector-3",
				Kind:    core.StepStartConnector,
				Summary: "Start mock connector",
			},
			{
				ID:      "mock-verify-4",
				Kind:    core.StepVerifyEndpoint,
				Summary: "Verify endpoint",
			},
		}
	case core.DesiredClosed:
		intent = core.IntentClose
		steps = []core.PlanStep{
			{
				ID:      "mock-stop-1",
				Kind:    core.StepStopConnector,
				Summary: "Stop mock connector",
			},
		}
	default:
		return nil, fmt.Errorf("mock: unsupported desired state: %s", profile.Desired)
	}

	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    profile.ID,
		ProfileRevision: profile.Revision,
		Provider:        profile.GetProvider().ProviderID,
		Intent:          intent,
		Steps:           steps,
		Preconditions:   []core.Precondition{},
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("mock fingerprint: %w", err)
	}

	return plan, nil
}

// Apply executes the plan steps and streams events.
func (p *Provider) Apply(ctx context.Context, plan core.OperationPlan) (<-chan core.Event, error) {
	connID := plan.ConnectionID

	// If this is an open plan, initialize connection state.
	p.mu.Lock()
	if _, exists := p.connections[connID]; !exists {
		p.connections[connID] = &mockConnection{
			runtime: &core.ConnectionRuntime{
				ConnectionID: connID,
			},
		}
		// On open, set up runtime with a public address
		if plan.Intent == core.IntentOpen {
			p.connections[connID].runtime.Endpoint.PublicAddress = fmt.Sprintf("https://mock-%s.example.com", connID)
			p.connections[connID].runtime.Connector.PID = int(len(connID))
			p.connections[connID].runtime.Connector.Status = core.ConnectorStatusRunning
			p.connections[connID].runtime.State = core.RuntimeOpen
		}
	}
	p.mu.Unlock()

	eventCh := make(chan core.Event)
	started := time.Now().UTC()

	go func() {
		defer close(eventCh)

		for i, step := range plan.Steps {
			stepTime := started.Add(time.Duration(i+1) * 50 * time.Millisecond)

			// Step started.
			p.mu.Lock()
			seq := p.nextSeq
			p.nextSeq++
			p.mu.Unlock()

			eventCh <- core.Event{
				Type:      core.EventOperationStepStarted,
				Sequence:  seq,
				Timestamp: stepTime,
				Data: core.OperationEvent{
					StepID:    step.ID,
					StepKind:  step.Kind,
					Stage:     core.StageStarted,
					Message:   step.Summary,
					Timestamp: stepTime,
				},
			}

			// Simulate work.
			select {
			case <-ctx.Done():
				p.mu.Lock()
				seq = p.nextSeq
				p.nextSeq++
				p.mu.Unlock()
				eventCh <- core.Event{
					Type:      core.EventOperationFailed,
					Sequence:  seq,
					Timestamp: time.Now().UTC(),
					Data: core.OperationEvent{
						Stage:     core.StageFailed,
						StepID:    step.ID,
						Error:     "context cancelled",
						Timestamp: time.Now().UTC(),
					},
				}
				return
			case <-time.After(10 * time.Millisecond):
			}

			// Step succeeded.
			p.mu.Lock()
			seq = p.nextSeq
			p.nextSeq++
			p.mu.Unlock()

			eventCh <- core.Event{
				Type:      core.EventOperationStepSucceeded,
				Sequence:  seq,
				Timestamp: stepTime.Add(5 * time.Millisecond),
				Data: core.OperationEvent{
					StepID:    step.ID,
					StepKind:  step.Kind,
					Stage:     core.StageSucceeded,
					Timestamp: stepTime.Add(5 * time.Millisecond),
				},
			}
		}

		// Operation completed.
		p.mu.Lock()
		completedSeq := p.nextSeq
		p.nextSeq++
		// Update stored runtime
		if plan.Intent == core.IntentClose {
			if conn, ok := p.connections[connID]; ok {
				conn.runtime.Connector.Status = core.ConnectorStatusStopped
				conn.runtime.State = core.RuntimeClosed
				conn.runtime.Endpoint.PublicAddress = ""
			}
		}
		p.mu.Unlock()

		eventCh <- core.Event{
			Type:      core.EventOperationCompleted,
			Sequence:  completedSeq,
			Timestamp: time.Now().UTC(),
			Data: core.OperationEvent{
				Stage:     core.StageSucceeded,
				Timestamp: time.Now().UTC(),
			},
		}
	}()

	return eventCh, nil
}

// Observe returns the observed state of a connection.
func (p *Provider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	conn, ok := p.connections[id]
	if !ok {
		return &core.ObservedConnection{
			ConnectionID: id,
			ProviderID:   "mock",
			Connector:    &core.ObservedConnector{Status: "stopped"},
		}, nil
	}

	status := "stopped"
	pid := 0
	if conn.runtime != nil {
		switch conn.runtime.Connector.Status {
		case core.ConnectorStatusRunning:
			status = "running"
		case core.ConnectorStatusCrashed:
			status = "crashed"
		case core.ConnectorStatusStopped:
			status = "stopped"
		case core.ConnectorStatusStarting:
			status = "starting"
		}
		pid = conn.runtime.Connector.PID
	}

	return &core.ObservedConnection{
		ConnectionID: id,
		ProviderID:   "mock",
		Connector: &core.ObservedConnector{
			PID:    pid,
			Status: status,
		},
	}, nil
}

// Repair executes a repair plan.
func (p *Provider) Repair(ctx context.Context, plan core.RepairPlan) (<-chan core.Event, error) {
	eventCh := make(chan core.Event)
	go func() {
		defer close(eventCh)

		p.mu.Lock()
		if conn, ok := p.connections[plan.ConnectionID]; ok {
			conn.runtime.Connector.Status = core.ConnectorStatusRunning
			conn.runtime.State = core.RuntimeOpen
		}
		seq := p.nextSeq
		p.nextSeq++
		p.mu.Unlock()

		time.Sleep(10 * time.Millisecond)

		eventCh <- core.Event{
			Type:      core.EventOperationCompleted,
			Sequence:  seq + 1,
			Timestamp: time.Now().UTC(),
			Data: core.OperationEvent{
				Stage:     core.StageSucceeded,
				Timestamp: time.Now().UTC(),
			},
		}
	}()
	return eventCh, nil
}

// Remove executes a removal plan.
func (p *Provider) Remove(ctx context.Context, plan core.RemovePlan) (<-chan core.Event, error) {
	eventCh := make(chan core.Event)
	go func() {
		defer close(eventCh)

		p.mu.Lock()
		delete(p.connections, plan.ConnectionID)
		seq := p.nextSeq
		p.nextSeq++
		p.mu.Unlock()

		eventCh <- core.Event{
			Type:      core.EventOperationCompleted,
			Sequence:  seq + 1,
			Timestamp: time.Now().UTC(),
			Data: core.OperationEvent{
				Stage:     core.StageSucceeded,
				Timestamp: time.Now().UTC(),
			},
		}
	}()
	return eventCh, nil
}

// InjectConnectorFailure simulates a connector crash for testing diagnostics.
func (p *Provider) InjectConnectorFailure(id core.ConnectionID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if conn, ok := p.connections[id]; ok {
		conn.runtime.Connector.Status = core.ConnectorStatusCrashed
		conn.runtime.Connector.PID = 0
		conn.runtime.State = core.RuntimeDegraded
	}
}

// GetConnectorStatus returns the mock connector status.
func (p *Provider) GetConnectorStatus(id core.ConnectionID) core.ConnectorStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if conn, ok := p.connections[id]; ok && conn.runtime != nil {
		return conn.runtime.Connector.Status
	}
	return core.ConnectorStatusStopped
}

// ExecuteStep executes a single plan step and returns the result.
func (p *Provider) ExecuteStep(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	p.mu.Lock()
	if _, exists := p.connections[connectionID]; !exists {
		p.connections[connectionID] = &mockConnection{
			runtime: &core.ConnectionRuntime{
				ConnectionID: connectionID,
			},
		}
	}
	p.mu.Unlock()

	// Simulate step execution.
	select {
	case <-ctx.Done():
		return core.StepResult{}, ctx.Err()
	case <-time.After(10 * time.Millisecond):
	}

	p.mu.Lock()
	switch step.Kind {
	case core.StepStopConnector:
		if conn, ok := p.connections[connectionID]; ok {
			conn.runtime.Connector.Status = core.ConnectorStatusStopped
			conn.runtime.State = core.RuntimeClosed
			conn.runtime.Endpoint.PublicAddress = ""
		}
	case core.StepStartConnector, core.StepRestartConnector:
		if conn, ok := p.connections[connectionID]; ok {
			conn.runtime.Connector.Status = core.ConnectorStatusRunning
			conn.runtime.State = core.RuntimeOpen
			conn.runtime.Connector.PID = int(len(connectionID))
			conn.runtime.Endpoint.PublicAddress = fmt.Sprintf("https://mock-%s.example.com", connectionID)
		}
	}
	p.mu.Unlock()

	return core.StepResult{
		StepID:    step.ID,
		Succeeded: true,
	}, nil
}
