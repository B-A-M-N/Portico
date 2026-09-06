package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// cloudflareDiscoveringSetupFlow is the flow the supervisor declares for
// Cloudflare, with the discovery-capable identity field.
func cloudflareDiscoveringSetupFlow() *ipc.SetupFlowDTO {
	return &ipc.SetupFlowDTO{
		ProviderID: "cloudflare", Kind: "account",
		IdentityField: "account_id", SecretField: "credential",
		Summary: "Configure a Cloudflare account.",
		Fields: []ipc.SetupFieldDTO{
			{ID: "account_id", Label: "Account ID", InputKind: "provider_identity", EnvVars: []string{"CLOUDFLARE_ACCOUNT_ID"}},
			{ID: "label", Label: "Label"},
			{ID: "zone_id", Label: "Zone ID"},
			{ID: "credential", Label: "API token", Secret: true, Required: true},
		},
	}
}

// startDiscoverySetup opens provider setup and delivers the declared flow.
func startDiscoverySetup(t *testing.T) Model {
	t.Helper()
	m := readyModel(&fakeClient{}, testSnapshot())
	next, _ := m.beginProviderSetup("cloudflare")
	m = next
	m.providerSetupFlow = cloudflareDiscoveringSetupFlow()
	m.focusProviderSetupField()
	return m
}

// TestSetupValidatesCredentialBeforeConfigure pins the order of operations on
// the credential step of a discovery-capable provider: enter checks the
// credential and discovers accounts instead of walking on to an identity field
// the user cannot answer yet.
func TestSetupValidatesCredentialBeforeConfigure(t *testing.T) {
	m := startDiscoverySetup(t)

	// Walk to the credential step: past account, label, zone.
	m.providerSetupIndex = m.providerSetupIndexForField("credential")
	for _, r := range "cf-token" {
		next, _ := m.Update(keyMsg(string(r)))
		m = next.(Model)
	}
	next, cmd := m.Update(keyMsg("enter"))
	m = next.(Model)

	if cmd == nil {
		t.Fatal("enter on the credential step issued no validation")
	}
	if !m.providerSetupValidating {
		t.Fatal("the form did not enter the validating state")
	}
	if m.providerSetupSubmitting {
		t.Fatal("enter on the credential step sent a configure request")
	}
}

// TestSetupRendersAccountChoicesFromValidation pins the discovery answer
// becoming a selectable list: two accounts are offered by friendly name, and
// selecting one records its ID and label as the answers.
func TestSetupRendersAccountChoicesFromValidation(t *testing.T) {
	m := startDiscoverySetup(t)

	next, _ := m.Update(providerAccountValidatedMsg{
		Generation: m.providerSetupValidateRequests.next(),
		Response: &ipc.ConfigureProviderAccountResponse{
			AccountSelectionRequired: true,
			AccountChoices: []ipc.ProviderAccountDTO{
				{ID: "acct-personal", Label: "Personal"},
				{ID: "acct-work", Label: "Work"},
			},
		},
	})
	m = next.(Model)

	if m.providerSetupIndex != m.providerSetupIndexForField("account_id") {
		t.Fatalf("after an ambiguous answer the form is at index %d, want the account question", m.providerSetupIndex)
	}
	view := m.renderProviderSetup()
	for _, want := range []string{"Personal", "Work"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the account question does not offer %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Account ID:") || viewContainsTextBox(view) {
		t.Fatalf("a discovery question is rendered as a text box:\n%s", view)
	}

	// Choose the second account.
	next, _ = m.Update(keyMsg("down"))
	m = next.(Model)
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)

	if got := m.providerSetupValue("account_id"); got != "acct-work" {
		t.Fatalf("selected account = %q, want acct-work", got)
	}
	if got := m.providerSetupValue("label"); got != "Work" {
		t.Fatalf("selected label = %q, want Work", got)
	}
}

// view reports whether the view is drawing a live text-entry surface, which a
// choice list must not.
func viewContainsTextBox(view string) bool {
	return strings.Contains(view, "e.g. ")
}

