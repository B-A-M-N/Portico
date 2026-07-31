package tunnel

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/google/uuid"
)

// Info holds the result of creating a tunnel.
type Info struct {
	TunnelID   string
	TunnelName string
	Token      string // For cloudflared tunnel run --token
}

// Manager manages Cloudflare Tunnel lifecycle via the API.
type Manager interface {
	Create(ctx context.Context, accountID, name string) (*Info, error)
	Get(ctx context.Context, accountID, tunnelID string) (*TunnelState, error)
	ConfigureIngress(ctx context.Context, accountID, tunnelID, hostname, originURL string) error
	GetToken(ctx context.Context, accountID, tunnelID string) (string, error)
	Delete(ctx context.Context, accountID, tunnelID string) error
}

// TunnelState represents the observed state of a tunnel.
type TunnelState struct {
	ID     string
	Name   string
	Status string // "active", "degraded", "down", etc.
}

// APIManager implements Manager using the Cloudflare API.
type APIManager struct {
	client *cf.API
}

// NewAPIManager creates a new tunnel manager with the given API client.
func NewAPIManager(client *cf.API) *APIManager {
	return &APIManager{client: client}
}

// Create creates a remotely-managed Cloudflare Tunnel.
func (m *APIManager) Create(ctx context.Context, accountID, name string) (*Info, error) {
	// Generate a random tunnel secret.
	secret := base64.StdEncoding.EncodeToString([]byte(uuid.New().String()))

	rc := cf.AccountIdentifier(accountID)
	tunnel, err := m.client.CreateTunnel(ctx, rc, cf.TunnelCreateParams{
		Name:      name,
		Secret:    secret,
		ConfigSrc: "cloudflare", // Remotely managed.
	})
	if err != nil {
		return nil, fmt.Errorf("creating tunnel: %w", err)
	}

	// Retrieve the run token.
	token, err := m.client.GetTunnelToken(ctx, rc, tunnel.ID)
	if err != nil {
		return nil, fmt.Errorf("getting tunnel token: %w", err)
	}

	return &Info{
		TunnelID:   tunnel.ID,
		TunnelName: tunnel.Name,
		Token:      token,
	}, nil
}

// Get retrieves the current state of a tunnel.
// Returns nil, nil if the tunnel is not found (deleted externally).
func (m *APIManager) Get(ctx context.Context, accountID, tunnelID string) (*TunnelState, error) {
	rc := cf.AccountIdentifier(accountID)
	tunnel, err := m.client.GetTunnel(ctx, rc, tunnelID)
	if err != nil {
		// errors.As, not a type assertion: the client wraps its errors, so a
		// direct assertion silently fails and a deleted tunnel is reported
		// as a failed lookup instead of an absent one. One of the four places
		// that made this check had already been corrected; the others had not.
		var cfErr *cf.Error
		if errors.As(err, &cfErr) && cfErr.StatusCode == 404 {
			return nil, nil
		}
		return nil, fmt.Errorf("getting tunnel: %w", err)
	}
	return &TunnelState{
		ID:     tunnel.ID,
		Name:   tunnel.Name,
		Status: tunnel.Status,
	}, nil
}
func (m *APIManager) ConfigureIngress(ctx context.Context, accountID, tunnelID, hostname, originURL string) error {
	rc := cf.AccountIdentifier(accountID)

	_, err := m.client.UpdateTunnelConfiguration(ctx, rc, cf.TunnelConfigurationParams{
		TunnelID: tunnelID,
		Config: cf.TunnelConfiguration{
			Ingress: []cf.UnvalidatedIngressRule{
				{
					Hostname: hostname,
					Service:  originURL,
				},
				{
					// Mandatory catch-all rule.
					Service: "http_status:404",
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("configuring tunnel ingress: %w", err)
	}
	return nil
}

// GetToken retrieves the tunnel connector run token.
func (m *APIManager) GetToken(ctx context.Context, accountID, tunnelID string) (string, error) {
	rc := cf.AccountIdentifier(accountID)
	token, err := m.client.GetTunnelToken(ctx, rc, tunnelID)
	if err != nil {
		return "", fmt.Errorf("getting tunnel token: %w", err)
	}
	return token, nil
}

// Delete removes a Cloudflare Tunnel.
// It first cleans up any lingering connections so the delete doesn't fail
// with "active connections" errors after cloudflared has been stopped.
func (m *APIManager) Delete(ctx context.Context, accountID, tunnelID string) error {
	rc := cf.AccountIdentifier(accountID)

	// Clean up stale connections before deletion.
	_ = m.client.CleanupTunnelConnections(ctx, rc, tunnelID)

	if err := m.client.DeleteTunnel(ctx, rc, tunnelID); err != nil {
		return fmt.Errorf("deleting tunnel: %w", err)
	}
	return nil
}
