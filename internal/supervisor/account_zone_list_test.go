package supervisor

import (
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// zoneListHandler builds a handler whose supervisor admits mutations, which
// the zone-listing handler requires (it reserves a durable-dependency lease).
func zoneListHandler(t *testing.T, validator AccountValidator) *supervisorHandler {
	t.Helper()
	h := reverifyHandler(t, validator)
	h.sup.mutating = true
	return h
}

// Zone-listing tests pin the ListProviderAccountZones handler (finding 7): the
// supervisor returns every DNS zone an account's stored credential can see so
// the wizard can offer a connection-scoped zone. The credential never leaves
// the supervisor.

// TestListProviderAccountZonesReturnsEveryVisibleZone pins the happy path: a
// Cloudflare account with a stored token lists all its zones.
func TestListProviderAccountZonesReturnsEveryVisibleZone(t *testing.T) {
	validator := &stubValidator{
		zones: []ZoneSummary{
			{ID: "zone-default", Name: "example.com"},
			{ID: "zone-b", Name: "other.net"},
		},
	}
	h := zoneListHandler(t, validator)
	storedAccount(t, h, core.AccountAuthenticated)

	resp, err := h.HandleListProviderAccountZones("cloudflare", "acct-1")
	if err != nil {
		t.Fatalf("ListProviderAccountZones: %v", err)
	}
	if validator.zoneCalls != 1 {
		t.Fatalf("ListZones calls = %d, want 1", validator.zoneCalls)
	}
	if len(resp.Zones) != 2 {
		t.Fatalf("zones = %d, want 2", len(resp.Zones))
	}
	if resp.Zones[0].ID != "zone-default" || resp.Zones[0].Name != "example.com" {
		t.Fatalf("first zone %+v, want zone-default/example.com", resp.Zones[0])
	}
	if resp.Zones[1].ID != "zone-b" || resp.Zones[1].Name != "other.net" {
		t.Fatalf("second zone %+v, want zone-b/other.net", resp.Zones[1])
	}
}

// TestListProviderAccountZonesRefusesNonCloudflare pins that only Cloudflare
// exposes zones.
func TestListProviderAccountZonesRefusesNonCloudflare(t *testing.T) {
	h := zoneListHandler(t, &stubValidator{})
	if _, err := h.HandleListProviderAccountZones("ngrok", "acct-1"); err == nil {
		t.Fatal("ngrok zone listing succeeded, want a refusal")
	}
}

// TestListProviderAccountZonesMissingAccount pins the not-found path.
func TestListProviderAccountZonesMissingAccount(t *testing.T) {
	h := zoneListHandler(t, &stubValidator{})
	if _, err := h.HandleListProviderAccountZones("cloudflare", "no-such"); err == nil {
		t.Fatal("listing zones for a missing account succeeded, want an error")
	}
}

// TestListProviderAccountZonesNeverLeaksTheCredential pins that a zone fetch
// failure and a successful one both keep the token out of the result.
func TestListProviderAccountZonesNeverLeaksTheCredential(t *testing.T) {
	validator := &stubValidator{zonesErr: errors.New("provider refused the token")}
	h := zoneListHandler(t, validator)
	storedAccount(t, h, core.AccountAuthenticated)

	_, err := h.HandleListProviderAccountZones("cloudflare", "acct-1")
	if err == nil {
		t.Fatal("a zone fetch that fails reported success")
	}
	if strings.Contains(err.Error(), "token-") {
		t.Fatalf("the zone failure leaks the credential: %v", err)
	}
}