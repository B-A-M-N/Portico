package screens

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Hostname flow tests pin the contract introduced with zone suggestions:
//
//   - When the bound Cloudflare account has a zone, the hostname step offers
//     `<connection>.<zone>` rows and Enter stores the chosen one. The user
//     confirms rather than invents.
//   - "Type a different hostname" (or Tab) switches to the text field.
//   - With no zone bound — or no zone metadata at all — the step is a plain
//     text field exactly as before, so nothing about the old contract changes.
//
// The distinction matters because the handler now has two modes: a menu that
// would swallow typed characters, and a field that would treat ArrowUp as
// nothing. Either mode applied to the other screen silently breaks Enter.
func zoneSnapshot(zoneName string) []ipc.ProviderDTO {
	return []ipc.ProviderDTO{{
		ID: "cloudflare", DisplayName: "Cloudflare",
		Availability: "ready", Readiness: "ready", Selectable: true,
		Accounts: []ipc.ProviderAccountDTO{{
			ID: "acct-1", Label: "Personal", Status: "authenticated",
			ZoneName: zoneName,
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

// enterPermanentExposure walks a wizard to the hostname step with the given
// snapshot bound.
func enterPermanentExposure(t *testing.T, snapshot []ipc.ProviderDTO) *WizardModel {
	t.Helper()
	// Walk the real constructor so the model starts in its production shape;
	// the tests then steer to the permanent-exposure hostname step directly.
	m := NewWizard(nil, nil)
	m.caps = providerCapabilities{providers: snapshot}
	m.state.Name = "app"
	m.state.Provider = "cloudflare"
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

func TestHostnameStepOffersZoneSuggestionsWhenZoneKnown(t *testing.T) {
	m := enterPermanentExposure(t, zoneSnapshot("example.com"))

	rows := m.hostnameChoiceRows()
	if len(rows) != 2 || rows[0] != "app.example.com" {
		t.Fatalf("expected the zone suggestion first, got %v", rows)
	}
	render := m.View()
	if !strings.Contains(render, "app.example.com") {
		t.Fatalf("the suggestion should be visible on the hostname screen, got:\n%s", render)
	}

	// Enter on the suggestion row stores it and moves on — the point of the
	// contract: the user confirms, they do not transcribe.
	m.HandleKey("enter")
	if m.state.Hostname != "app.example.com" {
		t.Fatalf("expected the suggested hostname to be stored, got %q", m.state.Hostname)
	}
	if m.state.Step != WizardStepProtection {
		t.Fatalf("expected to advance to protection, got step %d", m.state.Step)
	}
}

func TestHostnameStepManualRowSwitchesToTyping(t *testing.T) {
	m := enterPermanentExposure(t, zoneSnapshot("example.com"))

	m.HandleKey("down") // the manual row
	m.HandleKey("enter")
	if !m.hostnameManual {
		t.Fatalf("choosing the manual row must switch to the text field")
	}

	m.HandleKey("m")
	m.HandleKey("y")
	m.HandleKey(".")
	m.HandleKey("z")
	m.HandleKey("o")
	m.HandleKey("n")
	m.HandleKey("e")
	m.HandleKey("enter")
	if m.state.Hostname != "my.zone" {
		t.Fatalf("expected the typed hostname to be stored, got %q", m.state.Hostname)
	}
}

func TestHostnameStepWithoutZoneIsPlainField(t *testing.T) {
	// An authenticated account with no zone metadata: exactly the pre-existing
	// contract, byte for byte.
	m := enterPermanentExposure(t, zoneSnapshot(""))

	if len(m.hostnameChoiceRows()) != 1 {
		t.Fatalf("expected only the manual row, got %v", m.hostnameChoiceRows())
	}

	// Typed characters must land in the field — the failure mode if the menu
	// handler owned this screen.
	m.HandleKey("h")
	m.HandleKey("o")
	m.HandleKey("s")
	m.HandleKey("t")
	if got := m.inputValue(); got != "host" {
		t.Fatalf("typed keys should reach the text field, field is %q", got)
	}

	m.HandleKey("enter")
	if m.state.Hostname != "host" {
		t.Fatalf("expected the typed hostname to be stored, got %q", m.state.Hostname)
	}
	if m.state.Step != WizardStepProtection {
		t.Fatalf("expected to advance to protection, got step %d", m.state.Step)
	}
}

func TestReviewCarriesUsabilitySourceSection(t *testing.T) {
	m := enterPermanentExposure(t, zoneSnapshot("example.com"))
	m.state.SelectedService = &ipc.DiscoveredServiceDTO{
		Address: "127.0.0.1:3000", Protocol: "http", SuggestedName: "nextjs",
	}
	m.state.Hostname = "app.example.com"
	m.state.Protection = "none"
	m.state.AccountID = "acct-1"
	m.state.Provider = "cloudflare"

	render := m.renderReview()
	if !strings.Contains(render, "Where each answer came from") {
		t.Fatalf("the review must disclose where each answer came from, got:\n%s", render)
	}
	if !strings.Contains(render, "Address: found by Portico's scan") {
		t.Fatalf("a discovered address must be labeled as discovered, got:\n%s", render)
	}
	if !strings.Contains(render, "Hostname: proposed by Portico") {
		t.Fatalf("a suggestion-accepted hostname must be labeled as suggested, got:\n%s", render)
	}
}

func TestReviewMarksManualEntriesAsManualRequired(t *testing.T) {
	// A manually typed address must be distinguishable from a discovered one
	// on the very screen the user approves.
	m := enterPermanentExposure(t, zoneSnapshot(""))
	m.state.SelectedService = nil
	m.state.SourceAddress = "10.0.0.7:8080"
	m.state.Hostname = "host.example.com"
	m.state.Protection = "none"

	render := m.renderReview()
	if !strings.Contains(render, "Address: yours to enter") {
		t.Fatalf("a manual address must be labeled manual-required, got:\n%s", render)
	}
}
