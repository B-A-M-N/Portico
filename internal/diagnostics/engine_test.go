package diagnostics

import (
	"context"
	"errors"
	"testing"

	"github.com/paoloanzn/portico/internal/core"
)

// --------------- fakes ---------------

type fakeOrigin struct {
	probe SegmentProbe
	calls int
}

func (f *fakeOrigin) ProbeOrigin(ctx context.Context, address string) SegmentProbe {
	f.calls++
	return f.probe
}

type fakeConnector struct {
	status core.ConnectorStatus
	ok     bool
	calls  int
}

func (f *fakeConnector) ObserveConnector(ctx context.Context, connID core.ConnectionID) (core.ConnectorStatus, bool) {
	f.calls++
	return f.status, f.ok
}

type fakeProvider struct {
	observed *core.ObservedConnection
	err      error
	calls    int
}

func (f *fakeProvider) ObserveProvider(ctx context.Context, connID core.ConnectionID) (*core.ObservedConnection, error) {
	f.calls++
	return f.observed, f.err
}

type fakeDNS struct {
	addrs []string
	err   error
	calls int
}

func (f *fakeDNS) ResolveHost(ctx context.Context, host string) ([]string, error) {
	f.calls++
	return f.addrs, f.err
}

type fakeEndpoint struct {
	probe SegmentProbe
	calls int
}

func (f *fakeEndpoint) ProbeEndpoint(ctx context.Context, publicAddress string) SegmentProbe {
	f.calls++
	return f.probe
}

// --------------- helpers ---------------

func pass() SegmentProbe {
	return SegmentProbe{Status: core.ProbePass}
}

func fail(msg string) SegmentProbe {
	return SegmentProbe{
		Status:   core.ProbeFail,
		Evidence: []core.Evidence{{Type: "http_probe", Source: "test", Message: msg}},
	}
}

type fixture struct {
	origin    *fakeOrigin
	connector *fakeConnector
	provider  *fakeProvider
	dns       *fakeDNS
	endpoint  *fakeEndpoint
	engine    *Engine
	profile   *core.ConnectionProfile
	runtime   *core.ConnectionRuntime
}

// healthyFixture builds an engine where every segment passes.
func healthyFixture() *fixture {
	f := &fixture{
		origin:    &fakeOrigin{probe: pass()},
		connector: &fakeConnector{status: core.ConnectorStatusRunning, ok: true},
		provider: &fakeProvider{observed: &core.ObservedConnection{
			ConnectionID: "conn-1",
			ProviderID:   "mock",
			Tunnel:       &core.ObservedTunnel{ID: "tun-1", State: "healthy"},
		}},
		dns:      &fakeDNS{addrs: []string{"198.51.100.7"}},
		endpoint: &fakeEndpoint{probe: pass()},
	}
	f.engine = New(Deps{
		Origin:    f.origin,
		Connector: f.connector,
		Provider:  f.provider,
		DNS:       f.dns,
		Endpoint:  f.endpoint,
	})
	f.profile = &core.ConnectionProfile{
		ID:      "conn-1",
		Name:    "test",
		Desired: core.DesiredOpen,
		Source: core.SourceSpec{
			Kind:     core.SourceExisting,
			Existing: &core.ExistingServiceSpec{Address: "127.0.0.1:3000", Protocol: core.ProtocolHTTP},
		},
	}
	f.runtime = &core.ConnectionRuntime{
		ConnectionID: "conn-1",
		State:        core.RuntimeOpen,
		Connector:    core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
		Endpoint: core.EndpointRuntime{
			PublicAddress: "https://app.example.com",
			Hostname:      "app.example.com",
		},
	}
	return f
}

func diagnose(t *testing.T, f *fixture) []core.DiagnosticFinding {
	t.Helper()
	findings, err := f.engine.Diagnose(context.Background(), f.profile, f.runtime)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	return findings
}

// --------------- tests ---------------

func TestHealthyRouteProducesNoFindings(t *testing.T) {
	f := healthyFixture()
	findings := diagnose(t, f)
	if len(findings) != 0 {
		t.Fatalf("healthy route must produce no findings, got %+v", findings)
	}
}

func TestOriginFailureIsFirstCausalFinding(t *testing.T) {
	f := healthyFixture()
	f.origin.probe = fail("connection refused")
	// Downstream segments are also broken; they must not be reported.
	f.connector.status = core.ConnectorStatusCrashed
	f.dns.err = errors.New("NXDOMAIN")
	f.endpoint.probe = fail("timeout")

	findings := diagnose(t, f)
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 finding (first causal failure), got %d: %+v", len(findings), findings)
	}
	if findings[0].Segment != core.SegmentLocalService {
		t.Errorf("expected local_service segment, got %s", findings[0].Segment)
	}
	if len(findings[0].Evidence) == 0 {
		t.Errorf("finding must carry probe evidence")
	}
	// Causal order: downstream segments must not even be probed.
	if f.connector.calls != 0 || f.provider.calls != 0 || f.dns.calls != 0 || f.endpoint.calls != 0 {
		t.Errorf("downstream probes ran after origin failure: connector=%d provider=%d dns=%d endpoint=%d",
			f.connector.calls, f.provider.calls, f.dns.calls, f.endpoint.calls)
	}
}

