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
		name      string
		process   CheckState
		transport CheckState
		service   CheckState
		desired   DesiredConnectionState
		want      HealthState
	}{
		{
			name:      "all ok, desired open",
			process:   CheckPass,
			transport: CheckPass,
			service:   CheckPass,
			desired:   DesiredOpen,
			want:      HealthHealthy,
		},
		{
			name:      "process down, desired open",
			process:   CheckFail,
			transport: CheckPass,
			service:   CheckPass,
			desired:   DesiredOpen,
			want:      HealthUnhealthy,
		},
		{
			name:      "transport down, desired open",
			process:   CheckPass,
			transport: CheckFail,
			service:   CheckPass,
			desired:   DesiredOpen,
			want:      HealthUnhealthy,
		},
		{
			name:      "service down but process and transport ok",
			process:   CheckPass,
			transport: CheckPass,
			service:   CheckFail,
			desired:   DesiredOpen,
			want:      HealthDegraded,
		},
		{
			name:      "all down, desired open",
			process:   CheckFail,
			transport: CheckFail,
			service:   CheckFail,
			desired:   DesiredOpen,
			want:      HealthUnhealthy,
		},
		{
			name:      "not applicable, desired closed",
			process:   CheckNotApplicable,
			transport: CheckNotApplicable,
			service:   CheckNotApplicable,
			desired:   DesiredClosed,
			want:      HealthClosed,
		},
		{
			name:      "not applicable, desired open",
			process:   CheckNotApplicable,
			transport: CheckNotApplicable,
			service:   CheckNotApplicable,
			desired:   DesiredOpen,
			want:      HealthUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := &HealthReport{
				ConnectionID: "test-conn",
				Process:      HealthCheck{State: tt.process, LastChecked: time.Now()},
				Transport:    HealthCheck{State: tt.transport, LastChecked: time.Now()},
				Service:      HealthCheck{State: tt.service, LastChecked: time.Now()},
			}

			got := report.Compute(tt.desired)
			if got != tt.want {
				t.Errorf("Compute() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestHealthReportUnknown verifies that a report with no checks returns unknown.
func TestHealthReportUnknown(t *testing.T) {
	report := &HealthReport{ConnectionID: "test-conn"}
	if got := report.Compute(DesiredOpen); got != HealthUnknown {
		t.Errorf("Compute() = %q, want unknown", got)
	}
}

// TestHealthReportRefresh verifies the Refresh method updates state and timestamp.
func TestHealthReportRefresh(t *testing.T) {
	report := &HealthReport{
		ConnectionID: "test-conn",
		Process:      HealthCheck{State: CheckPass, LastChecked: time.Now()},
		Transport:    HealthCheck{State: CheckPass, LastChecked: time.Now()},
		Service:      HealthCheck{State: CheckPass, LastChecked: time.Now()},
	}

	report.Refresh(DesiredOpen)
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

// TestCheckStateString verifies CheckState string values.
func TestCheckStateString(t *testing.T) {
	tests := []struct {
		state CheckState
		want  string
	}{
		{CheckPass, "pass"},
		{CheckFail, "fail"},
		{CheckUnknown, "unknown"},
		{CheckNotApplicable, "not_applicable"},
		{CheckSkipped, "skipped"},
	}

	for _, tt := range tests {
		if string(tt.state) != tt.want {
			t.Errorf("CheckState(%q).String() = %q, want %q", tt.state, string(tt.state), tt.want)
		}
	}
}

// Ensure errors is imported.
var _ = errors.New
