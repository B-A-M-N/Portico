package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

const (
	// These limits apply only after a connector and, for named tunnels, a DNS
	// record have been created. They keep propagation from making an apply hang
	// indefinitely while allowing the common DNS/refused race to settle.
	endpointVerificationAttempts = 5
	endpointVerificationDelay    = 250 * time.Millisecond
	endpointVerificationBudget   = 5 * time.Second
	dnsVerificationAttempts      = 5
	dnsVerificationDelay         = 250 * time.Millisecond
	dnsVerificationBudget        = 5 * time.Second
)

// probeClock is deliberately small so retry tests can advance time without
// sleeping. Production uses wallProbeClock, which always observes cancellation.
type probeClock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type wallProbeClock struct{}

func (wallProbeClock) Now() time.Time { return time.Now() }

func (wallProbeClock) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type dnsResolver interface {
	LookupHost(context.Context, string) ([]string, error)
}

type endpointProbe struct {
	statusCode int
}

type endpointStatusError struct {
	target string
	status int
}

func (e *endpointStatusError) Error() string {
	return fmt.Sprintf("%s returned status %d", e.target, e.status)
}

// endpointVerification is carried in the plan rather than reconstructed from
// current profile state at execution time. This keeps a reviewed plan's health
// contract stable across a supervisor restart.
type endpointVerification struct {
	openAI              bool
	healthPath          string
	healthStatusProfile string
	healthTimeout       time.Duration
	healthDisabled      bool
	configurationErr    error
}

func endpointVerificationParameters(profile *core.ConnectionProfile, originURL string) map[string]string {
	params := map[string]string{
		"origin_url":   originURL,
		"profile_kind": profile.EffectiveProfileKind(),
	}

	if source := profile.GetSource(); source.Existing != nil {
		health := source.Existing.Health
		if health.Configured && !health.Enabled {
			params["health_disabled"] = "true"
		}
		if health.Path != "" {
			params["health_path"] = health.Path
		}
		if health.Timeout > 0 {
			params["health_timeout"] = health.Timeout.String()
		}
	}

	// HealthCheckSpec currently carries the path and timeout. A driver option is
	// the provider-local extension point for an explicit expected-status profile
	// without changing the shared connection model. The plan records it so the
	// same profile is used for both local and public verification.
	for _, key := range []string{
		"health_status_profile", "health_statuses", "health_status",
		"expected_statuses", "expected_status",
	} {
		if value := strings.TrimSpace(profile.Driver.Options[key]); value != "" {
			params["health_status_profile"] = value
			break
		}
	}
	return params
}

func endpointVerificationFromParameters(params map[string]string) endpointVerification {
	spec := endpointVerification{
		healthPath:          strings.TrimSpace(params["health_path"]),
		healthStatusProfile: strings.TrimSpace(params["health_status_profile"]),
	}
	spec.openAI = params["profile_kind"] == string(core.ProfileOpenAICompatible) ||
		params["application_profile"] == string(core.ProfileOpenAICompatible)
	if value := strings.TrimSpace(params["health_timeout"]); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			spec.configurationErr = fmt.Errorf("invalid health timeout %q", value)
		} else {
			spec.healthTimeout = timeout
		}
	}
	if err := validateEndpointVerificationConfiguration(spec); err != nil {
		spec.configurationErr = err
	}
	if value := strings.TrimSpace(params["health_disabled"]); value != "" {
		disabled, err := strconv.ParseBool(value)
		if err != nil {
			spec.configurationErr = fmt.Errorf("invalid health_disabled value %q", value)
		} else {
			spec.healthDisabled = disabled
		}
	}
	return spec
}

func (p *Provider) probeNow() time.Time {
	if p.clock != nil {
		return p.clock.Now()
	}
	return time.Now()
}

func (p *Provider) probeSleep(ctx context.Context, delay time.Duration) error {
	if p.clock != nil {
		return p.clock.Sleep(ctx, delay)
	}
	return (wallProbeClock{}).Sleep(ctx, delay)
}

func (p *Provider) lookupHost(ctx context.Context, hostname string) ([]string, error) {
	if p.resolver != nil {
		return p.resolver.LookupHost(ctx, hostname)
	}
	return net.DefaultResolver.LookupHost(ctx, hostname)
}

