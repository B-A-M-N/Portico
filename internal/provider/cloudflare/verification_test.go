package cloudflare

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

type verificationTestClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *verificationTestClock) Now() time.Time { return c.now }

func (c *verificationTestClock) Sleep(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.sleeps = append(c.sleeps, delay)
	c.now = c.now.Add(delay)
	return nil
}

type verificationTestResolver struct {
	results []struct {
		addresses []string
		err       error
	}
	calls int
}

func (r *verificationTestResolver) LookupHost(context.Context, string) ([]string, error) {
	r.calls++
	if len(r.results) == 0 {
		return []string{"127.0.0.1"}, nil
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result.addresses, result.err
}

type alwaysRunningConnector struct{}

func (alwaysRunningConnector) Start(_ context.Context, cfg core.ProcessConfig) (core.ConnectorHandle, error) {
	return core.ConnectorHandle{ConnectionID: cfg.ConnectionID, PID: 4242}, nil
}

func (alwaysRunningConnector) Stop(core.ConnectionID, time.Duration) error { return nil }

func (alwaysRunningConnector) Observe(id core.ConnectionID) (core.ConnectorHandle, bool) {
	return core.ConnectorHandle{ConnectionID: id, PID: 4242}, true
}

func newVerificationTestProvider(publicURL string, clock *verificationTestClock) *Provider {
	return &Provider{
		clock:         clock,
		connectorProc: alwaysRunningConnector{},
		connections:   map[core.ConnectionID]*cfConnection{"conn": {publicURL: publicURL, connectorPID: 4242, connectorStarted: true}},
	}
}

func executeEndpointVerification(t *testing.T, p *Provider, params map[string]string) core.StepResult {
	t.Helper()
	result, err := p.ExecuteStep(context.Background(), "conn", core.PlanStep{
		ID:   "cf-verify-endpoint",
		Kind: core.StepVerifyEndpoint,
		Technical: core.TechnicalOperation{
			Parameters: params,
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStep returned transport error: %v", err)
	}
	return result
}

func TestEndpointVerificationAcceptsOpenAIRoot404(t *testing.T) {
	requests := 0
	paths := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"test"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	clock := &verificationTestClock{now: time.Unix(0, 0)}
	result := executeEndpointVerification(t, newVerificationTestProvider(server.URL, clock), map[string]string{
		"profile_kind": string(core.ProfileOpenAICompatible),
	})
	if !result.Succeeded {
		t.Fatalf("OpenAI-compatible root 404 failed: %v", result.Error)
	}
	if requests != 2 || strings.Join(paths, ",") != "/,/v1/models" {
		t.Fatalf("verification requests = %d/%q, want root then /v1/models", requests, paths)
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("root 404 caused retries: %v", clock.sleeps)
	}
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], "OpenAI-compatible") {
		t.Fatalf("notes = %#v, want OpenAI transport note", result.Notes)
	}
}

func TestEndpointVerificationHonorsExplicitHealthPathAndStatusProfile(t *testing.T) {
	paths := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusNoContent)
		default:
			// The root is only the transport probe. The configured health path
			// owns application status for this verification.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	profile := cfTestProfile(core.ExposureTemporary, "", core.ProtectionSpec{Kind: core.ProtectionNone})
	profile.Spec.ServiceExposure.Source.Existing.Health = core.HealthCheckSpec{
		Enabled:    true,
		Configured: true,
		Path:       "/healthz",
		Timeout:    2 * time.Second,
	}
	profile.Driver.Options = map[string]string{"health_status_profile": "204"}
	params := endpointVerificationParameters(profile, "")
	if params["health_path"] != "/healthz" || params["health_status_profile"] != "204" {
		t.Fatalf("verification parameters = %#v, missing explicit health contract", params)
	}

	clock := &verificationTestClock{now: time.Unix(0, 0)}
	p := newVerificationTestProvider(server.URL, clock)
	result := executeEndpointVerification(t, p, params)
	if !result.Succeeded {
		t.Fatalf("explicit health verification failed: %v", result.Error)
	}
	if got := strings.Join(paths, ","); got != "/,/healthz" {
		t.Fatalf("request paths = %q, want root then configured health path", got)
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("successful explicit health check caused retries: %v", clock.sleeps)
	}
}

