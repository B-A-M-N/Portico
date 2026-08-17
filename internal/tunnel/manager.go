package tunnel

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

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
	secret := base64.StdEncoding.EncodeToString([]byte(uuid.New().String()))

	rc := cf.AccountIdentifier(accountID)
	tunnel, err := m.client.CreateTunnel(ctx, rc, cf.TunnelCreateParams{
		Name:      name,
		Secret:    secret,
		ConfigSrc: "cloudflare",
	})
	if err != nil {
		var cfErr *cf.Error
		if errors.As(err, &cfErr) {
			switch {
			case cfErr.StatusCode == http.StatusUnauthorized || cfErr.StatusCode == http.StatusForbidden:
				return nil, fmt.Errorf("unauthorized: %w", err)
			case cfErr.StatusCode == http.StatusTooManyRequests:
				return nil, fmt.Errorf("rate limited: %w", err)
			}
		}
		return nil, fmt.Errorf("transient: %w", err)
	}

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
		var cfErr *cf.Error
		if errors.As(err, &cfErr) {
			switch {
			case cfErr.StatusCode == http.StatusNotFound:
				return nil, nil
			case cfErr.StatusCode == http.StatusUnauthorized || cfErr.StatusCode == http.StatusForbidden:
				return nil, fmt.Errorf("unauthorized: %w", err)
			case cfErr.StatusCode == http.StatusTooManyRequests:
				return nil, fmt.Errorf("rate limited: %w", err)
			}
		}
		// 5xx, network failures, timeouts, cancellations, and anything
		// unclassified are transient — never treated as missing.
		return nil, fmt.Errorf("transient: %w", err)
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
// Returns nil if the tunnel is already gone (404).
func (m *APIManager) Delete(ctx context.Context, accountID, tunnelID string) error {
	rc := cf.AccountIdentifier(accountID)

	_ = m.client.CleanupTunnelConnections(ctx, rc, tunnelID)

	err := m.client.DeleteTunnel(ctx, rc, tunnelID)
	if err != nil {
		var cfErr *cf.Error
		if errors.As(err, &cfErr) {
			switch {
			case cfErr.StatusCode == http.StatusNotFound:
				return nil // already gone
			case cfErr.StatusCode == http.StatusUnauthorized || cfErr.StatusCode == http.StatusForbidden:
				return fmt.Errorf("unauthorized: %w", err)
			case cfErr.StatusCode == http.StatusTooManyRequests:
				return fmt.Errorf("rate limited: %w", err)
			}
		}
		return fmt.Errorf("transient: %w", err)
	}
	return nil
}
