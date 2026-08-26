package controller

import (
	"context"
	"fmt"

	"github.com/B-A-M-N/portico/internal/core"
)

// clientTunnelRepairSteps repairs the local half of a client-mediated tunnel.
// There is no Portico-owned public endpoint or remote tunnel resource to
// recreate. A stopped, crashed, unknown, or unstable client is restored by
// replaying the provider's open plan, preserving its causal order: verify the
// MCP origin, validate the client, start the client/gateway, and verify it.
func (c *Controller) clientTunnelRepairSteps(
	ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime,
) ([]core.PlanStep, error) {
	if profile.Spec.ClientTunnel == nil {
		return nil, core.ErrValidation("this connection has no client tunnel specification")
	}

	// Prefer an authoritative provider observation. Runtime is only a fallback
	// because it can be stale after a supervisor restart.
	observed, observeErr := c.Observe(ctx, profile.ID)
	status := core.ConnectorStatusUnknown
	if observeErr == nil && observed != nil && observed.Connector != nil {
		status = core.ConnectorStatus(observed.Connector.Status)
	} else if rt != nil {
		status = rt.Connector.Status
	} else if observeErr != nil {
		return nil, fmt.Errorf("observing client tunnel: %w", observeErr)
	}

	switch status {
	case core.ConnectorStatusRunning, core.ConnectorStatusStarting:
		return nil, nil
	}

	steps, err := c.planOpenSteps(ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("planning client tunnel restart: %w", err)
	}
	if len(steps) == 0 {
		return nil, core.ErrValidation(
			"the client tunnel is unhealthy, but its provider produced no restart steps")
	}
	return steps, nil
}
