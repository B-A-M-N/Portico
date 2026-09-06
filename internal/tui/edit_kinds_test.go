package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Editing the properties each connection kind actually has.
//
// The edit screen offered a name, a hostname, a protection mode, its identities,
// two lifecycle answers and an account. The controller has classified changes to
// the source, the protocol, the exposure mode, the local port, the remote target
// and the direction since those kinds existed — so a connection created with the
// wrong address or the wrong port could only be deleted and made again.

// rowFor finds one property on the edit screen.
func rowFor(t *testing.T, m Model, field editableField) editRow {
	t.Helper()
	for _, row := range m.edit.rows() {
		if row.field == field {
			return row
		}
	}
	t.Fatalf("the edit screen does not offer field %d", field)
	return editRow{}
}

// selectRow moves the cursor onto a property.
func selectRow(t *testing.T, m Model, field editableField) Model {
	t.Helper()
	for i, row := range m.edit.rows() {
		if row.field == field {
			m.edit.cursor = i
			return m
		}
	}
	t.Fatalf("the edit screen does not offer field %d", field)
	return m
}

// TestAPortForwardsFieldsCanBeEdited pins that the kind's own properties are
// offered and reach the request.
func TestAPortForwardsFieldsCanBeEdited(t *testing.T) {
	m := editingModel(t, &fakeClient{}, forwardDetail())

	// The forward's properties, and none of a published service's.
	for _, field := range []editableField{
		editLocalPort, editRemoteHost, editRemotePort, editForwardProtocol,
	} {
		row := rowFor(t, m, field)
		if row.current == "" {
			t.Errorf("field %d is offered with no current value", field)
		}
	}
	for _, row := range m.edit.rows() {
		if row.field == editProtection || row.field == editExposureMode {
			t.Errorf("a port forward is offered %q, which it has no spec for", row.label)
		}
	}

	// Change the local port.
	m = selectRow(t, m, editLocalPort)
	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)
	if !m.edit.typing {
		t.Fatal("the local port did not open an editor")
	}
	// The field is prefilled, so correcting a value does not mean retyping it.
	if got := m.edit.field.Value(); got != "15432" {
		t.Errorf("the field was prefilled with %q, want the current port", got)
	}
	m.edit.field.SetValue("15433")
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)

	if !m.edit.dirty() {
		t.Fatal("the change was not recorded")
	}
	req := m.edit.request()
	if req.PortForward == nil {
		t.Fatal("the request carries no forward, so the change could not be applied")
	}
	if req.PortForward.LocalPort != 15433 {
		t.Fatalf("local port = %d, want 15433", req.PortForward.LocalPort)
	}
	// The unchanged fields carry their current values: the supervisor merges field
	// by field, so a zero would be read as port zero rather than as no change.
	if req.PortForward.RemoteHost != "db.internal" || req.PortForward.RemotePort != 5432 {
		t.Fatalf("the unchanged fields were not carried: %+v", req.PortForward)
	}
	if req.ExpectedRevision != 3 {
		t.Fatalf("expected revision = %d, want the one the edit was built against (3)",
			req.ExpectedRevision)
	}
}

// TestAClientTunnelEditsItsEffectiveProperties pins the P0-03 correction: the
// tunnel's ID and MCP origin are editable, while the legacy native client
// profile never appears as a field.
func TestAClientTunnelEditsItsEffectiveProperties(t *testing.T) {
	m := editingModel(t, &fakeClient{}, clientTunnelDetail())

	row := rowFor(t, m, editTunnelID)
	if !row.editable {
		t.Fatal("a tunnel ID cannot be changed")
	}
	for _, row := range m.edit.rows() {
		if strings.EqualFold(row.label, "Client profile") ||
			strings.Contains(strings.ToLower(row.label), "native profile") {
			t.Fatalf("the legacy client profile is exposed as an effective field: %q", row.label)
		}
	}

	m = selectRow(t, m, editTunnelID)
	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)
	m.edit.field.SetValue("tunnel_" + strings.Repeat("f", 32))
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)

	m = selectRow(t, m, editTunnelMCP)
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)
	m.edit.field.SetValue("http://127.0.0.1:9100/mcp")
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)

	req := m.edit.request()
	if req.ClientTunnel == nil {
		t.Fatal("the request carries no client-tunnel arm")
	}
	if req.ClientTunnel.TunnelID != "tunnel_"+strings.Repeat("f", 32) {
		t.Fatalf("tunnel ID = %q", req.ClientTunnel.TunnelID)
	}
	if req.ClientTunnel.MCP.Endpoint != "http://127.0.0.1:9100/mcp" {
		t.Fatalf("MCP endpoint = %q", req.ClientTunnel.MCP.Endpoint)
	}
	if req.ClientTunnel.Profile != "" {
		t.Fatalf("a normal edit re-sent the legacy profile: %q", req.ClientTunnel.Profile)
	}
}

