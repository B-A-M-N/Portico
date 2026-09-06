package core

import "testing"

func TestManualUsabilityFieldsMustExplainWhy(t *testing.T) {
	field := UsabilityField{ID: "command.executable", Kind: UsabilityExecutable, Source: UsabilityManualRequired}
	if err := field.Validate(); err == nil {
		t.Fatal("a manual executable without a reason was accepted")
	}

	field.ManualReason = "Portico cannot determine which executable to run"
	if err := field.Validate(); err != nil {
		t.Fatalf("manual field with reason rejected: %v", err)
	}
}

func TestUsabilitySourcesAreTheStableFiveClasses(t *testing.T) {
	fields := []UsabilityField{
		{ID: "source", Kind: UsabilityLocalEndpoint, Source: UsabilityDiscovered},
		{ID: "port", Kind: UsabilityPort, Source: UsabilityInferred},
		{ID: "provider", Kind: UsabilityProviderResource, Source: UsabilitySelected},
		{ID: "hostname", Kind: UsabilityID, Source: UsabilitySuggested},
		{ID: "tunnel", Kind: UsabilityID, Source: UsabilityManualRequired, ManualReason: "created outside Portico"},
	}
	if err := ValidateUsabilityFields(fields); err != nil {
		t.Fatalf("valid classification rejected: %v", err)
	}
}
