package screens

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Adopting an OpenAI Secure MCP Tunnel.
//
// The adapter for this has a full lifecycle — capabilities, a setup flow, a plan,
// step execution, observation — and refuses to plan without a tunnel ID, because
// Portico cannot create the tunnel: creation happens in the OpenAI platform's own
// organization settings. The wizard had no way to collect one, so buildRequest
// returned "client tunnel connections are not yet supported" and the whole adapter
// was unreachable.

// tunnelSnapshot has the tunnel provider registered and usable.
func tunnelSnapshot() []ipc.ProviderDTO {
	return []ipc.ProviderDTO{{
		ID: "openai_tunnel", Name: "openai_tunnel",
		DisplayName: "OpenAI Secure MCP Tunnel",
		Selectable:  true, Availability: "ready", Readiness: "ready",
		Stability: "experimental",
		Capabilities: &ipc.CapabilitySetDTO{
			Kinds:           []string{"client_tunnel"},
			PrivateExposure: true,
		},
	}}
}

// wizardAtTunnelID drives the wizard to the tunnel question.
func wizardAtTunnelID(t *testing.T) *WizardModel {
	t.Helper()
	m := NewWizard(nil, tunnelSnapshot())

	// Find and take the ChatGPT outcome.
	index := -1
	for i, recipe := range wizardRecipes {
		if recipe.ConnectionKind == "client_tunnel" {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("no client-tunnel outcome is offered")
	}
	for range index {
		m.HandleKey("down")
	}
	m.HandleKey("enter")
	if m.Step() == WizardStepOutcome {
		t.Fatalf("the outcome was refused with the provider available: %v", m.err)
	}

	// Name it.
	m.setInput("chatgpt-mcp")
	m.HandleKey("enter")
	if m.Step() != WizardStepTunnelID {
		t.Fatalf("step after naming = %d, want the tunnel question", m.Step())
	}
	return m
}

// TestTheTunnelOutcomeIsAvailableWhenTheProviderIs pins that availability is read
// from the snapshot rather than written into the recipe.
func TestTheTunnelOutcomeIsAvailableWhenTheProviderIs(t *testing.T) {
	withProvider := NewWizard(nil, tunnelSnapshot())
	var recipe wizardRecipe
	for _, r := range wizardRecipes {
		if r.ConnectionKind == "client_tunnel" {
			recipe = r
		}
	}
	if reason := withProvider.recipeUnavailable(recipe); reason != "" {
		t.Fatalf("the outcome is refused with the provider registered: %s", reason)
	}

	// Without it, the refusal says what is missing and where to look.
	without := NewWizard(nil, nil)
	reason := without.recipeUnavailable(recipe)
	if reason == "" {
		t.Fatal("the outcome is offered with no provider to deliver it")
	}
	if !strings.Contains(reason, "Setup") {
		t.Errorf("the refusal offers no next step: %q", reason)
	}

	// A provider that is present but not usable gives its own reason.
	blocked := NewWizard(nil, []ipc.ProviderDTO{{
		ID: "openai_tunnel", DisplayName: "OpenAI Secure MCP Tunnel",
		Selectable:   false,
		SetupActions: []string{"install tunnel-client", "export CONTROL_PLANE_API_KEY"},
	}})
	reason = blocked.recipeUnavailable(recipe)
	if !strings.Contains(reason, "tunnel-client") {
		t.Errorf("the refusal does not carry the provider's own setup actions: %q", reason)
	}
}

// TestATunnelIDIsRequiredAndChecked pins that Portico does not invent one.
func TestATunnelIDIsRequiredAndChecked(t *testing.T) {
	m := wizardAtTunnelID(t)

	// The question says Portico does not create tunnels.
	view := m.View()
	if !strings.Contains(view, "does not create tunnels") {
		t.Errorf("the question does not say Portico cannot create a tunnel:\n%s", view)
	}

	// Empty is refused.
	m.setInput("")
	m.HandleKey("enter")
	if m.Step() != WizardStepTunnelID {
		t.Fatal("an empty tunnel ID was accepted")
	}
	if m.err == nil || !strings.Contains(m.err.Error(), "cannot create one") {
		t.Errorf("the refusal does not explain why an ID is needed: %v", m.err)
	}

	// A malformed ID is refused here rather than by the client at open time.
	m.setInput("tunnel_short")
	m.HandleKey("enter")
	if m.Step() != WizardStepTunnelID {
		t.Fatal("a malformed tunnel ID was accepted")
	}
	if m.err == nil {
		t.Error("a malformed ID produced no error")
	}

	// A well-formed one is taken.
	m.setInput("tunnel_" + strings.Repeat("a", 32))
	m.HandleKey("enter")
	if m.Step() != WizardStepTunnelMCP {
		t.Fatalf("a valid tunnel ID did not advance the wizard: step %d, err %v", m.Step(), m.err)
	}
}

// TestTheTunnelRequestCarriesWhatWasCollected pins the whole flow through to the
// create request.
func TestTheTunnelRequestCarriesWhatWasCollected(t *testing.T) {
	m := wizardAtTunnelID(t)

	id := "tunnel_" + strings.Repeat("b", 32)
	m.setInput(id)
	m.HandleKey("enter")

	m.setInput("http://127.0.0.1:8000")
	m.HandleKey("enter")
	if m.Step() != WizardStepTunnelProfile {
		t.Fatalf("step after the MCP address = %d, err %v", m.Step(), m.err)
	}

	m.setInput("work")
	m.HandleKey("enter")
	if m.Step() != WizardStepReview {
		t.Fatalf("step after the profile = %d, want review", m.Step())
	}

	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Kind != "client_tunnel" {
		t.Fatalf("kind = %q, want client_tunnel", req.Kind)
	}
	if req.ClientTunnel == nil {
		t.Fatal("the request carries no client tunnel")
	}
	if req.ClientTunnel.TunnelID != id {
		t.Fatalf("tunnel ID = %q, want the one collected", req.ClientTunnel.TunnelID)
	}
	if req.ClientTunnel.Profile != "work" {
		t.Fatalf("profile = %q", req.ClientTunnel.Profile)
	}
	if req.ClientTunnel.MCP.Endpoint != "http://127.0.0.1:8000" {
		t.Fatalf("MCP endpoint = %q", req.ClientTunnel.MCP.Endpoint)
	}
	if req.ClientTunnel.Client != "openai_secure_mcp_tunnel" {
		t.Fatalf("client = %q", req.ClientTunnel.Client)
	}
	// Nothing else is carried: a client tunnel has no source, exposure or
	// protection, and sending them would ask the supervisor to change the kind.
	if req.Source.Kind != "" || req.Exposure.Mode != "" || req.Protection.Kind != "" {
		t.Errorf("the request carries service-exposure arms: %+v", req)
	}
	if req.PortForward != nil || req.PrivateNetwork != nil {
		t.Error("the request carries another kind's arm")
	}
}

// TestTheProfileIsOptional pins that a choice most users do not need to make is
// not demanded.
func TestTheProfileIsOptional(t *testing.T) {
	m := wizardAtTunnelID(t)
	m.setInput("tunnel_" + strings.Repeat("c", 32))
	m.HandleKey("enter")
	m.setInput("http://127.0.0.1:8000")
	m.HandleKey("enter")

	// Empty profile: accepted.
	m.setInput("")
	m.HandleKey("enter")
	if m.Step() != WizardStepReview {
		t.Fatalf("an empty profile was refused: step %d, err %v", m.Step(), m.err)
	}
	req, err := m.buildRequest()
	if err != nil {
		t.Fatal(err)
	}
	if req.ClientTunnel.Profile != "" {
		t.Errorf("an empty profile became %q", req.ClientTunnel.Profile)
	}
}

// TestTheTunnelReviewDoesNotClaimPorticoCreatedIt pins item 13's honesty
// requirement.
func TestTheTunnelReviewDoesNotClaimPorticoCreatedIt(t *testing.T) {
	m := wizardAtTunnelID(t)
	m.setInput("tunnel_" + strings.Repeat("d", 32))
	m.HandleKey("enter")
	m.setInput("http://127.0.0.1:8000")
	m.HandleKey("enter")
	m.setInput("")
	m.HandleKey("enter")

	view := m.View()
	if !strings.Contains(view, "did not create the tunnel") {
		t.Errorf("the review does not say Portico did not create the tunnel:\n%s", view)
	}
	if !strings.Contains(view, "will not delete it") {
		t.Errorf("the review does not say the tunnel survives deletion:\n%s", view)
	}
	// It says there is no public address, which is the whole point of this kind.
	if !strings.Contains(view, "no public address") {
		t.Errorf("the review does not say there is no public address:\n%s", view)
	}
	// And it does not describe a provider tunnel it is not creating.
	if strings.Contains(view, "A DNS record") {
		t.Errorf("the review describes DNS a client tunnel does not have:\n%s", view)
	}
}

// TestTheTunnelQuestionsAdvertiseTheirActions pins that each has contextual help.
func TestTheTunnelQuestionsAdvertiseTheirActions(t *testing.T) {
	m := wizardAtTunnelID(t)
	for _, step := range []int{WizardStepTunnelID, WizardStepTunnelMCP, WizardStepTunnelProfile} {
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
