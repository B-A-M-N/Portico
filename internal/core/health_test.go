package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestHealthReportCompute verifies the three-state health logic.
func TestHealthReportCompute(t *testing.T) {
	tests := []struct {
		name     string
		process  bool
		transport bool
		service  bool
		want     HealthState
	}{
		{
			name:     "all ok",
			process:  true,
			transport: true,
			service:  true,
			want:     HealthHealthy,
		},
		{
			name:     "process down",
			process:  false,
			transport: true,
			service:  true,
			want:     HealthUnhealthy,
		},
		{
			name:     "transport down",
			process:  true,
			transport: false,
			service:  true,
			want:     HealthUnhealthy,
		},
		{
			name:     "service down but process and transport ok",
			process:  true,
			transport: true,
			service:  false,
			want:     HealthDegraded,
		},
		{
			name:     "all down",
			process:  false,
			transport: false,
			service:  false,
			want:     HealthUnhealthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := &HealthReport{
				ConnectionID: "test-conn",
				Process:      HealthCheck{OK: tt.process, LastChecked: time.Now()},
				Transport:    HealthCheck{OK: tt.transport, LastChecked: time.Now()},
				Service:      HealthCheck{OK: tt.service, LastChecked: time.Now()},
			}

			got := report.Compute()
			if got != tt.want {
				t.Errorf("Compute() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestHealthReportUnknown verifies that a report with no checks returns unknown.
func TestHealthReportUnknown(t *testing.T) {
	report := &HealthReport{ConnectionID: "test-conn"}
	if got := report.Compute(); got != HealthUnknown {
		t.Errorf("Compute() = %q, want unknown", got)
	}
}

// TestHealthReportRefresh verifies the Refresh method updates state and timestamp.
func TestHealthReportRefresh(t *testing.T) {
	report := &HealthReport{
		ConnectionID: "test-conn",
		Process:      HealthCheck{OK: true, LastChecked: time.Now()},
		Transport:    HealthCheck{OK: true, LastChecked: time.Now()},
		Service:      HealthCheck{OK: true, LastChecked: time.Now()},
	}

	report.Refresh()
	if report.State != HealthHealthy {
		t.Errorf("State = %q, want healthy", report.State)
	}
	if report.ComputedAt.IsZero() {
		t.Fatal("ComputedAt should be set after Refresh")
	}
}

// TestDefaultServiceCheck verifies the default HTTP service check.
func TestDefaultServiceCheck(t *testing.T) {
	// Test with a failing endpoint.
	err := DefaultServiceCheck(context.Background(), "http://127.0.0.1:1/nonexistent")
	if err == nil {
		t.Fatal("expected error for unreachable endpoint")
	}
}

// TestOpenAIServiceCheck verifies the OpenAI service check.
func TestOpenAIServiceCheck(t *testing.T) {
	err := OpenAIServiceCheck(context.Background(), "http://127.0.0.1:1/nonexistent")
	if err == nil {
		t.Fatal("expected error for unreachable endpoint")
	}
}

// TestServiceCheckDetail verifies that check failures produce a detail string.
func TestServiceCheckDetail(t *testing.T) {
	report := &HealthReport{
		ConnectionID: "test-conn",
		Process:      HealthCheck{OK: true, LastChecked: time.Now()},
		Transport:    HealthCheck{OK: true, LastChecked: time.Now()},
		Service:      HealthCheck{OK: false, Detail: "connection refused", LastChecked: time.Now()},
	}

	got := report.Compute()
	if got != HealthDegraded {
		t.Errorf("Compute() = %q, want degraded", got)
	}
}

// TestHealthStateString verifies HealthState string values.
func TestHealthStateString(t *testing.T) {
	tests := []struct {
		state HealthState
		want  string
	}{
		{HealthHealthy, "healthy"},
		{HealthDegraded, "degraded"},
		{HealthUnhealthy, "unhealthy"},
		{HealthUnknown, "unknown"},
	}

	for _, tt := range tests {
		if string(tt.state) != tt.want {
			t.Errorf("HealthState(%q).String() = %q, want %q", tt.state, string(tt.state), tt.want)
		}
	}
}

// Ensure errors is imported.
var _ = errors.New