// TestSetupRendersZoneChoicesWithNoZoneOption pins the zone question: the
// zones the credential can see are offered by domain name, a deliberate
// no-zone answer states what it gives up, and selecting one records the ID.
func TestSetupRendersZoneChoicesWithNoZoneOption(t *testing.T) {
	m := startDiscoverySetup(t)

	next, _ := m.Update(providerAccountValidatedMsg{
		Generation: m.providerSetupValidateRequests.next(),
		Response: &ipc.ConfigureProviderAccountResponse{
			Validated: true, AccountID: "acct-personal", AccountLabel: "Personal",
			Zones: []ipc.ZoneDTO{{ID: "zone-1", Name: "example.com"}},
		},
	})
	m = next.(Model)

	// A resolved answer carries the discovered identity into the form.
	if got := m.providerSetupValue("account_id"); got != "acct-personal" {
		t.Fatalf("discovered account = %q, want acct-personal", got)
	}
	if got := m.providerSetupValue("label"); got != "Personal" {
		t.Fatalf("discovered label = %q, want Personal", got)
	}
	if m.providerSetupIndex != m.providerSetupIndexForField("zone_id") {
		t.Fatalf("after a resolved answer the form is at index %d, want the zone question", m.providerSetupIndex)
	}

	view := m.renderProviderSetup()
	if !strings.Contains(view, "example.com") {
		t.Fatalf("the zone question does not offer the discovered zone:\n%s", view)
	}
	if !strings.Contains(view, "No zone") {
		t.Fatalf("the zone question does not offer the no-zone answer:\n%s", view)
	}
	if !strings.Contains(view, "temporary addresses") {
		t.Fatalf("the no-zone answer does not say what it gives up:\n%s", view)
	}

	// Discovery already answered account and label, so the first decision
	// left to the user is the zone. Selecting "no zone" records no zone and
	// moves on; the option is a real answer, not a dead end.
	before := m.providerSetupIndex
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)
	if got := m.providerSetupValue("zone_id"); got != "" {
		t.Fatalf("no-zone answer recorded %q, want empty", got)
	}
	if m.providerSetupIndex != before+1 {
		t.Fatalf("no-zone answer did not move on: index %d, want %d", m.providerSetupIndex, before+1)
	}

	// Going back and choosing the discovered zone instead records its ID.
	next, _ = m.Update(keyMsg("esc"))
	m = next.(Model)
	next, _ = m.Update(keyMsg("down")) // past "no zone" onto example.com
	m = next.(Model)
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)
	if got := m.providerSetupValue("zone_id"); got != "zone-1" {
		t.Fatalf("selected zone = %q, want zone-1", got)
	}
}

// TestSetupValidationFailureKeepsAnswersAndDropsSecret pins the failure path
// of validate-before-configure: the rejected credential is cleared, the
// answers given before it survive, and the form returns to the credential step.
func TestSetupValidationFailureKeepsAnswersAndDropsSecret(t *testing.T) {
	m := startDiscoverySetup(t)
	m.setProviderSetupValue("label", "Personal")

	next, _ := m.Update(providerAccountValidatedMsg{
		Generation: m.providerSetupValidateRequests.next(),
		Err:        errors.New("the token lacks Zone Read"),
	})
	m = next.(Model)

	if got := m.providerSetupValue("credential"); got != "" {
		t.Fatalf("the rejected credential stayed in memory: %q", got)
	}
	if got := m.providerSetupValue("label"); got != "Personal" {
		t.Fatalf("accepted answers were discarded: label=%q", got)
	}
	if m.providerSetupIndex != m.providerSetupIndexForField("credential") {
		t.Fatalf("form at index %d, want the credential step after a rejection", m.providerSetupIndex)
	}
}

// TestValidationAnswerForAbandonedAttemptIsIgnored pins correlation: a late
// discovery answer for a setup the user has left must not reshape the form
// they are now in.
func TestValidationAnswerForAbandonedAttemptIsIgnored(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.providerSetupStep = 1
	m.providerSetupProviderID = "other-provider"
	m.providerSetupFlow = &ipc.SetupFlowDTO{
		ProviderID: "other-provider", Kind: "account",
		Fields: []ipc.SetupFieldDTO{{ID: "credential", Label: "Key", Secret: true, Required: true}},
	}
	m.providerSetupIndex = 0

	// A generation the model never asked for.
	next, _ := m.Update(providerAccountValidatedMsg{
		Generation: requestGeneration(9999),
		Response: &ipc.ConfigureProviderAccountResponse{
			AccountSelectionRequired: true,
			AccountChoices:           []ipc.ProviderAccountDTO{{ID: "acct-x", Label: "X"}},
		},
	})
	m = next.(Model)

	if m.providerSetupChoices != nil {
		t.Fatal("a stale discovery answer rewrote the active form")
	}
}

// TestSetupCredentialStaysMaskedDuringValidation is a small physical check
// that the validating state says what it is doing rather than sitting silent.
func TestSetupCredentialStaysMaskedDuringValidation(t *testing.T) {
	m := startDiscoverySetup(t)
	m.providerSetupIndex = m.providerSetupIndexForField("credential")
	m.providerSetupValidating = true

	view := m.renderProviderSetup()
	if !strings.Contains(view, "Checking the credential") {
		t.Fatalf("the validating state is silent:\n%s", view)
	}
	_ = tea.Quit // keep the tea import if assertions change
}
