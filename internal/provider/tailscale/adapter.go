// Package tailscale implements the core.Provider interface for Tailscale.
//
// Tailscale creates a private network overlay between devices. Portico uses
// it to join a tailnet or expose a local service to a tailnet without a
// public address.
//
// This package is currently a SCAFFOLD. It is not wired into the provider
// composition root (see builtin/catalog.go, which keeps Tailscale as
// "not_implemented"). Do not register this adapter until every method below
// is implemented against the real tailscale CLI/LocalAPI.
package tailscale

import (
	"context"
	"fmt"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// Provider implements the core.Provider interface for Tailscale.
type Provider struct {
	bin        string
	processMgr core.ConnectorProcessService
}

// New creates a new Tailscale provider.
func New(bin string, processMgr core.ConnectorProcessService) *Provider {
	return &Provider{
		bin:        bin,
		processMgr: processMgr,
	}
}

// Identity returns the provider identity.
func (p *Provider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID:          "tailscale",
		Name:        "tailscale",
		DisplayName: "Tailscale",
	}
}

// Capabilities declares a private-network provider.
func (p *Provider) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		Kinds: []core.ConnectionKind{core.ConnectionPrivateNetwork},
		TemporaryAddresses: core.CapabilitySupport{
			Supported: false,
			Notes:     []string{"Tailscale creates no public address"},
		},
		CustomHostnames: core.CapabilitySupport{Supported: false},
		ManagedDNS:      core.CapabilitySupport{Supported: false},
		PrivateExposure: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityExperimental,
		},
		BuiltInProtection: []core.ProtectionCapability{
			{Kind: core.ProtectionNone, Supported: true, Stability: core.StabilityStable},
		},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolTCP: {Supported: true, Private: true},
		},
		Telemetry: core.TelemetryCapability{Supported: false},
	}, nil
}

// Authenticate is a no-op.
func (p *Provider) Authenticate(context.Context, core.AuthRequest) error { return nil }

// Plan produces the operation plan for a private network connection.
func (p *Provider) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	if desired.Profile == nil {
		return nil, fmt.Errorf("tailscale: profile required")
	}

	profile := desired.Profile
	if profile.Kind != core.ConnectionPrivateNetwork {
		return nil, fmt.Errorf("tailscale: connection kind %q is not supported", profile.Kind)
	}

	spec := profile.Spec.PrivateNetwork
	if spec == nil {
		return nil, fmt.Errorf("tailscale: private network spec is required")
	}

	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    profile.ID,
		ProfileRevision: profile.Revision,
		Provider:        "tailscale",
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}

	switch profile.Desired {
	case core.DesiredOpen:
		plan.Intent = core.IntentOpen
		plan.Steps = []core.PlanStep{
			{
				ID:      "ts-verify",
				Kind:    core.StepValidateAccount,
				Summary: "Verify tailscale is connected to a tailnet",
				Technical: core.TechnicalOperation{
					Provider: "tailscale",
					Type:     "verify_connection",
				},
			},
			{
				ID:      "ts-expose",
				Kind:    core.StepStartConnector,
				Summary: fmt.Sprintf("Expose local service to tailnet (mode: %s)", spec.Mode),
				Technical: core.TechnicalOperation{
					Provider: "tailscale",
					Type:     "expose_service",
					Parameters: map[string]string{
						"network_id":   spec.NetworkID,
						"mode":         string(spec.Mode),
						"expose_local": fmt.Sprintf("%t", spec.ExposeLocal),
					},
				},
			},
			{
				ID:      "ts-verify-endpoint",
				Kind:    core.StepVerifyEndpoint,
				Summary: "Verify service is reachable on the tailnet",
			},
		}
		plan.Expected.State = core.RuntimeOpen
		plan.Expected.PrivateAddress = "tailnet IP"

	case core.DesiredClosed:
		plan.Intent = core.IntentClose
		plan.Steps = []core.PlanStep{
			{
				ID:      "ts-unexpose",
				Kind:    core.StepStopConnector,
				Summary: "Remove service from tailnet",
				Technical: core.TechnicalOperation{
					Provider: "tailscale",
					Type:     "unexpose_service",
				},
			},
		}
		plan.Expected.State = core.RuntimeClosed

	default:
		return nil, fmt.Errorf("tailscale: unsupported desired state %q", profile.Desired)
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("tailscale: plan fingerprint: %w", err)
	}
	return plan, nil
}

// ExecuteStep executes a single plan step.
//
// SCAFFOLD: All methods below must be implemented against the real tailscale
// CLI/LocalAPI before this provider can be registered. The current
// implementations are intentionally conservative — they return errors rather
// than silently succeeding.
func (p *Provider) ExecuteStep(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	switch step.Kind {
	case core.StepValidateAccount:
		return p.executeVerifyConnection(), nil
	case core.StepStartConnector:
		return p.executeExpose(connectionID, step), nil
	case core.StepVerifyEndpoint:
		return p.executeVerifyEndpoint(connectionID), nil
	case core.StepStopConnector:
		return p.executeUnexpose(connectionID), nil
	default:
		return core.StepResult{StepID: step.ID, Succeeded: false},
			fmt.Errorf("tailscale: unsupported step kind %q", step.Kind)
	}
}

// Observe returns the observed state of a connection.
func (p *Provider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	observed := &core.ObservedConnection{
		ConnectionID: id,
		ProviderID:   "tailscale",
	}

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

	return observed, nil
}

func (p *Provider) executeVerifyConnection() core.StepResult {
	// SCAFFOLD: implement actual tailscale status check via:
	//   - tailscale status --json
	//   - or LocalAPI /api/v2/tailnet/{tailnet}/devices
	// Must distinguish: daemon running, authenticated, tailnet joined.
	return core.StepResult{Succeeded: false, Error: fmt.Errorf("tailscale: verify_connection not implemented")}
}

func (p *Provider) executeExpose(connectionID core.ConnectionID, step core.PlanStep) core.StepResult {
	// SCAFFOLD: implement actual tailscale expose via:
	//   - tailscale serve (for HTTP/S)
	//   - tailscale funnel (for public exposure)
	//   - tailscale up --advertise-routes (for subnet routing)
	// Must handle: mode=join vs mode=expose, network_id validation.
	return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("tailscale: expose_service not implemented")}
}

func (p *Provider) executeVerifyEndpoint(connectionID core.ConnectionID) core.StepResult {
	// SCAFFOLD: verify the service is actually reachable on the tailnet.
	return core.StepResult{Succeeded: false, Error: fmt.Errorf("tailscale: verify_endpoint not implemented")}
}

func (p *Provider) executeUnexpose(connectionID core.ConnectionID) core.StepResult {
	// SCAFFOLD: remove the service from the tailnet.
	return core.StepResult{Succeeded: false, Error: fmt.Errorf("tailscale: unexpose_service not implemented")}
}

// Ensure tailscale satisfies the core.Provider interface.
var _ core.Provider = (*Provider)(nil)
