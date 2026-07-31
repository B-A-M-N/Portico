package screens

import (
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// wizardAtProviderStep builds a wizard that has answered everything a
// recommendation depends on.
func wizardAtProviderStep(t *testing.T, client ConnectionCreator) *WizardModel {
	t.Helper()
	m := NewWizard(client, fullCloudflareSnapshot())
	m.state.SourceType = "existing_service"
	m.state.SourceProtocol = "http"
	m.state.ExposureMode = "permanent_public"
	m.state.Hostname = "demo.example.com"
	m.state.Protection = "none"
	m.state.Step = WizardStepProvider
	return m
}

// TestAStaleRecommendationIsDiscarded pins the reason answers carry a
// fingerprint.
//
// The user can change a requirement while a request is in flight, and two
// requests can finish out of order. Comparing what an answer was computed from
// against what is currently true makes arrival order irrelevant; a counter
// would not, because a stale request can complete last.
func TestAStaleRecommendationIsDiscarded(t *testing.T) {
	m := wizardAtProviderStep(t, &fakeWizardClient{})
	m.recommendCmd() // records the fingerprint for the current answers

	stale := ProviderRecommendationMsg{
		Fingerprint: "answers|the|user|has|since|changed",
		Response: &ipc.ProviderRecommendationResponse{
			Recommended: &ipc.ProviderChoiceDTO{ProviderID: "wrong", DisplayName: "Wrong"},
		},
	}
	m.HandleRecommendation(stale)

	if m.recommendation != nil {
		t.Fatal("an answer computed for different requirements was installed")
	}
	if !m.recommendPending {
		t.Fatal("a stale answer ended the wait for the current one")
	}
}

// TestTheCurrentRecommendationIsInstalled is the other half: an answer for the
// requirements actually in force is used, and the recommended provider is
// highlighted rather than whichever happens to be first.
func TestTheCurrentRecommendationIsInstalled(t *testing.T) {
	m := wizardAtProviderStep(t, &fakeWizardClient{})
	m.recommendCmd()

	m.HandleRecommendation(ProviderRecommendationMsg{
		Fingerprint: m.recommendFingerprint,
		Response: &ipc.ProviderRecommendationResponse{
			Recommended: &ipc.ProviderChoiceDTO{
				ProviderID: "cloudflare", DisplayName: "Cloudflare",
				Reasons:   []string{"can own the hostname you asked for"},
				Tradeoffs: []string{"needs an account"},
			},
		},
	})

	if m.recommendPending {
		t.Fatal("the wait did not end when the answer arrived")
	}
	view := m.renderProvider()
	if !strings.Contains(view, "recommended") {
		t.Fatalf("the recommendation is not marked:\n%s", view)
	}
	if !strings.Contains(view, "can own the hostname you asked for") {
		t.Fatalf("the reasoning is not shown:\n%s", view)
	}
	if !strings.Contains(view, "needs an account") {
		t.Fatalf("the trade-off is not shown, so the choice is a verdict:\n%s", view)
	}
}

// TestAFailedRecommendationDoesNotPickAProvider pins that an evaluation Portico
// could not perform is reported rather than guessed around.
//
// Defaulting to a provider here is how a connection gets created that cannot
// work: nothing checked whether it could carry these requirements.
func TestAFailedRecommendationDoesNotPickAProvider(t *testing.T) {
	m := wizardAtProviderStep(t, &fakeWizardClient{})
	m.recommendCmd()

	m.HandleRecommendation(ProviderRecommendationMsg{
		Fingerprint: m.recommendFingerprint,
		Err:         errors.New("supervisor unreachable"),
	})

	if m.recommendation != nil {
		t.Fatal("a failed evaluation produced a recommendation anyway")
	}
	view := m.renderProvider()
	if !strings.Contains(view, "supervisor unreachable") {
		t.Fatalf("the failure is not reported:\n%s", view)
	}
	if !strings.Contains(view, "not a recommendation") {
		t.Fatalf("the fallback list is presented as if it were evaluated:\n%s", view)
	}
}

// TestARefusedProviderSaysWhatWouldFixIt pins that a filtered provider carries
// its setup actions, which the DTO previously dropped — so it could say why it
// was refused but not what to do about it.
func TestARefusedProviderSaysWhatWouldFixIt(t *testing.T) {
	m := wizardAtProviderStep(t, &fakeWizardClient{})
	m.recommendCmd()

	m.HandleRecommendation(ProviderRecommendationMsg{
		Fingerprint: m.recommendFingerprint,
		Response: &ipc.ProviderRecommendationResponse{
			Summary: "No provider can meet these requirements.",
			Filtered: []ipc.FilteredChoiceDTO{{
				ProviderID: "ngrok", DisplayName: "ngrok",
				Reason:       "does not support permanent custom hostnames",
				Reasons:      []string{"does not support permanent custom hostnames"},
				SetupActions: []string{"Reserve a domain in the ngrok dashboard"},
			}},
		},
	})

	choices := m.providerChoices()
	if len(choices) != 1 || choices[0].Available {
		t.Fatalf("a refused provider was offered as selectable: %#v", choices)
	}
	view := m.renderProvider()
	if !strings.Contains(view, "does not support permanent custom hostnames") {
		t.Fatalf("the refusal is not explained:\n%s", view)
	}
	if !strings.Contains(view, "Reserve a domain") {
		t.Fatalf("the refusal does not say what would fix it:\n%s", view)
	}
	if !strings.Contains(view, "No provider can meet these requirements") {
		t.Fatalf("the overall outcome is not stated:\n%s", view)
	}
}

// TestGoingBackDiscardsTheRecommendation ensures an answer computed from
// requirements the user is revisiting cannot survive into a different
// connection.
func TestGoingBackDiscardsTheRecommendation(t *testing.T) {
	m := wizardAtProviderStep(t, &fakeWizardClient{})
	m.recommendCmd()
	m.HandleRecommendation(ProviderRecommendationMsg{
		Fingerprint: m.recommendFingerprint,
		Response: &ipc.ProviderRecommendationResponse{
			Recommended: &ipc.ProviderChoiceDTO{ProviderID: "cloudflare"},
		},
	})
	if m.recommendation == nil {
		t.Fatal("setup failed: no recommendation installed")
	}

	m.goBack()

	if m.recommendation != nil {
		t.Fatal("a recommendation survived the user revisiting what it was computed from")
	}
}
