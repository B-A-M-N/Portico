package provider

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// fakeTelemetryProvider is a provider that reports telemetry.
type fakeTelemetryProvider struct {
	sample TelemetrySample
	err    error
}

func (f *fakeTelemetryProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "fake", Name: "fake", DisplayName: "Fake Provider"}
}

func (f *fakeTelemetryProvider) Capabilities(ctx context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		Telemetry: core.TelemetryCapability{Supported: true, Stability: core.StabilityExperimental},
	}, nil
}

func (f *fakeTelemetryProvider) Authenticate(ctx context.Context, req core.AuthRequest) error {
	return nil
}

func (f *fakeTelemetryProvider) Plan(ctx context.Context, conn core.DesiredConnection) (*core.OperationPlan, error) {
	return nil, nil
}

func (f *fakeTelemetryProvider) ExecuteStep(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	return core.StepResult{}, nil
}

func (f *fakeTelemetryProvider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	return nil, nil
}

func (f *fakeTelemetryProvider) Telemetry(ctx context.Context, id core.ConnectionID) (TelemetrySample, error) {
	return f.sample, f.err
}

// TestTelemetryCapability verifies that TelemetryCapability correctly identifies
// providers that implement the TelemetryProvider interface.
func TestTelemetryCapability(t *testing.T) {
	t.Run("provider implements TelemetryProvider", func(t *testing.T) {
		p := &fakeTelemetryProvider{
			sample: TelemetrySample{ConnectionCount: 5, RequestCount: 100},
		}
		ok, tp := TelemetryCapability(p)
		if !ok {
			t.Fatal("expected TelemetryCapability to return true")
		}
		if tp == nil {
			t.Fatal("expected TelemetryProvider to be non-nil")
		}
		sample, err := tp.Telemetry(context.Background(), "conn-1")
		if err != nil {
			t.Fatalf("Telemetry: %v", err)
		}
		if sample.ConnectionCount != 5 {
			t.Fatalf("ConnectionCount = %d, want 5", sample.ConnectionCount)
		}
	})

	t.Run("provider does not implement TelemetryProvider", func(t *testing.T) {
		// core.Provider interface does not include Telemetry, so a minimal
		// implementation won't satisfy TelemetryProvider.
		p := &fakeNoTelemetryProvider{}
		ok, tp := TelemetryCapability(p)
		if ok {
			t.Fatal("expected TelemetryCapability to return false for non-telemetry provider")
		}
		if tp != nil {
			t.Fatal("expected TelemetryProvider to be nil")
		}
	})
}

// fakeNoTelemetryProvider is a provider that does NOT implement TelemetryProvider.
type fakeNoTelemetryProvider struct{}

func (f *fakeNoTelemetryProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "no-telemetry", Name: "no-telemetry"}
}

func (f *fakeNoTelemetryProvider) Capabilities(ctx context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		Telemetry: core.TelemetryCapability{Supported: false},
	}, nil
}

func (f *fakeNoTelemetryProvider) Authenticate(ctx context.Context, req core.AuthRequest) error {
	return nil
}

func (f *fakeNoTelemetryProvider) Plan(ctx context.Context, conn core.DesiredConnection) (*core.OperationPlan, error) {
	return nil, nil
}

func (f *fakeNoTelemetryProvider) ExecuteStep(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	return core.StepResult{}, nil
}

func (f *fakeNoTelemetryProvider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	return nil, nil
}
