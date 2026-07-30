package ngrok

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/process"
)

// liveAgent skips unless the real ngrok agent is installed and has a usable
// credential. These tests drive the real agent end to end, because the defects
// this rebuild fixes — a synthesised tunnel identifier, a hardcoded forwarding
// port, deletion that did nothing — were all invisible to a stand-in.
func liveAgent(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ngrok")
	if err != nil {
		t.Skip("ngrok is not installed")
	}
	return bin
}

type liveProcessAdapter struct{ mgr *process.Manager }

func (a liveProcessAdapter) Start(ctx context.Context, cfg core.ProcessConfig) (core.ConnectorHandle, error) {
	mp, err := a.mgr.Start(ctx, process.ProcessConfig{ConnectionID: cfg.ConnectionID, Spec: cfg.Spec})
	if err != nil {
		return core.ConnectorHandle{}, err
	}
	pid := 0
	if mp.Cmd != nil && mp.Cmd.Process != nil {
		pid = mp.Cmd.Process.Pid
	}
	return core.ConnectorHandle{ConnectionID: mp.ConnectionID, Identity: mp.Identity, PID: pid}, nil
}
func (a liveProcessAdapter) Stop(id core.ConnectionID, d time.Duration) error {
	return a.mgr.Stop(id, d)
}
func (a liveProcessAdapter) Observe(id core.ConnectionID) (core.ConnectorHandle, bool) {
	return a.mgr.Observe(id)
}

// localEchoService starts a small HTTP server to forward to, so the tunnel
// points at something real rather than a dead port.
func localEchoService(t *testing.T) (addr string, stop func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "portico-live-origin")
	})}
	go srv.Serve(listener)
	return listener.Addr().String(), func() { srv.Close() }
}

