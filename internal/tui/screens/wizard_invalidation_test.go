package screens

import (
	"testing"
)

// TestChangingTheAddressDiscardsProtectionThatNeedsIt pins that an answer does
// not survive the invalidation of the answer it depended on.
//
// Protection requires an address that does not move. Switching to a generated
// address while a protection choice was already made left a combination core
// validation rejects, and the rejection arrived at create — a long way from the
// question that caused it.
func TestChangingTheAddressDiscardsProtectionThatNeedsIt(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot(), nil)
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "permanent_public"
	m.state.Protection = "email_otp"
	m.state.AllowedEmails = []string{"person@example.com"}
	m.state.Step = WizardStepExposure
	// Highlight the temporary option and choose it.
	m.selected = choiceIndex(m.exposureChoices(), "temporary_public")
	m.HandleKey("enter")

	if m.state.Protection != "" {
		t.Fatalf("protection %q survived a change that made it impossible", m.state.Protection)
	}
	if len(m.state.AllowedEmails) != 0 {
		t.Fatal("the people allowed through survived the protection being discarded")
	}
}

// TestChangingTheAddressKeepsProtectionThatStillWorks ensures the rule discards
// only what became impossible.
func TestChangingTheAddressKeepsProtectionThatStillWorks(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot(), nil)
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "temporary_public"
	m.state.Protection = "none"
	m.state.Step = WizardStepExposure
	m.selected = choiceIndex(m.exposureChoices(), "permanent_public")
	m.HandleKey("enter")

	if m.state.Protection != "none" {
		t.Fatalf("protection %q was discarded though it remained possible", m.state.Protection)
	}
}

// TestAQuestionLeavingTheSequenceDoesNotStrandTheUser pins the escape hatch.
//
// A question whose predicate stops holding while the user is on it — an account
// question after the provider it belonged to went away — would otherwise have
// nowhere to go back to, because the step is no longer in the sequence.
func TestAQuestionLeavingTheSequenceDoesNotStrandTheUser(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot(), nil)
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "temporary_public"
	m.state.Protection = "none"
	// On the account question, but no provider is selected, so it no longer
	// applies.
	m.state.Step = WizardStepAccount
	m.state.Provider = ""

	previous, ok := m.previousStep()
	if !ok {
		t.Fatal("a question that left the sequence offered no way back")
	}
	if !precedes(previous, WizardStepAccount) {
		t.Fatalf("back from the account question landed on %d, which does not precede it", previous)
	}

	m.goBack()
	if m.Step() == WizardStepAccount {
		t.Fatal("the user was left on a question that no longer applies")
	}
}

// TestTheEvaluationAndTheConnectionDescribeTheSameThing pins that a provider is
// scored against the connection that will actually be created.
//
// The request applies a protocol default for a service that is already
// listening. Applying it in only one of the two places scores a provider with
// no protocol constraint and then creates a connection that has one.
func TestTheEvaluationAndTheConnectionDescribeTheSameThing(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot(), nil)
	m.state = WizardState{
		Name: "demo", SourceType: "existing_service",
		SourceAddress: "127.0.0.1", Port: "8080",
		ExposureMode: "permanent_public", Hostname: "demo.example.com",
		Protection: "none", Provider: "cloudflare",
		// Deliberately unset: both sides must default it the same way.
		SourceProtocol: "",
	}

	scored := m.recommendationRequest()
	built := m.buildRequest()

	if scored.Protocol != built.Source.Existing.Protocol {
		t.Fatalf("scored protocol %q, created %q", scored.Protocol, built.Source.Existing.Protocol)
	}
	if scored.ExposureMode != built.Exposure.Mode {
		t.Fatalf("scored exposure %q, created %q", scored.ExposureMode, built.Exposure.Mode)
	}
	if scored.RequestedAddress != built.Exposure.RequestedAddress {
		t.Fatalf("scored address %q, created %q", scored.RequestedAddress, built.Exposure.RequestedAddress)
	}
	if scored.ProtectionKind != built.Protection.Kind {
		t.Fatalf("scored protection %q, created %q", scored.ProtectionKind, built.Protection.Kind)
	}
	if scored.ConnectionKind != built.Kind {
		t.Fatalf("scored kind %q, created %q", scored.ConnectionKind, built.Kind)
	}
}
