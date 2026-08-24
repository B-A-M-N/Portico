package controller

import (
	"context"
	"fmt"

	"github.com/B-A-M-N/portico/internal/core"
)

// Repairing a private-network connection.
//
// What can be wrong differs by mode, and so does what may be done about it:
//
//   - A publish has a serve Portico created. If the network is no longer serving the
//     address, that is drift Portico owns and can put back.
//   - A join has nothing Portico created. If the machine has left the network, the fix is
//     to sign it back in, which is a credential operation Portico cannot perform — so
//     repair reports it rather than pretending to fix it.
//
// This is the distinction that makes repair trustworthy for this kind: it acts on what
// Portico owns and says so when the answer is somewhere else.
//
// The controller decides WHETHER repair is needed (based on observation statuses);
// the selected provider decides HOW to realize it (by planning the desired
// private-network state). The controller does not manufacture Tailscale technical
// parameters.

// privateNetworkRepairSteps builds the steps that would put a private-network connection
// back into the state its profile asks for.
//
// An empty slice with no error means nothing needs repairing.
func (c *Controller) privateNetworkRepairSteps(ctx context.Context,
	profile *core.ConnectionProfile) ([]core.PlanStep, error) {

	spec := profile.Spec.PrivateNetwork
	if spec == nil {
		return nil, core.ErrValidation("this connection has no private network specification")
	}

	observed, err := c.Observe(ctx, profile.ID)
	if err != nil {
		return nil, fmt.Errorf("observing the private network connection: %w", err)
	}

	membershipPresent := true
	serveNeedsRepair := false

	for _, res := range observed.ResourceStatuses {
		switch res.Type {
		case core.ResourceTailnetMembership:
			// Only an authoritative absence counts. A transient answer says nothing
			// about membership, and repairing on that would act on a guess about a
			// machine-wide setting.
			if res.Status == core.ObservationMissing {
				membershipPresent = false
			}
		case core.ResourceTailnetServe:
			// Both missing and drifted are repairable: Portico owns the frontend
			// binding in both cases and may safely restore the exact route.
			if res.Status == core.ObservationMissing || res.Status == core.ObservationDrifted {
				serveNeedsRepair = true
			}
		}
	}

	// The machine has left the network. Portico did not sign it in and holds no
	// credential to sign it back in, so there is no step it could run. Refusing with the
	// reason is more use than a repair that would fail.
	if !membershipPresent {
		return nil, core.ErrValidation(
			"this machine is no longer on the private network. Portico did not sign it in and " +
				"cannot sign it back in; sign in with the network's own client, and the " +
				"connection will recover on its own")
	}

	if spec.Mode != core.PrivateNetworkExpose {
		// A join with intact membership has nothing to repair: there is no resource
		// Portico created.
		return nil, nil
	}

	if !serveNeedsRepair {
		return nil, nil
	}

	// The serve Portico owned is missing or drifted. Ask the selected provider to
	// plan the desired private-network state from the current profile and runtime.
	// The resulting Tailscale repair may carry: verify membership, verify origin,
	// serve, verify serve. The extra verification steps are read-only; the actual
	// mutation is the smallest one — restore the owned Serve route.
	//
	// planOpenSteps is used rather than PlanOpen so repair does not re-run the
	// full open-plan admission path (which would also fingerprint and observe).
	steps, err := c.planOpenSteps(ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("planning the serve restore: %w", err)
	}
	if len(steps) == 0 {
		return nil, core.ErrValidation(
			"this connection publishes a service but the provider produced no repair steps, " +
				"so Portico cannot publish it again")
	}
	return steps, nil
}
