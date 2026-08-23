package controller

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// Repairing a port forward.
//
// CanRepair returned false for every kind but service exposure, so the interface
// carried a permanent "repair is not available for this connection kind" check —
// for a kind whose provider has a full lifecycle: a plan, a start step, a stop
// step and an Observe that reports whether the listener is up. Nothing about the
// architecture prevented repair; only this function did.
//
// The four things that go wrong with a forward are a missing listener, a
// forwarding process that stopped, a target that has become unreachable, and a
// runtime that disagrees with what is actually listening. Each has a step, and
// each step is one the provider already executes — so a repair is the same
// mechanism as an open, applied to the parts that are broken.

// portForwardRepairSteps builds the repair for a forward.
//
// It returns no steps when nothing is wrong, which PlanRepair reports as
// ErrNoRepairNeeded: a healthy connection needing no repair is an answer, not a
// failure.
func (c *Controller) portForwardRepairSteps(
	ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime,
) ([]core.PlanStep, error) {
	spec := profile.Spec.PortForward
	if spec == nil {
		return nil, core.ErrValidation("this connection has no port forward to repair")
	}

	target := net.JoinHostPort(spec.RemoteHost, fmt.Sprint(spec.RemotePort))
	params := map[string]string{
		"local_port": fmt.Sprint(spec.LocalPort),
		"target":     target,
	}

	// What the provider currently reports, which is the authority on whether the
	// listener exists. The runtime is a projection of it: for an in-process relay
	// the two can disagree, and when they do it is the observation that is right.
	observed, observeErr := c.Observe(ctx, profile.ID)

	listening := false
	if observeErr == nil && observed != nil && observed.Connector != nil {
		listening = observed.Connector.Status == string(core.ConnectorStatusRunning)
	}

	runtimeThinksRunning := rt != nil && rt.Connector.Status == core.ConnectorStatusRunning

	var steps []core.PlanStep

	// A target that cannot be reached is repaired by nothing Portico can do to
	// the forward, so it is verified first: the step fails with the reason, which
	// is more use than a listener that starts and immediately refuses everything.
	steps = append(steps, core.PlanStep{
		ID:      "repair-verify-target",
		Kind:    core.StepVerifyOrigin,
		Summary: fmt.Sprintf("Check that %s is reachable", target),
		Technical: core.TechnicalOperation{
			Provider:   "portforward",
			Type:       "verify_target",
			Parameters: params,
		},
	})

	switch {
	case !listening:
		// Missing listener, or a forwarding process that stopped: the same
		// remedy, because the relay is the listener. Any stale registration is
		// stopped first so the restart binds cleanly rather than failing on an
		// address already in use.
		if runtimeThinksRunning || observeErr != nil {
			steps = append(steps, core.PlanStep{
				ID:      "repair-stop-stale-forward",
				Kind:    core.StepStopConnector,
				Summary: "Clear the stopped forward",
				Technical: core.TechnicalOperation{
					Provider: "portforward",
					Type:     "stop_forward",
				},
			})
		}
		steps = append(steps, core.PlanStep{
			ID:      "repair-restart-forward",
			Kind:    core.StepStartConnector,
			Summary: fmt.Sprintf("Forward 127.0.0.1:%d to %s again", spec.LocalPort, target),
			Technical: core.TechnicalOperation{
				Provider:   "portforward",
				Type:       "start_forward",
				Parameters: params,
			},
		})

	case !runtimeThinksRunning:
		// The forward is listening and the runtime says it is not. Nothing needs
		// restarting — restarting a working forward would break the connections
		// currently using it. What is wrong is the record, and the verification
		// step alone reconciles it, because applying a plan re-observes.
		return steps, nil

	default:
		// Listening, and the runtime agrees. There is nothing to repair, so the
		// verification step is not offered on its own: a plan whose only step is
		// a check is a plan that does nothing.
		return nil, nil
	}

	return steps, nil
}

// portForwardNeedsRepair reports whether a forward is in a state repair would
// change. It is the same comparison portForwardRepairSteps makes, used by
// CanRepair so the interface's offer and the backend's answer cannot disagree.
func (c *Controller) portForwardNeedsRepair(ctx context.Context, profile *core.ConnectionProfile) bool {
	c.mu.RLock()
	rt := c.runtimes[profile.ID]
	c.mu.RUnlock()
	steps, err := c.portForwardRepairSteps(ctx, profile, rt)
	return err == nil && len(steps) > 0
}

var _ = time.Now
