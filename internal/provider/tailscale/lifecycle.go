package tailscale

import (
	"context"
	"fmt"
	"net"
	"net/http"
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
		return p.serve(ctx, step), nil
	case "verify_serve":
		return p.verifyServe(ctx, step), nil
	case "unserve":
		return p.unserve(ctx, step), nil
	case "release":
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
	identity := status.NodeID()
	return core.StepResult{
		StepID: step.ID, Succeeded: true,
		Resources: []core.ProviderResource{{
			Type:       core.ResourceTailnetMembership,
			ExternalID: identity,
			Ownership:  core.OwnershipAdopted,
			Metadata: map[string]string{
				"tailnet":  status.TailnetName(),
				"address":  status.PrivateAddress(),
				"dns_name": status.MachineName(),
			},
		}},
	}
}

// verifyOrigin probes the local backend before publishing it. It reconstructs the
// exact route from the step so that protocol-aware probing (TCP dial vs HTTP/HTTPS
// probe) matches the protocol being published.
func (p *Provider) verifyOrigin(step core.PlanStep) core.StepResult {
	route, err := serveRouteFromStep(step.Technical.Parameters)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the plan carried no address to publish: %w", err)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	switch route.BackendProtocol {
	case "http", "https":
		if err := probeHTTPBackend(ctx, route); err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false,
				Error: fmt.Errorf("%s is not reachable: %w", route.BackendEndpoint(), err)}
		}
	default:
		// TCP (and anything else) — verify something is listening on the port.
		conn, err := net.DialTimeout("tcp", route.BackendTarget(), dialTimeout)
		if err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false,
				Error: fmt.Errorf("nothing is listening on %s: %w", route.BackendEndpoint(), err)}
		}
		conn.Close()
	}
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// probeHTTPBackend issues a GET to the canonical backend URL and reports whether it
// gets a response. Any response (even 5xx) is enough to confirm something is listening.
func probeHTTPBackend(ctx context.Context, route ServeRoute) error {
	endpoint := route.BackendEndpoint()
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// serve publishes the local address to the tailnet and persists the exact route.
//
// The route's identity is the durable ExternalID. The route's own ResourceMetadata
// reuses the canonical step encoding — these two can never drift, because they are
// the same map produced by the same function.
func (p *Provider) serve(ctx context.Context, step core.PlanStep) core.StepResult {
	route, err := serveRouteFromStep(step.Technical.Parameters)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
	}

	if _, err := p.runner.Run(ctx, route.OpenArgs()...); err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the tailscale client would not publish %s: %w", route.Identity(), err)}
	}

	status, statusErr := readStatus(ctx, p.runner)
	address := ""
	if statusErr == nil {
		address = status.PrivateAddress()
	}

	metadata := route.ResourceMetadata()
	metadata["address"] = address

	return core.StepResult{
		StepID: step.ID, Succeeded: true,
		Resources: []core.ProviderResource{{
			Type:       core.ResourceTailnetServe,
			ExternalID: route.Identity(),
			Ownership:  core.OwnershipManaged,
			Metadata:   metadata,
		}},
	}
}

// verifyServe confirms the client reports the exact route Portico created.
//
// Same frontend identity + same complete backend → success. Same frontend identity +
// different backend → failure (drift is reported by the step that repairs it). No
// frontend identity → failure (missing is reported by the step that recreates it).
func (p *Provider) verifyServe(ctx context.Context, step core.PlanStep) core.StepResult {
	route, err := serveRouteFromStep(step.Technical.Parameters)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}
	}

	serving, err := p.observedRoutes(ctx)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("could not read the tailscale serve configuration: %w", err)}
	}

	for _, observed := range serving {
		if observed.Equal(route) {
			return core.StepResult{StepID: step.ID, Succeeded: true}
		}
	}
	return core.StepResult{StepID: step.ID, Succeeded: false,
		Error: fmt.Errorf("the client accepted the request but does not report serving %s", route.Identity())}
}

// unserve withdraws the exact serve Portico configured. The route must be
// reconstructable from the step; if it cannot, the step fails.
//
// A successful absence after an idempotent repeated close is acceptable only when a
// follow-up authoritative observation confirms the frontend binding is gone.
func (p *Provider) unserve(ctx context.Context, step core.PlanStep) core.StepResult {
	route, err := serveRouteFromStep(step.Technical.Parameters)
	if err != nil {
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("cannot reconstruct the serve to withdraw: %w", err)}
	}

	if _, err := p.runner.Run(ctx, route.CloseArgs()...); err != nil {
		// The close command failed. Verify whether the route is still present — an
		// idempotent repeated close may report failure when the binding is already gone.
		serving, statusErr := p.observedRoutes(ctx)
		if statusErr == nil {
			for _, observed := range serving {
				if observed.IsSameIdentity(route) {
					return core.StepResult{StepID: step.ID, Succeeded: false,
						Error: fmt.Errorf("the tailscale client would not stop publishing %s: %w", route.Identity(), err)}
				}
			}
			// The frontend binding is gone — the close effectively succeeded.
			return core.StepResult{StepID: step.ID, Succeeded: true}
		}
		return core.StepResult{StepID: step.ID, Succeeded: false,
			Error: fmt.Errorf("the tailscale client would not stop publishing %s: %w", route.Identity(), err)}
	}
	return core.StepResult{StepID: step.ID, Succeeded: true}
}

