package tailscale

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// Provider implements private-network connections through the local tailscale client.
//
// It holds no in-memory serve authority. All route derivation goes through ServeRoute
// in serve.go; the durable ProviderResource inventory is the only authority for
// restart, close, repair, and reconciliation.
type Provider struct {
	runner CommandRunner
}

// New builds a Tailscale provider over the given runner.
func New(runner CommandRunner) *Provider {
	if runner == nil {
		runner = NewExecRunner(Binary)
	}
	return &Provider{runner: runner}
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
		steps, expected, err := p.openSteps(ctx, profile, spec, mode, desired.Runtime)
		if err != nil {
			return nil, err
		}
		plan.Intent = core.IntentOpen
		plan.Steps = steps
		plan.Expected = expected

	case core.DesiredClosed:
		plan.Intent = core.IntentClose
		plan.Steps = p.closeSteps(profile, spec, mode, desired.Runtime)
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
	spec *core.PrivateNetworkSpec, mode core.PrivateNetworkMode, runtime *core.ConnectionRuntime,
) ([]core.PlanStep, core.ExpectedOutcome, error) {

	var expected core.ExpectedOutcome

	baseParams := map[string]string{"mode": string(mode)}
	if spec.NetworkID != "" {
		baseParams["network"] = spec.NetworkID
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
		Technical: core.TechnicalOperation{Provider: "tailscale", Type: "verify_membership", Parameters: baseParams},
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
		route, err := serveRouteForProfile(spec)
		if err != nil {
			return nil, expected, err
		}

		// Foreign-collision check: if a live managed ProviderResource exists for the
		// same frontend identity, Portico owns it. If a different resource occupies the
		// same frontend and Portico has no live managed row, refuse before mutation.
		if runtime != nil {
			if collision, err := serveCollision(route, runtime.Provider.Resources); err != nil {
				return nil, expected, err
			} else if collision {
				return nil, expected, core.ErrValidation(fmt.Sprintf(
					"port %s on the tailnet is already configured by something other than this "+
						"connection; Portico will not overwrite a serve it did not create",
					route.Identity()))
			}
		}

		routeParams := route.StepParameters()
		for k, v := range baseParams {
			routeParams[k] = v
		}

		steps = append(steps,
			core.PlanStep{
				ID: "tailnet-verify-origin", Kind: core.StepVerifyOrigin,
				Summary:   fmt.Sprintf("Verify %s is reachable on this machine", route.BackendEndpoint()),
				Technical: core.TechnicalOperation{Provider: "tailscale", Type: "verify_origin", Parameters: routeParams},
			},
			core.PlanStep{
				ID: "tailnet-serve", Kind: core.StepCreateTunnel,
				Summary: fmt.Sprintf("Publish %s to the tailnet, and only to the tailnet", route.BackendEndpoint()),
				Technical: core.TechnicalOperation{
					Provider: "tailscale", Type: "serve", Parameters: routeParams,
				},
			},
			core.PlanStep{
				ID: "tailnet-verify-serve", Kind: core.StepVerifyConnector,
				Summary:   "Confirm the tailnet is serving it",
				Technical: core.TechnicalOperation{Provider: "tailscale", Type: "verify_serve", Parameters: routeParams},
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

// serveOwnership classifies who holds the desired frontend binding.
type serveOwnership int

const (
	// serveFrontendFree means no live resource occupies the frontend identity.
	serveFrontendFree serveOwnership = iota
	// serveFrontendOwned means Portico has a live managed resource for the exact
	// frontend identity, so it may verify, repair, or re-plan it.
	serveFrontendOwned
	// serveFrontendForeign means the frontend identity is occupied by something
	// Portico does not manage, so it must not be overwritten.
	serveFrontendForeign
)

// serveCollision reports whether the desired frontend identity may be mutated.
//
// It returns true (refuse) when the frontend is occupied by anything Portico does
// not own. The four cases are distinguished explicitly:
//
//	nothing occupies the identity                  -> safe to create
//	live managed resource for the identity          -> Portico owns it, safe
//	live non-managed resource for the identity      -> foreign, refuse
//	live resource whose metadata will not parse     -> ownership unverifiable, refuse
//
// The last case is the one that must fail closed. Skipping an unparseable resource
// would let a corrupt row read as "nothing is there", and Portico would then
// overwrite a binding whose ownership it could not establish.
func serveCollision(route ServeRoute, resources []core.ProviderResource) (bool, error) {
	ownership := serveFrontendFree

	for _, res := range resources {
		if res.Type != core.ResourceTailnetServe || !res.IsLive() {
			continue
		}

		other, err := serveRouteFromResource(res.Metadata)
		if err != nil {
			// The row cannot be interpreted, so it cannot be ruled out as the
			// occupant of this frontend. Refuse rather than guess: overwriting a
			// route Portico cannot verify it owns is the failure this prevents.
			return true, fmt.Errorf(
				"the tracked serve resource %s carries metadata Portico cannot parse, so it "+
					"cannot confirm whether it owns %s: %w", res.ExternalID, route.Identity(), err)
		}

		if !other.IsSameIdentity(route) {
			// A different frontend. Two Portico Serve connections may coexist, and
			// the same backend may sit behind two different frontends.
			continue
		}

		if res.Ownership == core.OwnershipManaged {
			ownership = serveFrontendOwned
			continue
		}
		// Adopted or external: the user configured this, not Portico.
		return true, nil
	}

	return ownership == serveFrontendForeign, nil
}

// closeSteps builds the steps that take a connection down.
//
// For expose mode, the exact route comes from the live managed ProviderResource in
// the runtime — NOT from the current profile. The profile is what the user currently
// wants; the resource is what Portico actually owns remotely. Deletion must target
// the second one.
func (p *Provider) closeSteps(profile *core.ConnectionProfile, spec *core.PrivateNetworkSpec,
	mode core.PrivateNetworkMode, runtime *core.ConnectionRuntime,
) []core.PlanStep {
	baseParams := map[string]string{"mode": string(mode)}
	if spec.NetworkID != "" {
		baseParams["network"] = spec.NetworkID
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
			Technical: core.TechnicalOperation{Provider: "tailscale", Type: "release", Parameters: baseParams},
		}}
	}

	// Expose mode: withdraw the exact route Portico created. Prefer the live managed
	// resource; fall back to the profile only if no resource has been persisted yet.
	route, err := serveRouteForClose(spec, runtime)
	if err != nil {
		// If we cannot reconstruct the route from either durable or profile state,
		// the close cannot safely target anything. Return a failing unserve step so
		// the error surfaces at execution rather than silently leaking the route.
		return []core.PlanStep{{
			ID: "tailnet-unserve", Kind: core.StepStopConnector,
			Summary:   fmt.Sprintf("Stop publishing the tailnet serve (route unavailable: %v)", err),
			Technical: core.TechnicalOperation{Provider: "tailscale", Type: "unserve", Parameters: baseParams},
		}}
	}

	routeParams := route.StepParameters()
	for k, v := range baseParams {
		routeParams[k] = v
	}

	return []core.PlanStep{{
		ID: "tailnet-unserve", Kind: core.StepStopConnector,
		Summary:   fmt.Sprintf("Stop publishing %s to the tailnet", route.Identity()),
		Technical: core.TechnicalOperation{Provider: "tailscale", Type: "unserve", Parameters: routeParams},
	}}
}

// serveRouteForClose reconstructs the route to withdraw for an expose close. It
// prefers the live managed ProviderResource (what Portico actually owns remotely)
// over the current profile (what the user wants now).
func serveRouteForClose(spec *core.PrivateNetworkSpec, runtime *core.ConnectionRuntime) (ServeRoute, error) {
	if runtime != nil {
		for _, res := range runtime.Provider.Resources {
			if res.Type != core.ResourceTailnetServe || !res.IsLive() || res.Ownership != core.OwnershipManaged {
				continue
			}
			route, err := serveRouteFromResource(res.Metadata)
			if err != nil {
				return ServeRoute{}, fmt.Errorf(
					"the managed serve resource %s carries metadata Portico cannot parse, "+
						"so Portico cannot safely withdraw it: %v", res.ExternalID, err)
			}
			return route, nil
		}
	}
	// Fall back to the profile — this happens when a profile has not yet produced a
	// resource (e.g. planned but never executed).
	return serveRouteForProfile(spec)
}

// serveRouteForProfile is the single provider-owned conversion from a private-network
// spec to a ServeRoute. It consumes LocalAddress and LocalProtocol and produces the
// deterministic route described in serve.go.
func serveRouteForProfile(spec *core.PrivateNetworkSpec) (ServeRoute, error) {
	if spec == nil {
		return ServeRoute{}, core.ErrValidation("publishing a service to the tailnet needs a private network specification")
	}
	address := strings.TrimSpace(spec.LocalAddress)
	if address == "" {
		return ServeRoute{}, core.ErrValidation(
			"publishing a service to the tailnet needs the address it is listening on, and this " +
				"connection does not carry one")
	}
	protocol := strings.TrimSpace(string(spec.LocalProtocol))
	if protocol == "" {
		protocol = "http"
	}
	return newServeRouteFromAddress(protocol, address)
}
