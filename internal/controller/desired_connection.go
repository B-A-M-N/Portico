package controller

import (
	"context"
	"fmt"

	"github.com/B-A-M-N/portico/internal/core"
)

// buildDesiredConnection is the single authority for constructing the
// DesiredConnection handed to a provider's Plan.
//
// Every path that asks a provider to plan an open — PlanOpen, edit reopen
// (planOpenSteps), and every repair that reuses open steps — must go through
// here. Before this helper existed, each caller assembled its own struct and
// the copies drifted: the shared reopen helper omitted gateway targeting
// entirely, so an OpenAI-compatible connection repaired or edited after its
// gateway died would replan pointing at the RAW origin, silently discarding
// the authentication/SSE boundary the normal open path guarantees.
//
// Gateway rule (audit P0-2, unchanged): for a profile whose intent requires
// a gateway, the plan carries either the live endpoint (a gateway is already
// running) or the symbolic core.GatewayTargetRef the executor resolves at
// apply AFTER the supervisor has started one — failing closed if none runs.
// The raw origin never leaks into such a plan.
func (c *Controller) buildDesiredConnection(
	ctx context.Context,
	profile *core.ConnectionProfile,
	runtimeCopy *core.ConnectionRuntime,
) (*core.ResolvedOrigin, core.DesiredConnection, error) {
	openProfile := profile.DeepCopy()
	openProfile.Desired = core.DesiredOpen

	// Only service-exposure connections have a local origin model. Port
	// forwards, client tunnels, and private networks have no service origin —
	// pass nil.
	var resolvedOrigin *core.ResolvedOrigin
	if openProfile.Kind == core.ConnectionServiceExposure {
		var err error
		resolvedOrigin, err = c.prepareOriginForConnection(ctx, openProfile.ID, openProfile.GetSource())
		if err != nil {
			return nil, core.DesiredConnection{}, fmt.Errorf("origin preparation: %w", err)
		}
	}

	desired := core.DesiredConnection{
		Profile: openProfile,
		Runtime: runtimeCopy,
		Origin:  resolvedOrigin,
	}
	if openProfile.RequiresGateway() {
		// Preview is non-mutating: no gateway may run yet at planning time.
		// The plan pins either the live endpoint or the symbolic reference;
		// snapshotting "no endpoint" would permanently bypass the boundary
		// and snapshotting a guessed port would point at nothing.
		desired.GatewayRequired = true
		desired.GatewayEndpoint = c.effectiveGatewayTarget(openProfile.ID)
		if desired.GatewayEndpoint == "" {
			desired.GatewayEndpoint = core.GatewayTargetRef
		} else if resolvedOrigin != nil && desired.GatewayEndpoint != core.GatewayTargetRef {
			// Providers use GatewayEndpoint when constructing transport steps, but
			// some provider implementations also inspect Origin directly. Give
			// that provider-facing copy the effective target while retaining the
			// raw origin in resolvedOrigin for the supervisor's upstream gateway
			// and any controller-owned origin lifecycle step.
			providerOrigin := *resolvedOrigin
			providerOrigin.URL = desired.GatewayEndpoint
			desired.Origin = &providerOrigin
		}
	}
	return resolvedOrigin, desired, nil
}

// bindGatewayTargets performs the apply-time half of gateway late binding.
// Preview plans retain the symbolic gateway reference because the gateway is
// supervisor-owned and does not exist yet. Once the supervisor has started it,
// provider planning is repeated without mutation to obtain the live endpoint,
// and only symbolic parameters in the immutable apply copy are replaced.
func (c *Controller) bindGatewayTargets(ctx context.Context, plan *core.OperationPlan) (*core.OperationPlan, error) {
	if plan == nil || !planHasGatewayTarget(plan) {
		return plan, nil
	}

	c.mu.RLock()
	profile := c.profiles[plan.ConnectionID]
	runtime := c.runtimes[plan.ConnectionID]
	c.mu.RUnlock()
	if profile == nil {
		return nil, core.ErrProfileNotFound(plan.ConnectionID)
	}
	planningProfile := profile
	if plan.Intent == core.IntentEdit && plan.EditPayload != nil && plan.EditPayload.Profile != nil {
		planningProfile = plan.EditPayload.Profile
	}

	// Re-plan after the gateway exists so providers that derive additional
	// launch metadata from the effective origin see the same live endpoint.
	plannedSteps, planErr := c.planOpenStepsWithRuntime(ctx, planningProfile, runtime)
	target := c.effectiveGatewayTarget(plan.ConnectionID)
	if planErr == nil {
		for _, step := range plannedSteps {
			if value := step.Technical.Parameters["origin_url"]; value != "" && value != core.GatewayTargetRef {
				target = value
				break
			}
		}
	}
	if target == "" || target == core.GatewayTargetRef {
		if planErr != nil {
			return nil, fmt.Errorf("resolve gateway target: %w", planErr)
		}
		return nil, fmt.Errorf("resolve gateway target: gateway is not running")
	}

	bound := plan.DeepCopy()
	for i := range bound.Steps {
		for key, value := range bound.Steps[i].Technical.Parameters {
			if value == core.GatewayTargetRef {
				bound.Steps[i].Technical.Parameters[key] = target
			}
		}
		if bound.Steps[i].Compensation != nil {
			for key, value := range bound.Steps[i].Compensation.Technical.Parameters {
				if value == core.GatewayTargetRef {
					bound.Steps[i].Compensation.Technical.Parameters[key] = target
				}
			}
		}
	}
	if err := bound.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("recompute gateway-bound plan fingerprint: %w", err)
	}
	return bound, nil
}

