package tailscale

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// A recorded tailscale client.
//
// The lifecycle is the part most worth holding in place, and a test that could only run
// against a live tailnet would not be run. So the client is faked at the command
// boundary: every invocation is recorded, and the responses are what a real client
// prints. That keeps the tests about Portico's behaviour rather than about Tailscale's.

// fakeRunner answers tailscale subcommands from a script and records what was asked.
type fakeRunner struct {
	mu       sync.Mutex
	invoked  []string
	respond  map[string]func() ([]byte, error)
	fallback func(args []string) ([]byte, error)
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{respond: map[string]func() ([]byte, error){}}
}

// Run records the invocation and returns the scripted answer.
func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	f.mu.Lock()
	f.invoked = append(f.invoked, joined)
	responder := f.respond[joined]
	fallback := f.fallback
	f.mu.Unlock()

	if responder != nil {
		return responder()
	}
	// Prefix match, so a test can script "serve" without naming every argument.
	f.mu.Lock()
	for prefix, r := range f.respond {
		if strings.HasPrefix(joined, prefix) {
			responder = r
			break
		}
	}
	f.mu.Unlock()
	if responder != nil {
		return responder()
	}
	if fallback != nil {
		return fallback(args)
	}
	return nil, fmt.Errorf("fakeRunner: nothing scripted for %q", joined)
}

// on scripts a response for an invocation prefix.
func (f *fakeRunner) on(prefix string, out string, err error) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond[prefix] = func() ([]byte, error) { return []byte(out), err }
	return f
}

// calls returns every invocation, in order.
func (f *fakeRunner) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.invoked...)
}

// called reports whether an invocation with the given prefix was made.
func (f *fakeRunner) called(prefix string) bool {
	for _, call := range f.calls() {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

// The status documents a real client prints, reduced to the fields Portico reads.
const (
	statusRunning = `{
		"BackendState": "Running",
		"Self": {
			"DNSName": "workstation.tail0abc.ts.net.",
			"TailscaleIPs": ["100.101.102.103"],
			"Online": true
		},
		"CurrentTailnet": {"Name": "example.com"}
	}`

	statusNeedsLogin = `{"BackendState": "NeedsLogin", "Self": null, "CurrentTailnet": null}`

	statusNeedsMachineAuth = `{
		"BackendState": "NeedsMachineAuth",
		"Self": {"DNSName": "workstation.tail0abc.ts.net.", "TailscaleIPs": []},
		"CurrentTailnet": {"Name": "example.com"}
	}`

	statusStopped = `{"BackendState": "Stopped", "Self": null, "CurrentTailnet": null}`

	// A machine on a different tailnet than the one a connection names.
	statusOtherTailnet = `{
		"BackendState": "Running",
		"Self": {"DNSName": "workstation.other.ts.net.", "TailscaleIPs": ["100.9.9.9"]},
		"CurrentTailnet": {"Name": "other.example"}
	}`
)

// serveNothing is what the client prints with no serve configured.
const serveNothing = `{}`

// serveHTTP is a web handler proxying to a local address.
func serveHTTP(target string) string {
	return fmt.Sprintf(`{
		"Web": {
			"workstation.tail0abc.ts.net:443": {
				"Handlers": {"/": {"Proxy": %q}}
			}
		}
	}`, "http://"+target)
}
