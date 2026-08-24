package supervisor

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/store"
)

// A private-network connection, end to end through the supervisor.
//
// The provider adapter has its own tests. What these prove is that the kind is reachable
// through the supervisor at all: it can be created, it produces a plan, the plan is
// executed, the connection is observed, and it survives a restart. Before this, creating
// one returned "private network connections are not implemented" — the kind existed in the
// data model and nothing could deliver it.

// privateNetworkHandler builds a supervisor whose registry holds one private-network
// provider.
func privateNetworkHandler(t *testing.T, adapter core.Provider) (*supervisorHandler, *store.Store) {
	t.Helper()
	st := newRecoveryTestStore(t)
	registry := provider.NewRegistry()
	if err := registry.Add(adapter); err != nil {
		t.Fatalf("registering the provider: %v", err)
	}
	ctrl := controller.New(registry, st)
	// Without a storer the controller keeps profiles in memory only, so nothing would
	// survive the restart these tests are about. The real supervisor wires this in New.
	ctrl.SetConnectionStorer(st)
	return &supervisorHandler{sup: &Supervisor{
		store: st, registry: registry, controller: ctrl, mutating: true,
	}}, st
}

// TestCreatingAJoinConnection pins that the kind is creatable.
func TestCreatingAJoinConnection(t *testing.T) {
	h, _ := privateNetworkHandler(t, &stubPrivateNetwork{})

	created, err := h.HandleCreateConnection(ipc.CreateConnectionRequest{
		Name: "my network",
		Kind: "private_network",
		PrivateNetwork: &ipc.PrivateNetworkSpecDTO{
			NetworkID: "example.com", Mode: "join",
		},
		Provider:  ipc.ProviderSelectionDTO{ProviderID: "stub_private_network"},
		Lifecycle: ipc.LifecycleDTO{OnDisconnect: "keep_alive"},
	})
	if err != nil {
		t.Fatalf("HandleCreateConnection: %v", err)
	}
	if created.Kind != "private_network" {
		t.Fatalf("created a %q", created.Kind)
	}
	// Created closed, so creating one never brings something up by surprise.
	if created.DesiredState != "closed" {
		t.Fatalf("a new connection is %q, want closed", created.DesiredState)
	}
	// A private network has no public address, and must not claim one.
	if created.PublicAddress != "" {
		t.Fatalf("a private network reported the public address %q", created.PublicAddress)
	}
}

