package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
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

// DefaultServiceCheck probes the endpoint over HTTP and treats status < 400 as healthy.
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
	if resp.StatusCode >= 400 {
		return fmt.Errorf("service returned status %d", resp.StatusCode)
	}
	return nil
}

// OpenAIServiceCheck performs an OpenAI-compatible service check.
// It probes /v1/models to verify the endpoint is working.
func OpenAIServiceCheck(ctx context.Context, endpoint string) error {
	url := endpoint + "/v1/models"
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("/v1/models returned status %d", resp.StatusCode)
	}
	return nil
}
