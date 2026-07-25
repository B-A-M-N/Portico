package supervisor

import (
	"context"
	"errors"

	"github.com/paoloanzn/portico/internal/controller"
	"github.com/paoloanzn/portico/internal/core"
)

// ReconcileInput contains all the information needed to compute
// the smallest plan to reach desired state.
type ReconcileInput struct {
	Profile   *core.ConnectionProfile
	Runtime   *core.ConnectionRuntime
	Resources []core.ProviderResource
	Observed  *core.ObservedConnection
}

// reconcileDecision represents the action needed for a connection.
type reconcileDecision struct {
	Action string // "none", "open", "close", "repair", "recreate"
	Plan   *core.OperationPlan
}

// computeReconcileDecision compares desired vs observed state and
// returns the smallest action needed. This is the single source of
// truth for restart, reconciliation, and repair planning.
func (s *Supervisor) computeReconcileDecision(ctx context.Context, input ReconcileInput) (*reconcileDecision, error) {
	desired := input.Profile.Desired

	// If no runtime exists, we need a full open or are already closed.
	if input.Runtime == nil {
		if desired == core.DesiredOpen {
			plan, err := s.controller.PlanOpen(ctx, input.Profile.ID)
			if err != nil {
				return nil, err
			}
			return &reconcileDecision{Action: "open", Plan: plan}, nil
		}
		return &reconcileDecision{Action: "none"}, nil
	}

	// Desired closed dominates - do not repair a connection the user wants closed.
	if desired == core.DesiredClosed {
		if input.Runtime.State == core.RuntimeClosed {
			return &reconcileDecision{Action: "none"}, nil
		}
		plan, err := s.controller.PlanClose(ctx, input.Profile.ID)
		if err != nil {
			return nil, err
		}
		return &reconcileDecision{Action: "close", Plan: plan}, nil
	}

	// Desired open - check what needs to be done.
	switch input.Runtime.State {
	case core.RuntimeOpen:
		// Check connector health.
		if input.Runtime.Connector.Status == core.ConnectorStatusRunning {
			return &reconcileDecision{Action: "none"}, nil
		}
		if input.Runtime.Connector.Status == core.ConnectorStatusUnknown {
			// Identity could not be verified: never signal the PID —
			// require an explicit repair instead of a blind restart.
			return s.repairDecision(ctx, input.Profile.ID)
		}
		// Connector not running - restart it.
		plan := s.buildConnectorRestartPlan(input.Profile.ID, input.Profile)
		if plan == nil {
			return &reconcileDecision{Action: "none"}, nil
		}
		return &reconcileDecision{Action: "repair", Plan: plan}, nil

	case core.RuntimeClosed, core.RuntimeUnknown:
		// Delta planning: if a live tunnel resource is tracked and the
		// provider has not authoritatively confirmed it missing, retain
		// existing infrastructure and only restart the connector.
		// Full recreation requires either no live tunnel or a confirmed
		// not-found from observation.
		tunnel := liveTunnelResource(input.Resources)
		if tunnel != nil && !observedMissing(input.Observed, core.ResourceTunnel, tunnel.ExternalID) {
			plan := s.buildConnectorRestartPlan(input.Profile.ID, input.Profile)
			if plan != nil {
				return &reconcileDecision{Action: "repair", Plan: plan}, nil
			}
		}
		// Need full open plan.
		plan, err := s.controller.PlanOpen(ctx, input.Profile.ID)
		if err != nil {
			return nil, err
		}
		return &reconcileDecision{Action: "open", Plan: plan}, nil

	case core.RuntimeDegraded, core.RuntimeError:
		return s.repairDecision(ctx, input.Profile.ID)

	case core.RuntimeOrphaned:
		// Managed resources need cleanup.
		plan, err := s.controller.PlanDelete(ctx, input.Profile.ID)
		if err != nil {
			return nil, err
		}
		return &reconcileDecision{Action: "recreate", Plan: plan}, nil
	}

	return &reconcileDecision{Action: "none"}, nil
}

// repairDecision plans a repair, mapping the typed no-repair result to "none".
func (s *Supervisor) repairDecision(ctx context.Context, connID core.ConnectionID) (*reconcileDecision, error) {
	plan, err := s.controller.PlanRepair(ctx, connID)
	if errors.Is(err, controller.ErrNoRepairNeeded) {
		return &reconcileDecision{Action: "none"}, nil
	}
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return &reconcileDecision{Action: "none"}, nil
	}
	return &reconcileDecision{Action: "repair", Plan: plan}, nil
}

// liveTunnelResource returns the tracked tunnel resource that is still
// considered live (not removed), or nil.
func liveTunnelResource(resources []core.ProviderResource) *core.ProviderResource {
	for i := range resources {
		r := &resources[i]
		if r.Type != core.ResourceTunnel {
			continue
		}
		switch r.Lifecycle {
		case core.LifecycleRemoved:
			continue
		}
		return r
	}
	return nil
}

// observedMissing reports whether observation authoritatively classified
// the given resource as not found. Transient, unauthorized, or absent
// observations never count as missing.
func observedMissing(obs *core.ObservedConnection, resType core.ResourceType, externalID string) bool {
	if obs == nil {
		return false
	}
	for _, st := range obs.ResourceStatuses {
		if st.Type == resType && st.ExternalID == externalID {
			return st.Status == core.ObservationMissing
		}
	}
	return false
}
