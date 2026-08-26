package profile

import "testing"

func TestOpenAIProfileIsProviderNeutral(t *testing.T) {
	p := Profile{
		Kind:                ProfileOpenAICompatible,
		Target:              TargetSpec{Host: "127.0.0.1", Port: 8080, Protocol: "http", BasePath: "/v1"},
		Gateway:             GatewaySpec{Enabled: true, AuthRequired: true, AllowSSE: true},
		TransportProviderID: "cloudflare",
	}
	if p.Kind != ProfileOpenAICompatible || p.TransportProviderID != "cloudflare" {
		t.Fatalf("profile lost intent/transport separation: %+v", p)
	}
}
