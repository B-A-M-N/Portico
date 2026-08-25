package supervisor

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/core"
)

// Whether a connection can be repaired is one answer.
//
// The TUI kept its own list of repairable kinds, the Inspect handler kept a
// second, differently-worded one, and the controller kept the real one. The
// interface therefore refused a repair the backend supported the moment port
// forwarding became repairable.
//
// The answer now travels on the connection DTO. The projection names the kinds
// because it runs without a controller in hand, so this pins the two against each
// other: neither can move without the other.

// TestRepairabilityMatchesTheController pins the projection against CanRepair.
func TestRepairabilityMatchesTheController(t *testing.T) {
	c := controller.New(nil, nil)

	for _, kind := range []core.ConnectionKind{
		core.ConnectionServiceExposure,
		core.ConnectionPortForward,
		core.ConnectionPrivateNetwork,
		core.ConnectionClientTunnel,
	} {
		profile := &core.ConnectionProfile{ID: "conn-1", Kind: kind}
		want := c.CanRepair(profile)
		got := kindSupportsRepair(kind)
		if got != want {
			t.Errorf("%s: the projection says repairable=%v, the controller says %v",
				kind, got, want)
		}
	}
}

// TestTheConnectionDTOCarriesRepairability pins that a client can read the answer
// rather than deriving it.
func TestTheConnectionDTOCarriesRepairability(t *testing.T) {
	forward := &core.ConnectionProfile{
		ID:   "conn-forward",
		Name: "database",
		Kind: core.ConnectionPortForward,
		Spec: core.ConnectionSpec{
			PortForward: &core.PortForwardSpec{
				LocalPort: 15432, RemoteHost: "db.internal", RemotePort: 5432,
				Protocol: core.ProtocolTCP, Direction: core.PortForwardLocal,
			},
		},
	}
	if dto := connectionSummaryDTO(forward, nil); !dto.Repairable {
		t.Error("a port forward is projected as unrepairable")
	}

	tunnel := &core.ConnectionProfile{
		ID: "conn-tunnel", Name: "mcp", Kind: core.ConnectionClientTunnel,
	}
	if dto := connectionSummaryDTO(tunnel, nil); !dto.Repairable {
		t.Error("a client tunnel is projected as unrepairable")
	}
}
