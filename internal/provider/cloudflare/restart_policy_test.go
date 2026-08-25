package cloudflare

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// TestConnectorSpecIsNeverBlindlyRestarted pins item 20: cloudflared must not
// carry RestartAlways. The generic process actor would re-execute the saved
// argv on crash, which cannot be correct for either mode — a permanent
// relaunch needs a fresh token file (the original is deleted after readiness),
// and a quick-tunnel relaunch receives a NEW trycloudflare.com URL that only
// provider planning can rediscover. Semantic restart belongs to supervisor
// reconciliation through the provider.
func TestConnectorSpecIsNeverBlindlyRestarted(t *testing.T) {
	p, err := NewQuickTunnel("cloudflared", t.TempDir(), fakeConnectorProcessService{})
	if err != nil {
		t.Fatalf("NewQuickTunnel: %v", err)
	}
	spec := p.connectorSpec("conn-1")
	if spec.Restart != core.RestartNever {
		t.Fatalf("connector restart policy = %q; a stateful connector must not be "+
			"blindly re-executed by the generic process actor", spec.Restart)
	}
}
