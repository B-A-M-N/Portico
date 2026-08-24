package screens

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Creating a private-network connection through the wizard.
//
// The kind has two modes that answer different questions, so the flow asks which first: a
// join publishes nothing and needs no address, and a publish needs one. Neither produces a
// public address, and the review says so rather than leaving the user to infer it from the
// absence of one.
//
// Keys go through Update, never by assigning state. A question whose field is not wired to
// receive keystrokes passes a test that sets the value directly and fails for every real
// user — which is exactly the state the port-forward and tunnel questions were in.

// tailnetSnapshot has the private-network provider registered and usable.
func tailnetSnapshot() []ipc.ProviderDTO {
	return []ipc.ProviderDTO{{
		ID: "tailscale", Name: "tailscale", DisplayName: "Tailscale",
		Selectable: true, Availability: "ready", Readiness: "ready",
		Stability: "beta",
		Capabilities: &ipc.CapabilitySetDTO{
			Kinds:           []string{"private_network"},
			PrivateExposure: true,
			ProtectionModes: []string{"private_network"},
			Protocols:       []string{"http", "https", "tcp"},
		},
	}}
}

// wizardAtPrivateNetworkMode drives the wizard to the mode question.
func wizardAtPrivateNetworkMode(t *testing.T, name string) *WizardModel {
	t.Helper()
	m := NewWizard(nil, tailnetSnapshot())
	takeOutcome(t, m, "private_network", name)
	if m.Step() != WizardStepPrivateNetworkMode {
		t.Fatalf("step after naming = %d, want the mode question: %v", m.Step(), m.Err())
	}
	return m
}

// TestJoiningNeedsNoAddress pins that the join path asks nothing it does not need.
func TestJoiningNeedsNoAddress(t *testing.T) {
	m := wizardAtPrivateNetworkMode(t, "my network")

	// The first option is the join. Its detail says nothing is created, because that is
	// the surprising part.
	view := m.View()
	if !strings.Contains(view, "nothing is created") {
		t.Errorf("the join option does not say it creates nothing:\n%s", view)
	}

	m.HandleKey("enter")
	if m.Step() != WizardStepReview {
		t.Fatalf("a join went to step %d instead of review: %v", m.Step(), m.Err())
	}

	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Kind != "private_network" {
		t.Fatalf("kind = %q", req.Kind)
	}
	if req.PrivateNetwork == nil {
		t.Fatal("the request carries no private network arm")
	}
	if req.PrivateNetwork.Mode != "join" {
		t.Fatalf("mode = %q, want join", req.PrivateNetwork.Mode)
	}
	// A join publishes nothing, so there is no address on the request.
	if req.PrivateNetwork.LocalAddress != "" {
		t.Errorf("a join carries the address %q", req.PrivateNetwork.LocalAddress)
	}
	if req.PrivateNetwork.ExposeLocal {
		t.Error("a join is marked as exposing something local")
	}
	// And no other kind's arm, nor a service-exposure arm that would carry an exposure
	// mode a public renderer could act on.
	if req.Source.Kind != "" || req.Exposure.Mode != "" || req.Protection.Kind != "" {
		t.Errorf("the request carries service-exposure fields: %+v", req)
	}
	if req.PortForward != nil || req.ClientTunnel != nil {
		t.Error("the request carries another kind's arm")
	}
}

// TestPublishingCollectsTheAddress pins the expose path end to end.
func TestPublishingCollectsTheAddress(t *testing.T) {
	m := wizardAtPrivateNetworkMode(t, "api")

	// Move to the second option and take it.
	m.HandleKey("down")
	m.HandleKey("enter")
	if m.Step() != WizardStepPrivateNetworkAddress {
		t.Fatalf("step after choosing publish = %d, want the address question", m.Step())
	}

	// The field receives keystrokes, one at a time, as a user types.
	typeInto(m, "127.0.0.1:3000")
	if got := m.inputValue(); got != "127.0.0.1:3000" {
		t.Fatalf("the address field holds %q: the question cannot be answered", got)
	}
	m.HandleKey("enter")
	if m.Step() != WizardStepReview {
		t.Fatalf("step after the address = %d, want review: %v", m.Step(), m.Err())
	}

	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.PrivateNetwork.Mode != "expose" {
		t.Fatalf("mode = %q, want expose", req.PrivateNetwork.Mode)
	}
	if req.PrivateNetwork.LocalAddress != "127.0.0.1:3000" {
		t.Fatalf("the address did not reach the request: %q", req.PrivateNetwork.LocalAddress)
	}
	if !req.PrivateNetwork.ExposeLocal {
		t.Error("the request does not record that something local is exposed")
	}
	// The address is on the private-network arm, not in Source: the request is a tagged
	// union, and populating Source would declare a service exposure.
	if req.Source.Existing != nil {
		t.Error("the address was also written into the service-exposure source")
	}
}

