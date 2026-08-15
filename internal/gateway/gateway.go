// Package gateway implements the Portico Gateway — a local HTTP proxy that
// authenticates clients and forwards requests to upstream services through
// transport tunnels.
//
// The gateway serves two purposes:
//  1. Security: prevents accidentally exposing unauthenticated local servers
//  2. Observability: provides structured logging, metrics, and health checks
//
// The gateway is streaming-transparent: SSE chunks are forwarded immediately
// without buffering. This is critical for OpenAI-compatible APIs where both
// Chat Completions and Responses use SSE streaming.
package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Gateway is a local HTTP proxy that authenticates clients and forwards
// requests to the upstream service through the transport tunnel.
type Gateway struct {
	// listenAddr is the local address the gateway listens on.
	listenAddr string

	// upstream is the tunnel endpoint to forward requests to.
	upstream *url.URL

	// authRequired requires Bearer token authentication at the gateway.
	authRequired bool

	// validTokens are the Bearer tokens accepted by the gateway.
	validTokens map[string]struct{}

	// server is the underlying HTTP server.
	server *http.Server

	// listener is the underlying network listener.
	listener net.Listener

	// mu protects token updates.
	mu sync.RWMutex

	// flushInterval controls how often SSE chunks are flushed.
	// 0 means flush every chunk immediately.
	flushInterval time.Duration
}

// Config configures the Portico Gateway.
type Config struct {
	// ListenAddr is the local address to listen on.
	// If empty, a dynamic port on 127.0.0.1 is used.
	ListenAddr string

	// Upstream is the tunnel endpoint URL to forward requests to.
	Upstream string

	// AuthRequired requires Bearer token authentication.
	AuthRequired bool

	// ValidTokens are the accepted Bearer tokens.
	// If empty and AuthRequired is true, auto-generates a token.
	ValidTokens []string

	// FlushInterval controls SSE chunk flushing. 0 = immediate.
	FlushInterval time.Duration
}

// New creates a new Portico Gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.Upstream == "" {
		return nil, fmt.Errorf("gateway: upstream URL is required")
	}

	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("gateway: parse upstream: %w", err)
	}

	validTokens := make(map[string]struct{})
	for _, t := range cfg.ValidTokens {
		validTokens[t] = struct{}{}
	}

	// Auto-generate token if auth required but no tokens provided.
	if cfg.AuthRequired && len(validTokens) == 0 {
		tok, err := generateToken()
		if err != nil {
			return nil, fmt.Errorf("gateway: generate token: %w", err)
		}
		validTokens[tok] = struct{}{}
		slog.Info("gateway: auto-generated auth token", "token", tok[:8]+"...")
	}

	g := &Gateway{
		listenAddr:    cfg.ListenAddr,
		upstream:      upstream,
		authRequired:  cfg.AuthRequired,
		validTokens:   validTokens,
		flushInterval: cfg.FlushInterval,
	}

	return g, nil
}

// Start starts the gateway listener.
func (g *Gateway) Start(ctx context.Context) error {
	listenAddr := g.listenAddr
	if listenAddr == "" {
		listenAddr = "127.0.0.1:0" // dynamic port
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("gateway: listen: %w", err)
	}
	g.listener = listener

	g.server = &http.Server{
		Handler: g.handler(),
	}

	slog.Info("gateway: started", "addr", g.Addr(), "upstream", g.upstream.String())

	go func() {
		<-ctx.Done()
		_ = g.Stop()
	}()

	return g.server.Serve(listener)
}

// Stop gracefully shuts down the gateway.
func (g *Gateway) Stop() error {
	if g.server == nil {
		return nil
	}
	return g.server.Close()
}

// Addr returns the actual listen address (useful when dynamic port was used).
func (g *Gateway) Addr() string {
	if g.listener == nil {
		return ""
	}
	return g.listener.Addr().String()
}

// URL returns the gateway's HTTP URL.
func (g *Gateway) URL() string {
	return "http://" + g.Addr()
}

// BaseURL returns the gateway URL with base path.
func (g *Gateway) BaseURL() string {
	return g.URL()
}

// AddToken adds a valid Bearer token.
func (g *Gateway) AddToken(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.validTokens[token] = struct{}{}
}

// RemoveToken removes a valid Bearer token.
func (g *Gateway) RemoveToken(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.validTokens, token)
}

// HasToken reports whether a token is valid.
func (g *Gateway) HasToken(token string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.validTokens[token]
	return ok
}

// handler returns the HTTP handler for the gateway.
func (g *Gateway) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authenticate.
		if g.authRequired {
			if !g.authenticate(w, r) {
				return
			}
		}

		// Proxy the request.
		g.proxy(w, r)
	})
}

// authenticate checks the Bearer token.
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == auth || token == "" {
		g.unauthorized(w, "missing or invalid Authorization header")
		return false
	}

	g.mu.RLock()
	_, ok := g.validTokens[token]
	g.mu.RUnlock()

	if !ok {
		g.unauthorized(w, "invalid token")
		return false
	}

	return true
}

func (g *Gateway) unauthorized(w http.ResponseWriter, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="portico-gateway"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `{"error":{"message":"%s","type":"unauthorized"}}`, reason)
}

// proxy forwards the request to the upstream.
func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request) {
	// Create the proxy request.
	proxyReq := r.Clone(r.Context())
	proxyReq.URL.Host = g.upstream.Host
	proxyReq.URL.Scheme = g.upstream.Scheme
	proxyReq.RequestURI = ""

	// Forward the request. No timeout for streaming responses.
	client := &http.Client{}
	resp, err := client.Do(proxyReq)
	if err != nil {
		g.proxyError(w, err)
		return
	}
	defer resp.Body.Close()

	// Copy response headers.
	for k, v := range resp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}

	// Handle SSE responses specially.
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	w.WriteHeader(resp.StatusCode)

	if isSSE {
		g.flushSSE(w, resp.Body)
	} else {
		_, _ = io.Copy(w, resp.Body)
	}
}

// flushSSE copies SSE chunks immediately without buffering.
func (g *Gateway) flushSSE(w http.ResponseWriter, body io.ReadCloser) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Fallback: just copy.
		_, _ = io.Copy(w, body)
		return
	}

	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		_, _ = fmt.Fprintln(w, line)
		flusher.Flush() // immediate flush — no buffering
	}
}

func (g *Gateway) proxyError(w http.ResponseWriter, err error) {
	slog.Warn("gateway: proxy error", "error", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = fmt.Fprintf(w, `{"error":{"message":"%s","type":"gateway_error"}}`, err.Error())
}

// generateToken generates a random Bearer token.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return "portico_" + hex.EncodeToString(b), nil
}
