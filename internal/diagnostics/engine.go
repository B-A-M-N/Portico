// Package diagnostics probes the route segments of a connection in
// causal order (origin -> connector -> provider -> dns -> endpoint)
// and produces a finding for the first failing segment only, so users
// see the cause instead of downstream noise (SPEC 16.4).
package diagnostics

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// SegmentProbe is the outcome of probing one route segment.
type SegmentProbe struct {
	Status   core.ProbeStatus
	Evidence []core.Evidence
}

// OriginProber probes a local origin address for liveness.
type OriginProber interface {
	ProbeOrigin(ctx context.Context, address string) SegmentProbe
}

// ConnectorObserver reports the observed connector status for a
// connection. ok is false when nothing is known about the connector.
type ConnectorObserver interface {
	ObserveConnector(ctx context.Context, connID core.ConnectionID) (core.ConnectorStatus, bool)
}

// ConnectorObserverFunc adapts a function to ConnectorObserver.
type ConnectorObserverFunc func(ctx context.Context, connID core.ConnectionID) (core.ConnectorStatus, bool)

// ObserveConnector implements ConnectorObserver.
func (f ConnectorObserverFunc) ObserveConnector(ctx context.Context, connID core.ConnectionID) (core.ConnectorStatus, bool) {
	return f(ctx, connID)
}

// ProviderObserver observes provider-side state for a connection.
type ProviderObserver interface {
	ObserveProvider(ctx context.Context, connID core.ConnectionID) (*core.ObservedConnection, error)
}

// ProviderObserverFunc adapts a function to ProviderObserver.
type ProviderObserverFunc func(ctx context.Context, connID core.ConnectionID) (*core.ObservedConnection, error)

// ObserveProvider implements ProviderObserver.
func (f ProviderObserverFunc) ObserveProvider(ctx context.Context, connID core.ConnectionID) (*core.ObservedConnection, error) {
	return f(ctx, connID)
}

// DNSProber resolves a public hostname.
type DNSProber interface {
	ResolveHost(ctx context.Context, host string) ([]string, error)
}

// EndpointProber probes the public endpoint of a connection.
type EndpointProber interface {
	ProbeEndpoint(ctx context.Context, publicAddress string) SegmentProbe
}

// Deps are the segment probes the engine evaluates. Any nil dependency
// causes the corresponding segment to be skipped (unknown is never
// treated as failure).
type Deps struct {
	Origin    OriginProber
	Connector ConnectorObserver
	Provider  ProviderObserver
	DNS       DNSProber
	Endpoint  EndpointProber
}

// Engine performs diagnostics on connections by probing route segments
// in causal order and reporting the first failing segment.
type Engine struct {
	deps Deps
}

// New creates a diagnostics engine.
func New(deps Deps) *Engine {
	return &Engine{deps: deps}
}

// Diagnose probes the route of a connection and returns at most one
// finding: the first failing causal segment. A healthy route returns
// no findings.
func (e *Engine) Diagnose(ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime) ([]core.DiagnosticFinding, error) {
	if profile == nil {
		return nil, fmt.Errorf("no profile available")
	}
	if rt == nil {
		return nil, fmt.Errorf("no runtime state available")
	}

	// An intentionally closed connection has nothing to diagnose.
	if profile.Desired == core.DesiredClosed || rt.State == core.RuntimeClosed {
		return nil, nil
	}

	checks := []func(context.Context, *core.ConnectionProfile, *core.ConnectionRuntime) *core.DiagnosticFinding{
		e.checkOrigin,
		e.checkConnector,
		e.checkProvider,
		e.checkDNS,
		e.checkEndpoint,
	}
	for _, check := range checks {
		if f := check(ctx, profile, rt); f != nil {
			return []core.DiagnosticFinding{*f}, nil
		}
	}
	return nil, nil
}

