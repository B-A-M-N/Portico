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

	"github.com/paoloanzn/portico/internal/core"
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
	address := originAddress(profile)
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
	default:
		// running, starting, or unknown. Unknown identity must never
		// be treated as failure (never signal on PID alone).
		return nil
	}
}

// checkProvider inspects the provider edge segment.
func (e *Engine) checkProvider(ctx context.Context, profile *core.ConnectionProfile, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	var evidence []core.Evidence
	if e.deps.Provider != nil {
		observed, err := e.deps.Provider.ObserveProvider(ctx, profile.ID)
		if err == nil && observed != nil {
			if observed.Tunnel == nil && rt.State == core.RuntimeOpen {
				evidence = append(evidence, core.Evidence{
					Type:    "provider_observation",
					Source:  string(observed.ProviderID),
					Message: "Provider reports no tunnel for this connection",
				})
			}
		}
		// Observation errors are treated as unknown, not failure.
	}

	if rt.State == core.RuntimeOpen && rt.Endpoint.PublicAddress == "" {
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

// originAddress extracts the probe address for the local origin, if
// the profile references an existing local service.
func originAddress(profile *core.ConnectionProfile) string {
	if profile.Source.Existing == nil {
		return ""
	}
	return profile.Source.Existing.Address
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
