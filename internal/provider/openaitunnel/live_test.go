//go:build live

package openaitunnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/process"
)

// realClient locates the actual tunnel-client binary. These tests run against
// the real client rather than a stand-in, because a stand-in would happily
// accept invented flags and never reveal that they were wrong — which is
// exactly what happened when this adapter was built from documentation alone.
func realClient(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath(ClientBinary)
	if err != nil {
		t.Skipf("%s is not installed; skipping live client verification", ClientBinary)
	}
	return path
}

// processAdapter bridges the real process manager to the provider contract, so
// the launch path under test is the production one.
type processAdapter struct{ mgr *process.Manager }

func (a processAdapter) Start(ctx context.Context, cfg core.ProcessConfig) (core.ConnectorHandle, error) {
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
func (a processAdapter) Stop(id core.ConnectionID, d time.Duration) error { return a.mgr.Stop(id, d) }
func (a processAdapter) Observe(id core.ConnectionID) (core.ConnectorHandle, bool) {
	return a.mgr.Observe(id)
}

// TestRealClientAcceptsTheGeneratedInvocation is the check that documentation
// alone could not provide: every flag Portico generates must exist on the real
// binary. A wrong flag fails here rather than in production.
func TestRealClientAcceptsTheGeneratedInvocation(t *testing.T) {
	bin := realClient(t)
	t.Setenv(CredentialEnvVar, "sk-invalid-probe-key")

	p := New(bin, nil)
	p.healthURLFile = filepath.Join(t.TempDir(), "health.url")

	profile := tunnelProfile(core.DesiredOpen)
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var start core.PlanStep
	for _, step := range plan.Steps {
		if step.Kind == core.StepStartConnector {
			start = step
		}
	}
	spec := p.clientProcessSpec(start)

	// A flag error surfaces immediately; the client is killed as soon as it
	// gets past parsing, so this does not wait on a network call.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, spec.Executable, spec.Args...)
	cmd.Env = append(os.Environ(), spec.Env...)
	output, _ := cmd.CombinedOutput()

	text := string(output)
	// Flag parsing errors are what this test exists to catch.
	for _, bad := range []string{"unknown flag", "unknown command", "flag provided but not defined"} {
		if strings.Contains(strings.ToLower(text), bad) {
			t.Fatalf("the real client rejected Portico's invocation (%s):\nargs: %v\n%s",
				bad, spec.Args, firstLines(text, 5))
		}
	}
}

// TestRealClientReportsLivenessAndReadinessSeparately verifies against the real
// binary that /healthz and /readyz mean different things.
//
// The client answers /healthz as soon as its HTTP server is up but returns 503
// from /readyz until it has reached the control plane. Treating liveness as
// readiness would report a tunnel as open while it was still unauthenticated.
func TestRealClientReportsLivenessAndReadinessSeparately(t *testing.T) {
	bin := realClient(t)
	t.Setenv(CredentialEnvVar, "sk-invalid-probe-key")

	mgr := process.NewManager()
	p := New(bin, processAdapter{mgr: mgr})
	p.healthURLFile = filepath.Join(t.TempDir(), "health.url")

	profile := tunnelProfile(core.DesiredOpen)
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var start core.PlanStep
	for _, step := range plan.Steps {
		if step.Kind == core.StepStartConnector {
			start = step
		}
	}
	if res, err := p.ExecuteStep(context.Background(), profile.ID, start); err != nil || !res.Succeeded {
		t.Fatalf("start failed: %v %v", err, res.Error)
	}
	defer mgr.Stop(profile.ID, 3*time.Second)

	// Wait for the client to report where its health server is listening.
	var base string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if resolved, err := p.adminBase(); err == nil {
			base = resolved
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if base == "" {
		t.Fatal("the real client never reported a health URL")
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("health URL %q is not loopback", base)
	}

	ctx := context.Background()
	// Liveness succeeds: the process is up.
	if err := p.probe(ctx, base+"/healthz"); err != nil {
		t.Fatalf("the real client did not answer /healthz: %v", err)
	}
	// Readiness fails: the credential is invalid, so it cannot reach the
	// control plane. Portico must not call this open.
	if err := p.probe(ctx, base+"/readyz"); err == nil {
		t.Fatal("/readyz reported ready with an invalid credential; readiness is not being checked")
	}

	// Observation must reflect that distinction rather than reporting running.
	observed, err := p.Observe(ctx, profile.ID)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observed.Connector == nil {
		t.Fatal("no connector observation")
	}
	if observed.Connector.PID == 0 {
		t.Fatal("observation lost the process identity")
	}
}

// TestRealClientRejectsAMalformedTunnelID confirms the identifier format
// Portico validates against is the one the client enforces.
func TestRealClientRejectsAMalformedTunnelID(t *testing.T) {
	bin := realClient(t)
	t.Setenv(CredentialEnvVar, "sk-invalid-probe-key")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "run",
		"--control-plane.tunnel-id", "tunnel_tooshort",
		"--control-plane.api-key", credentialReference,
		"--health.listen-addr", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), CredentialEnvVar+"=sk-invalid-probe-key")
	output, _ := cmd.CombinedOutput()

	if !strings.Contains(string(output), "invalid tunnel ID") {
		t.Fatalf("the real client did not reject a malformed tunnel ID as expected:\n%s", firstLines(string(output), 5))
	}

	// Portico must reject the same value at plan time, before starting anything.
	p := New(bin, nil)
	profile := tunnelProfile(core.DesiredOpen)
	profile.Spec.ClientTunnel.TunnelID = "tunnel_tooshort"
	if _, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile}); err == nil {
		t.Fatal("Portico accepted a tunnel ID the real client rejects")
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
