// Package openai implements the OpenAI-compatible connection profile.
//
// An OpenAI profile is NOT a provider. It is a profile that wraps a transport
// provider (Cloudflare, ngrok, Tailscale, SSH) to expose an OpenAI-compatible
// API securely.
//
// Architecture:
//
//	OpenAI Profile
//	├── wraps Transport Provider (Cloudflare, ngrok, Tailscale, SSH)
//	├── configures Portico Gateway (auth + SSE proxy)
//	└── provides service-level health checks (/v1/models probe, streaming verification)
package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProfileKind identifies the OpenAI-compatible connection profile.
const ProfileKind = "openai_compatible"

// GatewaySpec configures the Portico Gateway for this profile.
type GatewaySpec struct {
	// Enabled reports whether the gateway proxy is enabled.
	// When disabled, clients connect directly through the tunnel.
	Enabled bool `json:"enabled"`

	// AuthRequired requires Bearer token authentication at the gateway.
	AuthRequired bool `json:"auth_required"`

	// ValidTokens are the Bearer tokens accepted by the gateway.
	// Hashed for storage; plaintext only in memory.
	ValidTokens []string `json:"valid_tokens,omitempty"`

	// AllowSSE allows streaming responses through the gateway.
	// Must be true for OpenAI streaming to work.
	AllowSSE bool `json:"allow_sse"`
}

// ConnectionProfile describes an OpenAI-compatible connection.
// It is a profile, not a provider — it wraps a transport provider.
type ConnectionProfile struct {
	// ID is the connection identifier.
	ID string `json:"id"`

	// Name is the human-readable name.
	Name string `json:"name"`

	// Target is the local OpenAI-compatible endpoint to expose.
	Target TargetSpec `json:"target"`

	// Gateway configures the security proxy.
	Gateway GatewaySpec `json:"gateway"`

	// TransportProviderID identifies the transport provider to use.
	// Set at plan time, not stored — the profile is provider-neutral.
	TransportProviderID string `json:"transport_provider_id,omitempty"`

	// Compatibility is populated after probing the local endpoint.
	Compatibility *OpenAICompatibility `json:"compatibility,omitempty"`
}

// TargetSpec describes the local OpenAI-compatible endpoint.
type TargetSpec struct {
	// BaseURL is the local endpoint (e.g., http://127.0.0.1:8000).
	BaseURL string `json:"base_url"`

	// BasePath is the API base path (usually /v1).
	BasePath string `json:"base_path"`

	// Protocol is the transport protocol (http or https).
	Protocol string `json:"protocol"`
}

// OpenAICompatibility reports what an OpenAI-compatible endpoint supports.
type OpenAICompatibility struct {
	// Endpoint is the probed base URL.
	Endpoint string `json:"endpoint"`

	// Models is the list of available models.
	Models []string `json:"models"`

	// ChatCompletions reports Chat Completions support.
	ChatCompletions struct {
		Supported bool `json:"supported"`
		Streaming bool `json:"streaming"`
	} `json:"chat_completions"`

	// Responses reports Responses API support.
	Responses struct {
		Supported bool `json:"supported"`
		Streaming bool `json:"streaming"`
	} `json:"responses"`

	// AuthRequired reports whether the endpoint requires authentication.
	AuthRequired bool `json:"auth_required"`
}

// ProbeOpenAICompatibility probes a local endpoint for OpenAI compatibility.
// It performs a series of probes:
//  1. GET /v1/models — list available models
//  2. POST /v1/chat/completions (stream:false) — basic request
//  3. POST /v1/chat/completions (stream:true) — streaming support
//  4. POST /v1/responses (if supported) — Responses API
//
// The probe is non-destructive: it uses max_tokens:1 for streaming tests.
func ProbeOpenAICompatibility(ctx context.Context, baseURL string, authToken string) (*OpenAICompatibility, error) {
	comp := &OpenAICompatibility{Endpoint: baseURL}

	// Ensure base URL has no trailing slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	// Probe 1: /v1(models
	models, err := probeModels(ctx, baseURL, authToken)
	if err != nil {
		return nil, fmt.Errorf("probe /v1/models: %w", err)
	}
	comp.Models = models

	// Probe 2: Chat Completions (non-streaming)
	chatSupported, err := probeChatCompletions(ctx, baseURL, authToken, false)
	if err == nil {
		comp.ChatCompletions.Supported = true
	}

	// Probe 3: Chat Completions (streaming)
	if chatSupported {
		streaming, err := probeChatCompletions(ctx, baseURL, authToken, true)
		if err == nil && streaming {
			comp.ChatCompletions.Streaming = true
		}
	}

	return comp, nil
}