// TestAnAddressWithoutAPortIsRefused pins that the mistake is caught on the question.
func TestAnAddressWithoutAPortIsRefused(t *testing.T) {
	m := wizardAtPrivateNetworkMode(t, "api")
	m.HandleKey("down")
	m.HandleKey("enter")

	for _, bad := range []string{"", "localhost", "my-service"} {
		m.setInput(bad)
		m.HandleKey("enter")
		if m.Step() != WizardStepPrivateNetworkAddress {
			t.Fatalf("the address %q was accepted", bad)
		}
		if m.Err() == nil {
			t.Errorf("the address %q produced no error", bad)
		}
	}

	// A well-formed one is taken.
	m.setInput("127.0.0.1:8080")
	m.HandleKey("enter")
	if m.Step() != WizardStepReview {
		t.Fatalf("a valid address was refused: %v", m.Err())
	}
}

// TestTheReviewPromisesNoPublicAddress pins the honesty requirement for this kind.
//
// The whole point is that nothing becomes reachable from the internet. A review that did
// not say so would leave the user to infer it from the absence of an address, and the
// absence of a thing is not a statement about it.
func TestTheReviewPromisesNoPublicAddress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		presses []string
		address string
	}{
		{name: "join", presses: []string{"enter"}},
		{name: "publish", presses: []string{"down", "enter"}, address: "127.0.0.1:3000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := wizardAtPrivateNetworkMode(t, "thing")
			for _, key := range tc.presses {
				m.HandleKey(key)
			}
			if tc.address != "" {
				typeInto(m, tc.address)
				m.HandleKey("enter")
			}
			if m.Step() != WizardStepReview {
				t.Fatalf("did not reach review: step %d, %v", m.Step(), m.Err())
			}

			view := m.View()
			// The five questions a review has to answer.
			for _, answered := range []string{
				"What will be reachable", "How it will be reached", "Who can reach it",
				"What Portico will create and manage", "When you close it",
			} {
				if !strings.Contains(view, answered) {
					t.Errorf("review does not answer %q:\n%s", answered, view)
				}
			}
			if !strings.Contains(view, "no public address") &&
				!strings.Contains(view, "not by Portico") {
				t.Errorf("review does not make the private nature explicit:\n%s", view)
			}
			// Internal vocabulary stays out of it.
			for _, leaked := range []string{"private_network", "expose_local", "keep_alive"} {
				if strings.Contains(view, leaked) {
					t.Errorf("review leaks the internal term %q:\n%s", leaked, view)
				}
			}
		})
	}
}

// TestTheJoinReviewSaysPorticoWillNotSignTheMachineOut pins the ownership statement.
//
// Being on the network is machine-wide and predates the connection. A user closing this
// must know their machine stays on the network, because the alternative — Portico signing
// it out — would break everything else on it.
func TestTheJoinReviewSaysPorticoWillNotSignTheMachineOut(t *testing.T) {
	m := wizardAtPrivateNetworkMode(t, "my network")
	m.HandleKey("enter")

	view := m.View()
	if !strings.Contains(view, "stays on the network") {
		t.Errorf("the review does not say the machine stays on the network:\n%s", view)
	}
	if !strings.Contains(view, "will not sign it out") {
		t.Errorf("the review does not say Portico will not sign the machine out:\n%s", view)
	}
	if !strings.Contains(view, "Nothing.") {
		t.Errorf("the review does not say Portico creates nothing:\n%s", view)
	}
}

// TestBothQuestionsAdvertiseTheirActions pins contextual help for the new steps.
func TestBothQuestionsAdvertiseTheirActions(t *testing.T) {
	m := wizardAtPrivateNetworkMode(t, "thing")
	for _, step := range []int{WizardStepPrivateNetworkMode, WizardStepPrivateNetworkAddress} {
		m.state.Step = step
		actions := m.Actions()
		if len(actions) == 0 {
			t.Fatalf("step %d offers no actions", step)
		}
		for _, action := range actions {
			if action.Label != "" && action.Help == "" {
				t.Errorf("step %d advertises %q with no explanation", step, action.Label)
			}
		}
	}
}

// TestGoingBackKeepsTheAnswer pins that the mode question remembers what was chosen.
func TestGoingBackKeepsTheAnswer(t *testing.T) {
	m := wizardAtPrivateNetworkMode(t, "api")
	m.HandleKey("down")
	m.HandleKey("enter")
	typeInto(m, "127.0.0.1:3000")
	m.HandleKey("enter")

	// Back from review to the address, and back again to the mode.
	m.HandleKey("esc")
	if m.Step() != WizardStepPrivateNetworkAddress {
		t.Fatalf("esc from review went to step %d", m.Step())
	}
	if got := m.inputValue(); got != "127.0.0.1:3000" {
		t.Errorf("the address was lost going back: %q", got)
	}
	m.HandleKey("esc")
	if m.Step() != WizardStepPrivateNetworkMode {
		t.Fatalf("esc from the address went to step %d", m.Step())
	}
	// The cursor is on the option that was chosen, not reset to the first.
	if m.SelectedIndex() != 1 {
		t.Errorf("the mode cursor is at %d, want the chosen option", m.SelectedIndex())
	}
}
