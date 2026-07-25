package discovery

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultProbeTimeout bounds a single HTTP probe.
const DefaultProbeTimeout = 1 * time.Second

// maxProbeBody is the maximum number of response body bytes read per probe.
const maxProbeBody = 16 << 10 // 16 KiB

// ProbeOutcome is the evidence-based result of probing a single TCP port.
type ProbeOutcome struct {
	// Scheme is "http" or "https" when an HTTP response was observed,
	// empty when the listener did not speak HTTP.
	Scheme   string
	Evidence []string
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

// Probe issues GET http://127.0.0.1:<port>/ with a bounded timeout.
func (p *HTTPProber) Probe(ctx context.Context, port int) ProbeOutcome {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}

	// Plain HTTP first.
	if out, ok := p.get(ctx, fmt.Sprintf("http://127.0.0.1:%d/", port), timeout, nil); ok {
		out.Scheme = "http"
		return out
	}

	// Retry over TLS. Verification is skipped only for local
	// classification; the evidence records that it was not verified.
	tlsCfg := &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- loopback classification only
	if out, ok := p.get(ctx, fmt.Sprintf("https://127.0.0.1:%d/", port), timeout, tlsCfg); ok {
		out.Scheme = "https"
		out.Evidence = append(out.Evidence, "TLS certificate not verified (local classification only)")
		return out
	}

	return ProbeOutcome{Evidence: []string{"no HTTP response on GET /"}}
}

// get performs a single bounded GET and reports whether an HTTP
// response was received.
func (p *HTTPProber) get(ctx context.Context, url string, timeout time.Duration, tlsCfg *tls.Config) (ProbeOutcome, bool) {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ProbeOutcome{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return ProbeOutcome{}, false
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, maxProbeBody)

	evidence := []string{fmt.Sprintf("GET / -> HTTP %d", resp.StatusCode)}
	if server := resp.Header.Get("Server"); server != "" {
		evidence = append(evidence, "Server: "+server)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		evidence = append(evidence, "Content-Type: "+ct)
	}
	return ProbeOutcome{Evidence: evidence}, true
}