func TestEndpointVerificationRetriesTransientPublic5xx(t *testing.T) {
	statuses := []int{http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusOK}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		status := statuses[requests]
		requests++
		w.WriteHeader(status)
	}))
	defer server.Close()

	clock := &verificationTestClock{now: time.Unix(0, 0)}
	result := executeEndpointVerification(t, newVerificationTestProvider(server.URL, clock), nil)
	if !result.Succeeded {
		t.Fatalf("transient 5xx verification failed: %v", result.Error)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
	if len(clock.sleeps) != 2 || clock.sleeps[0] != endpointVerificationDelay || clock.sleeps[1] != endpointVerificationDelay {
		t.Fatalf("retry sleeps = %v, want two %v sleeps", clock.sleeps, endpointVerificationDelay)
	}
}

type refusedThenOKTransport struct {
	calls int
}

func (t *refusedThenOKTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	if t.calls == 1 {
		return nil, &url.Error{Op: http.MethodGet, URL: req.URL.String(), Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: syscall.ECONNREFUSED,
		}}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestEndpointVerificationRetriesConnectionRefused(t *testing.T) {
	transport := &refusedThenOKTransport{}
	clock := &verificationTestClock{now: time.Unix(0, 0)}
	p := newVerificationTestProvider("http://endpoint.test", clock)
	p.httpClient = &http.Client{Transport: transport}

	result := executeEndpointVerification(t, p, nil)
	if !result.Succeeded {
		t.Fatalf("connection-refused verification failed: %v", result.Error)
	}
	if transport.calls != 2 || len(clock.sleeps) != 1 || clock.sleeps[0] != endpointVerificationDelay {
		t.Fatalf("transport calls/sleeps = %d/%v, want 2/%v", transport.calls, clock.sleeps, endpointVerificationDelay)
	}
}

func TestEndpointVerificationRetriesDNSPropagation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resolver := &verificationTestResolver{results: []struct {
		addresses []string
		err       error
	}{
		{err: errors.New("temporary failure in name resolution")},
		{err: errors.New("no such host")},
		{addresses: []string{"192.0.2.10"}},
	}}
	clock := &verificationTestClock{now: time.Unix(0, 0)}
	p := newVerificationTestProvider(server.URL, clock)
	p.zoneID = "zone-1"
	p.resolver = resolver
	p.connections["conn"].hostname = "new.example.test"

	result := executeEndpointVerification(t, p, nil)
	if !result.Succeeded {
		t.Fatalf("DNS propagation verification failed: %v", result.Error)
	}
	if resolver.calls != 3 {
		t.Fatalf("DNS lookups = %d, want 3", resolver.calls)
	}
	if len(clock.sleeps) != 2 || clock.sleeps[0] != dnsVerificationDelay || clock.sleeps[1] != dnsVerificationDelay {
		t.Fatalf("DNS retry sleeps = %v, want two %v sleeps", clock.sleeps, dnsVerificationDelay)
	}
}

func TestEndpointVerificationDoesNotRetryAuthOrConfigurationErrors(t *testing.T) {
	t.Run("auth status", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()

		clock := &verificationTestClock{now: time.Unix(0, 0)}
		result := executeEndpointVerification(t, newVerificationTestProvider(server.URL, clock), nil)
		if !result.Succeeded {
			t.Fatalf("auth-gated endpoint failed: %v", result.Error)
		}
		if requests != 1 || len(clock.sleeps) != 0 {
			t.Fatalf("auth probe requests/sleeps = %d/%v, want 1/none", requests, clock.sleeps)
		}
	})

	t.Run("invalid status profile", func(t *testing.T) {
		clock := &verificationTestClock{now: time.Unix(0, 0)}
		p := newVerificationTestProvider("http://127.0.0.1:1", clock)
		result := executeEndpointVerification(t, p, map[string]string{"health_status_profile": "600"})
		if result.Succeeded || result.Error == nil {
			t.Fatalf("invalid status profile result = %#v, want failure", result)
		}
		if len(clock.sleeps) != 0 {
			t.Fatalf("configuration error caused retries: %v", clock.sleeps)
		}
	})

	t.Run("empty DNS hostname", func(t *testing.T) {
		clock := &verificationTestClock{now: time.Unix(0, 0)}
		p := newVerificationTestProvider("", clock)
		if err := p.verifyDNSResolutionWithRetry(context.Background(), " "); err == nil {
			t.Fatal("empty hostname unexpectedly resolved")
		}
		if len(clock.sleeps) != 0 {
			t.Fatalf("DNS configuration error caused retries: %v", clock.sleeps)
		}
	})
}
