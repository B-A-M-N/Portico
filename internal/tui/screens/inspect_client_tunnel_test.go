package screens

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Item 13: a client tunnel's inspect screen reports THREE separate facts —
// local MCP readiness, tunnel runtime readiness, and ChatGPT attachment — and
// never collapses "unknown" into success. Portico cannot see ChatGPT-side
// setup at all, so attachment is always stated as unknown rather than inferred
// from the other two checks passing.

func clientTunnelInspect(health *ipc.HealthDTO) *InspectModel {
	return &InspectModel{
		Connection: &ipc.ConnectionDTO{
			ID: "ct-1", Name: "chatgpt-mcp", Kind: "client_tunnel",
			UserState: "open", RuntimeState: "open",
			ProviderID: "client_tunnel",
		},
		Detail: &ipc.ConnectionDetailDTO{Health: health},
	}
}

func TestClientTunnelInspectReportsThreeSeparateFacts(t *testing.T) {
	view := clientTunnelInspect(&ipc.HealthDTO{
		Service:   ipc.HealthCheckDTO{State: "ok"},
		Transport: ipc.HealthCheckDTO{State: "ok"},
	}).View()

	if !strings.Contains(view, "TUNNEL READINESS") {
		t.Fatalf("the inspect screen has no tunnel readiness section:\n%s", view)
	}
	if !strings.Contains(view, "Local MCP:") || !strings.Contains(view, "Tunnel runtime:") {
		t.Fatalf("local MCP and runtime readiness are not reported separately:\n%s", view)
	}
	if !strings.Contains(view, "ChatGPT attachment: unknown") {
		t.Fatalf("ChatGPT attachment was not reported as unknown:\n%s", view)
	}
}

// TestUnknownAttachmentIsNeverSuccess pins the honesty rule even when both
// Portico-visible checks pass.
func TestUnknownAttachmentIsNeverSuccess(t *testing.T) {
	view := clientTunnelInspect(&ipc.HealthDTO{
		Service:   ipc.HealthCheckDTO{State: "ok"},
		Transport: ipc.HealthCheckDTO{State: "ok"},
	}).View()

	lower := strings.ToLower(view)
	for _, overclaim := range []string{"chatgpt attachment: ready", "chatgpt attachment: verified", "chatgpt: connected"} {
		if strings.Contains(lower, overclaim) {
			t.Fatalf("attachment unknown was promoted to %q", overclaim)
		}
	}
}

// TestNotReadyRuntimeCarriesItsReason pins that a refused credential or
// unreachable control plane surfaces its reason, not just a bare failure.
func TestNotReadyRuntimeCarriesItsReason(t *testing.T) {
	view := clientTunnelInspect(&ipc.HealthDTO{
		Service:   ipc.HealthCheckDTO{State: "ok"},
		Transport: ipc.HealthCheckDTO{State: "problem", Detail: "control plane unreachable or credential rejected"},
	}).View()

	if !strings.Contains(view, "not ready") || !strings.Contains(view, "credential rejected") {
		t.Fatalf("the runtime failure lost its reason:\n%s", view)
	}
}
