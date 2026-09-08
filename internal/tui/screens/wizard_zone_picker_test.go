package screens

import (
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Zone picker tests pin finding-7's UI half: a permanent Cloudflare connection
// must be able to select which DNS zone it targets, from every zone the
// account's credential can see — not just the single zone bound to the account.
//
// The account carries one "selected default" zone; the connection carries its
// own zone. Without the picker a user managing app.example and other sites
// under the same Cloudflare identity could only ever target the account's one
// zone, and updating the account's default would silently move old connections.

// zonePickerSnapshot returns a Cloudflare whose account carries a default zone,
// matching a real snapshot after setup selects one.
func zonePickerSnapshot() []ipc.ProviderDTO {
	return []ipc.ProviderDTO{{
		ID: "cloudflare", DisplayName: "Cloudflare",
		Availability: "ready", Readiness: "ready", Selectable: true,
		Accounts: []ipc.ProviderAccountDTO{{
			ID: "acct-1", Label: "Personal", Status: "authenticated",
			ZoneID: "zone-default", ZoneName: "example.com",
		}},
		Capabilities: &ipc.CapabilitySetDTO{
			TemporaryAddresses: true,
			CustomHostnames:    true,
			ManagedDNS:         true,
			ProtectionModes:    []string{"none", "email_otp"},
			Protocols:          []string{"http", "https"},
		},
	}}
}

// wizardAtCloudflareHostname drives a wizard into the permanent-Cloudflare
// hostname step with a real client, then dispatches the async zone fetch as a
// message so the zone rows are ready. It constructs the wizard's state up to
// the exposure question directly, then crosses the exposure→hostname
// transition — the precise place the zone listing is started — by pressing
// Enter on the permanent-public exposure choice.
func wizardAtCloudflareHostname(t *testing.T, client *fakeWizardClient) *WizardModel {
	t.Helper()
	m := NewWizard(client, zonePickerSnapshot())
	// Prepare the state a real keyflow reaches just before the exposure choice,
	// without depending on the exact sequence of earlier questions.
	m.state.Name = "app"
	m.state.SourceType = "existing_service"
	m.state.SourceAddress = "127.0.0.1:3000"
	m.state.SourceProtocol = "http"
	m.state.Provider = "cloudflare"
	m.state.AccountID = "acct-1"
	m.state.ConnectionKind = "service_exposure"
	m.state.Step = WizardStepExposure
	m.selected = choiceIndex(m.exposureChoices(), "permanent_public")

	// Selecting permanent public enters the hostname step and starts the zone
	// fetch. The returned command is the async fetch; dispatching it is what
	// performs the IPC call, so assert after.
	cmd := m.HandleKey("enter")
	if m.Step() != WizardStepHostname {
		t.Fatalf("step after choosing permanent exposure = %d, want hostname", m.Step())
	}
	if cmd == nil {
		t.Fatal("reaching the hostname step did not start a zone fetch")
	}
	msg, ok := cmd().(WizardZoneMsg)
	if !ok {
		t.Fatalf("the hostname step returned %T, want a zone fetch", cmd())
	}
	m.HandleZones(msg)
	if client.zoneListCalls == 0 {
		t.Fatal("dispatching the zone fetch did not call the client")
	}
	return m
}

// TestWizardZonePickerOffersEveryAccountZone pins that the account's whole zone
// set is offered, not just the selected default.
func TestWizardZonePickerOffersEveryAccountZone(t *testing.T) {
	client := &fakeWizardClient{zones: []ipc.ZoneDTO{
		{ID: "zone-default", Name: "example.com"},
		{ID: "zone-a", Name: "app.example.org"},
		{ID: "zone-b", Name: "other.net"},
	}}
	m := wizardAtCloudflareHostname(t, client)

	// The async fetch ran once.
	if client.zoneListCalls != 1 {
		t.Fatalf("zone list calls = %d, want 1", client.zoneListCalls)
	}
	// The chosen zone rows include every zone, default first.
	rows := m.hostnameChoiceRows()
	var joined string
	for _, r := range rows {
		joined += r + "|"
	}
	for _, want := range []string{"app.app.example.org", "app.other.net"} {
		if !strings.Contains(joined, want) {
			t.Errorf("hostname rows do not offer %q: %s", want, rows)
		}
	}
	if len(rows) != 4 {
		t.Fatalf("expected three zone rows plus manual, got %v", rows)
	}
}

// TestWizardChoosingAZoneRowCarriesTheConnectionsOwnZone pins the finding-7
// payoff: picking the second zone must stamp that zone's ID on the connection,
// not the account default.
func TestWizardChoosingAZoneRowCarriesTheConnectionsOwnZone(t *testing.T) {
	client := &fakeWizardClient{zones: []ipc.ZoneDTO{
		{ID: "zone-default", Name: "example.com"},
		{ID: "zone-b", Name: "other.net"},
	}}
	m := wizardAtCloudflareHostname(t, client)

	// Choose the second zone row (other.net).
	m.HandleKey("down")
	m.HandleKey("enter")

	if m.state.Hostname != "app.other.net" {
		t.Fatalf("chosen hostname = %q, want app.other.net", m.state.Hostname)
	}
	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if got := req.Provider.Options["zone_id"]; got != "zone-b" {
		t.Fatalf("the connection carries zone_id %q, want the chosen zone-b", got)
	}
}

// TestWizardZonePickerDefaultRowCarriesDefaultZone pins that picking the default
// zone row stamps the default zone explicitly (not silently zeroed), so a later
// account-default change cannot retarget the connection.
func TestWizardZonePickerDefaultRowCarriesDefaultZone(t *testing.T) {
	client := &fakeWizardClient{zones: []ipc.ZoneDTO{
		{ID: "zone-default", Name: "example.com"},
		{ID: "zone-b", Name: "other.net"},
	}}
	m := wizardAtCloudflareHostname(t, client)

	// First row is the default zone; select it.
	m.HandleKey("enter")

	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if got := req.Provider.Options["zone_id"]; got != "zone-default" {
		t.Fatalf("the connection carries zone_id %q, want the explicitly chosen zone-default", got)
	}
}

// TestWizardZonePickerTypedHostnameCarriesNoChosenZone pins that typing a
// hostname by hand records no explicit per-connection zone: Portico did not
// attach a zone the user never picked. The request still falls back to the
// account-default zone (the legacy path), because a permanent Cloudflare
// connection needs a zone and the account's selected one is the honest default.
func TestWizardZonePickerTypedHostnameCarriesNoChosenZone(t *testing.T) {
	client := &fakeWizardClient{zones: []ipc.ZoneDTO{
		{ID: "zone-default", Name: "example.com"},
		{ID: "zone-b", Name: "other.net"},
	}}
	m := wizardAtCloudflareHostname(t, client)

	// Move to the manual row and type a hostname.
	for m.selected != len(m.hostnameChoiceRows())-1 {
		m.HandleKey("down")
	}
	m.HandleKey("enter") // switch to typing
	m.setInput("custom.example.org")
	m.HandleKey("enter")

	if m.zoneSelected != nil {
		t.Fatalf("a typed hostname must not record an explicit zone choice, got %#v", *m.zoneSelected)
	}
	req, err := m.buildRequest()
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	// No explicit choice: the connection relies on the account's default zone,
	// preserving the pre-finding-7 path for a user who types the hostname.
	if got := req.Provider.Options["zone_id"]; got != "zone-default" {
		t.Fatalf("a typed hostname with no explicit zone falls back to the account default, got %q", got)
	}
}

// TestWizardZoneFetchFailureIsNotAnEmptyAccount pins that a failed zone fetch
// is not presented as "an account with no zones": zones stay nil (not an empty
// slice), so the hostname step keeps the account's own suggestions and the flow
// is never blocked behind a false "no zones available" state.
func TestWizardZoneFetchFailureIsNotAnEmptyAccount(t *testing.T) {
	client := &fakeWizardClient{
		zonesErr: errors.New("zone listing refused"),
	}
	snap := zonePickerSnapshot() // the account carries a known default zone
	m := wizardZoneOnly(t, snap, client)

	cmd := m.zoneCmd()
	if msg, ok := cmd().(WizardZoneMsg); !ok {
		t.Fatalf("expected a zone fetch, got %T", cmd())
	} else {
		m.HandleZones(msg)
	}

	// Zones stay nil (never an empty slice) so "loading failed" and "scanned,
	// found none" stay distinct, and the record of the failure is kept.
	if m.zones != nil {
		t.Fatal("a failed fetch left a zone list behind")
	}
	if m.zonesErr == nil {
		t.Fatal("a failed fetch did not record the error")
	}
	// The step still offers the account's own zone suggestion — the failure did
	// not stand in for "this account has no zones".
	rows := m.hostnameChoiceRows()
	var joined string
	for _, r := range rows {
		joined += r + "|"
	}
	if !strings.Contains(joined, "app.example.com") {
		t.Fatalf("after a failed fetch the account's own zone is gone: %s", rows)
	}
}

// wizardZoneOnly builds a wizard positioned at the permanent-Cloudflare
// hostname step with a given client and snapshot.
func wizardZoneOnly(t *testing.T, snap []ipc.ProviderDTO, client *fakeWizardClient) *WizardModel {
	t.Helper()
	m := NewWizard(client, snap)
	m.state.Name = "app"
	m.state.Provider = "cloudflare"
	m.state.AccountID = "acct-1"
	m.state.ConnectionKind = "service_exposure"
	m.state.SourceType = "existing_service"
	m.state.SourceAddress = "127.0.0.1:3000"
	m.state.SourceProtocol = "http"
	m.state.ExposureMode = "permanent_public"
	m.state.Step = WizardStepHostname
	m.hostnameManual = false
	m.selected = 0
	m.setInput("")
	return m
}