func (p *Provider) probeHTTP(ctx context.Context, target string, timeout time.Duration) (endpointProbe, error) {
	client := p.httpClient
	if client == nil {
		client = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
				},
			},
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return endpointProbe{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return endpointProbe{}, err
	}
	defer resp.Body.Close()
	// Drain a bounded response so the default transport can reuse the
	// connection, while a broken/proxy response cannot consume unbounded memory.
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return endpointProbe{}, fmt.Errorf("read %s response: %w", target, err)
	}
	return endpointProbe{statusCode: resp.StatusCode}, nil
}

func (p *Provider) verifyLocalOriginWithVerification(ctx context.Context, originURL string, spec endpointVerification) error {
	if err := validateEndpointVerificationConfiguration(spec); err != nil {
		return err
	}
	root, err := p.probeHTTP(ctx, originURL, 5*time.Second)
	if err != nil {
		return err
	}
	if root.statusCode >= 500 {
		return &endpointStatusError{target: "origin", status: root.statusCode}
	}
	if spec.healthDisabled {
		return nil
	}
	if spec.healthPath != "" {
		healthURL, err := joinHealthPath(originURL, spec.healthPath)
		if err != nil {
			return err
		}
		return p.verifyApplicationURL(ctx, healthURL, spec, "origin health endpoint")
	}
	if spec.openAI && root.statusCode == http.StatusNotFound {
		_, err := p.verifyOpenAIModelsURL(ctx, originURL, "origin OpenAI endpoint")
		return err
	}
	return validateApplicationStatus(root.statusCode, spec, "origin")
}

// verifyDNSResolution performs one DNS lookup. The post-create caller wraps it
// in a bounded retry because a newly-created record may not be visible to the
// resolver immediately.
func (p *Provider) verifyDNSResolution(ctx context.Context, hostname string) error {
	if strings.TrimSpace(hostname) == "" {
		return &dnsConfigurationError{message: "hostname is empty"}
	}
	addresses, err := p.lookupHost(ctx, hostname)
	if err != nil {
		return err
	}
	if len(addresses) == 0 {
		return fmt.Errorf("no addresses resolved for %s", hostname)
	}
	return nil
}

func (p *Provider) verifyDNSResolutionWithRetry(ctx context.Context, hostname string) error {
	retryCtx, cancel := context.WithTimeout(ctx, dnsVerificationBudget)
	defer cancel()
	return p.retryProbe(retryCtx, dnsVerificationAttempts, dnsVerificationDelay, dnsVerificationBudget,
		func() error { return p.verifyDNSResolution(retryCtx, hostname) }, isRetryableDNSProbeError)
}

func (p *Provider) verifyPublicEndpointWithVerification(ctx context.Context, publicURL string, spec endpointVerification) ([]string, error) {
	if err := validateEndpointVerificationConfiguration(spec); err != nil {
		return nil, err
	}
	var notes []string
	healthURL := ""
	var err error
	if spec.healthPath != "" && !spec.healthDisabled {
		healthURL, err = joinHealthPath(publicURL, spec.healthPath)
		if err != nil {
			return nil, err
		}
	}
	retryCtx, cancel := context.WithTimeout(ctx, endpointVerificationBudget)
	defer cancel()

	err = p.retryProbe(retryCtx, endpointVerificationAttempts, endpointVerificationDelay, endpointVerificationBudget,
		func() error {
			root, probeErr := p.probeHTTP(retryCtx, publicURL, 10*time.Second)
			if probeErr != nil {
				return probeErr
			}
			if root.statusCode >= 500 {
				return &endpointStatusError{target: "public endpoint", status: root.statusCode}
			}

			if healthURL != "" {
				healthCtx, cancel := contextWithProbeTimeout(retryCtx, spec.healthTimeout)
				health, healthErr := p.probeHTTP(healthCtx, healthURL, 10*time.Second)
				cancel()
				if healthErr != nil {
					return healthErr
				}
				if health.statusCode >= 500 {
					return &endpointStatusError{target: "public health endpoint", status: health.statusCode}
				}
				note, healthErr := validateExplicitHealthStatus(health.statusCode, spec.healthStatusProfile, "public health endpoint")
				notes = appendOrReplaceNote(notes, note)
				return healthErr
			}
			if spec.openAI && root.statusCode == http.StatusNotFound {
				note, modelsErr := p.verifyOpenAIModelsURL(retryCtx, publicURL, "public OpenAI endpoint")
				notes = appendOrReplaceNote(notes, note)
				return modelsErr
			}

			note, rootErr := validateApplicationStatusWithNote(root.statusCode, spec, "public endpoint")
			notes = appendOrReplaceNote(notes, note)
			return rootErr
		}, isRetryableEndpointProbeError)
	if err != nil {
		return nil, err
	}
	return notes, nil
}

