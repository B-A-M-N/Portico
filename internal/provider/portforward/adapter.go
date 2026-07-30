// Package portforward implements core.Provider for local port forwarding.
//
// A local forward is a relay owned entirely by Portico: it listens on a local
// port and copies bytes to a remote address. There is no provider account, no
// remote resource and no public address, so the whole lifecycle is executable
// without any external service.
//
// Remote forwarding — publishing a local port at a remote endpoint — is not
// implemented, because it requires a provider to terminate the remote side.
// It is refused explicitly rather than silently treated as a local forward.
package portforward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// relay is one running local forward.
type relay struct {
	listener net.Listener
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	target   string
}

// Provider implements local port forwarding.
type Provider struct {
	mu     sync.Mutex
	relays map[core.ConnectionID]*relay
	// dialTimeout bounds the reachability probe and each forwarded connection.
	dialTimeout time.Duration
}

// New creates a local port-forward provider.
func New() *Provider {
	return &Provider{
		relays:      make(map[core.ConnectionID]*relay),
		dialTimeout: 5 * time.Second,
	}
}

// Identity returns the provider identity.
func (p *Provider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID:          "portforward",
		Name:        "portforward",
		DisplayName: "Local port forward",
	}
}

// Capabilities declares a local-only provider.
//
// Nothing here is published. Declaring the public capabilities unsupported is
// what stops the recommendation engine offering this provider for an exposure,
// and stops a public provider being offered for a forward.
func (p *Provider) Capabilities(context.Context) (core.Capabilities, error) {
	return core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{
			Supported: false,
			Notes:     []string{"a forward is local; it creates no public address"},
		},
		CustomHostnames: core.CapabilitySupport{Supported: false},
		ManagedDNS:      core.CapabilitySupport{Supported: false},
		PrivateExposure: core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
		BuiltInProtection: []core.ProtectionCapability{
			// The listener binds loopback only; there is no policy to apply.
			{Kind: core.ProtectionNone, Supported: true, Stability: core.StabilityStable},
		},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolTCP: {Supported: true, Private: true},
		},
		Telemetry:  core.TelemetryCapability{Supported: false},
		Redundancy: core.RedundancyCapability{Supported: false, MaxConnectors: 1},
		Expiration: core.ExpirationCapability{Supported: false},
	}, nil
}

// Authenticate is a no-op: a local forward has no account.
func (p *Provider) Authenticate(context.Context, core.AuthRequest) error { return nil }

// Plan produces the operation plan for a local forward.
func (p *Provider) Plan(_ context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	profile := desired.Profile
	if profile == nil {
		return nil, fmt.Errorf("portforward: profile required")
	}
	if profile.Kind != core.ConnectionPortForward {
		return nil, fmt.Errorf("portforward: connection kind %q is not supported", profile.Kind)
	}
	spec := profile.Spec.PortForward
	if spec == nil {
		return nil, fmt.Errorf("portforward: port forward spec is required")
	}
	if spec.Direction == core.PortForwardRemote {
		return nil, core.ErrValidation(
			"remote port forwarding is not implemented: publishing a local port at a remote endpoint " +
				"requires a provider to terminate the remote side")
	}
	if spec.Protocol == core.ProtocolUDP {
		return nil, core.ErrValidation("UDP forwarding is not implemented; only TCP forwards are supported")
	}

	target := net.JoinHostPort(spec.RemoteHost, fmt.Sprint(spec.RemotePort))
	params := map[string]string{
		"local_port": fmt.Sprint(spec.LocalPort),
		"target":     target,
	}

	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    profile.ID,
		ProfileRevision: profile.Revision,
		Provider:        "portforward",
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}

	switch profile.Desired {
	case core.DesiredOpen:
		plan.Intent = core.IntentOpen
		plan.Steps = []core.PlanStep{
			{
				ID: "forward-verify-target", Kind: core.StepVerifyOrigin,
				Summary:   fmt.Sprintf("Verify %s is reachable", target),
				Technical: core.TechnicalOperation{Provider: "portforward", Type: "verify_target", Parameters: params},
			},
			{
				ID: "forward-start", Kind: core.StepStartConnector,
				Summary:   fmt.Sprintf("Forward 127.0.0.1:%d to %s", spec.LocalPort, target),
				Technical: core.TechnicalOperation{Provider: "portforward", Type: "start_forward", Parameters: params},
			},
		}
		plan.Expected.State = core.RuntimeOpen
		plan.Expected.PrivateAddress = fmt.Sprintf("127.0.0.1:%d", spec.LocalPort)

	case core.DesiredClosed:
		plan.Intent = core.IntentClose
		plan.Steps = []core.PlanStep{
			{
				ID: "forward-stop", Kind: core.StepStopConnector,
				Summary:   "Stop the forward",
				Technical: core.TechnicalOperation{Provider: "portforward", Type: "stop_forward"},
			},
		}
		plan.Expected.State = core.RuntimeClosed

	default:
		return nil, fmt.Errorf("portforward: unsupported desired state %q", profile.Desired)
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("portforward: plan fingerprint: %w", err)
	}
	return plan, nil
}