// checkOrigin probes the local service segment.
func (e *Engine) checkOrigin(ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	if e.deps.Origin == nil {
		return nil
	}
	address := originAddress(profile, rt)
	if address == "" {
		return nil
	}

	probe := e.deps.Origin.ProbeOrigin(ctx, address)
	if probe.Status != core.ProbeFail {
		return nil
	}
	return &core.DiagnosticFinding{
		ID:           core.FindingID(fmt.Sprintf("find-%s-origin", profile.ID)),
		ConnectionID: profile.ID,
		Segment:      core.SegmentLocalService,
		Severity:     core.SeverityError,
		Summary:      "Local service is not responding",
		Explanation:  fmt.Sprintf("The local service at %s did not answer. Everything downstream (connector, provider, public endpoint) depends on it, so fix the local service first.", address),
		Evidence:     probe.Evidence,
		ObservedAt:   time.Now().UTC(),
	}
}

// checkConnector inspects the connector segment.
func (e *Engine) checkConnector(ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	status := rt.Connector.Status
	if e.deps.Connector != nil {
		if observed, ok := e.deps.Connector.ObserveConnector(ctx, profile.ID); ok {
			status = observed
		}
	}

	restart := core.RepairOption{
		Summary:     "Restart connector",
		Explanation: "Restart the connector process. The tunnel, DNS, and access policy will not be recreated. The public address will remain unchanged.",
		IsSafe:      true,
	}

	switch status {
	case core.ConnectorStatusCrashed:
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-conn", profile.ID)),
			ConnectionID: profile.ID,
			Segment:      core.SegmentConnector,
			Severity:     core.SeverityError,
			Summary:      "Connector process stopped",
			Explanation:  "The connector process that maintains the tunnel has exited. The local service is still running, but the public endpoint is not reachable.",
			Evidence: []core.Evidence{
				{Type: "process_status", Source: "supervisor", Message: "Connector process exited unexpectedly"},
			},
			RepairOptions: []core.RepairOption{restart},
			ObservedAt:    time.Now().UTC(),
		}
	case core.ConnectorStatusStopped:
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-conn-stop", profile.ID)),
			ConnectionID: profile.ID,
			Segment:      core.SegmentConnector,
			Severity:     core.SeverityWarning,
			Summary:      "Connector is stopped",
			Explanation:  "The connector is not running. The connection will not be reachable until the connector starts.",
			Evidence: []core.Evidence{
				{Type: "process_status", Source: "supervisor", Message: "Connector process is not running while the connection is desired open"},
			},
			RepairOptions: []core.RepairOption{restart},
			ObservedAt:    time.Now().UTC(),
		}
	case core.ConnectorStatusUnknown:
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-conn-identity", profile.ID)),
			ConnectionID: profile.ID,
			Segment:      core.SegmentConnector,
			Severity:     core.SeverityWarning,
			Summary:      "Connector identity could not be verified",
			Explanation:  "Portico will not signal the recorded PID because its process identity no longer matches. Confirm the existing process or start a fresh connector through a repair plan.",
			Evidence: []core.Evidence{
				{Type: "process_identity", Source: "supervisor", Message: "Recorded connector PID cannot be safely verified"},
			},
			RepairOptions: []core.RepairOption{restart},
			ObservedAt:    time.Now().UTC(),
		}
	case core.ConnectorStatusUnstable:
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-conn-unstable", profile.ID)),
			ConnectionID: profile.ID,
			Segment:      core.SegmentConnector,
			Severity:     core.SeverityError,
			Summary:      "Connector restart budget exhausted",
			Explanation:  "The connector repeatedly exited and Portico stopped automatic restarts. Review its log, then use a repair plan to start a fresh connector.",
			Evidence: []core.Evidence{
				{Type: "process_status", Source: "supervisor", Message: "Connector restart budget exhausted"},
			},
			RepairOptions: []core.RepairOption{restart},
			ObservedAt:    time.Now().UTC(),
		}
	default:
		// Running and starting produce no causal finding.
		return nil
	}
}