// verifyOpenAIModelsURL handles the common OpenAI-compatible layout where the
// service has no route at its base URL but does expose /v1/models. A root 404
// proves only that the edge answered; this probe is what distinguishes that
// expected layout from an actually missing service.
func (p *Provider) verifyOpenAIModelsURL(ctx context.Context, baseURL, target string) (string, error) {
	modelsURL, err := joinHealthPath(baseURL, "/v1/models")
	if err != nil {
		return "", err
	}
	probeCtx, cancel := contextWithProbeTimeout(ctx, 10*time.Second)
	defer cancel()
	probe, err := p.probeHTTP(probeCtx, modelsURL, 10*time.Second)
	if err != nil {
		return "", err
	}
	if probe.statusCode >= 200 && probe.statusCode < 300 {
		return "transport reachable; OpenAI-compatible /v1/models responded successfully", nil
	}
	if probe.statusCode == http.StatusUnauthorized || probe.statusCode == http.StatusForbidden {
		return fmt.Sprintf("transport reachable; %s requires authentication (status %d), so application health is unknown", target, probe.statusCode), nil
	}
	return "", &endpointStatusError{target: target + " /v1/models", status: probe.statusCode}
}

func (p *Provider) verifyApplicationURL(ctx context.Context, target string, spec endpointVerification, label string) error {
	probeCtx, cancel := contextWithProbeTimeout(ctx, spec.healthTimeout)
	defer cancel()
	probe, err := p.probeHTTP(probeCtx, target, 10*time.Second)
	if err != nil {
		return err
	}
	if probe.statusCode >= 500 {
		return &endpointStatusError{target: label, status: probe.statusCode}
	}
	_, err = validateExplicitHealthStatus(probe.statusCode, spec.healthStatusProfile, label)
	return err
}

func validateEndpointVerificationConfiguration(spec endpointVerification) error {
	if spec.configurationErr != nil {
		return spec.configurationErr
	}
	if spec.healthStatusProfile == "" {
		return nil
	}
	_, err := compileStatusProfile(spec.healthStatusProfile)
	return err
}

func contextWithProbeTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func joinHealthPath(base, healthPath string) (string, error) {
	healthPath = strings.TrimSpace(healthPath)
	if healthPath == "" {
		return base, nil
	}
	if strings.Contains(healthPath, "://") {
		return "", fmt.Errorf("health path must be a path, not a URL: %q", healthPath)
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("endpoint %q does not parse as a URL", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(healthPath, "/")
	u.RawPath = ""
	return u.String(), nil
}

func validateApplicationStatus(status int, spec endpointVerification, target string) error {
	_, err := validateApplicationStatusWithNote(status, spec, target)
	return err
}

func validateApplicationStatusWithNote(status int, spec endpointVerification, target string) (string, error) {
	if spec.healthStatusProfile != "" {
		return validateExplicitHealthStatus(status, spec.healthStatusProfile, target)
	}
	return validateRootApplicationStatusWithNote(status, spec.openAI, target)
}

func validateRootApplicationStatusWithNote(status int, openAI bool, target string) (string, error) {
	if status >= 200 && status < 400 {
		return "", nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Sprintf("transport reachable; %s requires authentication (status %d), so application health is unknown", target, status), nil
	}
	if openAI && status == http.StatusNotFound {
		return "transport reachable; HTTP 404 at the root is valid for an OpenAI-compatible upstream; application health was not checked", nil
	}
	return "", &endpointStatusError{target: target, status: status}
}

func validateExplicitHealthStatus(status int, rawProfile, target string) (string, error) {
	matcher, err := compileStatusProfile(rawProfile)
	if err != nil {
		return "", err
	}
	if matcher(status) {
		return "", nil
	}
	if rawProfile == "" && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		return fmt.Sprintf("transport reachable; %s requires authentication (status %d), so application health is unknown", target, status), nil
	}
	return "", &endpointStatusError{target: target, status: status}
}

func compileStatusProfile(raw string) (func(int) bool, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return func(status int) bool { return status >= 200 && status < 400 }, nil
	}
	switch raw {
	case "any", "reachable":
		return func(status int) bool { return status >= 100 && status < 600 }, nil
	case "success", "2xx":
		return func(status int) bool { return status >= 200 && status < 300 }, nil
	case "2xx_3xx", "2xx-3xx", "success_or_redirect":
		return func(status int) bool { return status >= 200 && status < 400 }, nil
	case "auth":
		return func(status int) bool { return status == http.StatusUnauthorized || status == http.StatusForbidden }, nil
	}

	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
	if len(parts) == 0 {
		return nil, fmt.Errorf("health status profile is empty")
	}
	var ranges [][2]int
	for _, part := range parts {
		if len(part) == 3 && part[1:] == "xx" && part[0] >= '1' && part[0] <= '5' {
			base := int(part[0]-'0') * 100
			ranges = append(ranges, [2]int{base, base + 99})
			continue
		}
		if strings.Count(part, "-") == 1 {
			bounds := strings.SplitN(part, "-", 2)
			low, lowErr := strconv.Atoi(bounds[0])
			high, highErr := strconv.Atoi(bounds[1])
			if lowErr != nil || highErr != nil || low < 100 || high > 599 || low > high {
				return nil, fmt.Errorf("invalid health status range %q", part)
			}
			ranges = append(ranges, [2]int{low, high})
			continue
		}
		status, err := strconv.Atoi(part)
		if err != nil || status < 100 || status > 599 {
			return nil, fmt.Errorf("invalid health status %q", part)
		}
		ranges = append(ranges, [2]int{status, status})
	}
	return func(status int) bool {
		for _, allowed := range ranges {
			if status >= allowed[0] && status <= allowed[1] {
				return true
			}
		}
		return false
	}, nil
}

func appendOrReplaceNote(notes []string, note string) []string {
	if note == "" {
		return notes
	}
	if len(notes) == 0 {
		return []string{note}
	}
	notes[len(notes)-1] = note
	return notes
}

func (p *Provider) retryProbe(ctx context.Context, attempts int, delay, budget time.Duration,
	operation func() error, retryable func(error) bool) error {
	if attempts < 1 {
		attempts = 1
	}
	deadline := p.probeNow().Add(budget)
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = operation()
		if lastErr == nil {
			return nil
		}
		if !retryable(lastErr) || attempt == attempts-1 {
			return lastErr
		}

		wait := delay
		if budget > 0 {
			remaining := deadline.Sub(p.probeNow())
			if remaining <= 0 {
				return lastErr
			}
			if wait > remaining {
				wait = remaining
			}
		}
		if err := p.probeSleep(ctx, wait); err != nil {
			return err
		}
	}
	return lastErr
}

func isRetryableDNSProbeError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var configurationErr *dnsConfigurationError
	if errors.As(err, &configurationErr) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTemporary || dnsErr.IsTimeout || dnsErr.IsNotFound
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such host") ||
		strings.Contains(message, "temporary") ||
		strings.Contains(message, "timed out") ||
		strings.Contains(message, "connection refused") ||
		strings.Contains(message, "connection reset") ||
		strings.Contains(message, "server misbehaving") ||
		strings.Contains(message, "no addresses resolved")
}

func isRetryableEndpointProbeError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var statusErr *endpointStatusError
	if errors.As(err, &statusErr) {
		return statusErr.status >= 500 && statusErr.status < 600
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "connection refused") ||
		strings.Contains(message, "connection reset")
}