func TestConnectorCrashProducesConnectorFinding(t *testing.T) {
	f := healthyFixture()
	f.connector.status = core.ConnectorStatusCrashed
	f.endpoint.probe = fail("timeout") // downstream noise

	findings := diagnose(t, f)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	got := findings[0]
	if got.Segment != core.SegmentConnector {
		t.Errorf("expected connector segment, got %s", got.Segment)
	}
	if got.Severity != core.SeverityError {
		t.Errorf("expected error severity, got %s", got.Severity)
	}
	repair := SmallestRepair(&got)
	if repair == nil || !repair.IsSafe {
		t.Errorf("connector crash must offer a safe restart repair, got %+v", repair)
	}
	if f.endpoint.calls != 0 {
		t.Errorf("endpoint must not be probed after connector failure")
	}
}

func TestConnectorStoppedWhileDesiredOpen(t *testing.T) {
	f := healthyFixture()
	f.connector.status = core.ConnectorStatusStopped

	findings := diagnose(t, f)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Segment != core.SegmentConnector {
		t.Errorf("expected connector segment, got %s", findings[0].Segment)
	}
	if findings[0].Severity != core.SeverityWarning {
		t.Errorf("expected warning severity, got %s", findings[0].Severity)
	}
}

func TestProviderEdgeMissingTunnel(t *testing.T) {
	f := healthyFixture()
	f.provider.observed = &core.ObservedConnection{
		ConnectionID: "conn-1",
		ProviderID:   "mock",
		Tunnel:       nil,
	}

	findings := diagnose(t, f)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Segment != core.SegmentProviderEdge {
		t.Errorf("expected provider_edge segment, got %s", findings[0].Segment)
	}
	if f.dns.calls != 0 || f.endpoint.calls != 0 {
		t.Errorf("downstream probes ran after provider edge failure")
	}
}

func TestProviderEdgeNoPublicAddressWhileOpen(t *testing.T) {
	f := healthyFixture()
	f.runtime.Endpoint = core.EndpointRuntime{}

	findings := diagnose(t, f)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Segment != core.SegmentProviderEdge {
		t.Errorf("expected provider_edge segment, got %s", findings[0].Segment)
	}
}

func TestDNSFailureProducesAddressFinding(t *testing.T) {
	f := healthyFixture()
	f.dns.addrs = nil
	f.dns.err = errors.New("NXDOMAIN")
	f.endpoint.probe = fail("timeout") // downstream noise

	findings := diagnose(t, f)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Segment != core.SegmentAddress {
		t.Errorf("expected address segment, got %s", findings[0].Segment)
	}
	if f.endpoint.calls != 0 {
		t.Errorf("endpoint must not be probed after DNS failure")
	}
}

func TestEndpointFailureIsLastSegment(t *testing.T) {
	f := healthyFixture()
	f.endpoint.probe = fail("HTTP 530")

	findings := diagnose(t, f)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Segment != core.SegmentEndpoint {
		t.Errorf("expected endpoint segment, got %s", findings[0].Segment)
	}
}

func TestClosedConnectionIsNotDiagnosed(t *testing.T) {
	f := healthyFixture()
	f.profile.Desired = core.DesiredClosed
	f.runtime.State = core.RuntimeClosed
	f.connector.status = core.ConnectorStatusStopped
	f.origin.probe = fail("connection refused")

	findings := diagnose(t, f)
	if len(findings) != 0 {
		t.Fatalf("closed connection must produce no findings, got %+v", findings)
	}
	if f.origin.calls != 0 {
		t.Errorf("closed connection must not be probed")
	}
}

func TestNilRuntimeErrors(t *testing.T) {
	f := healthyFixture()
	if _, err := f.engine.Diagnose(context.Background(), f.profile, nil); err == nil {
		t.Fatalf("expected error for nil runtime")
	}
}

func TestNilDependenciesAreSkipped(t *testing.T) {
	f := healthyFixture()
	f.engine = New(Deps{}) // no probers at all

	findings := diagnose(t, f)
	// Only the provider-edge check has enough runtime-local information
	// to run without dependencies, and the endpoint is set, so it passes.
	if len(findings) != 0 {
		t.Fatalf("nil deps must not fabricate failures, got %+v", findings)
	}
}