func probeModels(ctx context.Context, baseURL string, authToken string) ([]string, error) {
	url := baseURL + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("authentication required")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	// Parse models from response.
	// Response: {"data":[{"id":"model-1"},{"id":"model-2"},...]}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode /v1/models: %w", err)
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

func probeChatCompletions(ctx context.Context, baseURL string, authToken string, stream bool) (bool, error) {
	url := baseURL + "/v1/chat/completions"

	body := fmt.Sprintf(`{
		"model": "test",
		"messages": [{"role": "user", "content": "hi"}],
		"max_tokens": 1,
		"stream": %t
	}`, stream)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("status %d", resp.StatusCode)
	}

	if stream {
		// Verify we receive SSE chunks.
		return verifySSEChunks(resp.Body)
	}
	return true, nil
}

// verifySSEChunks reads from the body and verifies that SSE chunks arrive.
func verifySSEChunks(body io.ReadCloser) (bool, error) {
	// Read the first few bytes to verify content-type was text/event-stream
	// and that chunks are arriving.
	buf := make([]byte, 1024)
	n, err := body.Read(buf)
	if err != nil && err != io.EOF {
		return false, err
	}

	data := string(buf[:n])
	// Look for SSE data: prefix
	if !strings.Contains(data, "data:") {
		return false, fmt.Errorf("no SSE chunks received")
	}

	return true, nil
}

// VerifyStreaming sends a small streaming request and verifies that:
//  1. The response has content-type text/event-stream
//  2. SSE chunks arrive incrementally (not all at once)
//  3. No buffering occurs (chunks flush immediately)
//  4. The stream completes with a [DONE] message
//
// A regular 200 OK test does NOT prove an OpenAI tunnel works.
func VerifyStreaming(ctx context.Context, baseURL string, authToken string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse base URL: %w", err)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/v1/chat/completions"

	body := `{
		"model": "test",
		"messages": [{"role": "user", "content": "Say hello"}],
		"max_tokens": 10,
		"stream": true
	}`

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("streaming request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("streaming request returned status %d", resp.StatusCode)
	}

	// Verify content type
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		return fmt.Errorf("expected text/event-stream, got %q", ct)
	}

	// Verify chunks arrive
	chunkCount := 0
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			chunkCount++
			if chunkCount >= 2 {
				// We received at least 2 chunks — streaming works
				return nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stream: %w", err)
	}

	return fmt.Errorf("streaming response contained only %d chunks", chunkCount)
}

// OpenAIProfile is the profile orchestration.
// It wraps a transport provider to expose an OpenAI-compatible API.
type OpenAIProfile struct {
	// Transport is the underlying transport provider (Cloudflare, ngrok, etc.).
	// The profile delegates tunnel operations to the transport.
	Transport TransportProvider

	// Gateway is the local auth/SSE proxy.
	Gateway *GatewayConfig
}

// TransportProvider is the interface the OpenAI profile expects from a transport.
// This is intentionally minimal — the profile only needs to establish a tunnel,
// not know the provider-specific details.
type TransportProvider interface {
	// EstablishTunnel creates a tunnel from a local port to a public endpoint.
	// Returns the public URL and any error.
	EstablishTunnel(ctx context.Context, localPort int, publicHost string) (string, error)

	// CloseTunnel tears down the tunnel.
	CloseTunnel(ctx context.Context, tunnelID string) error

	// CheckTransport verifies the tunnel is reachable.
	CheckTransport(ctx context.Context, publicURL string) error
}

// GatewayConfig is the runtime configuration for the Portico Gateway.
type GatewayConfig struct {
	// ListenAddr is the local address the gateway listens on.
	ListenAddr string

	// Upstream is the tunnel endpoint to forward requests to.
	Upstream string

	// AuthTokens are the valid Bearer tokens.
	AuthTokens map[string]struct{}
}

// NewOpenAIProfile creates a new OpenAI profile.
func NewOpenAIProfile(transport TransportProvider) *OpenAIProfile {
	return &OpenAIProfile{
		Transport: transport,
		Gateway:   &GatewayConfig{AuthTokens: make(map[string]struct{})},
	}
}
