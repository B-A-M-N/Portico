package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HealthState is the overall connection health.
type HealthState string

const (
	// HealthHealthy means all applicable checks pass.
	HealthHealthy HealthState = "healthy"
	// HealthDegraded means process/transport OK but service not.
	HealthDegraded HealthState = "degraded"
	// HealthUnhealthy means process or transport is down.
	HealthUnhealthy HealthState = "unhealthy"
	// HealthUnknown means health has not been checked yet.
	HealthUnknown HealthState = "unknown"
	// HealthClosed means the connection is intentionally closed.
	HealthClosed HealthState = "closed"
)

// CheckState is the result of a single health check.
type CheckState string

const (
	CheckPass          CheckState = "pass"
	CheckFail          CheckState = "fail"
	CheckUnknown       CheckState = "unknown"
	CheckNotApplicable CheckState = "not_applicable"
	CheckSkipped       CheckState = "skipped"
)

// HealthCheck is the result of a single health check.
type HealthCheck struct {
	// State is the check result.
	State CheckState `json:"state"`

	// Detail carries a human-readable explanation.
	Detail string `json:"detail,omitempty"`

	// LastChecked reports when the check was last run.
	LastChecked time.Time `json:"last_checked,omitempty"`
}

// HealthReport carries the three-state health assessment for a connection.
//
// Portico distinguishes three health dimensions:
//  1. PROCESS  — Is the connector alive?
//  2. TRANSPORT — Is the externally reachable tunnel alive?
//  3. SERVICE  — Does the intended application actually work?
//
// Each check has a state (pass/fail/unknown/not_applicable/skipped).
// The overall health is computed from these states, taking into account
// the connection's desired state and kind.
type HealthReport struct {
	// ConnectionID identifies the connection being reported.
	ConnectionID ConnectionID `json:"connection_id"`

	// State is the overall health state.
	State HealthState `json:"state"`

	// Process reports whether the connector process is alive.
	Process HealthCheck `json:"process"`

	// Transport reports whether the externally reachable tunnel is alive.
	Transport HealthCheck `json:"transport"`

	// Service reports whether the intended application actually works.
	Service HealthCheck `json:"service"`

	// ComputedAt reports when the health report was computed.
	ComputedAt time.Time `json:"computed_at"`
}

// Compute determines the overall health state from the checks.
//
// Logic:
//  1. If all checks pass → healthy
//  2. If process or transport failed → unhealthy
//  3. If service failed → degraded
//  4. If no checks have run → unknown
//
// Closed connections with no process/transport are "closed" not "unhealthy".
func (r *HealthReport) Compute(desired DesiredConnectionState) HealthState {
	// Check if all checks are not applicable (e.g., closed connection).
	allNotApplicable := r.Process.State == CheckNotApplicable &&
		r.Transport.State == CheckNotApplicable &&
		r.Service.State == CheckNotApplicable

	if allNotApplicable {
		if desired == DesiredClosed {
			return HealthClosed
		}
		return HealthUnknown
	}

	// If any check failed → unhealthy.
	if r.Process.State == CheckFail || r.Transport.State == CheckFail {
		return HealthUnhealthy
	}

	// If service failed → degraded.
	if r.Service.State == CheckFail {
		return HealthDegraded
	}

	// If all applicable checks pass → healthy.
	if (r.Process.State == CheckPass || r.Process.State == CheckNotApplicable) &&
		(r.Transport.State == CheckPass || r.Transport.State == CheckNotApplicable) &&
		(r.Service.State == CheckPass || r.Service.State == CheckNotApplicable) {
		return HealthHealthy
	}

	return HealthUnknown
}

// Refresh recomputes the overall state and updates ComputedAt.
func (r *HealthReport) Refresh(desired DesiredConnectionState) {
	r.State = r.Compute(desired)
	r.ComputedAt = time.Now().UTC()
}

