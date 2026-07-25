package discovery

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultCacheTTL is how long a discovery scan result stays fresh.
const DefaultCacheTTL = 30 * time.Second

// DefaultConcurrency bounds the number of concurrent HTTP probes.
const DefaultConcurrency = 8

// DiscoveredService is a classified local service candidate.
type DiscoveredService struct {
	Address    string
	Port       int
	Protocol   string // "http", "https", "unknown-tcp", or "udp"
	PID        int
	Process    string
	Confidence Confidence
	Evidence   []string
}

// Result is a completed discovery scan.
type Result struct {
	Services  []DiscoveredService
	ScannedAt time.Time
}

// Discoverer is the interface the supervisor holds for local service
// discovery. Discover returns a cached result when fresh; Refresh
// always performs a new scan.
type Discoverer interface {
	Discover(ctx context.Context) (*Result, error)
	Refresh(ctx context.Context) (*Result, error)
}

// ProcessInfoFunc reads process metadata for a PID. Either return
// value may be empty when the information is unavailable.
type ProcessInfoFunc func(pid int) (comm, cmdline string)

// Options configures a CachedDiscoverer. Zero values select
// production defaults.
type Options struct {
	Enumerator  ListenerEnumerator
	Prober      Prober
	ProcessInfo ProcessInfoFunc
	TTL         time.Duration
	Concurrency int
	Now         func() time.Time
}

// CachedDiscoverer runs the discovery pipeline
// (enumerate -> enrich -> probe -> classify -> sort) and caches the
// result for a TTL.
type CachedDiscoverer struct {
	enum        ListenerEnumerator
	prober      Prober
	procInfo    ProcessInfoFunc
	ttl         time.Duration
	concurrency int
	now         func() time.Time

	mu       sync.Mutex
	cached   *Result
	cachedAt time.Time
}

// NewDiscoverer creates a CachedDiscoverer, filling in defaults for
// any unset option.
func NewDiscoverer(opts Options) *CachedDiscoverer {
	if opts.Enumerator == nil {
		opts.Enumerator = NewSSEnumerator()
	}
	if opts.Prober == nil {
		opts.Prober = NewHTTPProber(DefaultProbeTimeout)
	}
	if opts.ProcessInfo == nil {
		opts.ProcessInfo = readProcInfo
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultCacheTTL
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &CachedDiscoverer{
		enum:        opts.Enumerator,
		prober:      opts.Prober,
		procInfo:    opts.ProcessInfo,
		ttl:         opts.TTL,
		concurrency: opts.Concurrency,
		now:         opts.Now,
	}
}

// Discover returns the cached result if it is still fresh, otherwise
// performs a new scan.
func (d *CachedDiscoverer) Discover(ctx context.Context) (*Result, error) {
	d.mu.Lock()
	if d.cached != nil && d.now().Sub(d.cachedAt) < d.ttl {
		res := d.cached
		d.mu.Unlock()
		return res, nil
	}
	d.mu.Unlock()
	return d.Refresh(ctx)
}

// Refresh always performs a new scan and updates the cache.
func (d *CachedDiscoverer) Refresh(ctx context.Context) (*Result, error) {
	res, err := d.scan(ctx)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.cached = res
	d.cachedAt = d.now()
	d.mu.Unlock()
	return res, nil
}

// scan runs the full pipeline once.
func (d *CachedDiscoverer) scan(ctx context.Context) (*Result, error) {
	sockets, err := d.enum.List(ctx)
	if err != nil {
		return nil, err
	}

	candidates := selectCandidates(sockets)
	if len(candidates) > MaxCandidates {
		candidates = candidates[:MaxCandidates]
	}

	services := make([]DiscoveredService, len(candidates))
	sem := make(chan struct{}, d.concurrency)
	var wg sync.WaitGroup
	for i, sock := range candidates {
		wg.Add(1)
		go func(i int, sock ListeningSocket) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			services[i] = d.classify(ctx, sock)
		}(i, sock)
	}
	wg.Wait()

	// Deterministic order: port ascending, protocol as tie-breaker.
	sort.Slice(services, func(a, b int) bool {
		if services[a].Port != services[b].Port {
			return services[a].Port < services[b].Port
		}
		return services[a].Protocol < services[b].Protocol
	})

	return &Result{Services: services, ScannedAt: d.now().UTC()}, nil
}

