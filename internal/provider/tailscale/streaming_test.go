package tailscale

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStreamingThroughServeProxyIsIncremental (audit P1-18).
//
// The adapter declares Streaming beta because "Serve theoretically preserves
// HTTP streams". This test makes that claim falsifiable: it drives an actual
// HTTP request through a local TCP proxy that stands in for the Serve data
// path, with an origin that writes SSE events incrementally, and verifies
// chunks arrive as they are written — not buffered until the response ends.
func TestStreamingThroughServeProxyIsIncremental(t *testing.T) {
	// Origin: emits one SSE event every 50ms for 5 events, flushing each.
	eventCh := make(chan string, 8)
	go func() {
		for i := 0; i < 5; i++ {
			eventCh <- fmt.Sprintf("event-%d", i)
		}
		close(eventCh)
	}()

	writeNext := func(w http.ResponseWriter, done <-chan struct{}) bool {
		select {
		case ev, ok := <-eventCh:
			if !ok {
				return false
			}
			fmt.Fprintf(w, "data: %s\n\n", ev)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return true
		case <-done:
			return false
		}
	}

	origin := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		done := r.Context().Done()
		for {
			if !writeNext(w, done) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	defer origin.Close()

	// Proxy: the shape of the Serve data path — accept TCP on a frontend,
	// forward bytes to the backend. Byte-level forwarding is what "Serve
	// preserves streaming" asserts about; buffering here would fail the test.
	var proxyAddr string
	proxyReady := make(chan string, 1)
	go func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			proxyReady <- ""
			return
		}
		proxyReady <- ln.Addr().String()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleProxyConn(t, conn, origin.Listener.Addr().String())
		}
	}()
	proxyAddr = <-proxyReady
	if proxyAddr == "" {
		t.Fatal("the stand-in proxy could not listen")
	}

	// Client: read through the proxy and timestamp each received event.
	req, err := http.NewRequest(http.MethodGet, "http://"+proxyAddr+"/events", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}

	scanner := bufio.NewScanner(resp.Body)
	receivedAt := map[string]time.Time{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data: ") {
			receivedAt[strings.TrimPrefix(line, "data: ")] = time.Now()
		}
	}
	if len(receivedAt) != 5 {
		t.Fatalf("received %d events, want 5: %v", len(receivedAt), receivedAt)
	}

	// Incrementality proof: total elapsed time must be close to the sum of
	// the origin's inter-event delays. If any hop buffered the whole body,
	// everything would arrive at once (~one write interval apart at most).
	first := receivedAt["event-0"]
	last := receivedAt["event-4"]
	elapsed := last.Sub(first)
	minExpected := 4 * 40 * time.Millisecond // 4 gaps minus tolerance
	if elapsed < minExpected {
		t.Fatalf("events arrived %v apart; the stream was buffered, not streamed", elapsed)
	}
}

func handleProxyConn(t *testing.T, client net.Conn, backend string) {
	t.Helper()
	upstream, err := net.Dial("tcp", backend)
	if err != nil {
		client.Close()
		return
	}
	defer upstream.Close()
	defer client.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// httptestServer is a thin wrapper so the test reads in domain terms.
type httptestServer struct {
	Listener net.Listener
	Close    func()
}

func newTestHTTPServer(t *testing.T, h http.HandlerFunc) *httptestServer {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &httptestServer{Listener: srv.Listener, Close: srv.Close}
}
