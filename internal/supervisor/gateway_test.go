package supervisor

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

func TestGatewayManager_StartStop(t *testing.T) {
	mgr := newGatewayManager()

	// Start a test upstream server.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	upstream := "http://" + listener.Addr().String()
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		}),
	}
	go server.Serve(listener)
	defer server.Close()

	connID := core.ConnectionID("test-conn-1")
	endpoint, err := mgr.StartGateway(context.Background(), connID, upstream, nil)
	if err != nil {
		t.Fatalf("StartGateway: %v", err)
	}
	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Fatalf("unexpected endpoint: %s", endpoint)
	}

	// Verify gateway works.
	resp, err := http.Get(endpoint)
	if err != nil {
		t.Fatalf("GET through gateway: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}

	// Stop the gateway.
	if err := mgr.StopGateway(connID); err != nil {
		t.Fatalf("StopGateway: %v", err)
	}
}

func TestGatewayManager_StopAll(t *testing.T) {
	mgr := newGatewayManager()

	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	defer listener.Close()

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	}
	go server.Serve(listener)
	defer server.Close()

	upstream := "http://" + listener.Addr().String()

	// Start multiple gateways.
	connIDs := []core.ConnectionID{"conn-1", "conn-2"}
	for _, id := range connIDs {
		_, err := mgr.StartGateway(context.Background(), id, upstream, nil)
		if err != nil {
			t.Fatalf("StartGateway %s: %v", id, err)
		}
	}

	// Stop all.
	mgr.StopAll()

	// Verify gateways are stopped by checking internal state.
	if len(mgr.gateways) != 0 {
		t.Fatalf("expected 0 gateways, got %d", len(mgr.gateways))
	}
}

func TestGatewayManager_DoubleStart(t *testing.T) {
	mgr := newGatewayManager()

	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	defer listener.Close()

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	}
	go server.Serve(listener)
	defer server.Close()

	upstream := "http://" + listener.Addr().String()
	connID := core.ConnectionID("test-double")

	// Start gateway first time.
	_, err := mgr.StartGateway(context.Background(), connID, upstream, nil)
	if err != nil {
		t.Fatalf("first StartGateway: %v", err)
	}

	// Start again — should succeed (replaces old one).
	endpoint2, err := mgr.StartGateway(context.Background(), connID, upstream, nil)
	if err != nil {
		t.Fatalf("second StartGateway: %v", err)
	}
	if !strings.HasPrefix(endpoint2, "http://127.0.0.1:") {
		t.Fatalf("unexpected endpoint: %s", endpoint2)
	}

	// Should still be only 1 gateway.
	if len(mgr.gateways) != 1 {
		t.Fatalf("expected 1 gateway, got %d", len(mgr.gateways))
	}

	mgr.StopGateway(connID)
}

func TestGatewayNeeded(t *testing.T) {
	s := &Supervisor{}

	// Test: not a client tunnel.
	rt := &core.ConnectionRuntime{}
	p := &core.ConnectionProfile{Kind: core.ConnectionServiceExposure}
	if s.gatewayNeeded(p, rt) {
		t.Fatal("service exposure should not need gateway")
	}

	// Test: client tunnel but not OpenAI.
	p = &core.ConnectionProfile{
		Kind: core.ConnectionClientTunnel,
		Spec: core.ConnectionSpec{
			ClientTunnel: &core.ClientTunnelSpec{Client: "other"},
		},
	}
	if s.gatewayNeeded(p, rt) {
		t.Fatal("non-OpenAI client tunnel should not need gateway")
	}

	// Test: OpenAI Secure MCP Tunnel.
	p = &core.ConnectionProfile{
		Kind: core.ConnectionClientTunnel,
		Spec: core.ConnectionSpec{
			ClientTunnel: &core.ClientTunnelSpec{Client: core.ClientOpenAISecureMCPTunnel},
		},
	}
	if !s.gatewayNeeded(p, rt) {
		t.Fatal("OpenAI client tunnel should need gateway")
	}
}

func TestGatewayRuntimeDeepCopy(t *testing.T) {
	original := &core.ConnectionRuntime{
		ConnectionID: "test-conn",
		Gateway: &core.GatewayRuntime{
			Endpoint:      "http://127.0.0.1:8080",
			Upstream:      "https://tunnel.example.com",
			AuthEnabled:   true,
			CredentialRef: "ref-123",
			StartedAt:     time.Now().UTC(),
		},
	}

	copied := original.DeepCopy()
	if copied.Gateway == nil {
		t.Fatal("deep copy should preserve gateway")
	}
	if copied.Gateway.Endpoint != original.Gateway.Endpoint {
		t.Fatal("deep copy should preserve endpoint")
	}
	if copied.Gateway.CredentialRef != original.Gateway.CredentialRef {
		t.Fatal("deep copy should preserve credential ref")
	}
}
