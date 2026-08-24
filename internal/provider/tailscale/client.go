// Package tailscale implements core.Provider for Portico's private_network
// connection kind.
//
// # What is mapped, and why
//
// Portico's `private_network` spec has a network identifier and two modes: `join`
// and `expose`. Tailscale has two primitives that answer exactly those:
//
//   - `tailscale up` brings this machine onto a tailnet. That is `join`: the machine
//     becomes reachable by the other devices on the network, and nothing local is
//     published.
//   - `tailscale serve` publishes one local address to the tailnet, and only to the
//     tailnet. That is `expose`: a service on this machine becomes reachable by the other
//     devices, with no public address anywhere.
//
// Nothing else in Tailscale is implemented. `tailscale funnel` publishes to the whole
// internet, which is a public exposure and belongs to `service_exposure` and its
// providers — offering it here would let a user pick "private network" and get a
// public address. Exit nodes, subnet routes, ACL editing and device management are
// Tailscale features, not answers to a question Portico's model asks.
//
// # Why the CLI rather than the API
//
// The local daemon is what actually joins the network and publishes the service, and
// `tailscale` is its supported interface. The control-plane HTTP API manages devices
// and ACLs in a tailnet and cannot bring this machine onto it, so an API-based adapter
// could report on the network without being able to join it. Portico needs the
// machine's own membership, so the CLI is not a shortcut here — it is the correct
// dependency.
//
// # What Portico owns
//
// Joining is a machine-wide state that predates Portico and outlives it, so Portico
// does not own it: a connection that finds the machine already joined records that and
// does not claim it, and closing such a connection does not log the machine out.
// Serving is per-address and Portico does own what it created, so closing a serve
// connection withdraws exactly the serve it configured.
//
// # ServeRoute is the canonical representation
//
// All Serve-specific code operates on `ServeRoute` defined in `serve.go`. There are no
// parallel meanings (string target, map[string]bool, backend-as-identity). One route
// type, one normalization, one identity.
package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Binary is the client Portico drives.
const Binary = "tailscale"

// CommandRunner runs a tailscale subcommand and returns its combined output.
//
// It is an interface so the lifecycle can be tested against recorded invocations
// rather than a live tailnet. A test that could only run against a real network would
// not be run, and the lifecycle is the part most worth holding in place.
type CommandRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// execRunner runs the real binary.
type execRunner struct {
	binary  string
	timeout time.Duration
}

// NewExecRunner builds a runner for the installed client.
func NewExecRunner(binary string) CommandRunner {
	if binary == "" {
		binary = Binary
	}
	// Every call is bounded. `tailscale up` on a machine that needs to authenticate
	// will otherwise wait for a browser login that is never going to happen inside a
	// supervisor, and the operation would hang rather than reporting what is needed.
	return &execRunner{binary: binary, timeout: 20 * time.Second}
}

func (r *execRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.binary, args...).CombinedOutput()
	if err != nil {
		// The client's own message is the useful part — "logged out", "needs login",
		// "serve config conflicts" — so it is carried rather than replaced.
		return out, fmt.Errorf("%s %s: %w: %s",
			r.binary, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Status is the part of `tailscale status --json` Portico reads.
//
// Only the fields the lifecycle depends on are declared. Decoding the whole document
// would couple Portico to fields it does not use and break when they change.
type Status struct {
	// BackendState is "Running", "Stopped", "NeedsLogin", "NeedsMachineAuth" or
	// "NoState".
	BackendState string `json:"BackendState"`
	// Self describes this machine.
	Self *StatusDevice `json:"Self"`
	// CurrentTailnet names the network this machine is on, when it is on one.
	CurrentTailnet *StatusTailnet `json:"CurrentTailnet"`
}

// StatusDevice is one device on the tailnet.
type StatusDevice struct {
	// ID is the stable Tailscale node identifier. It does not change when the
	// device is renamed, and is the identity Portico uses for membership.
	ID string `json:"ID"`
	// DNSName is the machine's name on the tailnet, with a trailing dot. It is
	// human-visible and mutable, so it is not identity.
	DNSName string `json:"DNSName"`
	// TailscaleIPs are the addresses other devices reach this machine at.
	TailscaleIPs []string `json:"TailscaleIPs"`
	// Online reports whether the control plane currently considers it reachable.
	Online bool `json:"Online"`
}

// StatusTailnet identifies the network.
type StatusTailnet struct {
	Name string `json:"Name"`
}

// Joined reports whether this machine is on a tailnet and running.
func (s *Status) Joined() bool {
	return s != nil && s.BackendState == "Running"
}

// NeedsLogin reports whether the daemon is waiting to be authenticated.
//
// This is the state that must not be reported as a failure to join: nothing is broken,
// the machine has not been authorised yet, and the user has to do that.
func (s *Status) NeedsLogin() bool {
	return s != nil && (s.BackendState == "NeedsLogin" || s.BackendState == "NoState")
}

// NeedsMachineAuth reports that the tailnet admin has not yet approved this device.
func (s *Status) NeedsMachineAuth() bool {
	return s != nil && s.BackendState == "NeedsMachineAuth"
}

// TailnetName is the network this machine is on, or empty.
func (s *Status) TailnetName() string {
	if s == nil || s.CurrentTailnet == nil {
		return ""
	}
	return s.CurrentTailnet.Name
}

// MachineName is this machine's name on the tailnet, without the trailing dot.
func (s *Status) MachineName() string {
	if s == nil || s.Self == nil {
		return ""
	}
	return strings.TrimSuffix(s.Self.DNSName, ".")
}

// NodeID is the stable Tailscale node identifier, or a DNS-name-based fallback for
// resource rows written by an earlier implementation that did not record it. The
// fallback keeps existing connections working across an upgrade; new rows always
// carry the real ID.
func (s *Status) NodeID() string {
	if s == nil || s.Self == nil {
		return ""
	}
	if id := strings.TrimSpace(s.Self.ID); id != "" {
		return id
	}
	// Compatibility: an older Portico stored the DNS name as identity. Match it so
	// the resource is still recognised.
	return s.MachineName()
}

// PrivateAddress is the address other devices on the tailnet reach this machine at.
//
// The DNS name is preferred over the IP because it is stable across reconnections and
// is what a user would type. The IP is the fallback for a tailnet without MagicDNS.
func (s *Status) PrivateAddress() string {
	if name := s.MachineName(); name != "" {
		return name
	}
	if s != nil && s.Self != nil && len(s.Self.TailscaleIPs) > 0 {
		return s.Self.TailscaleIPs[0]
	}
	return ""
}

// readStatus asks the client what state the machine is in.
func readStatus(ctx context.Context, runner CommandRunner) (*Status, error) {
	out, err := runner.Run(ctx, "status", "--json")
	if err != nil {
		// `tailscale status` exits non-zero when logged out but still prints usable
		// JSON, so the output is parsed before the error is reported. Treating the
		// exit code as authoritative would report a logged-out machine as an
		// unreachable daemon, which sends the user looking in the wrong place.
		if status, parseErr := parseStatus(out); parseErr == nil && status.BackendState != "" {
			return status, nil
		}
		return nil, err
	}
	return parseStatus(out)
}

// parseStatus decodes what the client printed.
func parseStatus(out []byte) (*Status, error) {
	var status Status
	if err := json.Unmarshal(out, &status); err != nil {
		return nil, fmt.Errorf("could not read the tailscale client's status output: %w", err)
	}
	return &status, nil
}
