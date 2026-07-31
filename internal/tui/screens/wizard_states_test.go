package screens

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// TestNoEligibleProviderCannotProduceAConnection pins the empty state.
//
// When nothing can carry the requirements the wizard must say so and stop —
// saving a connection with no provider leaves a row that fails the moment
// anyone opens it, which is a worse outcome than a clear refusal.
func TestNoEligibleProviderCannotProduceAConnection(t *testing.T) {
	m := wizardAtProviderStep(t, &fakeWizardClient{})
	m.recommendCmd()
	m.HandleRecommendation(ProviderRecommendationMsg{
		Fingerprint: m.recommendFingerprint,
		Response: &ipc.ProviderRecommendationResponse{
			Summary: "No provider can meet these requirements (permanent_public, email_otp protection).",
			Filtered: []ipc.FilteredChoiceDTO{{
				ProviderID: "cloudflare", DisplayName: "Cloudflare",
				Reason:  "does not support permanent custom hostnames",
				Reasons: []string{"does not support permanent custom hostnames"},
			}},
		},
	})

	view := m.renderProvider()
	if !strings.Contains(view, "No provider can meet these requirements") {
		t.Fatalf("the outcome is not stated:\n%s", view)
	}

	// Selecting the refused provider must not advance.
	m.selected = 0
	m.HandleKey("enter")
	if m.Step() == WizardStepReview || m.state.Provider != "" {
		t.Fatalf("a refused provider was accepted: step=%d provider=%q", m.Step(), m.state.Provider)
	}
	if m.err == nil {
		t.Fatal("selecting a refused provider said nothing")
	}

	// And review refuses to build a connection with no provider.
	m.state.Step = WizardStepReview
	m.err = nil
	if cmd := m.HandleKey("enter"); cmd != nil {
		t.Fatal("a connection with no provider was submitted")
	}
	if m.err == nil {
		t.Fatal("review accepted a connection with no provider without comment")
	}
}

// TestAProviderWithOnlyPendingAccountsSaysSo pins the state that is easy to
// confuse with having no account at all.
//
// The user configured something and it did not take effect. Telling them "no
// account" would send them to repeat work they have already done.
func TestAProviderWithOnlyPendingAccountsSaysSo(t *testing.T) {
	snapshot := fullCloudflareSnapshot()
	snapshot[0].Accounts = nil
	snapshot[0].PendingAccounts = []ipc.ProviderAccountDTO{
		{ID: "acct-1", Label: "Personal", Status: "pending"},
	}

	m := NewWizard(&fakeWizardClient{}, snapshot)
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "permanent_public"
	m.state.Protection = "none"
	m.state.Provider = "cloudflare"
	m.state.Step = WizardStepProvider

	view := m.renderProvider()
	if !strings.Contains(view, "cannot be used yet") {
		t.Fatalf("a pending account is not distinguished from no account:\n%s", view)
	}
	if !strings.Contains(view, "Personal") {
		t.Fatalf("the pending account is not named:\n%s", view)
	}

	// It must never be offered for selection.
	if accounts := m.accountsFor("cloudflare"); len(accounts) != 0 {
		t.Fatalf("a pending account was offered as usable: %#v", accounts)
	}
}

// TestASingleAccountIsChosenWithoutAsking pins that a question with no choice
// in it is not asked.
func TestASingleAccountIsChosenWithoutAsking(t *testing.T) {
	snapshot := fullCloudflareSnapshot()
	snapshot[0].Accounts = []ipc.ProviderAccountDTO{
		{ID: "acct-a", Label: "Personal", Status: "authenticated"},
	}
	m := NewWizard(&fakeWizardClient{}, snapshot)
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "permanent_public"
	m.state.Protection = "none"
	m.state.Step = WizardStepProvider

	m.HandleKey("enter")

	if m.state.AccountID != "acct-a" {
		t.Fatalf("account = %q, want the only one", m.state.AccountID)
	}
	if m.Step() != WizardStepReview {
		t.Fatalf("step = %d, want review; there was nothing to ask", m.Step())
	}
}

