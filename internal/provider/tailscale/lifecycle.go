package tailscale

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// dialTimeout bounds the local reachability probe.
const dialTimeout = 5 * time.Second

// ExecuteStep executes one plan step.
func (p *Provider) ExecuteStep(ctx context.Context, connectionID core.ConnectionID,
	step core.PlanStep) (core.StepResult, error) {

	switch step.Technical.Type {
	case "verify_membership":
		return p.verifyMembership(ctx, step), nil
	case "verify_origin":
		return p.verifyOrigin(step), nil
	case "serve":
		return p.serve(ctx, connectionID, step), nil
	case "verify_serve":
		return p.verifyServe(ctx, step), nil
	case "unserve":
		return p.unserve(ctx, connectionID, step), nil
	case "release":
		// Releasing a join connection changes nothing on the machine. It exists so
		// closing has a step to journal and so the summary can tell the user the
		// machine stays signed in.
		p.forget(connectionID)
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil
	default:
		return core.StepResult{StepID: step.ID, Succeeded: false},
			fmt.Errorf("tailscale: unsupported step %q", step.Technical.Type)
	}
}

// verifyMembership confirms the machine is on the tailnet.
func (p *Provider) verifyMembership(ctx context.Context, step core.PlanStep) core.StepResult {
	status, err := readStatus(ctx, p.runner)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("could not ask the tailscale client for its status: %w", err)}
	}
	switch {
	case status.NeedsLogin():
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("this machine is not signed in to a tailnet; run `tailscale up`")}
	case status.NeedsMachineAuth():
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("this machine is waiting to be approved for the tailnet")}
	case !status.Joined():
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the tailscale client reports %q rather than running",
				status.BackendState)}
	}

	// The observed membership is recorded as a resource so the supervisor persists
	// what was confirmed. Ownership is adopted, never managed: Portico did not sign
	// this machine in and must not delete the membership when the connection goes.
	return core.StepResult{
		StepID: step.ID, Succeeded: true,
		Resources: []core.ProviderResource{{
			Type:       core.ResourceTailnetMembership,
			ExternalID: status.MachineName(),
			Ownership:  core.OwnershipAdopted,
			Metadata: map[string]string{
				"tailnet": status.TailnetName(),
				"address": status.PrivateAddress(),
			},
		}},
	}
}

// verifyOrigin probes the local address before publishing it.
//
// Publishing an address nothing is listening on produces a tailnet name that refuses
// every connection, which is harder to diagnose than a refusal now.
func (p *Provider) verifyOrigin(step core.PlanStep) core.StepResult {
	target := step.Technical.Parameters["target"]
	if target == "" {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the plan carried no address to publish")}
	}
	conn, err := net.DialTimeout("tcp", target, dialTimeout)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("nothing is listening on %s: %w", target, err)}
	}
	conn.Close()
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// serve publishes the local address to the tailnet.
func (p *Provider) serve(ctx context.Context, connectionID core.ConnectionID,
	step core.PlanStep) core.StepResult {

	target := step.Technical.Parameters["target"]
	if target == "" {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the plan carried no address to publish")}
	}

	// `--bg` so the serve outlives the command. Without it the client would hold the
	// foreground until interrupted, and the step would never return.
	if _, err := p.runner.Run(ctx, "serve", "--bg", target); err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the tailscale client would not publish %s: %w", target, err)}
	}

	p.remember(connectionID, target)

	status, statusErr := readStatus(ctx, p.runner)
	address := ""
	if statusErr == nil {
		address = status.PrivateAddress()
	}

	// The serve is a resource Portico created, so it is owned: closing the connection
	// withdraws exactly this, and nothing else the user configured by hand.
	return core.StepResult{
		StepID: step.ID, Succeeded: true,
		Resources: []core.ProviderResource{{
			Type:       core.ResourceTailnetServe,
			ExternalID: target,
			Ownership:  core.OwnershipManaged,
			Metadata:   map[string]string{"address": address},
		}},
	}
}

