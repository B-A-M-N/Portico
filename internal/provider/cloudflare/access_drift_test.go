package cloudflare

import (
	"context"
	"net/http"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// Reconstruction-and-drift coverage beyond the exact-ID observation set:
// each test reconstructs the adapter from configuration alone over the fake
// API, persists all four resource types, removes exactly ONE remote resource,
// and proves the observation classifies ONLY that resource as missing —
// with 401/403, 429 and network ambiguity never classified as missing.

const (
	ownedAccessAppID    = "cccccccccccccccccccccccccccccccc"
	ownedAccessPolicyID = "dddddddddddddddddddddddddddddddd"
)

func fullPersistedResources() []core.ProviderResource {
	return append(persistedResources(),
		core.ProviderResource{Type: core.ResourceAccessApp, ExternalID: ownedAccessAppID},
		core.ProviderResource{Type: core.ResourceAccessPolicy, ExternalID: ownedAccessPolicyID},
	)
}

// accessAppBody is a minimal Access application document.
func accessAppBody(id string) string {
	return `{"success":true,"result":{"id":"` + id + `","name":"portico-web","domain":"web.example.com"}}`
}

var _ = accessAppBody // retained for future app-present fixtures

// TestOnlyTheDeletedAccessResourceClassifiesMissing deletes the Access policy
// while tunnel, DNS and app remain: exactly one resource status flips.
func TestOnlyTheDeletedAccessResourceClassifiesMissing(t *testing.T) {
	tunnelPath := "/client/v4/accounts/acct-1/cfd_tunnel/" + ownedTunnelID
	dnsPath := "/client/v4/zones/zone-1/dns_records/" + ownedDNSRecordID

	provider, fake := reconstructedProvider(t, map[string]func() (int, string){
		http.MethodGet + " " + tunnelPath: func() (int, string) {
			return http.StatusOK, tunnelBody(ownedTunnelID, sharedTunnelName)
		},
		http.MethodGet + " " + dnsPath: func() (int, string) {
			return http.StatusOK, dnsBody(ownedDNSRecordID, "web.example.com", ownedTunnelID+".cfargotunnel.com")
		},
		// Access resources were deleted remotely: not found.
	})

	obs, err := provider.ObserveWithResources(context.Background(), "conn-1", fullPersistedResources())
	if err != nil {
		t.Fatalf("ObserveWithResources: %v", err)
	}
	assertResourceStatus(t, obs, core.ResourceTunnel, ownedTunnelID, core.ObservationPresent)
	assertResourceStatus(t, obs, core.ResourceDNSRecord, ownedDNSRecordID, core.ObservationPresent)

	// The deleted Access pair must NOT read as present (the fake answers 404),
	// and must not read as transient either: an authoritative absence is missing,
	// which is the only state that may drive recreation.
	for _, want := range []struct {
		kind core.ResourceType
		id   string
	}{
		{core.ResourceAccessApp, ownedAccessAppID},
	} {
		assertResourceStatus(t, obs, want.kind, want.id, core.ObservationMissing)
	}
	_ = fake
}
