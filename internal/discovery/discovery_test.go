package discovery

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEnum returns preset sockets and counts List calls.
type fakeEnum struct {
	sockets []ListeningSocket
	calls   atomic.Int32
	err     error
}

func (f *fakeEnum) List(ctx context.Context) ([]ListeningSocket, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.sockets, nil
}

// fakeProber returns preset outcomes per port and counts probes.
type fakeProber struct {
	outcomes map[int]ProbeOutcome
	calls    atomic.Int32
}

func (f *fakeProber) Probe(ctx context.Context, port int) ProbeOutcome {
	f.calls.Add(1)
	if out, ok := f.outcomes[port]; ok {
		return out
	}
	return ProbeOutcome{Evidence: []string{"no HTTP response on GET /"}}
}

func noProcInfo(pid int) (string, string) { return "", "" }

func newTestDiscoverer(enum *fakeEnum, prober *fakeProber, now func() time.Time) *CachedDiscoverer {
	return NewDiscoverer(Options{
		Enumerator:  enum,
		Prober:      prober,
		ProcessInfo: noProcInfo,
		TTL:         30 * time.Second,
		Concurrency: 4,
		Now:         now,
	})
}

func TestDiscovererClassification(t *testing.T) {
	enum := &fakeEnum{sockets: []ListeningSocket{
		{Protocol: "tcp", Address: "127.0.0.1", Port: 3000, PID: 42, Process: "node"},
		{Protocol: "tcp", Address: "0.0.0.0", Port: 8443},
		{Protocol: "tcp", Address: "::1", Port: 5432, PID: 7, Process: "postgres"},
		{Protocol: "udp", Address: "0.0.0.0", Port: 5353, Process: "avahi"},
		// Non-local bind must be excluded (discovery is local-only).
		{Protocol: "tcp", Address: "192.168.1.5", Port: 9999},
	}}
	prober := &fakeProber{outcomes: map[int]ProbeOutcome{
		3000: {Scheme: "http", Evidence: []string{"GET / -> HTTP 200"}},
		8443: {Scheme: "https", Evidence: []string{"GET / -> HTTP 200", "TLS certificate not verified (local classification only)"}},
	}}

	d := newTestDiscoverer(enum, prober, time.Now)
	res, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if len(res.Services) != 4 {
		t.Fatalf("expected 4 services, got %d: %+v", len(res.Services), res.Services)
	}

	byPort := map[int]DiscoveredService{}
	for _, svc := range res.Services {
		byPort[svc.Port] = svc
	}

	if svc := byPort[3000]; svc.Protocol != "http" {
		t.Errorf("port 3000: expected http, got %q", svc.Protocol)
	} else {
		if svc.Confidence != ConfidenceVeryLikely {
			t.Errorf("port 3000: probe success + process should be very_likely, got %q", svc.Confidence)
		}
		if svc.Process != "node" || svc.PID != 42 {
			t.Errorf("port 3000: process metadata lost: %+v", svc)
		}
		if len(svc.Evidence) == 0 {
			t.Errorf("port 3000: expected probe evidence")
		}
	}

	if svc := byPort[8443]; svc.Protocol != "https" {
		t.Errorf("port 8443: expected https, got %q", svc.Protocol)
	} else if svc.Confidence != ConfidenceLikely {
		t.Errorf("port 8443: probe success without process should be likely, got %q", svc.Confidence)
	}

	if svc := byPort[5432]; svc.Protocol != "unknown-tcp" {
		t.Errorf("port 5432: expected unknown-tcp, got %q", svc.Protocol)
	} else if svc.Confidence != ConfidencePossible {
		t.Errorf("port 5432: expected possible, got %q", svc.Confidence)
	}

	if svc := byPort[5353]; svc.Protocol != "udp" {
		t.Errorf("port 5353: expected udp, got %q", svc.Protocol)
	}
	if _, ok := byPort[9999]; ok {
		t.Errorf("non-local bind 192.168.1.5 must not be discovered")
	}

	// UDP listeners must never be probed.
	if int(prober.calls.Load()) != 3 {
		t.Errorf("expected 3 TCP probes, got %d", prober.calls.Load())
	}
}

