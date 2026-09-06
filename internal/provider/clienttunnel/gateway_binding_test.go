package clienttunnel

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// Gateway binding for OpenAI-MCP client tunnels.
//
// RequiresGateway() is true for a client tunnel carrying the OpenAI MCP
// intent, and the controller's shared desired-connection builder now supplies
// gateway targeting to every Plan call. These tests pin the provider half:
// the endpoint handed to tunnel-client argv is the EFFECTIVE one — the live
// gateway, or the symbolic reference when none runs yet — never the raw MCP
// URL that would bypass the authenticated front.

func TestGatewayRequiredPlanPinsSymbolicRefNotRawEndpoint(t *testing.T) {
	p, _ := testProvider(t)
	profile := tunnelProfile(core.DesiredOpen)
	profile.ProfileKind = core.ProfileOpenAIMCP

	plan, err := p.Plan(context.Background(), core.DesiredConnection{
		Profile:         profile,
		GatewayRequired: true,
		// No endpoint: the normal preview case — no gateway runs yet.
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, step := range plan.Steps {
		url := step.Technical.Parameters["mcp_server_url"]
		if url == "" {
			continue
		}
		if url == profile.Spec.ClientTunnel.MCP.Endpoint {
			t.Fatalf("step %q carries the RAW MCP endpoint %q; the gateway was bypassed at plan time",
				step.ID, url)
		}
		if url != core.GatewayTargetRef {
			t.Fatalf("step %q carries %q, want the symbolic gateway reference", step.ID, url)
		}
		return
	}
	t.Fatal("no step carries an mcp_server_url")
}

func TestResolvedGatewayEndpointReplacesRawEndpoint(t *testing.T) {
	p, _ := testProvider(t)
	profile := tunnelProfile(core.DesiredOpen)
	profile.ProfileKind = core.ProfileOpenAIMCP
	raw := profile.Spec.ClientTunnel.MCP.Endpoint

	const live = "http://127.0.0.1:49211"
	plan, err := p.Plan(context.Background(), core.DesiredConnection{
		Profile:         profile,
		GatewayRequired: true,
		GatewayEndpoint: live,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	found := false
	for _, step := range plan.Steps {
		if url := step.Technical.Parameters["mcp_server_url"]; url != "" {
			found = true
			if url != live {
				t.Fatalf("step %q targets %q, want the live gateway %q", step.ID, url, live)
			}
		}
	}
	if !found {
		t.Fatal("no step carried an mcp_server_url")
	}
	if strings.Contains(raw, live) {
		t.Fatal("fixture is degenerate: raw endpoint equals the gateway")
	}
}

func TestNonGatewayProfileKeepsDirectEndpoint(t *testing.T) {
	p, _ := testProvider(t)
	profile := tunnelProfile(core.DesiredOpen)
	raw := profile.Spec.ClientTunnel.MCP.Endpoint

	// No gateway requirement: nothing fronts this tunnel, so the direct URL
	// must survive untouched. The gate must be narrow.
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, step := range plan.Steps {
		if url := step.Technical.Parameters["mcp_server_url"]; url != "" {
			if url == core.GatewayTargetRef {
				t.Fatal("a non-gateway profile was pinned with the symbolic reference")
			}
			if url != raw {
				t.Fatalf("endpoint rewritten without a gateway requirement: %q -> %q", raw, url)
			}
			return
		}
	}
	t.Fatal("no step carried an mcp_server_url")
}