// TestLiveAgentCreatesRealTunnelWithExactIdentity drives the whole open plan
// against the real agent and asserts the properties the scaffolding faked.
func TestLiveAgentCreatesRealTunnelWithExactIdentity(t *testing.T) {
	bin := liveAgent(t)
	originAddr, stopOrigin := localEchoService(t)
	defer stopOrigin()

	mgr := process.NewManager()
	p, err := New("", bin, liveProcessAdapter{mgr: mgr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	connID := core.ConnectionID("live-forward-test")
	profile := newServiceExposureProfile(core.DesiredOpen)
	profile.ID = connID
	profile.Spec.ServiceExposure.Source.Existing.Address = originAddr

	plan, err := p.Plan(context.Background(), core.DesiredConnection{
		Profile: profile,
		Origin:  &core.ResolvedOrigin{URL: "http://" + originAddr},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		if agent, err := p.agentFor(connID); err == nil {
			agent.StopTunnel(context.Background(), tunnelName(connID))
		}
		mgr.Stop(connID, 5*time.Second)
	}()

	var tunnelResource *core.ProviderResource
	for _, step := range plan.Steps {
		res, execErr := p.ExecuteStep(ctx, connID, step)
		if execErr != nil {
			t.Fatalf("ExecuteStep(%s): %v", step.Kind, execErr)
		}
		if !res.Succeeded {
			if step.Kind == core.StepValidateAccount {
				t.Skipf("no usable ngrok credential: %v", res.Error)
			}
			t.Fatalf("step %s failed: %v", step.Kind, res.Error)
		}
		for i := range res.Resources {
			if res.Resources[i].Type == core.ResourceTunnel {
				tunnelResource = &res.Resources[i]
			}
		}
	}

	if tunnelResource == nil {
		t.Fatal("no tunnel resource was recorded")
	}
	// The identifier must come from the agent, not be synthesised from the
	// connection ID as the previous implementation did.
	if strings.Contains(tunnelResource.ExternalID, string(connID)) {
		t.Fatalf("tunnel identifier %q looks synthesised from the connection ID", tunnelResource.ExternalID)
	}
	if tunnelResource.ExternalID == "" {
		t.Fatal("tunnel resource has no external ID")
	}

	publicURL := tunnelResource.Metadata["public_url"]
	if !strings.HasPrefix(publicURL, "https://") {
		t.Fatalf("public URL %q is not a real ngrok URL", publicURL)
	}
	// The forwarding address must be the connection's origin, not a hardcoded
	// port. This is the defect that made every ngrok connection point at 8080.
	if !strings.Contains(tunnelResource.Metadata["forwards"], originAddr) {
		t.Fatalf("tunnel forwards to %q, want the connection origin %q",
			tunnelResource.Metadata["forwards"], originAddr)
	}

	// The published endpoint must actually reach the local service.
	client := &http.Client{Timeout: 20 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, publicURL, nil)
	req.Header.Set("ngrok-skip-browser-warning", "1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the published endpoint is not reachable: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if !strings.Contains(string(body), "portico-live-origin") {
		t.Fatalf("the endpoint did not reach the local service; got %q", string(body))
	}

	// Observation must rebuild state from the agent rather than memory.
	fresh, err := New("", bin, liveProcessAdapter{mgr: mgr})
	if err != nil {
		t.Fatalf("New (fresh): %v", err)
	}
	observed, err := fresh.Observe(ctx, connID)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observed.Tunnel == nil || observed.Tunnel.ID != tunnelResource.ExternalID {
		t.Fatalf("a provider with no prior memory did not rebuild the tunnel: %+v", observed.Tunnel)
	}

	// Telemetry must be real, not declared and absent.
	sample, err := fresh.Telemetry(ctx, connID)
	if err != nil {
		t.Fatalf("Telemetry: %v", err)
	}
	if sample.SampledAt.IsZero() {
		t.Fatal("telemetry sample has no timestamp")
	}
	// The counters themselves are ngrok's and are aggregated on its own
	// schedule, so a specific value is not asserted. What matters here is that
	// telemetry is retrievable at all: the adapter previously declared it
	// unsupported and returned nothing.
	if sample.ConnectionCount < 0 || sample.RequestCount < 0 {
		t.Fatalf("telemetry returned negative counters: %+v", sample)
	}
}

// TestLiveAgentDeletionActuallyRemovesTheTunnel pins the fix for deletion that
// used to be a successful no-op.
func TestLiveAgentDeletionActuallyRemovesTheTunnel(t *testing.T) {
	bin := liveAgent(t)
	originAddr, stopOrigin := localEchoService(t)
	defer stopOrigin()

	mgr := process.NewManager()
	p, err := New("", bin, liveProcessAdapter{mgr: mgr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	connID := core.ConnectionID("live-delete-test")
	profile := newServiceExposureProfile(core.DesiredOpen)
	profile.ID = connID
	profile.Spec.ServiceExposure.Source.Existing.Address = originAddr

	plan, err := p.Plan(context.Background(), core.DesiredConnection{
		Profile: profile,
		Origin:  &core.ResolvedOrigin{URL: "http://" + originAddr},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer mgr.Stop(connID, 5*time.Second)

	for _, step := range plan.Steps {
		res, execErr := p.ExecuteStep(ctx, connID, step)
		if execErr != nil || !res.Succeeded {
			if step.Kind == core.StepValidateAccount {
				t.Skipf("no usable ngrok credential")
			}
			t.Fatalf("step %s failed: %v %v", step.Kind, execErr, res.Error)
		}
	}

	agent, err := p.agentFor(connID)
	if err != nil {
		t.Fatalf("no agent API for the connection: %v", err)
	}
	if _, err := agent.Tunnel(ctx, tunnelName(connID)); err != nil {
		t.Fatalf("tunnel was not created: %v", err)
	}

	// Delete through the plan step, as a close would.
	res, err := p.ExecuteStep(ctx, connID, core.PlanStep{
		ID: "delete", Kind: core.StepDeleteTunnel,
		Technical: core.TechnicalOperation{Parameters: map[string]string{"tunnel_name": tunnelName(connID)}},
	})
	if err != nil || !res.Succeeded {
		t.Fatalf("delete step failed: %v %v", err, res.Error)
	}

	// The tunnel must genuinely be gone from the agent.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := agent.Tunnel(ctx, tunnelName(connID)); err != nil {
			return // gone, as required
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("the tunnel still exists after deletion; deletion is still a no-op")
}