func TestDiscovererDeterministicOrder(t *testing.T) {
	enum := &fakeEnum{sockets: []ListeningSocket{
		{Protocol: "tcp", Address: "127.0.0.1", Port: 9090},
		{Protocol: "tcp", Address: "127.0.0.1", Port: 80},
		{Protocol: "udp", Address: "127.0.0.1", Port: 5353},
		{Protocol: "tcp", Address: "127.0.0.1", Port: 3000},
	}}
	prober := &fakeProber{}

	d := newTestDiscoverer(enum, prober, time.Now)
	first, err := d.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	second, err := d.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	wantPorts := []int{80, 3000, 5353, 9090}
	for i, want := range wantPorts {
		if first.Services[i].Port != want {
			t.Errorf("first scan order[%d]: want port %d, got %d", i, want, first.Services[i].Port)
		}
		if second.Services[i].Port != want {
			t.Errorf("second scan order[%d]: want port %d, got %d", i, want, second.Services[i].Port)
		}
	}
}

func TestDiscovererDeduplicatesWildcardBinds(t *testing.T) {
	enum := &fakeEnum{sockets: []ListeningSocket{
		{Protocol: "tcp", Address: "0.0.0.0", Port: 3000},
		{Protocol: "tcp", Address: "::", Port: 3000, PID: 42, Process: "node"},
	}}
	prober := &fakeProber{outcomes: map[int]ProbeOutcome{
		3000: {Scheme: "http", Evidence: []string{"GET / -> HTTP 200"}},
	}}

	d := newTestDiscoverer(enum, prober, time.Now)
	res, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Services) != 1 {
		t.Fatalf("expected dual-stack listener deduplicated to 1 service, got %d", len(res.Services))
	}
	// The duplicate carrying process metadata must win.
	if res.Services[0].Process != "node" {
		t.Errorf("dedup should prefer entry with process metadata, got %+v", res.Services[0])
	}
}

func TestDiscovererCacheAndRefresh(t *testing.T) {
	enum := &fakeEnum{sockets: []ListeningSocket{
		{Protocol: "tcp", Address: "127.0.0.1", Port: 3000},
	}}
	prober := &fakeProber{}

	current := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return current }

	d := newTestDiscoverer(enum, prober, now)

	// First Discover scans.
	if _, err := d.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if enum.calls.Load() != 1 {
		t.Fatalf("expected 1 enumeration, got %d", enum.calls.Load())
	}

	// Second Discover within TTL returns the cache.
	if _, err := d.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if enum.calls.Load() != 1 {
		t.Errorf("Discover within TTL must use cache; enumerations = %d", enum.calls.Load())
	}

	// Refresh forces a re-scan even within TTL.
	if _, err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if enum.calls.Load() != 2 {
		t.Errorf("Refresh must re-scan; enumerations = %d", enum.calls.Load())
	}

	// After TTL expires, Discover scans again.
	current = current.Add(31 * time.Second)
	if _, err := d.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if enum.calls.Load() != 3 {
		t.Errorf("Discover past TTL must re-scan; enumerations = %d", enum.calls.Load())
	}
}

func TestParseSSLine(t *testing.T) {
	line := `tcp   LISTEN 0      4096       127.0.0.1:3000       0.0.0.0:*    users:(("node",pid=1234,fd=23))`
	sock, err := parseSSLine(line)
	if err != nil {
		t.Fatalf("parseSSLine: %v", err)
	}
	if sock.Protocol != "tcp" || sock.Address != "127.0.0.1" || sock.Port != 3000 {
		t.Errorf("unexpected socket: %+v", sock)
	}
	if sock.Process != "node" || sock.PID != 1234 {
		t.Errorf("process info not parsed: %+v", sock)
	}
}
