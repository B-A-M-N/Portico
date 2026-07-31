package screens

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// TestTheInspectScreenDescribesAForwardAsAForward pins the client half of audit
// finding 8. The supervisor can report a port forward correctly and this screen
// can still describe it as an exposed service, because it built its own route
// from parts that carry no kind.
func TestTheInspectScreenDescribesAForwardAsAForward(t *testing.T) {
	m := &InspectModel{
		Connection: &ipc.ConnectionDTO{
			ID: "pf-1", Name: "database", Kind: "port_forward",
			UserState: "Open", PrivateAddress: "127.0.0.1:5432",
			ProviderID: "portforward", ConnectorPID: 42, ConnectorState: "running",
		},
		Detail: &ipc.ConnectionDetailDTO{
			DesiredSpec: ipc.ConnectionSpecDTO{
				Kind:        "port_forward",
				PortForward: &ipc.PortForwardDTO{LocalPort: 5432, RemoteHost: "db.internal", RemotePort: 5432},
			},
			Segments: []ipc.RouteSegmentDTO{
				{ID: "local_route", Label: "Local listener on 127.0.0.1:5432", Status: "ok", Error: "reachable only from this machine"},
				{ID: "connector", Label: "Forwarding process", Status: "ok", Error: "PID 42"},
				{ID: "endpoint", Label: "Remote endpoint db.internal:5432", Status: "ok"},
			},
		},
	}

	overview := strings.Join(m.renderOverview(), "\n")
	if !strings.Contains(overview, "Port forward") {
		t.Errorf("the overview does not say what kind of connection this is:\n%s", overview)
	}
	if strings.Contains(overview, "Public:") {
		t.Errorf("the overview offers a public address for a port forward:\n%s", overview)
	}
	if !strings.Contains(overview, "127.0.0.1:5432") {
		t.Errorf("the overview does not show the listener:\n%s", overview)
	}

	route := strings.Join(m.renderRoute(), "\n")
	for _, claim := range []string{"Provider tunnel", "Public endpoint", "anyone with the address"} {
		if strings.Contains(route, claim) {
			t.Errorf("the route claims %q for a port forward:\n%s", claim, route)
		}
	}
	if !strings.Contains(route, "reachable only from this machine") {
		t.Errorf("the route drops the reachability the supervisor reported:\n%s", route)
	}
}

// TestTheInspectScreenShowsAPublicAddressWhenThereIsOne guards the opposite
// error.
func TestTheInspectScreenShowsAPublicAddressWhenThereIsOne(t *testing.T) {
	m := &InspectModel{
		Connection: &ipc.ConnectionDTO{
			ID: "se-1", Name: "site", Kind: "service_exposure",
			UserState: "Open", PublicAddress: "https://site.example.com",
		},
		Detail: &ipc.ConnectionDetailDTO{
			Segments: []ipc.RouteSegmentDTO{
				{ID: "endpoint", Label: "Public endpoint", Status: "ok", Error: "https://site.example.com"},
			},
		},
	}

	overview := strings.Join(m.renderOverview(), "\n")
	if !strings.Contains(overview, "Public:     https://site.example.com") {
		t.Errorf("a published service no longer shows its address:\n%s", overview)
	}
	if !strings.Contains(overview, "Published service") {
		t.Errorf("a published service is not named as one:\n%s", overview)
	}
}

// TestARouteWithNoSegmentsSaysSoRatherThanDrawingOne pins that the screen does
// not invent a route when the supervisor reported none.
func TestARouteWithNoSegmentsSaysSoRatherThanDrawingOne(t *testing.T) {
	m := &InspectModel{
		Connection: &ipc.ConnectionDTO{ID: "c1", Name: "x", RuntimeState: "closed"},
		Detail:     &ipc.ConnectionDetailDTO{},
	}
	route := strings.Join(m.renderRoute(), "\n")
	if !strings.Contains(route, "No route is established") {
		t.Errorf("an absent route was drawn anyway:\n%s", route)
	}
}
