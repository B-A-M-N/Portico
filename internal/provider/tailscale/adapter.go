package tailscale

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// Provider implements private-network connections through the local tailscale client.
type Provider struct {
	runner CommandRunner

	mu sync.Mutex
	// serving records the addresses Portico configured a serve for, per connection.
	// It is memory only: observation reads the client's own serve configuration, so a
	// restarted supervisor does not depend on this map being populated.
	serving map[core.ConnectionID]string
}

// New builds a Tailscale provider over the given runner.
func New(runner CommandRunner) *Provider {
	if runner == nil {
		runner = NewExecRunner(Binary)
	}
	return &Provider{runner: runner, serving: make(map[core.ConnectionID]string)}
}

// Identity names the provider.
func (p *Provider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID:          "tailscale",
		Name:        "tailscale",
		DisplayName: "Tailscale",
	}
}

// Capabilities declares a private-only provider.
//
// Every public capability is declared unsupported, which is what stops the
// recommendation engine offering Tailscale for a public exposure and stops a public
// provider being offered for a private network. `tailscale funnel` would publish to the
// internet, and is deliberately not implemented: a user choosing "private network" must
// not end up with a public address.
func (p *Provider) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		Kinds: []core.ConnectionKind{core.ConnectionPrivateNetwork},
		TemporaryAddresses: core.CapabilitySupport{
			Supported: false,
			Notes:     []string{"a tailnet address is private and stable, not a temporary public one"},
		},
		CustomHostnames: core.CapabilitySupport{
			Supported: false,
			Notes:     []string{"the machine's tailnet name comes from Tailscale, not from Portico"},
		},
		ManagedDNS: core.CapabilitySupport{
			Supported: false,
			Notes:     []string{"MagicDNS is managed by Tailscale for the whole tailnet"},
		},
		PrivateExposure: core.CapabilitySupport{Supported: true, Stability: core.StabilityBeta},
		BuiltInProtection: []core.ProtectionCapability{
			{
				// Reaching the service requires being on the tailnet, which is
				// membership rather than a policy Portico applies. Declaring a
				// protection mode Portico cannot enforce would offer the user a
				// choice that does nothing.
				Kind: core.ProtectionPrivateNet, Supported: true,
				Stability: core.StabilityBeta,
			},
		},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP:  {Supported: true, Private: true},
			core.ProtocolHTTPS: {Supported: true, Private: true},
			core.ProtocolTCP:   {Supported: true, Private: true},
		},
		Telemetry:  core.TelemetryCapability{Supported: false},
		Redundancy: core.RedundancyCapability{Supported: false, MaxConnectors: 1},
		Expiration: core.ExpirationCapability{Supported: false},
	}, nil
}

// Authenticate is a no-op.
//
// Authentication is the client's own: `tailscale up` authenticates the machine against
// the tailnet, interactively or with an auth key the operator supplied to the daemon.
// Portico holds no Tailscale credential, which is why the definition declares no
// account — accepting one here would store a secret Portico never uses.
func (p *Provider) Authenticate(context.Context, core.AuthRequest) error { return nil }

// Plan produces the operation plan for a private-network connection.
func (p *Provider) Plan(ctx context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	profile := desired.Profile
	if profile == nil {
		return nil, fmt.Errorf("tailscale: profile required")
	}
	if profile.Kind != core.ConnectionPrivateNetwork {
		return nil, fmt.Errorf("tailscale: connection kind %q is not supported", profile.Kind)
	}
	spec := profile.Spec.PrivateNetwork
	if spec == nil {
		return nil, fmt.Errorf("tailscale: private network spec is required")
	}

	mode := spec.Mode
	if mode == "" {
		mode = core.PrivateNetworkJoin
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
		steps, expected, err := p.openSteps(ctx, profile, spec, mode)
		if err != nil {
			return nil, err
		}
		plan.Intent = core.IntentOpen
		plan.Steps = steps
		plan.Expected = expected

	case core.DesiredClosed:
		plan.Intent = core.IntentClose
		plan.Steps = p.closeSteps(spec, mode)
		plan.Expected.State = core.RuntimeClosed

	default:
		return nil, fmt.Errorf("tailscale: unsupported desired state %q", profile.Desired)
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("tailscale: plan fingerprint: %w", err)
	}
	return plan, nil
}

