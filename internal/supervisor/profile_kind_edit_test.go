package supervisor

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// Regression tests for audit P0-8: the edit request union advertises
// ProfileKind, so plan/edit must apply it instead of silently dropping it.

func TestApplyEditRequestAppliesProfileKind(t *testing.T) {
	current := gatewayVerticalProfile()
	kind := string(core.ProfileWebService)

	proposed, err := applyEditRequest(current, ipc.UpdateConnectionRequest{
		ProfileKind: &kind,
	})
	if err != nil {
		t.Fatalf("applyEditRequest: %v", err)
	}
	if proposed.ProfileKind != core.ProfileWebService {
		t.Fatalf("proposed profile kind = %q, want %q — the requested workload change was silently discarded",
			proposed.ProfileKind, core.ProfileWebService)
	}
	if current.ProfileKind != core.ProfileOpenAICompatible {
		t.Fatal("applyEditRequest mutated the current profile")
	}
}

func TestApplyEditRequestRefusesUnknownProfileKind(t *testing.T) {
	current := gatewayVerticalProfile()
	kind := "quantum_relay"

	_, err := applyEditRequest(current, ipc.UpdateConnectionRequest{ProfileKind: &kind})
	if err == nil {
		t.Fatal("an unregistered profile kind was accepted")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("error should name the unregistered kind: %v", err)
	}
}

func TestApplyEditRequestRefusesEmptyProfileKind(t *testing.T) {
	current := gatewayVerticalProfile()
	kind := "  "

	_, err := applyEditRequest(current, ipc.UpdateConnectionRequest{ProfileKind: &kind})
	if err == nil {
		t.Fatal("an empty profile kind was accepted")
	}
}

func TestApplyEditRequestRefusesProfileKindOnNonServiceConnection(t *testing.T) {
	current := &core.ConnectionProfile{
		ID: "pf-1", Name: "forward", Kind: core.ConnectionPortForward,
		Spec: core.ConnectionSpec{PortForward: &core.PortForwardSpec{
			LocalPort: 8080, RemoteHost: "example.com", RemotePort: 443,
		}},
	}
	kind := string(core.ProfileOpenAICompatible)

	_, err := applyEditRequest(current, ipc.UpdateConnectionRequest{ProfileKind: &kind})
	if err == nil {
		t.Fatal("a profile-kind change on a port forward was accepted")
	}
}