// checkProvider inspects the provider edge segment.
func (e *Engine) checkProvider(ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	var evidence []core.Evidence
	if e.deps.Provider != nil {
		observed, err := e.deps.Provider.ObserveProvider(ctx, profile.ID)
		if err == nil && observed != nil {
			// An absent observed object is ambiguous. Only exact resource-status
			// lookups are actionable, and their stable physical order determines
			// the first causal finding.
			for _, status := range observed.ResourceStatuses {
				switch status.Status {
				case core.ObservationUnauthorized, core.ObservationRateLimited, core.ObservationTransient:
					return providerObservationFinding(profile.ID, observed.ProviderID, status)
				}
			}
			for _, resourceType := range []core.ResourceType{
				core.ResourceTunnel, core.ResourceDNSRecord, core.ResourceAccessApp, core.ResourceAccessPolicy,
			} {
				for _, status := range observed.ResourceStatuses {
					if status.Type == resourceType && status.Status == core.ObservationMissing {
						return missingProviderResourceFinding(profile.ID, observed.ProviderID, status)
					}
				}
			}
			// A drifted serve is one Portico owns and may safely restore. Surface it
			// as a provider-route finding rather than silently ignoring it.
			for _, status := range observed.ResourceStatuses {
				if status.Status == core.ObservationDrifted {
					return driftedProviderResourceFinding(profile.ID, observed.ProviderID, status)
				}
			}
		}
		// Observation errors are treated as unknown, not failure.
	}

	// An open connection with no public address is a fault only for a
	// connection that was supposed to get one. This engine is otherwise
	// kind-blind, so without this guard every healthy port forward, client
	// tunnel and private-only exposure is diagnosed as a tunnel that failed to
	// reach the provider edge — a severity-error finding, persisted, shown on
	// the detail screen, and offered to the user as something to repair.
	if rt.State == core.RuntimeOpen && rt.Endpoint.PublicAddress == "" && profile.ExpectsPublicAddress() {
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-edge", profile.ID)),
			ConnectionID: profile.ID,
			Segment:      core.SegmentProviderEdge,
			Severity:     core.SeverityError,
			Summary:      "Provider edge unavailable",
			Explanation:  "The provider has not assigned a public endpoint. The tunnel may not have connected to the provider edge.",
			Evidence:     evidence,
			ObservedAt:   time.Now().UTC(),
		}
	}
	if len(evidence) > 0 {
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-edge", profile.ID)),
			ConnectionID: profile.ID,
			Segment:      core.SegmentProviderEdge,
			Severity:     core.SeverityError,
			Summary:      "Provider tunnel is missing",
			Explanation:  "The provider no longer reports the tunnel backing this connection. The public endpoint will not be reachable until the tunnel is recreated.",
			Evidence:     evidence,
			ObservedAt:   time.Now().UTC(),
		}
	}
	return nil
}

// missingProviderResourceFinding turns an authoritative exact-ID 404 into a
// segment-specific finding. It does not infer absence from a nil resource or
// from an unavailable provider response.
func missingProviderResourceFinding(connectionID core.ConnectionID, providerID core.ProviderID, status core.ObservedResourceStatus) *core.DiagnosticFinding {
	evidence := []core.Evidence{{
		Type: "provider_observation", Source: string(providerID),
		Message: "Provider reports the tracked resource is missing",
		Data:    map[string]string{"resource_id": status.ExternalID, "resource_type": string(status.Type), "detail": status.Detail},
	}}
	switch status.Type {
	case core.ResourceTunnel:
		return &core.DiagnosticFinding{
			ID: core.FindingID(fmt.Sprintf("find-%s-edge", connectionID)), ConnectionID: connectionID,
			Segment: core.SegmentProviderEdge, Severity: core.SeverityError,
			Summary:     "Provider tunnel is missing",
			Explanation: "The provider no longer reports the tunnel backing this connection. The public endpoint will not be reachable until the tunnel is recreated.",
			Evidence:    evidence, ObservedAt: time.Now().UTC(),
		}
	case core.ResourceDNSRecord:
		return &core.DiagnosticFinding{
			ID: core.FindingID(fmt.Sprintf("find-%s-dns-resource", connectionID)), ConnectionID: connectionID,
			Segment: core.SegmentAddress, Severity: core.SeverityError,
			Summary:     "Managed DNS record is missing",
			Explanation: "The provider confirmed that Portico's DNS record is absent. The tunnel remains intact; repair should recreate only this DNS record.",
			Evidence:    evidence, ObservedAt: time.Now().UTC(),
		}
	case core.ResourceAccessApp, core.ResourceAccessPolicy:
		return &core.DiagnosticFinding{
			ID: core.FindingID(fmt.Sprintf("find-%s-protection-resource", connectionID)), ConnectionID: connectionID,
			Segment: core.SegmentProtection, Severity: core.SeverityError,
			Summary:     "Managed access protection is missing",
			Explanation: "The provider confirmed that a tracked Access application or policy is absent. Portico will not silently weaken protection; review and repair the exact protection resource.",
			Evidence:    evidence, ObservedAt: time.Now().UTC(),
		}
	default:
		return nil
	}
}

