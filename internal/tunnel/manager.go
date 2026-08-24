package tunnel

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
	// CreateTunnel creates a remote tunnel and returns its durable ID.
	// This is the mutation boundary — the tunnel ID is returned immediately
	// so it can be journaled before any dependent credential retrieval.
	CreateTunnel(ctx context.Context, accountID, name string) (*TunnelInfo, error)
	// Get retrieves the current state of a tunnel.
	Get(ctx context.Context, accountID, tunnelID string) (*TunnelState, error)
	// ConfigureIngress configures tunnel ingress rules.
	ConfigureIngress(ctx context.Context, accountID, tunnelID, hostname, originURL string) error
	// GetToken retrieves the tunnel connector run token.
	GetToken(ctx context.Context, accountID, tunnelID string) (string, error)
	// Delete removes a Cloudflare Tunnel.
	Delete(ctx context.Context, accountID, tunnelID string) error
	// LookupTunnelByName is a narrowly scoped deterministic-name recovery
	// query. It is used ONLY for resolving an interrupted create whose
	// outcome is unknown (network timeout after the request was sent). It
	// must never be used to infer ownership in the normal observation path.
	LookupTunnelByName(ctx context.Context, accountID, name string) (*TunnelInfo, error)
}

// TunnelInfo is the durable identity of a tunnel after creation.
type TunnelInfo struct {
	ID   string
	Name string
}

// TunnelState represents the observed state of a tunnel.
type TunnelState struct {
	ID     string
	Name   string
	Status string
}

// APIManager implements Manager using the Cloudflare API.
type APIManager struct {
	client *cf.API
}

// NewAPIManager creates a new tunnel manager with the given API client.
func NewAPIManager(client *cf.API) *APIManager {
	return &APIManager{client: client}
}

// CreateTunnel creates a remote Cloudflare Tunnel and returns its durable ID.
// On success the tunnel ID is returned immediately; callers must journal this
// ID before proceeding to credential retrieval so partial failures are
// recoverable.
func (m *APIManager) CreateTunnel(ctx context.Context, accountID, name string) (*TunnelInfo, error) {
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

	return &TunnelInfo{ID: tunnel.ID, Name: tunnel.Name}, nil
}

// Create creates a tunnel and retrieves its token atomically. It is retained
// for backward compatibility but callers should prefer CreateTunnel + GetToken
// so that tunnel creation is journaled before token retrieval.
func (m *APIManager) Create(ctx context.Context, accountID, name string) (*Info, error) {
	tunnel, err := m.CreateTunnel(ctx, accountID, name)
	if err != nil {
		return nil, err
	}
	token, err := m.GetToken(ctx, accountID, tunnel.ID)
	if err != nil {
		return nil, fmt.Errorf("getting tunnel token: %w", err)
	}
	return &Info{TunnelID: tunnel.ID, TunnelName: tunnel.Name, Token: token}, nil
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
		// The client retries a 429 itself and, when it runs out of retries, reports
		// exhaustion rather than the status that caused it — so the *cf.Error above
		// no longer carries 429 and the rate limit was being classified as a generic
		// transient failure. Both are transient, but they are not the same advice:
		// one says wait, the other says something is wrong. ObservationRateLimited
		// exists as its own state precisely because callers act on the difference.
		if IsRateLimitExhaustion(err) {
			return nil, fmt.Errorf("rate limited: %w", err)
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

// LookupTunnelByName resolves the identity of a tunnel by its deterministic
// name. It is used ONLY for recovery after an interrupted create whose
// outcome is unknown (network timeout after the request was sent).
//
// The normal observation path must never use this to infer ownership: a
// name collision with an external tunnel would otherwise adopt a foreign
// resource. Recovery records the attempted deterministic identity *before*
// sending the mutation, and this query is used solely to resolve whether a
// tunnel with that name exists (and what its ID is) so the controller can
// decide whether to adopt or delete it.
func (m *APIManager) LookupTunnelByName(ctx context.Context, accountID, name string) (*TunnelInfo, error) {
	rc := cf.AccountIdentifier(accountID)
	tunnels, _, err := m.client.ListTunnels(ctx, rc, cf.TunnelListParams{Name: name})
	if err != nil {
		return nil, fmt.Errorf("listing tunnels for recovery: %w", err)
	}
	for _, t := range tunnels {
		if t.Name == name {
			return &TunnelInfo{ID: t.ID, Name: t.Name}, nil
		}
	}
	return nil, nil
}

// IsRateLimitExhaustion reports that the client gave up retrying a rate limit.
//
// The Cloudflare client retries a 429 on its own schedule and, when it runs out of
// attempts, returns its own error rather than the status that caused it — so the
// 429 is no longer recoverable through errors.As. Matching the message is not
// something to be pleased about, but the alternative is reporting a rate limit as an
// unclassified failure, which tells the user something is broken when the answer is
// to wait.
//
// It is deliberately narrow: it matches the client's own exhaustion wording and
// nothing else, so an unrelated error is not quietly relabelled as a rate limit.
//
// Exported because the Cloudflare adapter's observation classifier has the same
// blind spot for the same reason. One implementation, used by both: a second copy of
// a string match is the copy that stops matching when the client's wording changes.
func IsRateLimitExhaustion(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "rate limit") &&
		(strings.Contains(message, "retries") || strings.Contains(message, "exceeded"))
}