// observedRoutes reads the full serve configuration the client reports. A non-zero
// exit code from `tailscale serve status --json` is an observation failure/transient,
// NOT a valid empty config — permission, daemon, and version failures can still
// produce partial output. Empty valid `{}` on success means "nothing served".
func (p *Provider) observedRoutes(ctx context.Context) ([]ServeRoute, error) {
	out, err := p.runner.Run(ctx, "serve", "status", "--json")
	if err != nil {
		return nil, err
	}
	return observedRoutes(out)
}

// Observe reports the connection's state from the durable resource inventory — never
// from adapter memory. The provider reconstructs solely from ProviderResource rows.
func (p *Provider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	return p.ObserveWithResources(ctx, id, nil)
}

// ObserveWithResources observes from the durable resource inventory.
func (p *Provider) ObserveWithResources(ctx context.Context, id core.ConnectionID,
	resources []core.ProviderResource) (*core.ObservedConnection, error) {

	obs := &core.ObservedConnection{
		ConnectionID: id,
		ProviderID:   "tailscale",
		Connector:    &core.ObservedConnector{Status: string(core.ConnectorStatusStopped)},
	}

	status, statusErr := readStatus(ctx, p.runner)

	var serving []ServeRoute
	var serveErr error
	if hasServeResource(resources) {
		serving, serveErr = p.observedRoutes(ctx)
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
				observed.Status = core.ObservationTransient
				observed.Detail = statusErr.Error()
			case status.NeedsLogin() || status.NeedsMachineAuth():
				observed.Status = core.ObservationMissing
				observed.Detail = fmt.Sprintf("the machine is %s", strings.ToLower(status.BackendState))
			case !status.Joined():
				observed.Status = core.ObservationTransient
				observed.Detail = fmt.Sprintf("the client reports %q", status.BackendState)
			case !membershipMatches(status, res.ExternalID):
				observed.Status = core.ObservationMissing
				observed.Detail = fmt.Sprintf(
					"this machine is now %q on the tailnet, not %q",
					status.NodeID(), res.ExternalID)
			default:
				observed.Status = core.ObservationPresent
			}

		case core.ResourceTailnetServe:
			switch {
			case serveErr != nil:
				observed.Status = core.ObservationTransient
				observed.Detail = serveErr.Error()
			default:
				expected, err := serveRouteFromResource(res.Metadata)
				if err != nil {
					observed.Status = core.ObservationTransient
					observed.Detail = err.Error()
					break
				}
				var matching *ServeRoute
				for i := range serving {
					if serving[i].IsSameIdentity(expected) {
						matching = &serving[i]
						break
					}
				}
				if matching == nil {
					observed.Status = core.ObservationMissing
					observed.Detail = fmt.Sprintf("the tailnet is not serving %s", res.ExternalID)
				} else if matching.Equal(expected) {
					observed.Status = core.ObservationPresent
				} else {
					observed.Status = core.ObservationDrifted
					observed.Detail = fmt.Sprintf(
						"the route %s is now serving %s, not %s",
						res.ExternalID, matching.BackendEndpoint(), expected.BackendEndpoint())
				}
			}

		default:
			observed.Status = core.ObservationTransient
			observed.Detail = fmt.Sprintf("tailscale does not manage a %s", res.Type)
		}

		obs.ResourceStatuses = append(obs.ResourceStatuses, observed)
	}

	membershipOK := true
	serveOK := !hasServeResource(resources)
	for _, res := range obs.ResourceStatuses {
		switch res.Type {
		case core.ResourceTailnetMembership:
			membershipOK = res.Status == core.ObservationPresent
		case core.ResourceTailnetServe:
			serveOK = res.Status == core.ObservationPresent
		}
	}
	if membershipOK && serveOK {
		obs.Connector.Status = string(core.ConnectorStatusRunning)
	}

	return obs, nil
}

func hasServeResource(resources []core.ProviderResource) bool {
	for _, res := range resources {
		if res.Type == core.ResourceTailnetServe {
			return true
		}
	}
	return false
}

// membershipMatches reports whether the observed membership corresponds to the
// recorded resource.
func membershipMatches(status *Status, recordedID string) bool {
	if recordedID == "" {
		return false
	}
	current := status.NodeID()
	if current == recordedID {
		return true
	}
	return status.MachineName() == recordedID && recordedID != ""
}