// classify enriches one listener with process metadata, probes it when
// it is a TCP listener, and assigns an evidence-based classification.
func (d *CachedDiscoverer) classify(ctx context.Context, sock ListeningSocket) DiscoveredService {
	svc := DiscoveredService{
		Address: fmt.Sprintf("127.0.0.1:%d", sock.Port),
		Port:    sock.Port,
		PID:     sock.PID,
		Process: sock.Process,
	}

	// Process enrichment from /proc where available.
	if sock.PID > 0 && d.procInfo != nil {
		comm, cmdline := d.procInfo(sock.PID)
		if svc.Process == "" && comm != "" {
			svc.Process = comm
		}
		if cmdline != "" {
			svc.Evidence = append(svc.Evidence, "cmdline: "+cmdline)
		}
	}
	hasProcess := svc.Process != ""
	if hasProcess {
		svc.Evidence = append(svc.Evidence,
			fmt.Sprintf("process: %s (pid %d)", svc.Process, sock.PID))
	}

	if !isTCP(sock.Protocol) {
		svc.Protocol = "udp"
		svc.Confidence = ConfidencePossible
		svc.Evidence = append(svc.Evidence, "udp listener (not probed)")
		return svc
	}

	outcome := d.prober.Probe(ctx, sock.Port)
	svc.Evidence = append(svc.Evidence, outcome.Evidence...)
	switch outcome.Scheme {
	case "http":
		svc.Protocol = "http"
	case "https":
		svc.Protocol = "https"
	default:
		svc.Protocol = "unknown-tcp"
		svc.Confidence = ConfidencePossible
		return svc
	}

	// SPEC 14.4: probe success + process metadata -> very likely;
	// probe success alone -> likely.
	if hasProcess {
		svc.Confidence = ConfidenceVeryLikely
	} else {
		svc.Confidence = ConfidenceLikely
	}
	return svc
}

// selectCandidates filters listeners to local bind addresses with a
// valid port and deduplicates by (family, port), preferring entries
// that carry process metadata.
func selectCandidates(sockets []ListeningSocket) []ListeningSocket {
	type key struct {
		tcp  bool
		port int
	}
	index := make(map[key]int)
	var out []ListeningSocket
	for _, sock := range sockets {
		if sock.Port <= 0 || !isLocalBind(sock.Address) {
			continue
		}
		k := key{tcp: isTCP(sock.Protocol), port: sock.Port}
		if i, ok := index[k]; ok {
			// Keep the duplicate with process metadata.
			if out[i].Process == "" && sock.Process != "" {
				out[i] = sock
			}
			continue
		}
		index[k] = len(out)
		out = append(out, sock)
	}
	return out
}

// isTCP reports whether the ss Netid column denotes a TCP listener.
func isTCP(protocol string) bool {
	return strings.HasPrefix(strings.ToLower(protocol), "tcp")
}

// isLocalBind reports whether the address is loopback or a wildcard
// bind. Discovery inspects only the local machine (SPEC 14.1).
func isLocalBind(addr string) bool {
	switch addr {
	case "127.0.0.1", "::1", "[::1]", "0.0.0.0", "*", "::", "[::]":
		return true
	}
	return false
}

// readProcInfo reads /proc/<pid>/comm and /proc/<pid>/cmdline.
func readProcInfo(pid int) (comm, cmdline string) {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		comm = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		cmdline = strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
	}
	return comm, cmdline
}
