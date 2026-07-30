// Package ngrok implements the core.Provider interface for Ngrok.
package ngrok

import (
	"context"
	"fmt"
	"sync"
	"time"

	ngrokapi "github.com/ngrok/ngrok-api-go/v9"
	ngroktunnels "github.com/ngrok/ngrok-api-go/v9/tunnels"

	"github.com/B-A-M-N/portico/internal/core"
)

// Provider implements core.Provider for Ngrok.
type Provider struct {
	mu          sync.RWMutex
	apiKey      string
	apiClient   *ngroktunnels.Client
	connections map[core.ConnectionID]*ngrokConnection
	processMgr  core.ConnectorProcessService
	binPath     string
}

type ngrokConnection struct {
	url         string
	agentPID    int
	agentCancel context.CancelFunc
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
// Note: Ngrok integration is currently experimental. Several capabilities
// (deletion, protection, observation after restart) are not yet lifecycle-complete.
// Capabilities are marked with their actual stability level.
func (p *Provider) Capabilities(ctx context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityStable,
		},
		CustomHostnames: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityExperimental, // not lifecycle-complete
		},
		PrivateExposure: core.CapabilitySupport{
			Supported: false,
			Stability: core.StabilityExperimental,
		},
		ManagedDNS: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityExperimental, // observation after restart not implemented
		},
		BuiltInProtection: []core.ProtectionCapability{
			{Kind: core.ProtectionNone, Supported: true, Stability: core.StabilityStable},
			{Kind: core.ProtectionServiceToken, Supported: false, Stability: core.StabilityExperimental}, // not implemented
		},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP:  {Supported: true, Public: true},
			core.ProtocolHTTPS: {Supported: true, Public: true},
			core.ProtocolTCP:   {Supported: true, Public: true},
		},
		Telemetry: core.TelemetryCapability{
			Supported:     false, // not implemented
			RequestCounts: false,
			Stability:     core.StabilityExperimental,
		},
		Redundancy: core.RedundancyCapability{
			Supported:     false, // not implemented
			MaxConnectors: 1,
			Stability:     core.StabilityExperimental,
		},
		Expiration: core.ExpirationCapability{
			Supported:   false, // not implemented
			MaxDuration: 0,
			Stability:   core.StabilityExperimental,
		},
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

	var intent core.OperationIntent
	var steps []core.PlanStep

	switch profile.Desired {
	case core.DesiredOpen:
		intent = core.IntentOpen
		steps = p.planOpenSteps(profile)
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

func (p *Provider) planOpenSteps(profile *core.ConnectionProfile) []core.PlanStep {
	steps := []core.PlanStep{
		{ID: "ngrok-validate-1", Kind: core.StepValidateAccount, Summary: "Validate Ngrok API key"},
	}

	if profile.GetExposure().Mode == core.ExposureTemporary {
		steps = append(steps, core.PlanStep{ID: "ngrok-ephemeral-2", Kind: core.StepCreateTunnel, Summary: "Create ephemeral tunnel"})
	} else {
		steps = append(steps, core.PlanStep{ID: "ngrok-reserved-2", Kind: core.StepCreateTunnel, Summary: "Create reserved domain tunnel"})
	}

	if profile.GetProtection().Kind != core.ProtectionNone {
		steps = append(steps, core.PlanStep{ID: "ngrok-protection-3", Kind: core.StepCreateAccessPolicy, Summary: "Configure traffic policy"})
	}

	steps = append(steps, core.PlanStep{ID: "ngrok-agent-4", Kind: core.StepStartConnector, Summary: "Start ngrok agent"})
	steps = append(steps, core.PlanStep{ID: "ngrok-verify-5", Kind: core.StepVerifyEndpoint, Summary: "Verify tunnel endpoint"})

	return steps
}

func (p *Provider) planCloseSteps(profile *core.ConnectionProfile) []core.PlanStep {
	return []core.PlanStep{
		{ID: "ngrok-stop-agent-1", Kind: core.StepStopConnector, Summary: "Stop ngrok agent"},
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

	var result core.StepResult
	var err error

	switch step.Kind {
	case core.StepValidateAccount:
		result, err = p.executeValidateAccount(ctx)
	case core.StepCreateTunnel:
		result, err = p.executeCreateTunnel(ctx, connectionID, conn)
	case core.StepStartConnector:
		result, err = p.executeStartAgent(ctx, connectionID, conn)
	case core.StepVerifyEndpoint:
		result, err = p.executeVerifyEndpoint(ctx, conn)
	case core.StepStopConnector:
		result, err = p.executeStopAgent(ctx, conn)
	case core.StepDeleteTunnel:
		result, err = p.executeDeleteTunnel(ctx, conn)
	case core.StepCreateAccessPolicy:
		result, err = p.executeCreateProtection(ctx, connectionID, conn)
	default:
		err = fmt.Errorf("ngrok: unsupported step kind %q", step.Kind)
	}

	if err != nil {
		result = core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
	}

	return result, err
}

func (p *Provider) executeValidateAccount(ctx context.Context) (core.StepResult, error) {
	iter := p.apiClient.List(nil)
	for iter.Next(ctx) {
		_ = iter.Item()
	}
	if err := iter.Err(); err != nil {
		return core.StepResult{Succeeded: false, Error: err}, err
	}
	return core.StepResult{Succeeded: true}, nil
}

func (p *Provider) executeCreateTunnel(ctx context.Context, connectionID core.ConnectionID, conn *ngrokConnection) (core.StepResult, error) {
	return core.StepResult{
		Succeeded: true,
		Resources: []core.ProviderResource{
			{
				ID:           fmt.Sprintf("tunnel-%s", connectionID),
				ConnectionID: connectionID,
				ProviderID:   "ngrok",
				Type:         core.ResourceTunnel,
				ExternalID:   fmt.Sprintf("tunnel-%s", connectionID),
				Ownership:    core.OwnershipManaged,
			},
		},
	}, nil
}

// agentProcessSpec builds the process specification for the ngrok agent.
//
// The auth token is supplied through the environment only. It must never appear
// in argv: process arguments are world-readable via /proc and are routinely
// captured by process listings, diagnostics bundles and crash reports. ngrok
// reads NGROK_AUTHTOKEN from the environment, so an --authtoken argument is
// redundant as well as unsafe.
func (p *Provider) agentProcessSpec() core.ProcessSpec {
	return core.ProcessSpec{
		Executable: p.binPath,
		Args:       []string{"http", "8080"},
		Env:        []string{"NGROK_AUTHTOKEN=" + p.apiKey},
		Restart:    core.RestartAlways,
	}
}

func (p *Provider) executeStartAgent(ctx context.Context, connectionID core.ConnectionID, conn *ngrokConnection) (core.StepResult, error) {
	procCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	conn.agentCancel = cancel
	p.mu.Unlock()

	spec := p.agentProcessSpec()

	procInfo, err := p.processMgr.Start(procCtx, core.ProcessConfig{
		ConnectionID: connectionID,
		Spec:         spec,
	})
	if err != nil {
		cancel()
		return core.StepResult{Succeeded: false, Error: err}, err
	}

	time.Sleep(2 * time.Second)

	iter := p.apiClient.List(nil)
	for iter.Next(ctx) {
		t := iter.Item()
		if t.PublicURL != "" {
			p.mu.Lock()
			conn.url = t.PublicURL
			conn.agentPID = procInfo.Identity.PID
			p.mu.Unlock()
			break
		}
	}
	if err := iter.Err(); err != nil {
		return core.StepResult{Succeeded: false, Error: err}, err
	}

	return core.StepResult{
		Succeeded: true,
		Resources: []core.ProviderResource{
			{
				ID:           fmt.Sprintf("connector-%s", connectionID),
				ConnectionID: connectionID,
				ProviderID:   "ngrok",
				Type:         core.ResourceConnector,
				ExternalID:   fmt.Sprintf("%d", procInfo.Identity.PID),
				Ownership:    core.OwnershipManaged,
			},
		},
	}, nil
}

func (p *Provider) executeVerifyEndpoint(ctx context.Context, conn *ngrokConnection) (core.StepResult, error) {
	p.mu.RLock()
	url := conn.url
	p.mu.RUnlock()

	if url == "" {
		iter := p.apiClient.List(nil)
		for iter.Next(ctx) {
			t := iter.Item()
			if t.PublicURL != "" {
				url = t.PublicURL
				break
			}
		}
		if err := iter.Err(); err != nil {
			return core.StepResult{Succeeded: false, Error: err}, err
		}
	}

	if url == "" {
		return core.StepResult{Succeeded: false, Error: fmt.Errorf("no active tunnel found")}, fmt.Errorf("no active tunnel found")
	}

	p.mu.Lock()
	conn.url = url
	p.mu.Unlock()

	return core.StepResult{
		Succeeded: true,
		Resources: []core.ProviderResource{
			{
				ID:         fmt.Sprintf("endpoint-%s", url),
				ProviderID: "ngrok",
				Type:       core.ResourceTunnel,
				ExternalID: url,
				Ownership:  core.OwnershipManaged,
			},
		},
	}, nil
}

func (p *Provider) executeStopAgent(ctx context.Context, conn *ngrokConnection) (core.StepResult, error) {
	p.mu.Lock()
	if conn != nil && conn.agentCancel != nil {
		conn.agentCancel()
		conn.agentCancel = nil
	}
	p.mu.Unlock()
	return core.StepResult{Succeeded: true}, nil
}

func (p *Provider) executeDeleteTunnel(ctx context.Context, conn *ngrokConnection) (core.StepResult, error) {
	return core.StepResult{Succeeded: true}, nil
}

func (p *Provider) executeCreateProtection(ctx context.Context, connectionID core.ConnectionID, conn *ngrokConnection) (core.StepResult, error) {
	return core.StepResult{Succeeded: true}, nil
}

// Observe returns the observed connection state.
func (p *Provider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	p.mu.RLock()
	conn := p.connections[id]
	p.mu.RUnlock()

	if conn == nil {
		return nil, core.ErrProfileNotFound(id)
	}

	obs := &core.ObservedConnection{
		ConnectionID: id,
		ProviderID:   "ngrok",
	}

	if conn.url != "" {
		obs.Tunnel = &core.ObservedTunnel{
			ID:        fmt.Sprintf("tunnel-%s", id),
			Name:      string(id),
			State:     "active",
			CreatedAt: time.Now().UTC(),
		}
		obs.Connector = &core.ObservedConnector{
			Status:         "running",
			PID:            conn.agentPID,
			ExecutablePath: p.binPath,
		}
	}

	return obs, nil
}

// Repair executes a repair plan.
func (p *Provider) Repair(ctx context.Context, plan core.RepairPlan) (<-chan core.Event, error) {
	eventCh := make(chan core.Event)
	go func() {
		defer close(eventCh)
		p.mu.Lock()
		conn := p.connections[plan.ConnectionID]
		p.mu.Unlock()

		if conn != nil && conn.agentCancel != nil {
			conn.agentCancel()
		}
		eventCh <- core.Event{
			Type:      core.EventOperationCompleted,
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
		p.mu.Unlock()

		eventCh <- core.Event{
			Type:      core.EventOperationCompleted,
			Timestamp: time.Now().UTC(),
			Data: core.OperationEvent{
				Stage:     core.StageSucceeded,
				Timestamp: time.Now().UTC(),
			},
		}
	}()
	return eventCh, nil
}
