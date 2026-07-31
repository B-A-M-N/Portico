package screens

import (
	"testing"
)

// TestBackNavigationIsTheInverseOfForward checks the table is self-consistent.
//
// This walks applicableSteps in both directions, so it proves the table agrees
// with itself — not that it agrees with the wizard. The predicates were derived
// by reading the forward handlers, so a misreading would be encoded here too.
// TestTheStepsVisitedGoingBackAreTheStepsVisitedGoingForward drives the real
// handlers and is the test that can catch that.
func TestBackNavigationIsTheInverseOfForward(t *testing.T) {
	for _, sourceKind := range []string{"existing_service", "directory", "command", "mcp_server"} {
		t.Run(sourceKind, func(t *testing.T) {
			m := NewWizard(nil, fullCloudflareSnapshot())
			m.state.SourceType = sourceKind
			m.state.ExposureMode = "permanent_public"
			m.state.Protection = "email_otp"
			if sourceKind == "directory" {
				m.state.DirectoryMode = "read"
			}

			steps := m.applicableSteps()
			if len(steps) < 3 {
				t.Fatalf("sequence too short to test: %v", steps)
			}

			// Walk to the end of the sequence, then back to the start.
			for i := len(steps) - 1; i > 0; i-- {
				m.state.Step = steps[i]
				previous, ok := m.previousStep()
				if !ok {
					t.Fatalf("step %d reported no previous step, but %d precedes it", steps[i], steps[i-1])
				}
				if previous != steps[i-1] {
					t.Fatalf("back from step %d landed on %d, want %d", steps[i], previous, steps[i-1])
				}
			}

			// The first question has nowhere to go back to.
			m.state.Step = steps[0]
			if _, ok := m.previousStep(); ok {
				t.Fatal("the first question offered a step to go back to")
			}
		})
	}
}

// TestSkippedQuestionsAreSkippedInBothDirections pins that a question which
// does not apply is absent going forward and going back.
func TestSkippedQuestionsAreSkippedInBothDirections(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "temporary_public" // no hostname question
	m.state.Protection = "none"               // no protection-rules question

	steps := m.applicableSteps()
	for _, id := range steps {
		if id == WizardStepHostname {
			t.Fatal("a temporary address asked for a hostname")
		}
		if id == WizardStepProtectionRules {
			t.Fatal("an unprotected connection asked who may reach it")
		}
		if id == WizardStepDirectoryMode || id == WizardStepMCPTransport {
			t.Fatalf("a plain service asked a question belonging to another source: %d", id)
		}
	}

	// Going back from protection must skip the hostname question, not land on it.
	m.state.Step = WizardStepProtection
	previous, ok := m.previousStep()
	if !ok || previous == WizardStepHostname {
		t.Fatalf("back from protection landed on %d, which does not apply", previous)
	}
}

// TestTheIntentQuestionIsRevisitedWhenItWasAsked pins a defect the hand-written
// handlers had: leaving the name step always returned to the outcome question,
// even for a user who had reached it through the intent question, so that
// answer could not be revised.
func TestTheIntentQuestionIsRevisitedWhenItWasAsked(t *testing.T) {
	viaIntent := NewWizard(nil, fullCloudflareSnapshot())
	viaIntent.state.Advanced = true
	viaIntent.state.SourceType = "existing_service"
	viaIntent.state.Step = WizardStepName
	previous, ok := viaIntent.previousStep()
	if !ok || previous != WizardStepIntent {
		t.Fatalf("back from name landed on %d, want the intent question the user answered", previous)
	}

	viaRecipe := NewWizard(nil, fullCloudflareSnapshot())
	viaRecipe.state.SourceType = "existing_service"
	viaRecipe.state.Step = WizardStepName
	previous, ok = viaRecipe.previousStep()
	if !ok || previous != WizardStepOutcome {
		t.Fatalf("back from name landed on %d, want the outcome question", previous)
	}
}
