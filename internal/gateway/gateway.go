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
//
// Security: Gateway credentials terminate at the gateway. The Portico
// Authorization header is stripped before forwarding. There is currently no
// upstream-auth injection mechanism; see the note on Gateway.director.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
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

	// proxy is the reverse proxy.
	proxy *httputil.ReverseProxy

	// server is the underlying HTTP server.
	server *http.Server

	// listener is the underlying network listener.
	listener net.Listener

	// mu protects lifecycle state.
	mu sync.RWMutex

	// state is the current lifecycle state.
	state GatewayState

	// startedAt records when the gateway was started.
	startedAt time.Time

	// stopCh signals the gateway to stop.
	stopCh chan struct{}
}

// GatewayState represents the lifecycle state of a gateway.
type GatewayState string

const (
	GatewayStateNew      GatewayState = "new"
	GatewayStateStarting GatewayState = "starting"
	GatewayStateReady    GatewayState = "ready"
	GatewayStateStopping GatewayState = "stopping"
	GatewayStateStopped  GatewayState = "stopped"
	GatewayStateFailed   GatewayState = "failed"
)

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
}

// New creates a new Portico Gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.Upstream == "" {
		return nil, fmt.Errorf("gateway: upstream URL is required")
	}

	upstream, parseErr := url.Parse(cfg.Upstream)
	if parseErr != nil {
		return nil, fmt.Errorf("gateway: parse upstream: %w", parseErr)
	}
	// Hardened upstream validation (audit item 28): the upstream must be an
	// absolute URL with an explicitly supported scheme. A relative or
	// scheme-less URL would make httputil.ReverseProxy resolve it against the
	// incoming request, letting a caller influence where traffic is sent.
	if !upstream.IsAbs() || upstream.Host == "" {
		return nil, fmt.Errorf("gateway: upstream %q must be an absolute URL with a host", cfg.Upstream)
	}
	switch upstream.Scheme {
	case "http", "https":
		// supported
	default:
		return nil, fmt.Errorf("gateway: upstream scheme %q is not supported (use http or https)", upstream.Scheme)
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
		// No token fragment is logged: even a prefix narrows an attacker's
		// search space. Correlation needs a non-reversible fingerprint, not
		// credential material.
		slog.Info("gateway authentication credential generated")
	}

	g := &Gateway{
		listenAddr:   cfg.ListenAddr,
		upstream:     upstream,
		authRequired: cfg.AuthRequired,
		validTokens:  validTokens,
		state:        GatewayStateNew,
		stopCh:       make(chan struct{}),
	}

	// Create the reverse proxy.
	g.proxy = &httputil.ReverseProxy{
		Director:       g.director,
		ModifyResponse: g.modifyResponse,
		ErrorHandler:   g.errorHandler,
	}

	return g, nil
}

// Start starts the gateway listener and blocks until the gateway stops.
// Use a goroutine to run Start asynchronously.
func (g *Gateway) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.state != GatewayStateNew {
		g.mu.Unlock()
		return fmt.Errorf("gateway: cannot start from state %s", g.state)
	}
	g.state = GatewayStateStarting
	g.mu.Unlock()

	listenAddr := g.listenAddr
	if listenAddr == "" {
		listenAddr = "127.0.0.1:0"
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		g.setState(GatewayStateFailed)
		return fmt.Errorf("gateway: listen: %w", err)
	}

	g.mu.Lock()
	g.listener = listener
	// Timeouts (audit item 28): ReadHeaderTimeout bounds slow-header attacks
	// without ever cutting off a legitimate request body; IdleTimeout reclaims
	// idle keep-alive connections. Deliberately NO WriteTimeout: it would kill
	// legitimate long-lived SSE streams, which this gateway exists to carry.
	readHeaderTimeout := 10 * time.Second
	idleTimeout := 120 * time.Second
	g.server = &http.Server{
		Handler:           g.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	g.state = GatewayStateReady
	g.startedAt = time.Now().UTC()
	g.mu.Unlock()

	slog.Info("gateway: started", "addr", g.Addr(), "upstream", g.upstream.String())

	// Serve in a goroutine so we can handle stop.
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- g.server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		_ = g.Stop()
		return ctx.Err()
	case <-g.stopCh:
		_ = g.Stop()
		return nil
	case err := <-serverErrCh:
		g.setState(GatewayStateFailed)
		return err
	}
}

// Stop gracefully shuts down the gateway.
func (g *Gateway) Stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.state == GatewayStateStopped || g.state == GatewayStateStopping {
		return nil
	}

	g.state = GatewayStateStopping
	close(g.stopCh)

	if g.server != nil {
		_ = g.server.Close()
	}

	g.state = GatewayStateStopped
	return nil
}

// Addr returns the actual listen address.
func (g *Gateway) Addr() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
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

// UpstreamURL returns the configured upstream without exposing mutable proxy
// internals to the supervisor.
func (g *Gateway) UpstreamURL() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.upstream == nil {
		return ""
	}
	return g.upstream.String()
}

// State returns the current lifecycle state.
func (g *Gateway) State() GatewayState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.state
}

// StartedAt returns when the gateway was started.
func (g *Gateway) StartedAt() time.Time {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.startedAt
}

func (g *Gateway) setState(state GatewayState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state = state
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
		g.proxy.ServeHTTP(w, r)
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

// director rewrites the request to route to the upstream.
//
// Upstream auth: the gateway credential is stripped here because it
// authenticates the caller TO PORTICO, and must not leak to the upstream.
// There is currently no upstream-auth injection mechanism; if one is added it
// must use a separate credential source, never the gateway's tokens.
func (g *Gateway) director(r *http.Request) {
	// Preserve the ORIGINAL host before it is overwritten: X-Forwarded-Host
	// must name the host the client used, not the upstream we are about to
	// point at. The old code set these after clobbering r.Host, so the
	// upstream saw its own name in X-Forwarded-Host — breaking virtual-host
	// routing and any origin check an application performed.
	originalHost := r.Host

	r.URL.Scheme = g.upstream.Scheme
	r.URL.Host = g.upstream.Host
	r.Host = g.upstream.Host

	// Strip the Portico Gateway credential — it terminates at the gateway.
	r.Header.Del("Authorization")

	// Set standard forwarded headers.
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		r.Header.Set("X-Forwarded-For", clientIP)
	} else {
		r.Header.Set("X-Forwarded-For", r.RemoteAddr)
	}
	r.Header.Set("X-Forwarded-Proto", "http")
	r.Header.Set("X-Forwarded-Host", originalHost)

	// Ensure hop-by-hop headers are not forwarded.
	r.Header.Del("Connection")
	r.Header.Del("Proxy-Connection")
	r.Header.Del("Keep-Alive")
	r.Header.Del("TE")
	r.Header.Del("Trailer")
	r.Header.Del("Transfer-Encoding")
	r.Header.Del("Upgrade")
}

// modifyResponse handles SSE responses.
func (g *Gateway) modifyResponse(resp *http.Response) error {
	// Ensure SSE responses are not buffered.
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Disable any response buffering.
		resp.Header.Set("X-Accel-Buffering", "no")
		resp.Header.Set("Cache-Control", "no-cache")
	}
	return nil
}

// errorHandler returns a handler for proxy errors.
func (g *Gateway) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	slog.Warn("gateway: proxy error", "error", err, "upstream", g.upstream.String())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte(`{"error":{"message":"Upstream connection failed","type":"gateway_error"}}`))
}

// generateToken generates a random Bearer token.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return "portico_" + hex.EncodeToString(b), nil
}
