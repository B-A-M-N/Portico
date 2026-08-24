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
	servePresent := true
	var serveTarget string

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
			if res.Status == core.ObservationMissing {
				servePresent = false
				serveTarget = res.ExternalID
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

	if servePresent {
		return nil, nil
	}

	target := serveTarget
	if target == "" {
		target = spec.LocalAddress
	}
	if target == "" {
		return nil, core.ErrValidation(
			"this connection publishes a service but records no address, so Portico cannot " +
				"publish it again")
	}

	params := map[string]string{
		"mode":   string(core.PrivateNetworkExpose),
		"target": target,
	}
	if spec.NetworkID != "" {
		params["network"] = spec.NetworkID
	}

	// The origin is verified before republishing. Publishing an address nothing is
	// listening on produces a name on the network that refuses every connection, which is
	// harder to diagnose than a refusal now.
	return []core.PlanStep{
		{
			ID: "private-network-verify-origin", Kind: core.StepVerifyOrigin,
			Summary: fmt.Sprintf("Verify %s is reachable on this machine", target),
			Technical: core.TechnicalOperation{
				Provider: profile.Driver.ProviderID,
				Type:     "verify_origin", Parameters: params,
			},
		},
		{
			ID: "private-network-reserve", Kind: core.StepCreateTunnel,
			Summary: fmt.Sprintf("Publish %s to the network again", target),
			Technical: core.TechnicalOperation{
				Provider: profile.Driver.ProviderID,
				Type:     "serve", Parameters: params,
			},
		},
		{
			ID: "private-network-verify-serve", Kind: core.StepVerifyConnector,
			Summary: "Confirm the network is serving it",
			Technical: core.TechnicalOperation{
				Provider: profile.Driver.ProviderID,
				Type:     "verify_serve", Parameters: params,
			},
		},
	}, nil
}