func planHasGatewayTarget(plan *core.OperationPlan) bool {
	if plan == nil {
		return false
	}
	for _, step := range plan.Steps {
		for _, value := range step.Technical.Parameters {
			if value == core.GatewayTargetRef {
				return true
			}
		}
		if step.Compensation != nil {
			for _, value := range step.Compensation.Technical.Parameters {
				if value == core.GatewayTargetRef {
					return true
				}
			}
		}
	}
	return false
}

// planOpenStepsWithRuntime is planOpenSteps with an explicit runtime source,
// so repair paths can pass their already-loaded runtime instead of having
// this helper reload it.
func (c *Controller) planOpenStepsWithRuntime(
	ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime,
) ([]core.PlanStep, error) {
	prov, err := c.providerForProfile(profile)
	if err != nil {
		return nil, err
	}

	c.mu.RLock()
	var runtimeCopy *core.ConnectionRuntime
	if rt == nil {
		if stored := c.runtimes[profile.ID]; stored != nil {
			runtimeCopy = stored.DeepCopy()
		}
	} else {
		runtimeCopy = rt.DeepCopy()
	}
	c.mu.RUnlock()

	resolvedOrigin, desired, err := c.buildDesiredConnection(ctx, profile, runtimeCopy)
	if err != nil {
		return nil, err
	}

	plan, err := prov.Plan(ctx, desired)
	if err != nil {
		return nil, err
	}
	if resolvedOrigin != nil && resolvedOrigin.Owned {
		insertStartOriginStep(plan, resolvedOrigin.URL)
	}
	return plan.Steps, nil
}

// planOpenSteps produces the steps that would open a connection under the
// given profile, without touching the stored profile.
//
// It exists so an edit can append the reopen to its own plan, and so repair can
// ask the provider to plan the desired state rather than manufacturing
// provider-specific technical parameters in controller code. The profile is
// passed explicitly rather than read from the controller, because during an
// edit the stored profile is still the previous one.
//
// The current runtime is passed so a provider can distinguish a frontend Portico
// owns from one the user configured manually. It is deep-copied: the provider
// must not be able to mutate controller state through it.
//
// Gateway targeting flows through buildDesiredConnection, exactly as PlanOpen:
// an edited-and-reopened OpenAI-compatible connection keeps its gateway in the
// traffic path rather than replanning against the raw origin.
func (c *Controller) planOpenSteps(ctx context.Context, profile *core.ConnectionProfile) ([]core.PlanStep, error) {
	return c.planOpenStepsWithRuntime(ctx, profile, nil)
}

// restartConnectorStep builds the connector-restart step used by repair.
//
// For a gateway-required profile the origin_url parameter is the symbolic
// core.GatewayTargetRef, which the executor resolves against the live gateway
// at apply time — failing closed if none runs. Hand-writing
// resolvedOrigin.URL here was the raw-origin bypass: repair manufactured the
// exact step the planning layer refuses to produce.
func (c *Controller) restartConnectorStep(profile *core.ConnectionProfile, mode string, originURL string) core.PlanStep {
	target := originURL
	if profile.RequiresGateway() {
		target = core.GatewayTargetRef
	}
	return core.PlanStep{
		ID:      "repair-restart-connector",
		Kind:    core.StepStartConnector,
		Summary: "Restart connector",
		Technical: core.TechnicalOperation{
			Provider:   profile.GetProvider().ProviderID,
			Type:       "start_connector",
			Parameters: map[string]string{"mode": mode, "origin_url": target},
		},
	}
}