// TestMultipleAccountsAreAlwaysAsked pins that a recommendation does not answer
// a question the user can see is open.
//
// The engine names an account for a multi-account provider by ordering, not by
// judgement — it scores the provider, not the account. Adopting that as the
// answer silently picked one of several.
func TestMultipleAccountsAreAlwaysAsked(t *testing.T) {
	snapshot := fullCloudflareSnapshot()
	snapshot[0].Accounts = []ipc.ProviderAccountDTO{
		{ID: "acct-a", Label: "Personal", Status: "authenticated"},
		{ID: "acct-b", Label: "Work", Status: "authenticated"},
	}
	m := NewWizard(&fakeWizardClient{}, snapshot)
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "permanent_public"
	m.state.Protection = "none"
	m.state.Step = WizardStepProvider
	m.recommendCmd()
	m.HandleRecommendation(ProviderRecommendationMsg{
		Fingerprint: m.recommendFingerprint,
		Response: &ipc.ProviderRecommendationResponse{
			Recommended: &ipc.ProviderChoiceDTO{
				ProviderID: "cloudflare", DisplayName: "Cloudflare", AccountID: "acct-b",
			},
		},
	})

	m.HandleKey("enter")

	if m.Step() != WizardStepAccount {
		t.Fatalf("step = %d, want the account question; there were two to choose from", m.Step())
	}
	if m.state.AccountID != "" {
		t.Fatalf("an account was chosen without asking: %q", m.state.AccountID)
	}
	// The suggested one is highlighted, so the recommendation still guides.
	accounts := m.accountsFor("cloudflare")
	if accounts[m.selected].ID != "acct-b" {
		t.Fatalf("cursor on %q, want the recommended account highlighted", accounts[m.selected].ID)
	}
}

// TestBackFromReviewReturnsToTheStepActuallyVisited pins that the declared
// sequence agrees with what the forward path did.
func TestBackFromReviewReturnsToTheStepActuallyVisited(t *testing.T) {
	single := fullCloudflareSnapshot()
	single[0].Accounts = []ipc.ProviderAccountDTO{{ID: "acct-a", Status: "authenticated"}}
	m := NewWizard(&fakeWizardClient{}, single)
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "permanent_public"
	m.state.Protection = "none"
	m.state.Step = WizardStepProvider
	m.HandleKey("enter") // one account, so review

	m.HandleKey("esc")
	if m.Step() != WizardStepProvider {
		t.Fatalf("back landed on %d; the account question was never asked", m.Step())
	}

	multi := fullCloudflareSnapshot()
	multi[0].Accounts = []ipc.ProviderAccountDTO{
		{ID: "acct-a", Status: "authenticated"}, {ID: "acct-b", Status: "authenticated"},
	}
	m2 := NewWizard(&fakeWizardClient{}, multi)
	m2.state.SourceType = "existing_service"
	m2.state.ExposureMode = "permanent_public"
	m2.state.Protection = "none"
	m2.state.Step = WizardStepProvider
	m2.HandleKey("enter") // two accounts, so the question is asked
	m2.HandleKey("enter") // answer it
	if m2.Step() != WizardStepReview {
		t.Fatalf("step = %d, want review", m2.Step())
	}
	m2.HandleKey("esc")
	if m2.Step() != WizardStepAccount {
		t.Fatalf("back landed on %d; the account question was visited", m2.Step())
	}
}

// TestAccountsAreScopedToTheirProvider ensures one provider's accounts are
// never offered for another.
func TestAccountsAreScopedToTheirProvider(t *testing.T) {
	snapshot := append(fullCloudflareSnapshot(), ipc.ProviderDTO{
		ID: "acme", DisplayName: "Acme", Availability: "ready", Readiness: "ready",
		Accounts: []ipc.ProviderAccountDTO{{ID: "acme-1", Label: "Acme account", Status: "authenticated"}},
		Capabilities: &ipc.CapabilitySetDTO{
			CustomHostnames: true, ProtectionModes: []string{"none"},
		},
	})
	m := NewWizard(&fakeWizardClient{}, snapshot)

	cloudflare := m.accountsFor("cloudflare")
	for _, account := range cloudflare {
		if strings.HasPrefix(account.ID, "acme") {
			t.Fatalf("another provider's account was offered for Cloudflare: %#v", cloudflare)
		}
	}
	acme := m.accountsFor("acme")
	if len(acme) != 1 || acme[0].ID != "acme-1" {
		t.Fatalf("acme accounts = %#v", acme)
	}
}