// ExecuteStep executes one plan step.
func (p *Provider) ExecuteStep(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	switch step.Kind {
	case core.StepVerifyOrigin:
		target := step.Technical.Parameters["target"]
		if target == "" {
			return core.StepResult{StepID: step.ID, Succeeded: false,
				Error: fmt.Errorf("the plan carried no forward target")}, nil
		}
		conn, err := net.DialTimeout("tcp", target, p.dialTimeout)
		if err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false,
				Error: fmt.Errorf("%s is not reachable: %w", target, err)}, nil
		}
		conn.Close()
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepStartConnector:
		if err := p.startForward(connectionID, step); err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}, nil
		}
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepStopConnector:
		p.StopForward(connectionID)
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	default:
		return core.StepResult{StepID: step.ID, Succeeded: false},
			fmt.Errorf("portforward: unsupported step kind %q", step.Kind)
	}
}

// startForward binds the local port and begins relaying.
func (p *Provider) startForward(connectionID core.ConnectionID, step core.PlanStep) error {
	localPort := step.Technical.Parameters["local_port"]
	target := step.Technical.Parameters["target"]
	if localPort == "" || target == "" {
		return fmt.Errorf("the plan did not carry both a local port and a target")
	}

	// Bind loopback only. Binding all interfaces would turn a local forward
	// into an unannounced public listener.
	address := net.JoinHostPort("127.0.0.1", localPort)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", address, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &relay{listener: listener, cancel: cancel, target: target}

	p.mu.Lock()
	if existing := p.relays[connectionID]; existing != nil {
		p.mu.Unlock()
		listener.Close()
		cancel()
		return fmt.Errorf("a forward is already running for this connection")
	}
	p.relays[connectionID] = r
	p.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		p.acceptLoop(ctx, r)
	}()
	return nil
}

func (p *Provider) acceptLoop(ctx context.Context, r *relay) {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			// A closed listener is the normal stop path.
			return
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			p.forward(ctx, client, r.target)
		}()
	}
}

// forward copies bytes in both directions until either side closes.
func (p *Provider) forward(ctx context.Context, client net.Conn, target string) {
	defer client.Close()

	dialer := net.Dialer{Timeout: p.dialTimeout}
	upstream, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// StopForward stops a running forward and waits for its goroutines.
func (p *Provider) StopForward(connectionID core.ConnectionID) {
	p.mu.Lock()
	r := p.relays[connectionID]
	delete(p.relays, connectionID)
	p.mu.Unlock()

	if r == nil {
		return
	}
	r.listener.Close()
	r.cancel()
	r.wg.Wait()
}

// LocalAddr returns the address a running forward is listening on, for tests
// and for observation.
func (p *Provider) LocalAddr(connectionID core.ConnectionID) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.relays[connectionID]
	if r == nil {
		return "", false
	}
	return r.listener.Addr().String(), true
}

// Observe reports whether the forward is listening.
func (p *Provider) Observe(_ context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	observed := &core.ObservedConnection{ConnectionID: id, ProviderID: "portforward"}
	p.mu.Lock()
	r := p.relays[id]
	p.mu.Unlock()

	status := string(core.ConnectorStatusStopped)
	if r != nil {
		status = string(core.ConnectorStatusRunning)
	}
	observed.Connector = &core.ObservedConnector{Status: status}
	return observed, nil
}

// ErrNotRunning reports a forward that is not active.
var ErrNotRunning = errors.New("no forward is running for this connection")
