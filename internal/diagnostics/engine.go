package diagnostics

import (
	"context"
	"fmt"
	"time"

	"github.com/paoloanzn/portico/internal/core"
	"github.com/paoloanzn/portico/internal/provider/mock"
)

// Engine performs diagnostics on connections by probing route segments
// and producing findings attached to specific segments.
type Engine struct {
	mockProv *mock.Provider
}

// New creates a diagnostics engine.
func New(mockProv *mock.Provider) *Engine {
	return &Engine{
		mockProv: mockProv,
	}
}

// Diagnose runs diagnostics on a connection and returns findings.
// It probes each segment of the route graph from local service to endpoint.
func (e *Engine) Diagnose(ctx context.Context, connID core.ConnectionID, rt *core.ConnectionRuntime) ([]core.DiagnosticFinding, error) {
	var findings []core.DiagnosticFinding

	if rt == nil {
		return findings, fmt.Errorf("no runtime state available")
	}

	// 1. Probe local service segment
	finding := e.probeLocalService(connID, rt)
	if finding != nil {
		findings = append(findings, *finding)
	}

	// 2. Probe connector segment
	finding = e.probeConnector(connID, rt)
	if finding != nil {
		findings = append(findings, *finding)
	}

	// 3. Probe provider edge segment
	finding = e.probeProviderEdge(connID, rt)
	if finding != nil {
		findings = append(findings, *finding)
	}

	// 4. Probe address segment
	finding = e.probeAddress(connID, rt)
	if finding != nil {
		findings = append(findings, *finding)
	}

	// 5. Probe endpoint segment
	finding = e.probeEndpoint(connID, rt)
	if finding != nil {
		findings = append(findings, *finding)
	}

	return findings, nil
}

func (e *Engine) probeLocalService(connID core.ConnectionID, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	// Local service is healthy if the runtime shows it should be
	if rt.Connector.Status == core.ConnectorStatusRunning {
		return nil
	}
	return nil
}

func (e *Engine) probeConnector(connID core.ConnectionID, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	status := e.mockProv.GetConnectorStatus(connID)

	switch status {
	case core.ConnectorStatusRunning:
		return nil
	case core.ConnectorStatusCrashed:
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-conn", connID)),
			ConnectionID: connID,
			Segment:      core.SegmentConnector,
			Severity:     core.SeverityError,
			Summary:      "Connector process stopped",
			Explanation:  "The connector process that maintains the tunnel has exited. The local service is still running, but the public endpoint is not reachable.",
			Evidence: []core.Evidence{
				{Type: "process_status", Source: "supervisor", Message: "Connector process exited unexpectedly"},
			},
			RepairOptions: []core.RepairOption{
				{
					Summary:     "Restart connector",
					Explanation: "Restart the connector process. The tunnel, DNS, and access policy will not be recreated. The public address will remain unchanged.",
					IsSafe:      true,
				},
			},
			ObservedAt: time.Now().UTC(),
		}
	case core.ConnectorStatusStopped:
		if rt.State == core.RuntimeClosed {
			return nil // Intentionally stopped
		}
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-conn-stop", connID)),
			ConnectionID: connID,
			Segment:      core.SegmentConnector,
			Severity:     core.SeverityWarning,
			Summary:      "Connector is stopped",
			Explanation:  "The connector is not running. The connection will not be reachable until the connector starts.",
			ObservedAt:   time.Now().UTC(),
		}
	default:
		return nil
	}
}

func (e *Engine) probeProviderEdge(connID core.ConnectionID, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	if rt.Endpoint.PublicAddress == "" && rt.State == core.RuntimeOpen {
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-edge", connID)),
			ConnectionID: connID,
			Segment:      core.SegmentProviderEdge,
			Severity:     core.SeverityError,
			Summary:      "Provider edge unavailable",
			Explanation:  "The provider has not assigned a public endpoint. The tunnel may not have connected to the provider edge.",
			ObservedAt:   time.Now().UTC(),
		}
	}
	return nil
}

func (e *Engine) probeAddress(connID core.ConnectionID, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	// Address/DNS segment health check
	if rt.Endpoint.PublicAddress != "" && rt.State == core.RuntimeDegraded {
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-dns", connID)),
			ConnectionID: connID,
			Segment:      core.SegmentAddress,
			Severity:     core.SeverityWarning,
			Summary:      "DNS or address drift detected",
			Explanation:  "The public address exists but does not match the expected endpoint for the current connector.",
			ObservedAt:   time.Now().UTC(),
		}
	}
	return nil
}

func (e *Engine) probeEndpoint(connID core.ConnectionID, rt *core.ConnectionRuntime) *core.DiagnosticFinding {
	if rt.State == core.RuntimeOpen && rt.Endpoint.PublicAddress == "" {
		return &core.DiagnosticFinding{
			ID:           core.FindingID(fmt.Sprintf("find-%s-endpoint", connID)),
			ConnectionID: connID,
			Segment:      core.SegmentEndpoint,
			Severity:     core.SeverityWarning,
			Summary:      "Endpoint not verified",
			Explanation:  "The connection state is open but the public endpoint has not been verified.",
			ObservedAt:   time.Now().UTC(),
		}
	}
	return nil
}

// ExplainFinding generates a plain-language explanation from a finding.
func ExplainFinding(finding *core.DiagnosticFinding) string {
	if finding == nil {
		return "No issues found."
	}

	return fmt.Sprintf("%s\n\n%s", finding.Summary, finding.Explanation)
}

// SmallestRepair returns the smallest repair that restores desired state
// given a finding. It prefers restart over recreation.
func SmallestRepair(finding *core.DiagnosticFinding) *core.RepairOption {
	if finding == nil || len(finding.RepairOptions) == 0 {
		return nil
	}

	// Return the safest repair option first
	for i := range finding.RepairOptions {
		if finding.RepairOptions[i].IsSafe {
			return &finding.RepairOptions[i]
		}
	}

	return &finding.RepairOptions[0]
}