// TestTheRequestStatesItsKind pins that the created connection declares what it
// is, rather than relying on the supervisor's default.
func TestTheRequestStatesItsKind(t *testing.T) {
	m := NewWizard(&fakeWizardClient{}, fullCloudflareSnapshot())
	m.state = WizardState{
		Name: "demo", SourceType: "existing_service",
		SourceAddress: "127.0.0.1", SourceProtocol: "http", Port: "8080",
		ExposureMode: "temporary_public", Protection: "none", Provider: "cloudflare",
	}
	if got := m.buildRequest().Kind; got != "service_exposure" {
		t.Fatalf("request kind = %q, want it stated explicitly", got)
	}
}

// TestAProviderDisappearingMidWizardIsReported pins what happens when the
// landscape moves under an open wizard.
//
// Carrying a selection for a provider that has gone means discovering it when
// the connection refuses to open. The user's answers survive — their intent did
// not change — but the choice that can no longer work does not.
func TestAProviderDisappearingMidWizardIsReported(t *testing.T) {
	m := NewWizard(&fakeWizardClient{}, fullCloudflareSnapshot())
	m.state.SourceType = "existing_service"
	m.state.Name = "demo"
	m.state.ExposureMode = "permanent_public"
	m.state.Hostname = "demo.example.com"
	m.state.Protection = "none"
	m.state.Provider = "cloudflare"
	m.state.AccountID = "acct-1"
	m.state.Step = WizardStepReview

	// The client is uninstalled while the user is on the review screen.
	gone := fullCloudflareSnapshot()
	gone[0].Availability = "client_missing"
	gone[0].Readiness = "needs_client"
	m.ProvidersChanged(gone)

	if m.state.Provider != "" {
		t.Fatal("a selection for a provider that is gone was carried forward")
	}
	if m.Step() != WizardStepProvider {
		t.Fatalf("step = %d, want a return to provider selection", m.Step())
	}
	if m.err == nil {
		t.Fatal("the change was not reported")
	}
	// The answers that are still valid must survive.
	if m.state.Name != "demo" || m.state.Hostname != "demo.example.com" {
		t.Fatalf("answers unrelated to the provider were discarded: %#v", m.state)
	}
}

// TestAnUnchangedSnapshotDoesNotDiscardARecommendation ensures a routine
// refresh does not throw away work.
func TestAnUnchangedSnapshotDoesNotDiscardARecommendation(t *testing.T) {
	m := wizardAtProviderStep(t, &fakeWizardClient{})
	m.recommendCmd()
	m.HandleRecommendation(ProviderRecommendationMsg{
		Fingerprint: m.recommendFingerprint,
		Response: &ipc.ProviderRecommendationResponse{
			Recommended: &ipc.ProviderChoiceDTO{ProviderID: "cloudflare"},
		},
	})

	// The same providers arriving again is a refresh, not a change.
	m.ProvidersChanged(fullCloudflareSnapshot())

	if m.recommendation == nil {
		t.Fatal("an unchanged snapshot discarded the recommendation")
	}
}

// TestAProviderBecomingAvailableRefreshesTheOptions covers the other direction:
// finishing setup in another window must be picked up.
func TestAProviderBecomingAvailableRefreshesTheOptions(t *testing.T) {
	m := NewWizard(&fakeWizardClient{}, quickTunnelOnlySnapshot())
	m.state.SourceType = "existing_service"

	if availableValues(m.exposureChoices()) == nil {
		t.Fatal("setup failed")
	}
	if len(availableValues(m.exposureChoices())) != 1 {
		t.Fatal("a quick-tunnel-only provider offered more than a temporary address")
	}

	m.ProvidersChanged(fullCloudflareSnapshot())

	if len(availableValues(m.exposureChoices())) != 2 {
		t.Fatalf("configuring a provider did not open up the options: %v",
			availableValues(m.exposureChoices()))
	}
}

