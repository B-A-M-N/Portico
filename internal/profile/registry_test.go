package profile

import "testing"

func TestProfileRegistryFiltersTransportsByCapability(t *testing.T) {
	registry := NewRegistry(Descriptor{
		ProfileKind:  ProfileOpenAICompatible,
		DisplayName:  "OpenAI-compatible",
		Capabilities: TransportCapabilities{HTTP: true, Streaming: true},
	})
	definition, ok := registry.Lookup(ProfileOpenAICompatible)
	if !ok || definition.Name() != "OpenAI-compatible" {
		t.Fatalf("definition lookup = %#v, %v", definition, ok)
	}
	if !definition.Compatible(TransportCapabilities{HTTP: true, Streaming: true}) {
		t.Fatal("compatible HTTP streaming transport was rejected")
	}
	if definition.Compatible(TransportCapabilities{HTTP: true}) {
		t.Fatal("non-streaming transport was accepted")
	}
}

func TestProfileRegistryRejectsDuplicateKinds(t *testing.T) {
	registry := NewRegistry()
	definition := Descriptor{ProfileKind: "generic_http"}
	if err := registry.Register(definition); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register(definition); err == nil {
		t.Fatal("duplicate profile kind was accepted")
	}
}