// DefaultServiceCheck probes the endpoint over HTTP. A response proves that
// the route exists even when the application requires authentication, so 2xx,
// 3xx, 401 and 403 are reachable. Other 4xx/5xx responses are failures.
func DefaultServiceCheck(ctx context.Context, endpoint string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return fmt.Errorf("service returned status %d", resp.StatusCode)
	}
	return nil
}

// MCPServiceCheck performs a bounded MCP protocol probe. SSE transports expose
// their stream through GET; HTTP and streamable HTTP accept an initialize
// request. Authentication responses still prove that the endpoint is present.
func MCPServiceCheck(ctx context.Context, endpoint string, transport MCPTransport) error {
	endpoint = strings.TrimRight(endpoint, "/")
	if endpoint == "" {
		return fmt.Errorf("MCP endpoint is empty")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var req *http.Request
	var err error
	if transport == MCPTransportSSE {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err == nil {
			req.Header.Set("Accept", "text/event-stream")
		}
	} else {
		body, marshalErr := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]string{"name": "portico", "version": "0.1"},
			},
		})
		if marshalErr != nil {
			return marshalErr
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 401/403 establish TRANSPORT REACHABILITY with authentication in the
	// way, not application health. The error wording says exactly that so
	// diagnostics do not read an auth wall as a working server.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("MCP endpoint is reachable but requires authentication (status %d); functional verification was not possible", resp.StatusCode)
	}
	if resp.StatusCode >= 400 || resp.StatusCode == http.StatusNoContent {
		return fmt.Errorf("MCP endpoint returned status %d", resp.StatusCode)
	}

	// Protocol VALIDITY, not just reachability (audit R5): a 200 carrying
	// HTML or arbitrary JSON does not prove the server understood the MCP
	// initialize request. Parse enough of the response to require a JSON-RPC
	// answer with our id and either a result or a protocol-valid error.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read MCP response: %w", err)
	}
	if transport == MCPTransportSSE {
		// An SSE answer is valid when it opens an event stream; the body may
		// be empty if the server holds the stream open.
		ct := resp.Header.Get("Content-Type")
		if strings.Contains(ct, "text/event-stream") {
			return nil
		}
		return fmt.Errorf("SSE endpoint answered Content-Type %q instead of text/event-stream", ct)
	}
	var rpc struct {
		Jsonrpc string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return fmt.Errorf("MCP response is not JSON-RPC: %w", err)
	}
	if rpc.Jsonrpc != "2.0" && rpc.Jsonrpc != "" {
		return fmt.Errorf("MCP response carries jsonrpc %q", rpc.Jsonrpc)
	}
	if rpc.Error != nil {
		// A protocol-valid JSON-RPC error still proves the server speaks MCP;
		// it refused initialize for its own protocol reasons.
		return nil
	}
	if rpc.Result == nil {
		return fmt.Errorf("MCP response carries neither result nor error")
	}
	return nil
}

// OpenAIServiceCheck performs an OpenAI-compatible service check.
//
// Health is more than HTTP reachability (audit R5): the URL is joined
// properly (a trailing slash on the endpoint must not produce //v1/models,
// and a base path must be preserved), and the response must have the shape
// of a models list — {"data": [...]} — rather than any arbitrary 200. The
// deeper compatibility probe remains the setup-time authority; this is its
// bounded runtime subset.
func OpenAIServiceCheck(ctx context.Context, endpoint string) error {
	base, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || base.Host == "" {
		return fmt.Errorf("OpenAI-compatible endpoint %q does not parse as a URL", endpoint)
	}
	// Join onto the endpoint's existing path (a server mounted under a base
	// path such as /api must keep it), and normalize any trailing slash away.
	modelsURL := *base
	modelsURL.Path = strings.TrimRight(base.Path, "/") + "/v1/models"
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL.String(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("/v1/models is reachable but requires authentication (status %d); functional verification was not possible", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("/v1/models returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read /v1/models: %w", err)
	}
	var models struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &models); err != nil {
		return fmt.Errorf("response is not an OpenAI-compatible models list: %w", err)
	}
	return nil
}