// openSteps builds the steps that bring a connection up.
func (p *Provider) openSteps(ctx context.Context, profile *core.ConnectionProfile,
	spec *core.PrivateNetworkSpec, mode core.PrivateNetworkMode) ([]core.PlanStep, core.ExpectedOutcome, error) {

	var expected core.ExpectedOutcome
	params := map[string]string{"mode": string(mode)}
	if spec.NetworkID != "" {
		params["network"] = spec.NetworkID
	}

	// The plan says what will happen on this machine, so it is built against what the
	// machine's state actually is. A plan that says "join the network" for a machine
	// already on it would describe work Portico is not going to do.
	status, statusErr := readStatus(ctx, p.runner)
	if statusErr != nil {
		return nil, expected, fmt.Errorf(
			"tailscale: could not read the client's status, so Portico cannot say what it "+
				"would need to change: %w", statusErr)
	}
	if status.NeedsLogin() {
		return nil, expected, core.ErrValidation(
			"this machine is not signed in to a tailnet. Run `tailscale up` and complete the " +
				"sign-in, then create the connection — Portico does not hold Tailscale " +
				"credentials and cannot sign in for you")
	}
	if status.NeedsMachineAuth() {
		return nil, expected, core.ErrValidation(
			"this machine is signed in but has not been approved for the tailnet. Approve it " +
				"in the Tailscale admin console, then create the connection")
	}
	if spec.NetworkID != "" && status.TailnetName() != "" &&
		!strings.EqualFold(spec.NetworkID, status.TailnetName()) {
		return nil, expected, core.ErrValidation(fmt.Sprintf(
			"this machine is on the tailnet %q, not %q. Portico cannot move a machine between "+
				"tailnets; sign in to the other one with `tailscale up` first",
			status.TailnetName(), spec.NetworkID))
	}

	steps := []core.PlanStep{{
		ID: "tailnet-verify-membership", Kind: core.StepValidateAccount,
		Summary:   "Confirm this machine is on the tailnet",
		Technical: core.TechnicalOperation{Provider: "tailscale", Type: "verify_membership", Parameters: params},
	}}

	expected.State = core.RuntimeOpen
	expected.PrivateAddress = status.PrivateAddress()

	switch mode {
	case core.PrivateNetworkJoin:
		// Joining is machine-wide and already true. There is nothing to create, so
		// the plan says so rather than inventing a step: the connection records that
		// this machine is reachable on the tailnet and watches that it stays so.
		if address := status.PrivateAddress(); address != "" {
			expected.PrivateAddress = address
		}

	case core.PrivateNetworkExpose:
		target, err := exposeTarget(profile)
		if err != nil {
			return nil, expected, err
		}
		params["target"] = target
		steps = append(steps,
			core.PlanStep{
				ID: "tailnet-verify-origin", Kind: core.StepVerifyOrigin,
				Summary:   fmt.Sprintf("Verify %s is reachable on this machine", target),
				Technical: core.TechnicalOperation{Provider: "tailscale", Type: "verify_origin", Parameters: params},
			},
			core.PlanStep{
				ID: "tailnet-serve", Kind: core.StepCreateTunnel,
				Summary: fmt.Sprintf("Publish %s to the tailnet, and only to the tailnet", target),
				Technical: core.TechnicalOperation{
					Provider: "tailscale", Type: "serve", Parameters: params,
				},
			},
			core.PlanStep{
				ID: "tailnet-verify-serve", Kind: core.StepVerifyConnector,
				Summary:   "Confirm the tailnet is serving it",
				Technical: core.TechnicalOperation{Provider: "tailscale", Type: "verify_serve", Parameters: params},
			},
		)
		if address := status.PrivateAddress(); address != "" {
			expected.PrivateAddress = address
		}

	default:
		return nil, expected, core.ErrValidation(fmt.Sprintf(
			"private network mode %q is not supported; Portico can join a tailnet or serve a "+
				"local address to one", mode))
	}

	return steps, expected, nil
}

// closeSteps builds the steps that take a connection down.
func (p *Provider) closeSteps(spec *core.PrivateNetworkSpec, mode core.PrivateNetworkMode) []core.PlanStep {
	params := map[string]string{"mode": string(mode)}
	if spec.NetworkID != "" {
		params["network"] = spec.NetworkID
	}

	if mode != core.PrivateNetworkExpose {
		// Closing a join connection stops Portico tracking the machine's membership.
		// It deliberately does not run `tailscale down`: membership is machine-wide
		// and predates this connection, so logging the machine out would take away
		// something Portico never created and break every other connection on it.
		return []core.PlanStep{{
			ID: "tailnet-release", Kind: core.StepStopConnector,
			Summary: "Stop tracking this machine's tailnet membership. The machine stays " +
				"signed in — Portico did not sign it in and will not sign it out.",
			Technical: core.TechnicalOperation{Provider: "tailscale", Type: "release", Parameters: params},
		}}
	}

	return []core.PlanStep{{
		ID: "tailnet-unserve", Kind: core.StepStopConnector,
		Summary:   "Stop publishing the service to the tailnet",
		Technical: core.TechnicalOperation{Provider: "tailscale", Type: "unserve", Parameters: params},
	}}
}

// exposeTarget is the local address a serve connection publishes.
//
// `expose` needs something to expose. The address comes from the profile's own source
// so the field means the same thing it means for every other kind, rather than
// Tailscale reading a differently named field.
func exposeTarget(profile *core.ConnectionProfile) (string, error) {
	if exposure := profile.Spec.ServiceExposure; exposure != nil && exposure.Source.Existing != nil {
		if address := strings.TrimSpace(exposure.Source.Existing.Address); address != "" {
			return normaliseTarget(address)
		}
	}
	if forward := profile.Spec.PortForward; forward != nil && forward.LocalPort > 0 {
		return normaliseTarget(fmt.Sprintf("127.0.0.1:%d", forward.LocalPort))
	}
	return "", core.ErrValidation(
		"publishing a service to the tailnet needs the address it is listening on, and this " +
			"connection does not carry one")
}

// normaliseTarget turns a user-supplied address into what the client expects.
func normaliseTarget(address string) (string, error) {
	// A bare port is the common shorthand.
	if !strings.Contains(address, ":") {
		return "", core.ErrValidation(fmt.Sprintf(
			"%q is not a host:port address; the tailnet needs to know which port to publish",
			address))
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", core.ErrValidation(fmt.Sprintf("%q is not a host:port address: %v", address, err))
	}
	if host == "" {
		host = "127.0.0.1"
	}
	// The same normalisation the serve configuration is read through, so a target
	// recorded here matches what the client reports back. Two spellings of one
	// address would make observation report the serve missing, and a serve reported
	// missing is one reconciliation recreates.
	return normaliseServeTarget(net.JoinHostPort(host, port)), nil
}
