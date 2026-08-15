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

// StartGateway starts a gateway for the connection and stores its endpoint in the runtime.
func (m *gatewayManager) StartGateway(ctx context.Context, connID core.ConnectionID, upstream string, authTokens []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop any existing gateway for this connection.
	if h, ok := m.gateways[connID]; ok {
		h.cancel()
		_ = h.gateway.Stop()
		delete(m.gateways, connID)
	}

	// Create the gateway.
	gw, err := gateway.New(gateway.Config{
		ListenAddr:    "127.0.0.1:0",
		Upstream:      upstream,
		AuthRequired:  len(authTokens) > 0,
		ValidTokens:   authTokens,
		FlushInterval: 0, // flush SSE chunks immediately
	})
	if err != nil {
		return "", fmt.Errorf("create gateway: %w", err)
	}

	// Start the gateway.
	gwCtx, cancel := context.WithCancel(ctx)
	go func() {
		if err := gw.Start(gwCtx); err != nil {
			slog.Error("gateway stopped", "connection", connID, "error", err)
		}
	}()

	// Wait for it to start.
	if err := waitForGateway(gw); err != nil {
		cancel()
		return "", fmt.Errorf("gateway start failed: %w", err)
	}

	m.gateways[connID] = &gatewayHandle{gateway: gw, cancel: cancel}

	endpoint := gw.URL()
	slog.Info("gateway started", "connection", connID, "endpoint", endpoint, "upstream", upstream)
	return endpoint, nil
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

// waitForGateway waits for the gateway to start listening.
func waitForGateway(gw *gateway.Gateway) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if gw.Addr() != "" {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("gateway did not start in time")
}
