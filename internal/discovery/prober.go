package discovery

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// DefaultProbeTimeout bounds a single HTTP probe.
const DefaultProbeTimeout = 250 * time.Millisecond

// maxProbeBody is the maximum number of response body bytes read per probe.
const maxProbeBody = 16 << 10 // 16 KiB

// ProbeOutcome is the evidence-based result of probing a single TCP port.
type ProbeOutcome struct {
	// Scheme is "http" or "https" when an HTTP response was observed,
	// empty when the listener did not speak HTTP.
	Scheme     string
	Evidence   []string
	StatusCode int
}

// Prober probes a local TCP port for an HTTP(S) service.
type Prober interface {
	Probe(ctx context.Context, port int) ProbeOutcome
}

// HTTPProber probes loopback TCP ports with a bounded GET request.
// It first attempts plain HTTP; if that fails it retries over TLS
// (without certificate verification, for local classification only).
type HTTPProber struct {
	Timeout time.Duration
}

// NewHTTPProber creates a prober with the given per-probe timeout.
// A non-positive timeout falls back to DefaultProbeTimeout.
func NewHTTPProber(timeout time.Duration) *HTTPProber {
	return &HTTPProber{Timeout: timeout}
}

// Probe issues a bounded HEAD-first loopback probe, falling back to GET only
// when the server does not implement HEAD.
func (p *HTTPProber) Probe(ctx context.Context, port int) ProbeOutcome {
	return p.ProbeAddress(ctx, "127.0.0.1", port)
}

// ProbeAddress probes a specific local address. It is used for IPv6-only
// listeners; the Prober interface retains Probe for existing callers.
func (p *HTTPProber) ProbeAddress(ctx context.Context, host string, port int) ProbeOutcome {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}

	address := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	// Plain HTTP first.
	if out, ok := p.request(ctx, http.MethodHead, fmt.Sprintf("http://%s/", address), timeout, nil); ok {
		if out.StatusCode == http.StatusMethodNotAllowed || out.StatusCode == http.StatusNotImplemented {
			out, ok = p.request(ctx, http.MethodGet, fmt.Sprintf("http://%s/", address), timeout, nil)
		}
		if ok {
			out.Scheme = "http"
			return out
		}
	}

	// Retry over TLS. Verification is skipped only for local
	// classification; the evidence records that it was not verified.
	tlsCfg := &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- loopback classification only
	if out, ok := p.request(ctx, http.MethodHead, fmt.Sprintf("https://%s/", address), timeout, tlsCfg); ok {
		if out.StatusCode == http.StatusMethodNotAllowed || out.StatusCode == http.StatusNotImplemented {
			out, ok = p.request(ctx, http.MethodGet, fmt.Sprintf("https://%s/", address), timeout, tlsCfg)
		}
		if ok {
			out.Scheme = "https"
			out.Evidence = append(out.Evidence, "TLS certificate not verified (local classification only)")
			return out
		}
	}

	return ProbeOutcome{Evidence: []string{"no HTTP response on GET /"}}
}

// request performs a single bounded HTTP request and reports whether an HTTP
// response was received.
func (p *HTTPProber) request(ctx context.Context, method, url string, timeout time.Duration, tlsCfg *tls.Config) (ProbeOutcome, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   tlsCfg,
	}
	client := &http.Client{
		Transport: transport,
		// At most one redirect (SPEC 14.3); do not follow further.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 1 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	defer transport.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return ProbeOutcome{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return ProbeOutcome{}, false
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, maxProbeBody)

	evidence := []string{fmt.Sprintf("%s / -> HTTP %d", method, resp.StatusCode)}
	if server := resp.Header.Get("Server"); server != "" {
		evidence = append(evidence, "Server: "+server)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		evidence = append(evidence, "Content-Type: "+ct)
	}
	return ProbeOutcome{Evidence: evidence, StatusCode: resp.StatusCode}, true
}