// TestOneProvidersAccountsAreNeverOfferedForAnother pins the removal of the
// aggregate account fallback.
//
// A provider with no accounts of its own fell back to a flat list holding every
// provider's accounts, so an accountless Cloudflare would have been bound to an
// ngrok account and failed later as an unavailable provider account.
func TestOneProvidersAccountsAreNeverOfferedForAnother(t *testing.T) {
	snapshot := quickTunnelOnlySnapshot() // Cloudflare, no accounts
	snapshot = append(snapshot, ipc.ProviderDTO{
		ID: "ngrok", DisplayName: "ngrok", Availability: "ready", Readiness: "ready",
		Accounts: []ipc.ProviderAccountDTO{{ID: "ngrok-1", Status: "authenticated"}},
		Capabilities: &ipc.CapabilitySetDTO{
			TemporaryAddresses: true, ProtectionModes: []string{"none"},
		},
	})
	m := NewWizard(&fakeWizardClient{}, snapshot)

	if accounts := m.accountsFor("cloudflare"); len(accounts) != 0 {
		t.Fatalf("an accountless provider borrowed another's accounts: %#v", accounts)
	}

	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "temporary_public"
	m.state.Protection = "none"
	m.state.Step = WizardStepProvider
	m.selected = choiceIndex(m.providerChoices(), "cloudflare")
	m.HandleKey("enter")

	if m.state.AccountID != "" {
		t.Fatalf("Cloudflare was bound to account %q, which belongs to another provider", m.state.AccountID)
	}
}

// TestAReplacedAccountIsNoticed pins that comparing account counts was not
// enough: one account replaced by another is the same count and a different
// world.
func TestAReplacedAccountIsNoticed(t *testing.T) {
	before := fullCloudflareSnapshot()
	before[0].Accounts = []ipc.ProviderAccountDTO{{ID: "acct-a", Status: "authenticated"}}
	m := NewWizard(&fakeWizardClient{}, before)
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "permanent_public"
	m.state.Protection = "none"
	m.state.Provider = "cloudflare"
	m.state.AccountID = "acct-a"
	m.state.Step = WizardStepReview

	after := fullCloudflareSnapshot()
	after[0].Accounts = []ipc.ProviderAccountDTO{{ID: "acct-b", Status: "authenticated"}}
	m.ProvidersChanged(after)

	if m.state.AccountID == "acct-a" {
		t.Fatal("a selection survived the account it named being replaced")
	}
	if m.err == nil {
		t.Fatal("the replacement was not reported")
	}
}

// TestADegradedProviderIsNotOfferedAsSelectable pins one definition of usable.
//
// The recommendation engine refuses experimental and degraded providers. The
// wizard's fallback list refused only missing clients and unimplemented ones,
// so when a recommendation failed it offered providers the engine considers
// unusable, and their capabilities made options look deliverable.
func TestADegradedProviderIsNotOfferedAsSelectable(t *testing.T) {
	snapshot := []ipc.ProviderDTO{{
		ID: "cloudflare", DisplayName: "Cloudflare",
		Availability: "degraded", Readiness: "error",
		Capabilities: &ipc.CapabilitySetDTO{
			TemporaryAddresses: true, CustomHostnames: true,
			ProtectionModes: []string{"none", "email_otp"},
		},
	}}
	m := NewWizard(&fakeWizardClient{}, snapshot)

	for _, choice := range m.snapshotChoices() {
		if choice.Value == "cloudflare" && choice.Available {
			t.Fatal("a degraded provider was offered as selectable")
		}
	}
	// Nor may its capabilities make an option look deliverable.
	m.state.SourceType = "existing_service"
	if values := availableValues(m.exposureChoices()); len(values) != 0 {
		t.Fatalf("a degraded provider made options appear available: %v", values)
	}
}
