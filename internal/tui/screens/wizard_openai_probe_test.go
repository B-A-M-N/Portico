package screens

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/profile/openai"
)

// Item 26: the "Share a local OpenAI-compatible API" outcome carries the
// openai_compatible intent into the request, probes the endpoint with the real
// protocol checks, and shows the result on review in the user's words. The
// internal term "profile kind" never appears in any view.

func TestOpenAICompatibleRecipeExistsAndIsOffered(t *testing.T) {
	var recipe *wizardRecipe
	for i := range wizardRecipes {
		if wizardRecipes[i].ProfileKind == string(core.ProfileOpenAICompatible) {
			recipe = &wizardRecipes[i]
		}
	}
	if recipe == nil {
		t.Fatal("no OpenAI-compatible outcome is offered")
	}
	if !recipe.ProbeOpenAICompatibility {
		t.Fatal("the OpenAI-compatible outcome does not request a compatibility probe")
	}
}

func TestWizardCarriesProfileKindIntoRequest(t *testing.T) {
	m := NewWizard(nil, nil)
	m.state.Advanced = true
	m.state.ConnectionKind = "service_exposure"
	m.state.SourceType = "existing_service"
	m.state.Name = "llama-box"
	m.state.SourceAddress = "http://127.0.0.1:8080"
	m.state.ExposureMode = "permanent_public"
	m.state.ProfileKind = string(core.ProfileOpenAICompatible)

	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.ProfileKind != string(core.ProfileOpenAICompatible) {
		t.Fatalf("request profile kind = %q, want %q", req.ProfileKind, core.ProfileOpenAICompatible)
	}
}

func TestProbeReplyUpdatesReviewForLiveWizardOnly(t *testing.T) {
	m := NewWizard(nil, nil)
	m.state.ProfileKind = string(core.ProfileOpenAICompatible)
	m.state.ConnectionKind = "service_exposure"

	stale := NewWizard(nil, nil)
	consumed := stale.HandleOpenAICompatProbed(OpenAICompatProbedMsg{
		id: m.id, generation: m.generation,
	})
	if consumed {
		t.Fatal("a reply for another wizard's ID was consumed")
	}

	msg := OpenAICompatProbedMsg{
		id: m.id, generation: m.generation,
		result: fakeCompat([]string{"llama-3", "qwen-2"}, false),
	}
	if !m.HandleOpenAICompatProbed(msg) {
		t.Fatal("the live wizard dropped its own probe reply")
	}
	view := m.renderReview()
	if !strings.Contains(view, "COMPATIBILITY CHECK") || !strings.Contains(view, "llama-3") {
		t.Fatalf("the probe result is not shown on review:\n%s", view)
	}
	if strings.Contains(strings.ToLower(view), "profile kind") {
		t.Fatalf("internal vocabulary leaked into the UI:\n%s", view)
	}
}

// TestReviewShowsCheckingWhileProbeRuns pins that an outcome awaiting its
// probe says so instead of silently showing nothing.
func TestReviewShowsCheckingWhileProbeRuns(t *testing.T) {
	m := NewWizard(nil, nil)
	m.state.ProfileKind = string(core.ProfileOpenAICompatible)
	m.state.ConnectionKind = "service_exposure"
	m.state.Name = "llama-box"

	view := m.renderReview()
	if !strings.Contains(view, "checking the endpoint") {
		t.Fatalf("the pending probe is not announced:\n%s", view)
	}
}

func TestEnterReviewFiresProbeOnce(t *testing.T) {
	m := NewWizard(nil, nil)
	m.state.ProfileKind = string(core.ProfileOpenAICompatible)
	m.state.ConnectionKind = "service_exposure"

	cmd := m.enterReview()
	if cmd == nil {
		t.Fatal("entering review for a probe outcome fired no probe")
	}
	msg := cmd()
	if _, ok := msg.(OpenAICompatProbedMsg); !ok {
		t.Fatalf("the probe command produced %T", msg)
	}
	if m.probeSummary != "" {
		t.Fatal("state changed before the probe ran")
	}

	// A second entry (user went back and returned) must not re-probe once a
	// result exists.
	m.openAIProbeResult = fakeCompat([]string{"m"}, false)
	m.probeSummary = "Models detected: m"
	if cmd2 := m.enterReview(); cmd2 != nil {
		t.Fatal("a completed probe was re-run")
	}
}

func fakeCompat(models []string, auth bool) *openai.OpenAICompatibility {
	return &openai.OpenAICompatibility{Models: models, AuthRequired: auth}
}
