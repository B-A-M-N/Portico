package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Editing a private-network connection.
//
// The controller has classified changes to the network and the mode since the kind existed,
// and to the published address since it became deliverable. The screen could change none of
// them, so a connection publishing the wrong address could only be deleted and made again.

// networkDetail is a saved private-network connection publishing a service.
func networkDetail(mode, address string) *ipc.ConnectionDetailDTO {
	return &ipc.ConnectionDetailDTO{
		Summary: ipc.ConnectionDTO{
			ID: "conn-network", Name: "api on my network",
			Kind: "private_network", DesiredState: "closed", UserState: "Closed",
			ProviderID: "tailscale",
		},
		Revision: 5,
		Driver:   ipc.DriverSelectionDTO{ProviderID: "tailscale"},
		DesiredSpec: ipc.ConnectionSpecDTO{
			Kind: "private_network",
			PrivateNetwork: &ipc.PrivateNetworkSpecDTO{
				NetworkID: "example.com", Mode: mode,
				ExposeLocal:  mode == "expose",
				LocalAddress: address, LocalProtocol: "http",
			},
		},
		Lifecycle: ipc.LifecycleDTO{OnDisconnect: "keep_alive"},
	}
}

// TestAPublishedAddressCanBeChanged pins the field most likely to be wrong.
func TestAPublishedAddressCanBeChanged(t *testing.T) {
	m := editingModel(t, &fakeClient{}, networkDetail("expose", "127.0.0.1:3000"))

	row := rowFor(t, m, editNetworkAddress)
	if !row.editable {
		t.Fatal("a published address cannot be changed")
	}
	// The explanation says what changing it does, because withdrawing the old address is
	// the consequence a user would not expect.
	if !strings.Contains(row.explain, "withdraws the old one") {
		t.Errorf("the field does not say the old address is withdrawn: %q", row.explain)
	}

	m = selectRow(t, m, editNetworkAddress)
	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)
	if !m.edit.typing {
		t.Fatal("the address did not open an editor")
	}
	if got := m.edit.field.Value(); got != "127.0.0.1:3000" {
		t.Errorf("the field was prefilled with %q, want the current address", got)
	}
	m.edit.field.SetValue("127.0.0.1:8080")
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)

	if !m.edit.dirty() {
		t.Fatal("the change was not recorded")
	}
	req := m.edit.request()
	if req.PrivateNetwork == nil {
		t.Fatal("the request carries no private network arm")
	}
	if req.PrivateNetwork.LocalAddress != "127.0.0.1:8080" {
		t.Fatalf("the address did not reach the request: %q", req.PrivateNetwork.LocalAddress)
	}
	// Unchanged fields carry their current values: the supervisor merges field by field.
	if req.PrivateNetwork.NetworkID != "example.com" {
		t.Errorf("the network was lost: %q", req.PrivateNetwork.NetworkID)
	}
	if req.ExpectedRevision != 5 {
		t.Errorf("expected revision = %d, want the one the edit was built against",
			req.ExpectedRevision)
	}
}

// TestAnAddressWithoutAPortIsRefusedOnScreen pins that validation happens where the value is
// still editable.
func TestAnAddressWithoutAPortIsRefusedOnScreen(t *testing.T) {
	m := editingModel(t, &fakeClient{}, networkDetail("expose", "127.0.0.1:3000"))
	m = selectRow(t, m, editNetworkAddress)
	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)

	for _, bad := range []string{"", "localhost", "service"} {
		m.edit.field.SetValue(bad)
		next, _ = m.Update(keyMsg("enter"))
		m = next.(Model)
		if m.edit.err == "" {
			t.Errorf("the address %q was accepted", bad)
		}
		if m.edit.networkAddress != nil {
			t.Fatalf("the address %q was recorded despite being refused", bad)
		}
	}
}

// TestAJoinIsNotOfferedAnAddress pins that a field the mode has nowhere to put is stated
// rather than hidden.
func TestAJoinIsNotOfferedAnAddress(t *testing.T) {
	m := editingModel(t, &fakeClient{}, networkDetail("join", ""))

	row := rowFor(t, m, editNetworkAddress)
	if row.editable {
		t.Fatal("a join is offered an address to publish")
	}
	if !strings.Contains(row.reason, "no address to publish") {
		t.Errorf("the reason does not explain why: %q", row.reason)
	}
}

// TestSwitchingToAJoinDropsTheAddress pins that an answer does not survive the invalidation
// of the answer it depended on.
//
// A join publishes nothing. Carrying the address forward would send the supervisor a mode
// with an address it has nowhere to put.
func TestSwitchingToAJoinDropsTheAddress(t *testing.T) {
	m := editingModel(t, &fakeClient{}, networkDetail("expose", "127.0.0.1:3000"))

	// Change the address, then switch the mode to a join.
	address := "127.0.0.1:9999"
	m.edit.networkAddress = &address
	m.edit.setChoice(editNetworkMode, "join")

	if m.edit.networkAddress != nil {
		t.Fatal("an address survived a switch to a mode that publishes nothing")
	}
	req := m.edit.request()
	if req.PrivateNetwork == nil {
		t.Fatal("the request carries no private network arm")
	}
	if req.PrivateNetwork.LocalAddress != "" {
		t.Fatalf("a join request carries the address %q", req.PrivateNetwork.LocalAddress)
	}
	if req.PrivateNetwork.ExposeLocal {
		t.Error("a join request is marked as exposing something local")
	}
}

// TestTheModeIsDescribedNotNamed pins the plain-language requirement.
func TestTheModeIsDescribedNotNamed(t *testing.T) {
	m := editingModel(t, &fakeClient{}, networkDetail("expose", "127.0.0.1:3000"))

	row := rowFor(t, m, editNetworkMode)
	if row.current == "expose" || row.current == "join" {
		t.Errorf("the mode is shown as the internal value %q", row.current)
	}
	if !strings.Contains(row.current, "publishes") {
		t.Errorf("the mode does not say what it does: %q", row.current)
	}
	if !row.editable {
		t.Error("the mode cannot be changed")
	}
	// Both answers are offered, and the explanation says neither is public.
	if len(row.choices) != 2 {
		t.Errorf("the mode offers %v", row.choices)
	}
	if !strings.Contains(row.explain, "Neither creates a public address") {
		t.Errorf("the mode does not say both answers stay private: %q", row.explain)
	}
}

// TestAPrivateNetworkIsNotOfferedAnotherKindsFields pins that the rows are this kind's own.
func TestAPrivateNetworkIsNotOfferedAnotherKindsFields(t *testing.T) {
	m := editingModel(t, &fakeClient{}, networkDetail("expose", "127.0.0.1:3000"))

	for _, row := range m.edit.rows() {
		switch row.field {
		case editLocalPort, editRemoteHost, editRemotePort, editForwardProtocol:
			t.Errorf("a private network is offered the port-forward field %q", row.label)
		case editHostname, editExposureMode:
			t.Errorf("a private network is offered the public field %q", row.label)
		}
	}
}
