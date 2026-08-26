package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/gateway"
)

// gatewayManager owns gateway instances per connection.
type gatewayManager struct {
	mu       sync.Mutex
	gateways map[core.ConnectionID]*gatewayHandle
}

type gatewayHandle struct {
	gateway *gateway.Gateway
	cancel  context.CancelFunc
}

func newGatewayManager() *gatewayManager {
	return &gatewayManager{
		gateways: make(map[core.ConnectionID]*gatewayHandle),
	}
}

// StartGateway starts a gateway for the connection and returns its endpoint.
// The gateway is started asynchronously and the function returns once it's ready.
func (m *gatewayManager) StartGateway(ctx context.Context, connID core.ConnectionID, spec core.GatewayStartSpec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop any existing gateway for this connection.
	if h, ok := m.gateways[connID]; ok {
		h.cancel()
		_ = h.gateway.Stop()
		delete(m.gateways, connID)
	}

	// Create the gateway. Authentication policy is taken from the spec as
	// declared — never inferred from token presence.
	gw, err := gateway.New(gateway.Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     spec.Upstream,
		AuthRequired: spec.AuthRequired,
		ValidTokens:  spec.AuthTokens,
	})
	if err != nil {
		return "", fmt.Errorf("create gateway: %w", err)
	}

	// Start the gateway.
	// Gateway lifetime is supervisor-owned. Detach it from an apply request's
	// cancellation; StopGateway and StopAll remain the explicit lifecycle
	// boundaries.
	gwCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		if err := gw.Start(gwCtx); err != nil {
			slog.Error("gateway stopped", "connection", connID, "error", err)
		}
	}()

	// Wait for it to be ready.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if gw.State() == gateway.GatewayStateReady && gw.Addr() != "" {
			m.gateways[connID] = &gatewayHandle{gateway: gw, cancel: cancel}
			endpoint := gw.URL()
			slog.Info("gateway started", "connection", connID, "endpoint", endpoint, "upstream", spec.Upstream, "auth_required", spec.AuthRequired)
			return endpoint, nil
		}
		if gw.State() == gateway.GatewayStateFailed {
			cancel()
			return "", fmt.Errorf("gateway failed to start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	_ = gw.Stop()
	return "", fmt.Errorf("gateway did not start in time")
}

// Runtime returns the gateway projection without exposing the gateway object
// or any authentication material.
//
// The projection consults the gateway's LIVE state, not just the fact that a
// handle exists (audit item 29): a gateway whose server goroutine exited
// after reaching Ready must not be reported as an available endpoint, which
// would leave Runtime.Gateway claiming a working front while nothing listened.
func (m *gatewayManager) Runtime(connID core.ConnectionID) (core.GatewayRuntime, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.gateways[connID]
	if !ok {
		return core.GatewayRuntime{}, false
	}
	if h.gateway.State() != gateway.GatewayStateReady || h.gateway.Addr() == "" {
		// Dead or dying: drop the stale handle so a later StartGateway can
		// rebuild cleanly. The caller sees "no gateway", which reconciliation
		// treats as degraded and repairs — the honest answer.
		h.cancel()
		delete(m.gateways, connID)
		return core.GatewayRuntime{}, false
	}
	return core.GatewayRuntime{
		Endpoint:  h.gateway.URL(),
		Upstream:  h.gateway.UpstreamURL(),
		StartedAt: h.gateway.StartedAt(),
	}, true
}

// StopGateway stops the gateway for the connection.
func (m *gatewayManager) StopGateway(connID core.ConnectionID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	h, ok := m.gateways[connID]
	if !ok {
		return nil // no gateway running
	}

	h.cancel()
	_ = h.gateway.Stop()
	delete(m.gateways, connID)

	slog.Info("gateway stopped", "connection", connID)
	return nil
}

// StopAll stops all gateways.
func (m *gatewayManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for connID, h := range m.gateways {
		h.cancel()
		_ = h.gateway.Stop()
		delete(m.gateways, connID)
	}
}