// driftedProviderResourceFinding turns an authoritative exact-ID drift into a
// provider-route finding. A drifted resource is one Portico owns and may safely
// restore; it is distinct from missing (absent) and transient (unverifiable).
// It produces a provider-route finding, not an "observation unavailable" finding
// and not an externally-removed lifecycle transition.
func driftedProviderResourceFinding(connectionID core.ConnectionID, providerID core.ProviderID, status core.ObservedResourceStatus) *core.DiagnosticFinding {
	return &core.DiagnosticFinding{
		ID:           core.FindingID(fmt.Sprintf("find-%s-route-drifted", connectionID)),
		ConnectionID: connectionID,
		Segment:      core.SegmentProviderEdge,
		Severity:     core.SeverityWarning,
		Summary:      "Managed provider route has drifted",
		Explanation: fmt.Sprintf(
			"Portico owns the route %s but the provider reports a different configuration than what Portico persisted. The route can be restored to the persisted configuration.",
			status.ExternalID),
		Evidence: []core.Evidence{{
			Type: "provider_observation", Source: string(providerID),
			Message: string(core.ObservationDrifted),
			Data:    map[string]string{"resource_id": status.ExternalID, "resource_type": string(status.Type), "detail": status.Detail},
		}},
		ObservedAt: time.Now().UTC(),
	}
}

// providerObservationFinding records an inability to determine provider
// state without incorrectly presenting it as missing infrastructure.
func providerObservationFinding(connectionID core.ConnectionID, providerID core.ProviderID, status core.ObservedResourceStatus) *core.DiagnosticFinding {
	summary := "Provider state could not be verified"
	explanation := "Portico could not determine whether the tracked provider resource exists. No infrastructure will be recreated until observation succeeds."
	severity := core.SeverityWarning
	switch status.Status {
	case core.ObservationUnauthorized:
		summary = "Provider authentication required"
		explanation = "The provider rejected the tunnel lookup. Reauthenticate the selected provider account, then run diagnostics again."
		severity = core.SeverityError
	case core.ObservationRateLimited:
		summary = "Provider is rate limiting observation"
		explanation = "The provider temporarily rate limited the tunnel lookup. Portico will not treat the tunnel as missing; try again after the rate limit clears."
	case core.ObservationTransient:
		summary = "Provider is temporarily unavailable"
		explanation = "The provider lookup failed transiently. Portico will not recreate infrastructure until the provider can be observed again."
	}
	return &core.DiagnosticFinding{
		ID:           core.FindingID(fmt.Sprintf("find-%s-edge", connectionID)),
		ConnectionID: connectionID,
		Segment:      core.SegmentProviderEdge,
		Severity:     severity,
		Summary:      summary,
		Explanation:  explanation,
		Evidence: []core.Evidence{{
			Type: "provider_observation", Source: string(providerID), Message: string(status.Status),
			Data: map[string]string{"resource_id": status.ExternalID, "detail": status.Detail},
		}},
		ObservedAt: time.Now().UTC(),
	}
}

