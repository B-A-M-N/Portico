package screens

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// cloudflareSnapshotWithZone returns a ready Cloudflare provider whose account
// carries a DNS zone, the shape the snapshot produces after account setup
// selects it.
func cloudflareSnapshotWithZone(accountID, cloudflareZoneID string) []ipc.ProviderDTO {
	snap := fullCloudflareSnapshot()
	snap[0].Accounts = []ipc.ProviderAccountDTO{{
		ID: accountID, Label: "Personal", Status: "authenticated",
		ZoneID: cloudflareZoneID, ZoneName: "example.com",
	}}
	return snap
}

// wizardPermanentCloudflare builds a wizard mid-way through a permanent
// Cloudflare service exposure, exactly as the create-request build sees it.
func wizardPermanentCloudflare(snap []ipc.ProviderDTO) *WizardModel {
	m := NewWizard(nil, snap)
	m.state = WizardState{
		Step: WizardStepReview, Name: "svc", SourceType: "existing_service",
		SourceAddress: "127.0.0.1", SourceProtocol: "http", Port: "8080",
		Provider: "cloudflare", AccountID: snap[0].Accounts[0].ID,
		ExposureMode: "permanent_public", Hostname: "svc.example.com", Protection: "none",
	}
	return m
}

// TestWizardPermanentCloudflareConnectionCarriesItsOwnZone is finding-7's
// transport: a permanent Cloudflare connection's create request stamps the
// connection's zone on Provider.Options, so the durable slot is populated from
// the start and a later account-default change cannot retarget it.
func TestWizardPermanentCloudflareConnectionCarriesItsOwnZone(t *testing.T) {
	m := wizardPermanentCloudflare(cloudflareSnapshotWithZone("acct-1", "zone-a"))
	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if got := req.Provider.Options["zone_id"]; got != "zone-a" {
		t.Fatalf("permanent Cloudflare request Options[zone_id] = %q, want zone-a (options: %#v)", got, req.Provider.Options)
	}
}

// TestWizardTemporaryCloudflareCarriesNoZone proves a Quick Tunnel connection
// never carries a zone.
func TestWizardTemporaryCloudflareCarriesNoZone(t *testing.T) {
	m := wizardPermanentCloudflare(cloudflareSnapshotWithZone("acct-1", "zone-a"))
	m.state.ExposureMode = "temporary_public"
	m.state.Hostname = ""
	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if z := req.Provider.Options["zone_id"]; z != "" {
		t.Fatalf("temporary Cloudflare request Options[zone_id] = %q, want empty", z)
	}
}

// TestWizardPermanentNonCloudflareCarriesNoZone proves the zone stamp is
// Cloudflare-only.
func TestWizardPermanentNonCloudflareCarriesNoZone(t *testing.T) {
	snap := []ipc.ProviderDTO{{
		ID: "ngrok", DisplayName: "ngrok", Availability: "ready", Readiness: "ready", Selectable: true,
		Accounts: []ipc.ProviderAccountDTO{{ID: "acct-1", Label: "Acc", Status: "authenticated"}},
	}}
	m := wizardPermanentCloudflare(snap)
	m.state.Provider = "ngrok"
	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if z := req.Provider.Options["zone_id"]; z != "" {
		t.Fatalf("non-Cloudflare request Options[zone_id] = %q, want empty", z)
	}
}
