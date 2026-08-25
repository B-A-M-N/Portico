package openaitunnel

import (
	"os"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// TestClientSpecIsNeverBlindlyRestarted pins item 8: the tunnel client must
// not carry RestartAlways. A crash stops the local gateway too, and the
// generic process actor would re-execute the client against the recorded
// --mcp.server-url — a gateway endpoint that no longer exists. Semantic
// restart belongs to supervisor reconciliation, which replans the whole chain
// (gateway, credential, client, /readyz).
func TestClientSpecIsNeverBlindlyRestarted(t *testing.T) {
	p, _ := testProvider(t)
	t.Setenv(CredentialEnvVar, "test-value")
	step := core.PlanStep{ID: "start", Kind: core.StepStartConnector, Technical: core.TechnicalOperation{
		Parameters: map[string]string{
			"tunnel_id":       "tunnel_0123456789abcdef0123456789abcdef",
			"health_url_file": "/tmp/unused.url",
			"mcp_server_url":  "http://127.0.0.1:49152",
		},
	}}
	spec := p.clientProcessSpec(step)
	if spec.Restart != core.RestartNever {
		t.Fatalf("client restart policy = %q; the client is not stateless and must "+
			"not be blindly re-executed by the generic process actor", spec.Restart)
	}
	if os.Getenv(CredentialEnvVar) == "" {
		t.Fatal("environment was not set up")
	}
}