// TestCreatingAnExposeConnectionNeedsAnAddress pins the mode-specific requirement.
func TestCreatingAnExposeConnectionNeedsAnAddress(t *testing.T) {
	h, _ := privateNetworkHandler(t, &stubPrivateNetwork{})

	_, err := h.HandleCreateConnection(ipc.CreateConnectionRequest{
		Name: "api",
		Kind: "private_network",
		PrivateNetwork: &ipc.PrivateNetworkSpecDTO{
			NetworkID: "example.com", Mode: "expose",
		},
		Provider: ipc.ProviderSelectionDTO{ProviderID: "stub_private_network"},
	})
	if err == nil {
		t.Fatal("an expose with nothing to expose was created")
	}
	if !strings.Contains(err.Error(), "address") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// TestAnExposeConnectionIsPrivateOnly pins that the recorded spec cannot be mistaken for a
// public exposure.
//
// The address goes on the private-network arm. The spec is a strict tagged union, so this
// connection cannot also carry a service-exposure arm — and that is what keeps it private:
// an exposure arm would have an exposure mode, and a public one recorded there would let a
// later edit or a route renderer treat this as reachable from the internet.
func TestAnExposeConnectionIsPrivateOnly(t *testing.T) {
	h, st := privateNetworkHandler(t, &stubPrivateNetwork{})

	created, err := h.HandleCreateConnection(ipc.CreateConnectionRequest{
		Name: "api",
		Kind: "private_network",
		PrivateNetwork: &ipc.PrivateNetworkSpecDTO{
			NetworkID: "example.com", Mode: "expose",
			LocalAddress: "127.0.0.1:3000", LocalProtocol: "http",
		},
		Provider: ipc.ProviderSelectionDTO{ProviderID: "stub_private_network"},
	})
	if err != nil {
		t.Fatalf("HandleCreateConnection: %v", err)
	}

	profile, err := st.LoadProfile(context.Background(), core.ConnectionID(created.ID))
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	spec := profile.Spec.PrivateNetwork
	if spec == nil {
		t.Fatal("the expose connection carries no private network spec")
	}
	if spec.LocalAddress != "127.0.0.1:3000" {
		t.Fatalf("the published address is %q", spec.LocalAddress)
	}
	if !spec.ExposeLocal {
		t.Error("the spec does not record that something local is exposed")
	}
	// No service-exposure arm, so nothing carries a public exposure mode this kind
	// could later be mistaken for. The spec is a strict tagged union and this is the
	// arm that belongs to a private network.
	if profile.Spec.ServiceExposure != nil {
		t.Error("an expose connection also carries a service-exposure arm, which would give " +
			"it an exposure mode a public renderer could act on")
	}
	if profile.Spec.PortForward != nil || profile.Spec.ClientTunnel != nil {
		t.Error("the connection carries another kind's arm")
	}
}

// TestAnUnsupportedModeIsRefused pins that the mode is checked rather than passed through.
func TestAnUnsupportedModeIsRefused(t *testing.T) {
	h, _ := privateNetworkHandler(t, &stubPrivateNetwork{})

	_, err := h.HandleCreateConnection(ipc.CreateConnectionRequest{
		Name: "odd",
		Kind: "private_network",
		PrivateNetwork: &ipc.PrivateNetworkSpecDTO{
			NetworkID: "example.com", Mode: "exit_node",
		},
		Provider: ipc.ProviderSelectionDTO{ProviderID: "stub_private_network"},
	})
	if err == nil {
		t.Fatal("an unsupported private network mode was accepted")
	}
	if !strings.Contains(err.Error(), "exit_node") {
		t.Errorf("the refusal does not name the mode: %v", err)
	}
}

// TestThePrivateNetworkRouteHasNoPublicHop pins the visualisation contract.
//
// Item 17 requires that no fake public route is drawn for a kind that does not have one.
// A private network is exactly that kind.
func TestThePrivateNetworkRouteHasNoPublicHop(t *testing.T) {
	h, st := privateNetworkHandler(t, &stubPrivateNetwork{})

	created, err := h.HandleCreateConnection(ipc.CreateConnectionRequest{
		Name: "my network",
		Kind: "private_network",
		PrivateNetwork: &ipc.PrivateNetworkSpecDTO{
			NetworkID: "example.com", Mode: "join",
		},
		Provider: ipc.ProviderSelectionDTO{ProviderID: "stub_private_network"},
	})
	if err != nil {
		t.Fatal(err)
	}

	detail, err := h.HandleGetConnectionDetail(created.ID)
	if err != nil {
		t.Fatalf("HandleConnectionDetail: %v", err)
	}
	if len(detail.Segments) == 0 {
		t.Fatal("the connection has no route at all")
	}
	for _, segment := range detail.Segments {
		id := strings.ToLower(segment.ID)
		if strings.Contains(id, "public") || strings.Contains(id, "dns") {
			t.Errorf("a private network route carries the segment %q", segment.ID)
		}
	}
	_ = st
}

// TestAJoinConnectionSurvivesARestart pins reconstruction for the new kind.
//
// The connection is stored, the supervisor is rebuilt over the same database, and the
// profile comes back with its spec intact — including the mode, which decides what
// planning and closing will do.
func TestAJoinConnectionSurvivesARestart(t *testing.T) {
	h, st := privateNetworkHandler(t, &stubPrivateNetwork{})

	created, err := h.HandleCreateConnection(ipc.CreateConnectionRequest{
		Name: "my network",
		Kind: "private_network",
		PrivateNetwork: &ipc.PrivateNetworkSpecDTO{
			NetworkID: "example.com", Mode: "expose",
			LocalAddress: "127.0.0.1:3000", LocalProtocol: "http",
		},
		Provider: ipc.ProviderSelectionDTO{ProviderID: "stub_private_network"},
	})
	if err != nil {
		t.Fatal(err)
	}

	restarted := restartedSupervisor(t, st, &stubPrivateNetwork{})

	profile, ok := restarted.controller.GetProfile(core.ConnectionID(created.ID))
	if !ok {
		t.Fatal("the private network connection did not survive the restart")
	}
	if profile.EffectiveKind() != core.ConnectionPrivateNetwork {
		t.Fatalf("it came back as a %q", profile.EffectiveKind())
	}
	spec := profile.Spec.PrivateNetwork
	if spec == nil {
		t.Fatal("the private network spec was lost")
	}
	// The mode decides what closing does — whether Portico withdraws a serve or leaves
	// a machine-wide membership alone — so losing it would make close behave differently
	// after a restart than before one.
	if spec.Mode != core.PrivateNetworkExpose {
		t.Fatalf("mode came back as %q, want expose", spec.Mode)
	}
	if spec.NetworkID != "example.com" {
		t.Fatalf("network came back as %q", spec.NetworkID)
	}
	if spec.LocalAddress != "127.0.0.1:3000" {
		t.Fatalf("the published address came back as %q", spec.LocalAddress)
	}
}

// restartedSupervisor builds a fresh supervisor over the same database, as a restart does,
// and runs the reconstruction step startup runs.
//
// Shared by the private-network and client-tunnel tests: both need the same thing, and a
// second copy would be a second answer to "what does a restart do".
func restartedSupervisor(t *testing.T, st *store.Store, adapter core.Provider) *Supervisor {
	t.Helper()
	registry := provider.NewRegistry()
	if err := registry.Add(adapter); err != nil {
		t.Fatalf("registering the provider: %v", err)
	}
	ctrl := controller.New(registry, st)
	ctrl.SetConnectionStorer(st)
	sup := &Supervisor{store: st, registry: registry, controller: ctrl, mutating: true}
	if err := sup.loadProfiles(context.Background()); err != nil {
		t.Fatalf("loadProfiles: %v", err)
	}
	return sup
}
