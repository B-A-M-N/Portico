package gateway

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestGatewayStarts verifies the gateway starts and returns an address.
func TestGatewayStarts(t *testing.T) {
	g, err := New(Config{
		Upstream:      "http://127.0.0.1:18080",
		AuthRequired:  false,
		FlushInterval: 0,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = g.Start(ctx)
	}()

	waitForGateway(t, g)

	if g.Addr() == "" {
		t.Fatal("gateway did not start")
	}
	t.Logf("gateway started on %s", g.Addr())
}

// TestGatewayProxiesWithoutAuth verifies the gateway proxies requests without auth.
func TestGatewayProxiesWithoutAuth(t *testing.T) {
	upstream, upstreamAddr := newTestServer(t, "hello", 200)
	defer upstream.Close()

	g, _ := New(Config{
		ListenAddr:    "127.0.0.1:0",
		Upstream:      "http://" + upstreamAddr,
		AuthRequired:  false,
		FlushInterval: 0,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = g.Start(ctx) }()

	waitForGateway(t, g)

	resp, err := http.Get(g.URL())
	if err != nil {
		t.Fatalf("GET through gateway: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
}

// TestGatewayRequiresAuth verifies auth enforcement.
func TestGatewayRequiresAuth(t *testing.T) {
	upstream, upstreamAddr := newTestServer(t, "hello", 200)
	defer upstream.Close()

	g, _ := New(Config{
		ListenAddr:    "127.0.0.1:0",
		Upstream:      "http://" + upstreamAddr,
		AuthRequired:  true,
		ValidTokens:   []string{"valid-token-123"},
		FlushInterval: 0,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = g.Start(ctx) }()

	waitForGateway(t, g)

	resp, err := http.Get(g.URL())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, g.URL(), nil)
	req.Header.Set("Authorization", "Bearer valid-token-123")
	client := &http.Client{}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET with token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
}

// TestGatewayFlushesSSEImmediately verifies SSE transparency — no buffering.
func TestGatewayFlushesSSEImmediately(t *testing.T) {
	sseServer, sseAddr := newTestSSEServer(t, 5)
	defer sseServer.Close()

	g, _ := New(Config{
		ListenAddr:    "127.0.0.1:0",
		Upstream:      "http://" + sseAddr,
		AuthRequired:  false,
		FlushInterval: 0,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = g.Start(ctx) }()

	waitForGateway(t, g)

	resp, err := http.Get(g.URL())
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}

	startTime := time.Now()
	scanner := bufio.NewScanner(resp.Body)
	chunkCount := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") && line != "data: [DONE]" {
			chunkCount++
		}
	}

	if chunkCount != 5 {
		t.Fatalf("got %d chunks, want 5", chunkCount)
	}

	elapsed := time.Since(startTime)
	t.Logf("received %d SSE chunks in %v", chunkCount, elapsed)

	if elapsed > 5*time.Second {
		t.Fatalf("SSE chunks took too long: %v (possible buffering)", elapsed)
	}
}

// TestGatewayTokenManagement verifies token add/remove.
func TestGatewayTokenManagement(t *testing.T) {
	g, _ := New(Config{
		Upstream:     "http://127.0.0.1:18080",
		AuthRequired: true,
	})

	if g.HasToken("tok-1") {
		t.Fatal("should not have tok-1 initially")
	}

	g.AddToken("tok-1")
	if !g.HasToken("tok-1") {
		t.Fatal("should have tok-1 after AddToken")
	}

	g.RemoveToken("tok-1")
	if g.HasToken("tok-1") {
		t.Fatal("should not have tok-1 after RemoveToken")
	}
}

// TestGatewayAutoTokenGeneration verifies token auto-generation.
func TestGatewayAutoTokenGeneration(t *testing.T) {
	g, _ := New(Config{
		Upstream:     "http://127.0.0.1:18080",
		AuthRequired: true,
	})

	found := false
	for tok := range g.validTokens {
		if strings.HasPrefix(tok, "portico_") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected auto-generated token with portico_ prefix")
	}
}

// --- test helpers ---

func waitForGateway(t *testing.T, g *Gateway) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if g.Addr() != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gateway did not start in time")
}

func newTestServer(t *testing.T, body string, status int) (*http.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	time.Sleep(100 * time.Millisecond)
	return server, listener.Addr().String()
}

func newTestSSEServer(t *testing.T, chunkCount int) (*http.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < chunkCount; i++ {
			fmt.Fprintf(w, "data: {\"id\":%d}\n\n", i)
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	time.Sleep(100 * time.Millisecond)
	return server, listener.Addr().String()
}