// checkDNS resolves the public hostname (address segment).
func (e *Engine) checkDNS(ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	if e.deps.DNS == nil {
		return nil
	}
	host := publicHost(rt)
	if host == "" || net.ParseIP(host) != nil {
		return nil
	}

	addrs, err := e.deps.DNS.ResolveHost(ctx, host)
	if err == nil && len(addrs) > 0 {
		return nil
	}
	msg := "no addresses returned"
	if err != nil {
		msg = err.Error()
	}
	return &core.DiagnosticFinding{
		ID:           core.FindingID(fmt.Sprintf("find-%s-dns", profile.ID)),
		ConnectionID: profile.ID,
		Segment:      core.SegmentAddress,
		Severity:     core.SeverityError,
		Summary:      "Public hostname does not resolve",
		Explanation:  fmt.Sprintf("DNS resolution failed for %s. The connector and provider are healthy, so only the DNS record needs attention.", host),
		Evidence: []core.Evidence{
			{Type: "dns_lookup", Source: "resolver", Message: msg, Data: map[string]string{"host": host}},
		},
		ObservedAt: time.Now().UTC(),
	}
}

// checkEndpoint probes the public endpoint segment.
func (e *Engine) checkEndpoint(ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	if e.deps.Endpoint == nil || rt.Endpoint.PublicAddress == "" {
		return nil
	}

	probe := e.deps.Endpoint.ProbeEndpoint(ctx, rt.Endpoint.PublicAddress)
	if probe.Status != core.ProbeFail {
		return nil
	}
	return &core.DiagnosticFinding{
		ID:           core.FindingID(fmt.Sprintf("find-%s-endpoint", profile.ID)),
		ConnectionID: profile.ID,
		Segment:      core.SegmentEndpoint,
		Severity:     core.SeverityWarning,
		Summary:      "Public endpoint is not reachable",
		Explanation:  "All internal segments look healthy, but the public endpoint probe failed. This may be a transient provider edge issue; retry observation before repairing.",
		Evidence:     probe.Evidence,
		ObservedAt:   time.Now().UTC(),
	}
}

// originAddress extracts the probe address for the local origin. It supports
// both external existing services and Portico-owned origins (directory,
// command, MCP-command). The runtime projection is preferred when the
// origin is owned so the diagnostics engine probes the actually-running
// service, not the configured value.
func originAddress(profile *core.ConnectionProfile, rt *core.ConnectionRuntime) string {
	if profile == nil {
		return ""
	}
	if rt != nil && rt.Origin.Ownership == core.OriginOwnershipOwned && rt.Origin.URL != "" {
		return rt.Origin.URL
	}
	switch profile.GetSource().Kind {
	case core.SourceExisting:
		if profile.GetSource().Existing != nil {
			return profile.GetSource().Existing.Address
		}
	case core.SourceDirectory, core.SourceCommand, core.SourceMCP:
		if rt != nil && rt.Origin.URL != "" {
			return rt.Origin.URL
		}
	}
	return ""
}

// publicHost extracts the hostname to resolve from the runtime
// endpoint state.
func publicHost(rt *core.ConnectionRuntime) string {
	if rt.Endpoint.Hostname != "" {
		return rt.Endpoint.Hostname
	}
	addr := rt.Endpoint.PublicAddress
	if addr == "" {
		return ""
	}
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+3:]
	}
	if i := strings.IndexAny(addr, "/?#"); i >= 0 {
		addr = addr[:i]
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// ExplainFinding generates a plain-language explanation from a finding.
func ExplainFinding(finding *core.DiagnosticFinding) string {
	if finding == nil {
		return "No issues found."
	}

	return fmt.Sprintf("%s\n\n%s", finding.Summary, finding.Explanation)
}

// SmallestRepair returns the smallest repair that restores desired state
// given a finding. It prefers restart over recreation.
func SmallestRepair(finding *core.DiagnosticFinding) *core.RepairOption {
	if finding == nil || len(finding.RepairOptions) == 0 {
		return nil
	}

	// Return the safest repair option first
	for i := range finding.RepairOptions {
		if finding.RepairOptions[i].IsSafe {
			return &finding.RepairOptions[i]
		}
	}

	return &finding.RepairOptions[0]
}
