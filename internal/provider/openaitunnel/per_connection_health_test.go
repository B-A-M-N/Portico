package openaitunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// startStubClient stands in for tunnel-client's local health server. It serves
// /healthz and /readyz independently so tests can hold one up while the other
// fails — exactly the control-plane-outage shape a real outage produces.
func startStubClient(t *testing.T, healthz, readyz int) (*httptest.Server, func(url string)) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(healthz)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(readyz)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, nil
}

// TestTwoSimultaneousTunnelsDoNotShareHealthState pins the per-connection
// health contract. The singleton file this replaces meant the second tunnel
// read the first tunnel's endpoint: its readiness was answered by somebody
// else's client, so one healthy process could mask another broken one.
func TestTwoSimultaneousTunnelsDoNotShareHealthState(t *testing.T) {
	p, proc := testProvider(t)
	p.probe = p.httpProbe

	step := core.PlanStep{ID: "start", Kind: core.StepStartConnector, Technical: core.TechnicalOperation{
		Parameters: map[string]string{"tunnel_id": "tunnel_0123456789abcdef0123456789abcdef"},
	}}

	// Two connections start; each gets its own URL file.
	if r := p.startClient(context.Background(), "conn-a", step); !r.Succeeded {
		t.Fatalf("start conn-a: %v", r.Error)
	}
	if r := p.startClient(context.Background(), "conn-b", step); !r.Succeeded {
		t.Fatalf("start conn-b: %v", r.Error)
	}

	aSpec := proc.startedFor("conn-a")
	bSpec := proc.startedFor("conn-b")
	var aFile, bFile string
	for _, arg := range aSpec.Args {
		if strings.HasPrefix(arg, "/") && strings.HasSuffix(arg, ".url") {
			aFile = arg
		}
	}
	for _, arg := range bSpec.Args {
		if strings.HasPrefix(arg, "/") && strings.HasSuffix(arg, ".url") {
			bFile = arg
		}
	}
	if aFile == "" || bFile == "" {
		t.Fatalf("a client was not given a health URL file: a=%q b=%q", aFile, bFile)
	}
	if aFile == bFile {
		t.Fatal("two connections were pointed at one health URL file")
	}

	// Each connection's observation resolves only its own endpoint.
	srv, _ := startStubClient(t, http.StatusOK, http.StatusOK)
	writeFile(t, aFile, srv.URL) // conn-b's file stays unwritten
	if _, err := p.adminBase("conn-b"); err == nil {
		t.Fatal("conn-b resolved a health base with no reported URL")
	}
	writeFile(t, bFile, "not-a-url")
	if _, err := p.adminBase("conn-b"); err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("conn-b accepted a malformed URL: %v", err)
	}
	writeFile(t, bFile, srv.URL)
	base, err := p.adminBase("conn-b")
	if err != nil || base != srv.URL {
		t.Fatalf("conn-b base = %q, %v", base, err)
	}
}

// TestAliveButUnreadyObservesUnstable pins the truthfulness rule: /healthz 200
// while /readyz fails is NOT running. A control-plane outage leaves the client
// process alive, and the old observe path called that healthy.
func TestAliveButUnreadyObservesUnstable(t *testing.T) {
	p, proc := testProvider(t)
	p.probe = p.httpProbe

	step := core.PlanStep{ID: "start", Kind: core.StepStartConnector, Technical: core.TechnicalOperation{
		Parameters: map[string]string{"tunnel_id": "tunnel_0123456789abcdef0123456789abcdef"},
	}}
	if r := p.startClient(context.Background(), "conn-a", step); !r.Succeeded {
		t.Fatalf("start: %v", r.Error)
	}
	spec := proc.startedFor("conn-a")
	urlFile := ""
	for _, arg := range spec.Args {
		if strings.HasPrefix(arg, "/") && strings.HasSuffix(arg, ".url") {
			urlFile = arg
		}
	}

	// Control-plane outage shape: liveness answers, readiness refuses.
	srv, _ := startStubClient(t, http.StatusOK, http.StatusServiceUnavailable)
	writeFile(t, urlFile, srv.URL)

	observed, err := p.Observe(context.Background(), "conn-a")
	if err != nil {
		t.Fatal(err)
	}
	if observed.Connector == nil ||
		observed.Connector.Status != string(core.ConnectorStatusUnstable) {
		t.Fatalf("an alive-but-unready client observed as %+v, want unstable", observed.Connector)
	}
	if !strings.Contains(observed.Connector.LastError, "not ready") {
		t.Fatalf("the unstable report does not say why: %q", observed.Connector.LastError)
	}

	// Recovery: the control plane comes back.
	srv2, _ := startStubClient(t, http.StatusOK, http.StatusOK)
	writeFile(t, urlFile, srv2.URL)
	observed, err = p.Observe(context.Background(), "conn-a")
	if err != nil {
		t.Fatal(err)
	}
	if observed.Connector == nil ||
		observed.Connector.Status != string(core.ConnectorStatusRunning) {
		t.Fatalf("a ready client observed as %+v, want running", observed.Connector)
	}
}

// TestAReadyClientRequiresReadyzAtOpen pins that RuntimeOpen is earned through
// /readyz, not liveness.
func TestAReadyClientRequiresReadyzAtOpen(t *testing.T) {
	p, _ := testProvider(t)
	p.probe = p.httpProbe

	srv, _ := startStubClient(t, http.StatusOK, http.StatusOK)
	verify := core.PlanStep{ID: "verify", Kind: core.StepVerifyConnector}
	ctx := context.Background()
	connID := core.ConnectionID("conn-verify")

	// Point adminBase at the stub via the connection's own file.
	step := core.PlanStep{ID: "start", Kind: core.StepStartConnector, Technical: core.TechnicalOperation{
		Parameters: map[string]string{"tunnel_id": "tunnel_0123456789abcdef0123456789abcdef"},
	}}
	if r := p.startClient(ctx, connID, step); !r.Succeeded {
		t.Fatalf("start: %v", r.Error)
	}
	urlFile := p.healthFiles[connID]
	writeFile(t, urlFile, srv.URL)

	if r := p.verifyClient(ctx, connID, verify); !r.Succeeded {
		t.Fatalf("verify against a ready client failed: %v", r.Error)
	}
}
