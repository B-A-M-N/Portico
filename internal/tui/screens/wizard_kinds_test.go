package screens

import (
	"strings"
	"testing"
)

// Creating each connection kind the product supports.
//
// Required coverage item 15 asks for the whole lifecycle of every supported kind.
// This is the create half at the wizard level: the questions a user answers and
// the request those answers produce. The preview, open, inspect, edit, repair,
// close and delete halves live with the surfaces that own them.
//
// A kind that cannot be built here cannot be built at all, which is how port
// forwarding came to have a full provider and an unreachable question — its text
// fields were absent from isTextStep, so they could not receive a keystroke.

// Text is entered with typeInto (field_test.go), which routes through Update the
// way a keystroke does. That is deliberate: a question whose field is not wired
// to receive keystrokes passes a test that sets the value directly and fails for
// every real user, which is exactly the state the port-forward and tunnel
// questions were in.

// takeOutcome selects the recipe for a connection kind and names the connection.
func takeOutcome(t *testing.T, m *WizardModel, kind, name string) {
	t.Helper()
	index := -1
	for i, recipe := range wizardRecipes {
		if recipe.ConnectionKind == kind {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatalf("no outcome offers the %s kind", kind)
	}
	for range index {
		m.HandleKey("down")
	}
	m.HandleKey("enter")
	if m.Step() == WizardStepOutcome {
		t.Fatalf("the %s outcome was refused: %v", kind, m.err)
	}
	typeInto(m, name)
	m.HandleKey("enter")
}

// TestCreatingAPortForward pins the forward kind end to end.
func TestCreatingAPortForward(t *testing.T) {
	m := NewWizard(nil, quickTunnelOnlySnapshot())
	takeOutcome(t, m, "port_forward", "database")

	if m.Step() != WizardStepPortForwardLocalPort {
		t.Fatalf("step after naming = %d, want the local port question", m.Step())
	}

	typeInto(m, "15432")
	if got := m.inputValue(); got != "15432" {
		t.Fatalf("the local port field holds %q: the question cannot be answered", got)
	}
	m.HandleKey("enter")

	typeInto(m, "db.internal")
	m.HandleKey("enter")
	typeInto(m, "5432")
	m.HandleKey("enter")

	if m.Step() != WizardStepPortForwardProtocol {
		t.Fatalf("step after the remote port = %d, err %v", m.Step(), m.err)
	}
	m.HandleKey("enter") // TCP, the only available answer

	if m.Step() != WizardStepReview {
		t.Fatalf("step after the protocol = %d, want review", m.Step())
	}

	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Kind != "port_forward" {
		t.Fatalf("kind = %q", req.Kind)
	}
	if req.PortForward == nil {
		t.Fatal("the request carries no forward")
	}
	if req.PortForward.LocalPort != 15432 || req.PortForward.RemotePort != 5432 {
		t.Fatalf("ports = %d -> %d", req.PortForward.LocalPort, req.PortForward.RemotePort)
	}
	if req.PortForward.RemoteHost != "db.internal" {
		t.Fatalf("remote host = %q", req.PortForward.RemoteHost)
	}
	if req.PortForward.Protocol != "tcp" {
		t.Fatalf("protocol = %q, want tcp", req.PortForward.Protocol)
	}
	// A forward has no source, exposure or protection: sending them would ask the
	// supervisor to change the kind, which it refuses.
	if req.Source.Kind != "" || req.Exposure.Mode != "" || req.Protection.Kind != "" {
		t.Errorf("the forward request carries service-exposure arms: %+v", req)
	}
}

// TestAPortForwardRefusesAnInvalidPort pins that validation happens on the
// question, where the value is still editable.
func TestAPortForwardRefusesAnInvalidPort(t *testing.T) {
	for _, bad := range []string{"0", "70000", "abc"} {
		m := NewWizard(nil, quickTunnelOnlySnapshot())
		takeOutcome(t, m, "port_forward", "database")

		typeInto(m, bad)
		m.HandleKey("enter")
		if m.Step() != WizardStepPortForwardLocalPort {
			t.Errorf("the port %q was accepted", bad)
		}
		if m.err == nil {
			t.Errorf("the port %q produced no error", bad)
		}
	}
}

// TestCreatingAPublishedService pins the service-exposure kind end to end.
func TestCreatingAPublishedService(t *testing.T) {
	m := NewWizard(nil, quickTunnelOnlySnapshot())
	takeOutcome(t, m, "service_exposure", "web")

	// The first outcome is a temporary share of an existing service, so the
	// service question comes next. With no scan result the only choice is manual
	// entry, which opens the address field.
	if m.Step() != WizardStepDiscovery {
		t.Fatalf("step after naming = %d, want the service question", m.Step())
	}
	m.HandleKey("enter")
	if m.Step() != WizardStepSource {
		t.Fatalf("manual entry did not open the address question: step %d", m.Step())
	}

	typeInto(m, "127.0.0.1:3000")
	m.HandleKey("enter")

	// Port, protocol, then the questions the outcome already answered are skipped.
	for m.Step() != WizardStepReview && m.Step() != WizardStepProvider {
		before := m.Step()
		m.HandleKey("enter")
		if m.Step() == before {
			t.Fatalf("the wizard stalled on step %d: %v", before, m.err)
		}
	}
	if m.Step() == WizardStepProvider {
		m.HandleKey("enter")
	}
	if m.Step() != WizardStepReview {
		t.Fatalf("the wizard did not reach review: step %d, err %v", m.Step(), m.err)
	}

	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Kind != "service_exposure" {
		t.Fatalf("kind = %q", req.Kind)
	}
	if req.Source.Existing == nil {
		t.Fatal("the request carries no existing-service source")
	}
	if !strings.Contains(req.Source.Existing.Address, "127.0.0.1") {
		t.Fatalf("address = %q", req.Source.Existing.Address)
	}
	if req.Exposure.Mode == "" {
		t.Error("the request carries no exposure mode")
	}
	// And no other kind's arm.
	if req.PortForward != nil || req.ClientTunnel != nil || req.PrivateNetwork != nil {
		t.Error("the service request carries another kind's arm")
	}
}

// TestPrivateNetworkIsNotOfferedYet pins that an unsupported kind is not
// advertised as creatable.
//
// Portico models private networks and has no provider that delivers one. Offering
// the outcome would be a fake affordance: the user would answer questions and the
// request would be refused.
func TestPrivateNetworkIsNotOfferedYet(t *testing.T) {
	for _, recipe := range wizardRecipes {
		if recipe.ConnectionKind == "private_network" {
			t.Fatalf("an outcome offers private_network, which no provider delivers: %q",
				recipe.Label)
		}
	}
}