// TestAnInvalidPortIsRefusedOnScreen pins that validation happens where the value
// is still editable.
func TestAnInvalidPortIsRefusedOnScreen(t *testing.T) {
	m := editingModel(t, &fakeClient{}, forwardDetail())
	m = selectRow(t, m, editRemotePort)
	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)

	for _, bad := range []string{"0", "70000", "not-a-port", ""} {
		m.edit.field.SetValue(bad)
		next, _ = m.Update(keyMsg("enter"))
		m = next.(Model)
		if m.edit.err == "" {
			t.Errorf("the port %q was accepted", bad)
		}
		if m.edit.remotePort != nil {
			t.Fatalf("the port %q was recorded despite being refused", bad)
		}
	}
}

// TestUDPIsNotOfferedAsAForwardProtocol pins that an unimplemented transport is
// not a selectable answer.
func TestUDPIsNotOfferedAsAForwardProtocol(t *testing.T) {
	m := editingModel(t, &fakeClient{}, forwardDetail())
	row := rowFor(t, m, editForwardProtocol)

	for _, choice := range row.choices {
		if choice == "udp" {
			t.Fatal("UDP is offered as a forward protocol Portico cannot deliver")
		}
	}
	if row.editable {
		t.Error("the protocol is offered as changeable when there is one answer")
	}
	if row.reason == "" {
		t.Error("the protocol says nothing about why it cannot be changed")
	}
}

// TestServiceExposureSourceAndModeCanBeEdited pins the published-service
// properties the controller classified and the screen could not change.
func TestServiceExposureSourceAndModeCanBeEdited(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	m.snapshot = accountSnapshot()
	m.edit.setProviderContext(
		m.providerCapabilities(m.edit.detail.Driver.ProviderID),
		m.providerAccounts(m.edit.detail.Driver.ProviderID))

	// The address.
	m = selectRow(t, m, editSourceAddress)
	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)
	if !m.edit.typing {
		t.Fatal("the service address did not open an editor")
	}
	m.edit.field.SetValue("127.0.0.1:9090")
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)

	req := m.edit.request()
	if req.Spec == nil || req.Spec.Source.Existing == nil {
		t.Fatal("the request carries no source, so the address could not be changed")
	}
	if req.Spec.Source.Existing.Address != "127.0.0.1:9090" {
		t.Fatalf("address = %q", req.Spec.Source.Existing.Address)
	}
}

