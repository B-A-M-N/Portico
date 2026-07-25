package diagnostics

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/paoloanzn/portico/internal/core"
)

// defaultDiagTimeout bounds a single diagnostic probe.
const defaultDiagTimeout = 3 * time.Second

// HTTPOriginProber probes a local origin over plain HTTP.
type HTTPOriginProber struct {
	Timeout time.Duration
}

// NewHTTPOriginProber creates an origin prober with the given timeout.
func NewHTTPOriginProber(timeout time.Duration) *HTTPOriginProber {
	return &HTTPOriginProber{Timeout: timeout}
}

// ProbeOrigin issues a bounded GET against the local origin address.
// Any HTTP response counts as alive; only transport failures fail.
func (p *HTTPOriginProber) ProbeOrigin(ctx context.Context, address string) SegmentProbe {
	url := address
	if !strings.Contains(url, "://") {
		url = "http://" + url
	}
	// Any HTTP response means the origin is alive; the status code is
	// irrelevant for liveness.
	_, evidence, err := boundedGet(ctx, url, p.Timeout, "origin")
	if err != nil {
		return SegmentProbe{Status: core.ProbeFail, Evidence: evidence}
	}
	return SegmentProbe{Status: core.ProbePass, Evidence: evidence}
}

// NetDNSProber resolves hostnames with the standard resolver.
type NetDNSProber struct {
	Resolver *net.Resolver
	Timeout  time.Duration
}

// NewNetDNSProber creates a DNS prober using the default resolver.
func NewNetDNSProber() *NetDNSProber {
	return &NetDNSProber{}
}

// ResolveHost looks up the host and returns its addresses.
func (p *NetDNSProber) ResolveHost(ctx context.Context, host string) ([]string, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultDiagTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resolver := p.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return resolver.LookupHost(ctx, host)
}

// HTTPSEndpointProber probes the public endpoint over HTTPS.
type HTTPSEndpointProber struct {
	Timeout time.Duration
}

// NewHTTPSEndpointProber creates an endpoint prober with the given timeout.
func NewHTTPSEndpointProber(timeout time.Duration) *HTTPSEndpointProber {
	return &HTTPSEndpointProber{Timeout: timeout}
}

// ProbeEndpoint issues a bounded GET against the public address.
// Transport failures and 5xx responses fail the segment; anything
// else means the endpoint answered.
func (p *HTTPSEndpointProber) ProbeEndpoint(ctx context.Context, publicAddress string) SegmentProbe {
	url := publicAddress
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}
	status, evidence, err := boundedGet(ctx, url, p.Timeout, "endpoint")
	if err != nil {
		return SegmentProbe{Status: core.ProbeFail, Evidence: evidence}
	}
	if status >= 500 {
		return SegmentProbe{Status: core.ProbeFail, Evidence: evidence}
	}
	return SegmentProbe{Status: core.ProbePass, Evidence: evidence}
}

// boundedGet performs a GET with a timeout, discards at most 16 KiB of
// body, and returns the status plus probe evidence. Credentials are
// never sent.
func boundedGet(ctx context.Context, url string, timeout time.Duration, source string) (int, []core.Evidence, error) {
	if timeout <= 0 {
		timeout = defaultDiagTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, []core.Evidence{{
			Type: "http_probe", Source: source,
			Message: fmt.Sprintf("invalid probe URL %s: %v", url, err),
		}}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, []core.Evidence{{
			Type: "http_probe", Source: source,
			Message: fmt.Sprintf("GET %s failed: %v", url, err),
		}}, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 16<<10)

	return resp.StatusCode, []core.Evidence{{
		Type: "http_probe", Source: source,
		Message: fmt.Sprintf("GET %s -> HTTP %d", url, resp.StatusCode),
	}}, nil
}