// verifyServe confirms the client reports the serve.
func (p *Provider) verifyServe(ctx context.Context, step core.PlanStep) core.StepResult {
	target := step.Technical.Parameters["target"]
	serving, err := p.servedTargets(ctx)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("could not read the tailscale serve configuration: %w", err)}
	}
	if !serving[target] {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the client accepted the request but does not report serving %s", target)}
	}
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// unserve withdraws the serve Portico configured.
func (p *Provider) unserve(ctx context.Context, connectionID core.ConnectionID,
	step core.PlanStep) core.StepResult {

	target := step.Technical.Parameters["target"]
	if target == "" {
		target = p.servedTarget(connectionID)
	}
	if target == "" {
		// Nothing recorded to withdraw. That is the desired state, so it succeeds
		// rather than failing a close because there was nothing to close.
		p.forget(connectionID)
		return core.StepResult{StepID: step.ID, Succeeded: true}
	}

	if _, err := p.runner.Run(ctx, "serve", "--bg", "off", target); err != nil {
		// Already withdrawn is success: closing twice must not fail. Anything else is
		// reported, because a serve left running keeps the service reachable after
		// the user asked for it to stop.
		serving, statusErr := p.servedTargets(ctx)
		if statusErr == nil && !serving[target] {
			p.forget(connectionID)
			return core.StepResult{StepID: step.ID, Succeeded: true}
		}
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the tailscale client would not stop publishing %s: %w", target, err)}
	}

	p.forget(connectionID)
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// Observe reports the connection's state from the client, not from memory.
func (p *Provider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	return p.ObserveWithResources(ctx, id, nil)
}

// ObserveWithResources observes from the durable resource inventory.
//
// A restarted supervisor has no memory of what it configured, so the resources it
// persisted are the input: the membership it confirmed and the exact serve target it
// created. Both are checked against the client's own state by exact identifier, never
// by looking for something that resembles Portico's.
func (p *Provider) ObserveWithResources(ctx context.Context, id core.ConnectionID,
	resources []core.ProviderResource) (*core.ObservedConnection, error) {

	obs := &core.ObservedConnection{
		ConnectionID: id,
		ProviderID:   "tailscale",
		Connector:    &core.ObservedConnector{Status: string(core.ConnectorStatusStopped)},
	}

	status, statusErr := readStatus(ctx, p.runner)

	// Serve state is read once, because several resources may ask about it.
	var serving map[string]bool
	var serveErr error
	if hasServeResource(resources) {
		serving, serveErr = p.servedTargets(ctx)
	}

	for _, res := range resources {
		if res.ExternalID == "" {
			continue
		}
		observed := core.ObservedResourceStatus{Type: res.Type, ExternalID: res.ExternalID}

		switch res.Type {
		case core.ResourceTailnetMembership:
			switch {
			case statusErr != nil:
				// A client that cannot be reached says nothing about membership.
				// Reporting it missing would make reconciliation act on a guess.
				observed.Status = core.ObservationTransient
				observed.Detail = statusErr.Error()
			case status.NeedsLogin() || status.NeedsMachineAuth():
				observed.Status = core.ObservationMissing
				observed.Detail = fmt.Sprintf("the machine is %s", strings.ToLower(status.BackendState))
			case !status.Joined():
				observed.Status = core.ObservationTransient
				observed.Detail = fmt.Sprintf("the client reports %q", status.BackendState)
			case status.MachineName() != res.ExternalID:
				// The machine is on a tailnet under a different name than the one
				// recorded. That is not the resource Portico confirmed, so it is not
				// reported as present.
				observed.Status = core.ObservationMissing
				observed.Detail = fmt.Sprintf(
					"this machine is now %q on the tailnet, not %q",
					status.MachineName(), res.ExternalID)
			default:
				observed.Status = core.ObservationPresent
				obs.Connector.Status = string(core.ConnectorStatusRunning)
			}

		case core.ResourceTailnetServe:
			switch {
			case serveErr != nil:
				observed.Status = core.ObservationTransient
				observed.Detail = serveErr.Error()
			case serving[res.ExternalID]:
				observed.Status = core.ObservationPresent
				obs.Connector.Status = string(core.ConnectorStatusRunning)
				p.remember(id, res.ExternalID)
			default:
				observed.Status = core.ObservationMissing
				observed.Detail = fmt.Sprintf("the tailnet is not serving %s", res.ExternalID)
			}

		default:
			observed.Status = core.ObservationTransient
			observed.Detail = fmt.Sprintf("tailscale does not manage a %s", res.Type)
		}

		obs.ResourceStatuses = append(obs.ResourceStatuses, observed)
	}

	// The tailnet address is not reported here. ObservedConnection has no endpoint
	// field, and inventing one would mean a second place the address comes from: the
	// plan's Expected.PrivateAddress is what the supervisor records, and the
	// membership resource's metadata carries what was observed.
	return obs, nil
}

// hasServeResource reports whether any resource needs the serve configuration read.
func hasServeResource(resources []core.ProviderResource) bool {
	for _, res := range resources {
		if res.Type == core.ResourceTailnetServe {
			return true
		}
	}
	return false
}

// servedTargets reads which local addresses the client is publishing.
func (p *Provider) servedTargets(ctx context.Context) (map[string]bool, error) {
	out, err := p.runner.Run(ctx, "serve", "status", "--json")
	if err != nil {
		// A client with no serve configuration exits non-zero on some versions while
		// printing usable output, so the output is parsed before the error is
		// reported — otherwise "nothing is served" reads as "the client is broken".
		if targets, parseErr := parseServeStatus(out); parseErr == nil {
			return targets, nil
		}
		return nil, err
	}
	return parseServeStatus(out)
}

// remember records the target Portico is serving for a connection.
func (p *Provider) remember(id core.ConnectionID, target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.serving[id] = target
}

// forget drops the recorded target.
func (p *Provider) forget(id core.ConnectionID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.serving, id)
}

// servedTarget is the target recorded for a connection, if this process configured it.
func (p *Provider) servedTarget(id core.ConnectionID) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.serving[id]
}
