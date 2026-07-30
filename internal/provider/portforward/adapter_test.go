package portforward

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// echoServer starts a TCP server that echoes what it receives.
func echoServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String(), func() { listener.Close(); <-done }
}

func forwardProfile(localPort int, remoteHost string, remotePort int, direction core.PortForwardDirection) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID:       "conn-forward",
		Name:     "forward",
		Revision: 1,
		Kind:     core.ConnectionPortForward,
		Spec: core.ConnectionSpec{
			PortForward: &core.PortForwardSpec{
				LocalPort:  localPort,
				RemoteHost: remoteHost,
				RemotePort: remotePort,
				Protocol:   core.ProtocolTCP,
				Direction:  direction,
			},
		},
		Driver:  core.DriverSelection{ProviderID: "portforward"},
		Desired: core.DesiredOpen,
	}
}

// TestLocalForwardActuallyForwardsTraffic is the end-to-end proof that this
// connection kind executes, rather than merely persisting and validating.
func TestLocalForwardActuallyForwardsTraffic(t *testing.T) {
	target, stopTarget := echoServer(t)
	defer stopTarget()

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatal(err)
	}
	var remotePort int
	fmt.Sscanf(portStr, "%d", &remotePort)

	p := New()
	// Port 0 lets the kernel choose, so the test never collides with another.
	profile := forwardProfile(0, host, remotePort, core.PortForwardLocal)

	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for _, step := range plan.Steps {
		res, execErr := p.ExecuteStep(context.Background(), profile.ID, step)
		if execErr != nil {
			t.Fatalf("ExecuteStep(%s): %v", step.Kind, execErr)
		}
		if !res.Succeeded {
			t.Fatalf("step %s failed: %v", step.Kind, res.Error)
		}
	}
	defer p.StopForward(profile.ID)

	local, ok := p.LocalAddr(profile.ID)
	if !ok {
		t.Fatal("forward is not listening")
	}

	conn, err := net.DialTimeout("tcp", local, 3*time.Second)
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	const payload = "portico forward round trip"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("round trip returned %q, want %q", buf, payload)
	}
}

// TestForwardBindsLoopbackOnly ensures a local forward does not become an
// unannounced public listener.
func TestForwardBindsLoopbackOnly(t *testing.T) {
	target, stopTarget := echoServer(t)
	defer stopTarget()
	host, portStr, _ := net.SplitHostPort(target)
	var remotePort int
	fmt.Sscanf(portStr, "%d", &remotePort)

	p := New()
	profile := forwardProfile(0, host, remotePort, core.PortForwardLocal)
	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, step := range plan.Steps {
		if _, err := p.ExecuteStep(context.Background(), profile.ID, step); err != nil {
			t.Fatalf("ExecuteStep: %v", err)
		}
	}
	defer p.StopForward(profile.ID)

	local, _ := p.LocalAddr(profile.ID)
	if !strings.HasPrefix(local, "127.0.0.1:") {
		t.Fatalf("forward listens on %q, which is not loopback-only", local)
	}
}

// TestStoppingAForwardReleasesThePort ensures the lifecycle is complete.
func TestStoppingAForwardReleasesThePort(t *testing.T) {
	target, stopTarget := echoServer(t)
	defer stopTarget()
	host, portStr, _ := net.SplitHostPort(target)
	var remotePort int
	fmt.Sscanf(portStr, "%d", &remotePort)

	p := New()
	profile := forwardProfile(0, host, remotePort, core.PortForwardLocal)
	plan, _ := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	for _, step := range plan.Steps {
		p.ExecuteStep(context.Background(), profile.ID, step)
	}
	local, _ := p.LocalAddr(profile.ID)

	p.StopForward(profile.ID)

	if _, ok := p.LocalAddr(profile.ID); ok {
		t.Fatal("forward still registered after stop")
	}
	// The port must be reusable.
	listener, err := net.Listen("tcp", local)
	if err != nil {
		t.Fatalf("port %s was not released: %v", local, err)
	}
	listener.Close()
}

// TestUnreachableTargetFailsBeforeBinding ensures a forward to nowhere does not
// leave a listener accepting connections it cannot serve.
func TestUnreachableTargetFailsBeforeBinding(t *testing.T) {
	p := New()
	p.dialTimeout = 200 * time.Millisecond
	profile := forwardProfile(0, "127.0.0.1", 1, core.PortForwardLocal)

	plan, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Steps[0].Kind != core.StepVerifyOrigin {
		t.Fatalf("first step is %s, want the target probe", plan.Steps[0].Kind)
	}
	res, err := p.ExecuteStep(context.Background(), profile.ID, plan.Steps[0])
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if res.Succeeded {
		t.Fatal("verification succeeded against an unreachable target")
	}
	if _, listening := p.LocalAddr(profile.ID); listening {
		t.Fatal("a listener was bound despite the target being unreachable")
	}
}

// TestRemoteForwardingIsRefusedNotSilentlyLocalised pins the honest boundary:
// remote forwarding needs a provider to terminate the remote side.
func TestRemoteForwardingIsRefusedNotSilentlyLocalised(t *testing.T) {
	p := New()
	profile := forwardProfile(8080, "example.com", 80, core.PortForwardRemote)

	_, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err == nil {
		t.Fatal("remote forwarding was accepted and would have been treated as local")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("refusal does not say it is unimplemented: %v", err)
	}
}

// TestUDPForwardingIsRefused keeps the protocol claim honest.
func TestUDPForwardingIsRefused(t *testing.T) {
	p := New()
	profile := forwardProfile(0, "127.0.0.1", 9, core.PortForwardLocal)
	profile.Spec.PortForward.Protocol = core.ProtocolUDP

	if _, err := p.Plan(context.Background(), core.DesiredConnection{Profile: profile}); err == nil {
		t.Fatal("UDP forwarding was accepted despite only TCP being implemented")
	}
}

// TestForwardCapabilitiesAreLocalOnly ensures the recommendation engine cannot
// offer this provider for a public exposure.
func TestForwardCapabilitiesAreLocalOnly(t *testing.T) {
	caps, err := New().Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.TemporaryAddresses.Supported || caps.CustomHostnames.Supported || caps.ManagedDNS.Supported {
		t.Fatal("a local forward declares a public-exposure capability")
	}
	if !caps.PrivateExposure.Supported {
		t.Fatal("private exposure is not declared supported")
	}
	if pc := caps.Protocols[core.ProtocolTCP]; pc.Public {
		t.Fatal("TCP is marked public")
	}
}
