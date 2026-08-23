package provider

import (
	"context"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// TelemetrySample is a provider-neutral snapshot of traffic metrics.
// Only include metrics with provider-neutral semantics — do not pretend
// every provider has HTTP request latency.
//
// Which metrics a provider supplies is part of the sample, because a counter
// that is absent and a counter that is genuinely zero are different facts and a
// zero cannot express both. ngrok's agent reports connection and request counts
// and no byte totals: presenting "0 bytes in" would be a measurement Portico
// never made.
type TelemetrySample struct {
	ConnectionCount int64     `json:"connection_count"`
	RequestCount    int64     `json:"request_count"`
	BytesIn         int64     `json:"bytes_in"`
	BytesOut        int64     `json:"bytes_out"`
	ProviderErrors  int64     `json:"provider_errors"`
	SampledAt       time.Time `json:"sampled_at"`

	// HasCounts, HasBytes and HasErrors report which of the above the provider
	// actually measured.
	HasCounts bool `json:"has_counts,omitempty"`
	HasBytes  bool `json:"has_bytes,omitempty"`
	HasErrors bool `json:"has_errors,omitempty"`
}

// TelemetryProvider is an optional interface for providers that can report
// traffic telemetry. It is not part of the core Provider interface because
// most providers cannot report it — adding it as a mandatory method would
// force every adapter to return empty or fake data.
type TelemetryProvider interface {
	// Telemetry returns the traffic snapshot for a connection.
	// It returns core.ErrUnsupported if the provider has no telemetry.
	Telemetry(ctx context.Context, id core.ConnectionID) (TelemetrySample, error)
}

// TelemetryCapability reports whether a provider supports telemetry.
// It is a convenience wrapper so callers don't need a type assertion.
func TelemetryCapability(p core.Provider) (bool, TelemetryProvider) {
	if tp, ok := p.(TelemetryProvider); ok {
		return true, tp
	}
	return false, nil
}
