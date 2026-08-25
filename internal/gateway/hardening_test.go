package gateway

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startUpstream starts a backend that records what reached it.
func startUpstream(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.host = r.Host
		captured.forwardedHost = r.Header.Get("X-Forwarded-Host")
		captured.forwardedFor = r.Header.Get("X-Forwarded-For")
		captured.auth = r.Header.Get("Authorization")
		if handler != nil {
			handler(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

type capturedRequest struct {
	host          string
	forwardedHost string
	forwardedFor  string
	auth          string
}

func testGateway(t *testing.T, upstream string) (*Gateway, string) {
	t.Helper()
	g, err := New(Config{Upstream: upstream})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go func() { _ = g.Start(newStoppedContext(t)) }()
	waitReady(t, g)
	return g, g.URL()
}

func waitReady(t *testing.T, g *Gateway) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if g.State() == GatewayStateReady && g.Addr() != "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("gateway did not become ready")
}

func newStoppedContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// TestRejectsNonAbsoluteUpstream pins the constructor-level hardening: a
// relative or scheme-less upstream would let httputil.ReverseProxy resolve it
// against the incoming request, so it must be refused at construction.
func TestRejectsNonAbsoluteUpstream(t *testing.T) {
	cases := map[string]string{
		"relative path":   "/local/origin",
		"scheme-less":     "127.0.0.1:8080",
		"unsupported ftp": "ftp://127.0.0.1:21/files",
		"javascript":      "javascript:alert(1)",
	}
	for name, upstream := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(Config{Upstream: upstream}); err == nil {
				t.Fatalf("upstream %q was accepted", upstream)
			} else if !strings.Contains(err.Error(), "upstream") {
				t.Fatalf("error does not name the defect: %v", err)
			}
		})
	}
}

// TestAcceptsAbsoluteHTTPUpstream pins the positive case.
func TestAcceptsAbsoluteHTTPUpstream(t *testing.T) {
	if _, err := New(Config{Upstream: "http://127.0.0.1:9/mcp"}); err != nil {
		t.Fatalf("a valid http upstream was refused: %v", err)
	}
	if _, err := New(Config{Upstream: "https://example.internal"}); err != nil {
		t.Fatalf("a valid https upstream was refused: %v", err)
	}
}

// TestForwardedHeadersPreserveOriginalHost pins the director fix: the old code
// overwrote r.Host and THEN set X-Forwarded-Host from it, so the upstream saw
// its own name instead of the host the client used.
func TestForwardedHeadersPreserveOriginalHost(t *testing.T) {
	srv, captured := startUpstream(t, nil)
	gw, url := testGateway(t, srv.URL)

	req, _ := http.NewRequest(http.MethodGet, url+"/v1/models", nil)
	req.Host = "web.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	if captured.forwardedHost != "web.example.com" {
		t.Fatalf("X-Forwarded-Host = %q, want the client's original host", captured.forwardedHost)
	}
	_ = gw
}

// TestGatewayCredentialNeverReachesUpstream pins credential termination even
// when authentication is enabled.
func TestGatewayCredentialNeverReachesUpstream(t *testing.T) {
	srv, captured := startUpstream(t, nil)
	const secret = "portico_test_gateway_token_1234"
	gw, err := New(Config{Upstream: srv.URL, AuthRequired: true, ValidTokens: []string{secret}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go func() { _ = gw.Start(newStoppedContext(t)) }()
	waitReady(t, gw)

	req, _ := http.NewRequest(http.MethodGet, gw.URL()+"/anything?q=secret-search", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticated request failed: %v", err)
	}
	resp.Body.Close()

	if captured.auth != "" {
		t.Fatalf("the gateway credential leaked to the upstream: %q", captured.auth)
	}
}

// TestSlowHeaderClientsAreBounded pins ReadHeaderTimeout: a client that sends
// headers byte-by-byte (slowloris) must be cut off by the server rather than
// holding a gateway slot indefinitely.
func TestSlowHeaderClientsAreBounded(t *testing.T) {
	srv, _ := startUpstream(t, nil)
	_, url := testGateway(t, srv.URL)

	rawConn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer rawConn.Close()

	// Send a partial request — no terminating blank line — then never finish.
	_, _ = rawConn.Write([]byte("GET / HTTP/1.1\r\nHost: slow.test\r\n"))

	start := time.Now()
	_ = rawConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(rawConn)
	// The server should close the connection after ReadHeaderTimeout (10s).
	buf := make([]byte, 1)
	if _, err := reader.Read(buf); err != nil {
		// Connection closed or reset by the server: this is the pass case,
		// bounded well under our 30s deadline.
		elapsed := time.Since(start)
		if elapsed > 25*time.Second {
			t.Fatalf("the slow client held the connection %v; ReadHeaderTimeout is not bounding it", elapsed)
		}
		return
	}
	t.Fatal("the server kept the partial request open past the header timeout")
}
