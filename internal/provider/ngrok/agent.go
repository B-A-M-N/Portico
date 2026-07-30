package ngrok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultAgentAPI is where the ngrok agent serves its local API. The agent
// prints the address it chose on startup; this is only the fallback.
const DefaultAgentAPI = "http://127.0.0.1:4040"

// agentTunnel is one tunnel as the local agent reports it.
//
// This is the authoritative record of what the agent actually created. The
// previous implementation synthesised a tunnel identifier and guessed the
// public URL by taking the first entry from an account-wide API listing, which
// could belong to a different connection entirely.
type agentTunnel struct {
	Name      string `json:"name"`
	ID        string `json:"ID"`
	URI       string `json:"uri"`
	PublicURL string `json:"public_url"`
	Proto     string `json:"proto"`
	Config    struct {
		Addr    string `json:"addr"`
		Inspect bool   `json:"inspect"`
	} `json:"config"`
	Metrics struct {
		Conns struct {
			Count int64   `json:"count"`
			Gauge int64   `json:"gauge"`
			P50   float64 `json:"p50"`
			P95   float64 `json:"p95"`
		} `json:"conns"`
		HTTP struct {
			Count int64   `json:"count"`
			Rate1 float64 `json:"rate1"`
			P50   float64 `json:"p50"`
			P95   float64 `json:"p95"`
		} `json:"http"`
	} `json:"metrics"`
}

// agentClient talks to the ngrok agent's local API.
type agentClient struct {
	baseURL string
	http    *http.Client
}

func newAgentClient(baseURL string) *agentClient {
	if baseURL == "" {
		baseURL = DefaultAgentAPI
	}
	return &agentClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// Tunnel returns one tunnel by the exact name Portico gave it.
//
// Addressing by name is what makes observation correct: a connection reads back
// the tunnel it created, never another connection's, and the same lookup
// rebuilds state after a supervisor restart without any cloud API call.
func (c *agentClient) Tunnel(ctx context.Context, name string) (*agentTunnel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tunnels/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the ngrok agent is not reachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, errTunnelNotFound
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("ngrok agent returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tunnel agentTunnel
	if err := json.NewDecoder(resp.Body).Decode(&tunnel); err != nil {
		return nil, fmt.Errorf("decode ngrok tunnel: %w", err)
	}
	return &tunnel, nil
}

// StopTunnel removes a tunnel by name. The agent answers 204 and the tunnel is
// genuinely gone, which is what makes deletion real rather than a no-op.
func (c *agentClient) StopTunnel(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/api/tunnels/"+name, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the ngrok agent is not reachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// Already gone is success for a removal.
		return nil
	case resp.StatusCode >= 400:
		return fmt.Errorf("ngrok agent refused to stop %s: status %d", name, resp.StatusCode)
	}
	return nil
}

// errTunnelNotFound reports a tunnel the agent does not have.
var errTunnelNotFound = fmt.Errorf("no such tunnel")
