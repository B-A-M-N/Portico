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
	// HealthHealthy means all three health checks pass.
	HealthHealthy HealthState = "healthy"
	// HealthDegraded means the process and transport are OK but the service is not.
	HealthDegraded HealthState = "degraded"
	// HealthUnhealthy means the process or transport is down.
	HealthUnhealthy HealthState = "unhealthy"
	// HealthUnknown means health has not been checked yet.
	HealthUnknown HealthState = "unknown"
)

// HealthCheck is the result of a single health check.
type HealthCheck struct {
	// OK reports whether the check passed.
	OK bool `json:"ok"`

	// Detail carries a human-readable explanation.
	Detail string `json:"detail,omitempty"`

	// LastChecked reports when the check was last run.
	LastChecked time.Time `json:"last_checked,omitempty"`
}

// HealthReport carries the three-state health assessment for a connection.
//
// Portico distinguishes three health states:
//  1. PROCESS  — Is the connector alive?
//  2. TRANSPORT — Is the externally reachable tunnel alive?
//  3. SERVICE  — Does the intended application actually work?
//
// This prevents a "CONNECTED" indicator when the origin service is down.
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

// Compute determines the overall health state from the three checks.
//
// Logic:
//  1. If process or transport is not OK → unhealthy
//  2. If service is not OK → degraded
//  3. If all three are OK → healthy
//  4. If no checks have been run → unknown
func (r *HealthReport) Compute() HealthState {
	if r.Process.LastChecked.IsZero() && r.Transport.LastChecked.IsZero() && r.Service.LastChecked.IsZero() {
		return HealthUnknown
	}

	if !r.Process.OK || !r.Transport.OK {
		return HealthUnhealthy
	}

	if !r.Service.OK {
		return HealthDegraded
	}

	return HealthHealthy
}

// Refresh recomputes the overall state and updates ComputedAt.
func (r *HealthReport) Refresh() {
	r.State = r.Compute()
	r.ComputedAt = time.Now().UTC()
}

// CheckServiceFunc probes a service endpoint to verify it works.
type CheckServiceFunc func(ctx context.Context, endpoint string) error

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