// TestProtectionChoicesComeFromTheProvider pins that the answers are what the
// provider can enforce rather than a fixed list.
func TestProtectionChoicesComeFromTheProvider(t *testing.T) {
	t.Run("a provider with an access policy offers it", func(t *testing.T) {
		m := editingModel(t, &fakeClient{}, exposedDetail())
		m.edit.setProviderContext(&ipc.CapabilitySetDTO{
			ProtectionModes: []string{"email_otp"},
		}, nil)

		row := rowFor(t, m, editProtection)
		if !row.editable {
			t.Fatal("a provider that can enforce a policy does not offer one")
		}
		var offered bool
		for _, choice := range row.choices {
			if choice == "email_otp" {
				offered = true
			}
		}
		if !offered {
			t.Fatalf("the declared policy is not offered: %v", row.choices)
		}
	})

	t.Run("a provider with none does not", func(t *testing.T) {
		m := editingModel(t, &fakeClient{}, exposedDetail())
		m.edit.setProviderContext(&ipc.CapabilitySetDTO{}, nil)

		row := rowFor(t, m, editProtection)
		for _, choice := range row.choices {
			if choice == "email_otp" {
				t.Fatal("a policy is offered for a provider that cannot enforce it")
			}
		}
		if row.editable {
			t.Error("protection is changeable for a provider with no policy to apply")
		}
		if row.reason == "" {
			t.Error("the refusal gives no reason")
		}
	})

	t.Run("the current value is always among the choices", func(t *testing.T) {
		// A connection already using a policy must be able to keep it even if the
		// provider's declaration has since narrowed — otherwise opening the edit
		// screen would propose changing something the user did not ask about.
		detail := exposedDetail()
		detail.DesiredSpec.ServiceExposure.Protection.Kind = "email_otp"
		m := editingModel(t, &fakeClient{}, detail)
		m.edit.setProviderContext(&ipc.CapabilitySetDTO{}, nil)

		row := rowFor(t, m, editProtection)
		var present bool
		for _, choice := range row.choices {
			if choice == "email_otp" {
				present = true
			}
		}
		if !present {
			t.Fatalf("the policy in use is not among the choices: %v", row.choices)
		}
	})
}

// TestAccountEditingIsHiddenForAccountlessProviders pins item 14's requirement.
func TestAccountEditingIsHiddenForAccountlessProviders(t *testing.T) {
	m := editingModel(t, &fakeClient{}, forwardDetail())
	// The forward provider has no accounts.
	m.edit.setProviderContext(&ipc.CapabilitySetDTO{}, nil)

	row := rowFor(t, m, editAccount)
	if row.editable {
		t.Fatal("an account selector is offered for a provider that has no accounts")
	}
	if !strings.Contains(row.reason, "does not use accounts") {
		t.Errorf("the reason does not say the provider is accountless: %q", row.reason)
	}
}

// TestTheAccountSelectorShowsLabelsNotIDs pins that a choice between accounts is
// a choice a person can make.
func TestTheAccountSelectorShowsLabelsNotIDs(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	// The connection's own account is among them, so the row names it rather than
	// reporting it as no longer configured.
	m.edit.setProviderContext(&ipc.CapabilitySetDTO{}, []ipc.ProviderAccountDTO{
		{ID: "acct-work", Label: "Work account", Status: "usable"},
		{ID: "acct-personal", Label: "Personal", Status: "usable"},
	})

	row := rowFor(t, m, editAccount)
	if !row.editable {
		t.Fatal("two usable accounts are not offered as a choice")
	}
	if !strings.Contains(row.current, "Work account") && !strings.Contains(row.current, "Personal") {
		t.Errorf("the account is shown as an identifier rather than a name: %q", row.current)
	}

	// An account that cannot be used says why, so a user does not pick it and
	// then find the connection will not open.
	m.edit.setProviderContext(&ipc.CapabilitySetDTO{}, []ipc.ProviderAccountDTO{
		{ID: "acct-work", Label: "Work account", Status: "usable"},
		{ID: "acct-bad", Label: "Old key", Status: "unusable",
			UnusableReason: "the token was revoked"},
	})
	if got := m.edit.accountLabelFor("acct-bad"); !strings.Contains(got, "revoked") {
		t.Errorf("an unusable account does not say why: %q", got)
	}
}

// TestChangingTheModeClearsAHostnameItCannotCarry pins that an answer does not
// survive the invalidation of the answer it depended on.
func TestChangingTheModeClearsAHostnameItCannotCarry(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	m.edit.setProviderContext(&ipc.CapabilitySetDTO{
		TemporaryAddresses: true, CustomHostnames: true,
	}, nil)

	// Set a hostname, then move to a mode that has nowhere to put one.
	hostname := "new.example.com"
	m.edit.hostname = &hostname
	m.edit.setChoice(editExposureMode, "temporary_public")

	if m.edit.hostname != nil {
		t.Fatal("a hostname survived a switch to a mode that assigns its own address")
	}
	req := m.edit.request()
	if req.Spec != nil && req.Spec.Exposure.RequestedAddress == "new.example.com" {
		t.Fatal("the request still asks for a hostname the mode cannot carry")
	}
}
