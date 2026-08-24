package supervisor

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// Adopting a client tunnel through the supervisor.
//
// The wizard collects a tunnel ID, the local MCP server, and a client profile. This endpoint
// used to refuse the kind outright, saying tunnels were created "through their provider's
// setup flow" — which was not true of any flow Portico has. The user answered three questions
// and the request was refused at the end, which is the fake affordance the completion criteria
// forbid.
//
// Portico does not create the tunnel. It adopts one that already exists, which is why these
// tests check that the recorded state says so.

// tunnelID is a well-formed identifier of the shape the client requires.
func tunnelID() string { return "tunnel_" + strings.Repeat("a", 32) }

// clientTunnelRequest is what the wizard builds.
func clientTunnelRequest() ipc.CreateConnectionRequest {
	return ipc.CreateConnectionRequest{
		Name: "chatgpt-mcp",
		Kind: "client_tunnel",
		ClientTunnel: &ipc.ClientTunnelSpecDTO{
			Client:   "openai_secure_mcp_tunnel",
			TunnelID: tunnelID(),
			Profile:  "work",
			MCP: ipc.MCPSourceDTO{
				Transport: "http", Endpoint: "http://127.0.0.1:8000",
			},
		},
		Provider:  ipc.ProviderSelectionDTO{ProviderID: "stub_client_tunnel"},
		Lifecycle: ipc.LifecycleDTO{OnDisconnect: "keep_alive"},
	}
}

// TestAdoptingAClientTunnelSucceeds pins that the wizard's request is not refused.
func TestAdoptingAClientTunnelSucceeds(t *testing.T) {
	h, st := privateNetworkHandler(t, &stubClientTunnel{})

	created, err := h.HandleCreateConnection(clientTunnelRequest())
	if err != nil {
		t.Fatalf("HandleCreateConnection: %v", err)
	}
	if created.Kind != "client_tunnel" {
		t.Fatalf("created a %q", created.Kind)
	}
	// A client tunnel has no public address and must not claim one.
	if created.PublicAddress != "" {
		t.Fatalf("a client tunnel reported the public address %q", created.PublicAddress)
	}
	// Created closed, so adopting one never starts a client by surprise.
	if created.DesiredState != "closed" {
		t.Fatalf("a new connection is %q, want closed", created.DesiredState)
	}

	profile, err := st.LoadProfile(context.Background(), core.ConnectionID(created.ID))
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	spec := profile.Spec.ClientTunnel
	if spec == nil {
		t.Fatal("the connection carries no client tunnel spec")
	}
	// The exact ID the user gave. Portico manages this tunnel and did not make it, so the
	// identifier is the whole basis of the connection.
	if spec.TunnelID != tunnelID() {
		t.Fatalf("the tunnel ID is %q", spec.TunnelID)
	}
	if spec.Profile != "work" {
		t.Fatalf("the client profile is %q", spec.Profile)
	}
	if spec.MCP.Endpoint != "http://127.0.0.1:8000" {
		t.Fatalf("the MCP endpoint is %q", spec.MCP.Endpoint)
	}
	// And no other kind's arm.
	if profile.Spec.ServiceExposure != nil || profile.Spec.PortForward != nil ||
		profile.Spec.PrivateNetwork != nil {
		t.Error("the connection carries another kind's arm")
	}
}

// TestATunnelIDIsRequired pins that Portico does not invent one.
func TestATunnelIDIsRequired(t *testing.T) {
	h, _ := privateNetworkHandler(t, &stubClientTunnel{})

	req := clientTunnelRequest()
	req.ClientTunnel.TunnelID = ""
	_, err := h.HandleCreateConnection(req)
	if err == nil {
		t.Fatal("a client tunnel was adopted with no tunnel to adopt")
	}
	if !strings.Contains(err.Error(), "cannot create one") {
		t.Errorf("the refusal does not say Portico cannot create a tunnel: %v", err)
	}
}

// TestATunnelNeedsSomewhereToForward pins that a connection which cannot open is refused
// rather than saved.
func TestATunnelNeedsSomewhereToForward(t *testing.T) {
	h, _ := privateNetworkHandler(t, &stubClientTunnel{})

	req := clientTunnelRequest()
	req.ClientTunnel.MCP = ipc.MCPSourceDTO{Transport: "http"}
	_, err := h.HandleCreateConnection(req)
	if err == nil {
		t.Fatal("a tunnel with nothing to forward to was adopted")
	}
	if !strings.Contains(err.Error(), "forward to") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// TestAClientTunnelSurvivesARestart pins reconstruction for the kind.
func TestAClientTunnelSurvivesARestart(t *testing.T) {
	h, st := privateNetworkHandler(t, &stubClientTunnel{})

	created, err := h.HandleCreateConnection(clientTunnelRequest())
	if err != nil {
		t.Fatal(err)
	}

	restarted := restartedSupervisor(t, st, &stubClientTunnel{})
	profile, ok := restarted.controller.GetProfile(core.ConnectionID(created.ID))
	if !ok {
		t.Fatal("the client tunnel did not survive the restart")
	}
	spec := profile.Spec.ClientTunnel
	if spec == nil {
		t.Fatal("the client tunnel spec was lost")
	}
	// The tunnel ID is what the connection manages. Losing it across a restart would leave
	// a connection that cannot say which tunnel it is for.
	if spec.TunnelID != tunnelID() {
		t.Fatalf("the tunnel ID came back as %q", spec.TunnelID)
	}
	if spec.MCP.Endpoint != "http://127.0.0.1:8000" {
		t.Fatalf("the MCP endpoint came back as %q", spec.MCP.Endpoint)
	}
}
