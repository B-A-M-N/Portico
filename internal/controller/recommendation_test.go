package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// snapshotRegistry is a registry stub returning fixed provider snapshots, so
// the recommendation engine can be tested against exact capability sets.
type snapshotRegistry struct {
	snaps []provider.ProviderSnapshot
}

func (r *snapshotRegistry) Get(core.ProviderID) core.Provider                      { return nil }
func (r *snapshotRegistry) List() []provider.ProviderSnapshot                      { return r.snaps }
func (r *snapshotRegistry) Snapshot() []provider.ProviderSnapshot                  { return r.snaps }
func (r *snapshotRegistry) Add(core.Provider) error                                { return nil }
func (r *snapshotRegistry) AddCatalogEntry(provider.CatalogEntry)                  {}
func (r *snapshotRegistry) Install(provider.Installation)                          {}
func (r *snapshotRegistry) Replace(core.Provider)                                  {}
func (r *snapshotRegistry) Remove(core.ProviderID)                                 {}
func (r *snapshotRegistry) SetAccounts(core.ProviderID, []core.ProviderAccountID)  {}
func (r *snapshotRegistry) SetAccountInfo(core.ProviderID, []provider.AccountInfo) {}
func (r *snapshotRegistry) GetAccounts(core.ProviderID) []core.ProviderAccountID   { return nil }
func (r *snapshotRegistry) DiscoverIdentities(context.Context) []provider.ProviderSnapshot {
	return r.snaps
}

func capableProvider(id core.ProviderID, authenticated bool) provider.ProviderSnapshot {
	return provider.ProviderSnapshot{
		ID:            id,
		DisplayName:   string(id),
		Authenticated: authenticated,
		Availability:  provider.AvailabilityReady,
		Accounts:      []provider.AccountInfo{{ID: core.ProviderAccountID("acct-" + string(id))}},
		Capabilities: core.Capabilities{
			TemporaryAddresses: core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
			CustomHostnames:    core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
			ManagedDNS:         core.CapabilitySupport{Supported: true, Stability: core.StabilityStable},
			Protocols: map[core.Protocol]core.ProtocolCapability{
				core.ProtocolHTTP: {Supported: true},
			},
			BuiltInProtection: []core.ProtectionCapability{
				{Kind: core.ProtectionEmailOTP, Supported: true},
			},
		},
	}
}

func recommendWith(t *testing.T, snaps []provider.ProviderSnapshot, input RecommendationInput) *Recommendation {
	t.Helper()
	c := &Controller{registry: &snapshotRegistry{snaps: snaps}}
	rec, err := c.Recommend(context.Background(), input)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	return rec
}

// TestRecommendationHonoursStatedRequirements pins audit item 6. The handler
// previously ignored the request entirely and returned the first authenticated
// provider with the generic reason "Provider is authenticated and ready".
func TestRecommendationHonoursStatedRequirements(t *testing.T) {
	weak := capableProvider("weak", true)
	weak.Capabilities.CustomHostnames = core.CapabilitySupport{Supported: false}
	weak.Capabilities.ManagedDNS = core.CapabilitySupport{Supported: false}

	rec := recommendWith(t, []provider.ProviderSnapshot{weak, capableProvider("strong", true)},
		RecommendationInput{ExposureMode: core.ExposurePermanent, RequestedAddress: "demo.example.com"})

	if rec.Recommended == nil {
		t.Fatalf("no recommendation returned; summary=%q", rec.Summary)
	}
	if rec.Recommended.ProviderID != "strong" {
		t.Fatalf("recommended %q, want strong", rec.Recommended.ProviderID)
	}
	// The provider that cannot serve a hostname must be reported as ineligible
	// with a reason, not silently dropped.
	var found bool
	for _, bad := range rec.Ineligible {
		if bad.ProviderID == "weak" {
			found = true
			if len(bad.BlockingReasons) == 0 {
				t.Fatal("ineligible provider carries no blocking reason")
			}
		}
	}
	if !found {
		t.Fatal("provider that cannot meet the requirement was not reported as ineligible")
	}
}

// TestRecommendationReturnsNothingWhenNoProviderQualifies ensures the engine
// declines rather than falling back to an unsuitable provider.
func TestRecommendationReturnsNothingWhenNoProviderQualifies(t *testing.T) {
	p := capableProvider("cloudflare", true)
	p.Capabilities.Protocols = map[core.Protocol]core.ProtocolCapability{core.ProtocolHTTP: {Supported: true}}

	rec := recommendWith(t, []provider.ProviderSnapshot{p},
		RecommendationInput{ExposureMode: core.ExposureTemporary, Protocol: core.ProtocolTCP})

	if rec.Recommended != nil {
		t.Fatalf("recommended %q despite unsupported protocol", rec.Recommended.ProviderID)
	}
	if !strings.Contains(rec.Summary, "No provider") {
		t.Fatalf("summary does not explain the empty result: %q", rec.Summary)
	}
	if !strings.Contains(rec.Summary, "tcp") {
		t.Fatalf("summary does not name the unmet requirement: %q", rec.Summary)
	}
}

// TestUnusableProvidersAreNeverRecommended ensures availability is a hard
// constraint: a provider whose client is missing cannot be recommended however
// well its declared capabilities match.
func TestUnusableProvidersAreNeverRecommended(t *testing.T) {
	for name, availability := range map[string]provider.Availability{
		"client missing":  provider.AvailabilityClientMissing,
		"not implemented": provider.AvailabilityNotImplemented,
		"experimental":    provider.AvailabilityExperimental,
		"degraded":        provider.AvailabilityDegraded,
	} {
		t.Run(name, func(t *testing.T) {
			p := capableProvider("p", true)
			p.Availability = availability
			rec := recommendWith(t, []provider.ProviderSnapshot{p},
				RecommendationInput{ExposureMode: core.ExposureTemporary})
			if rec.Recommended != nil {
				t.Fatalf("recommended an unusable provider (%s)", availability)
			}
			if len(rec.Ineligible) != 1 || len(rec.Ineligible[0].BlockingReasons) == 0 {
				t.Fatal("unusable provider was not reported with a reason")
			}
		})
	}
}

// TestRecommendationRejectsSSEOverTemporaryAddress pins a real capability
// constraint rather than a generic authentication check.
func TestRecommendationRejectsSSEOverTemporaryAddress(t *testing.T) {
	rec := recommendWith(t, []provider.ProviderSnapshot{capableProvider("cloudflare", true)},
		RecommendationInput{
			SourceKind:   core.SourceMCP,
			MCPTransport: core.MCPTransportSSE,
			ExposureMode: core.ExposureTemporary,
		})
	if rec.Recommended != nil {
		t.Fatal("recommended a temporary address for SSE, whose address must not change")
	}
}

// TestRecommendationPrefersAuthenticatedAndSelectedAccount checks the scoring
// order and that ties break deterministically.
func TestRecommendationPrefersAuthenticatedAndSelectedAccount(t *testing.T) {
	unauth := capableProvider("aaa", false)
	auth := capableProvider("zzz", true)

	// The preference names its provider: an account ID alone is ambiguous once
	// two providers can hold accounts of the same name.
	rec := recommendWith(t, []provider.ProviderSnapshot{unauth, auth},
		RecommendationInput{
			ExposureMode:      core.ExposureTemporary,
			PreferredProvider: "zzz",
			PreferredAccount:  "acct-zzz",
		})

	if rec.Recommended == nil || rec.Recommended.ProviderID != "zzz" {
		t.Fatalf("expected the authenticated provider with the selected account, got %+v", rec.Recommended)
	}
	if rec.Recommended.AccountID != "acct-zzz" {
		t.Fatalf("account = %q, want acct-zzz", rec.Recommended.AccountID)
	}
	if len(rec.Recommended.Strengths) == 0 {
		t.Fatal("recommendation gives no reasons")
	}
	// The preference must actually have been credited, not merely satisfied by
	// the provider happening to have one account.
	var credited bool
	for _, reason := range rec.Recommended.Strengths {
		if strings.Contains(reason, "account you selected") {
			credited = true
		}
	}
	if !credited {
		t.Fatal("the preferred account was not credited, so the preference did nothing")
	}
	// The provider without an account is still eligible and must not be
	// described as needing account setup: a Quick Tunnel and a local port
	// forward are ready and accountless. Whether a provider can be used is
	// settled by its availability, not by whether it holds a credential.
	if len(rec.Alternatives) != 1 || rec.Alternatives[0].ProviderID != "aaa" {
		t.Fatalf("alternatives = %+v", rec.Alternatives)
	}
	for _, tradeoff := range rec.Alternatives[0].Tradeoffs {
		if strings.Contains(tradeoff, "needs account setup") {
			t.Fatal("a ready provider was described as needing account setup")
		}
	}
}

// TestRecommendationStatesTradeoffs ensures the result explains consequences,
// not just a provider name.
func TestRecommendationStatesTradeoffs(t *testing.T) {
	rec := recommendWith(t, []provider.ProviderSnapshot{capableProvider("cloudflare", true)},
		RecommendationInput{ExposureMode: core.ExposureTemporary, ProtectionKind: core.ProtectionNone})

	if rec.Recommended == nil {
		t.Fatal("no recommendation")
	}
	joined := strings.Join(rec.Recommended.Tradeoffs, " | ")
	if !strings.Contains(joined, "changes each time") {
		t.Fatalf("tradeoffs do not mention the changing address: %q", joined)
	}
	if !strings.Contains(joined, "anyone with the address") {
		t.Fatalf("tradeoffs do not mention unrestricted access: %q", joined)
	}
}

// TestPrivateAndPublicProvidersAreNotInterchangeable pins the cross-provider
// half of audit item 5: a public provider must not be offered for a private
// client tunnel, and a private-only provider must not be offered for a public
// exposure.
func TestPrivateAndPublicProvidersAreNotInterchangeable(t *testing.T) {
	public := capableProvider("cloudflare", true)

	privateOnly := provider.ProviderSnapshot{
		ID:            "openai_tunnel",
		DisplayName:   "OpenAI Secure MCP Tunnel",
		Authenticated: true,
		Availability:  provider.AvailabilityReady,
		Accounts:      []provider.AccountInfo{{ID: "acct-openai"}},
		Capabilities: core.Capabilities{
			PrivateExposure: core.CapabilitySupport{Supported: true, Stability: core.StabilityExperimental},
			Protocols: map[core.Protocol]core.ProtocolCapability{
				core.ProtocolHTTP: {Supported: true, Private: true},
			},
		},
	}

	t.Run("public exposure never selects the private provider", func(t *testing.T) {
		rec := recommendWith(t, []provider.ProviderSnapshot{privateOnly, public},
			RecommendationInput{ExposureMode: core.ExposureTemporary})
		if rec.Recommended == nil || rec.Recommended.ProviderID != "cloudflare" {
			t.Fatalf("recommended %+v for a public exposure", rec.Recommended)
		}
	})

	t.Run("private exposure never selects the public provider", func(t *testing.T) {
		rec := recommendWith(t, []provider.ProviderSnapshot{privateOnly, public},
			RecommendationInput{ExposureMode: core.ExposurePrivate})
		if rec.Recommended == nil {
			t.Fatalf("no provider offered for a private exposure; summary=%q", rec.Summary)
		}
		if rec.Recommended.ProviderID != "openai_tunnel" {
			t.Fatalf("recommended %q for a private exposure", rec.Recommended.ProviderID)
		}
		// Cloudflare must be reported ineligible with a reason, not dropped.
		var found bool
		for _, bad := range rec.Ineligible {
			if bad.ProviderID == "cloudflare" && len(bad.BlockingReasons) > 0 {
				found = true
			}
		}
		if !found {
			t.Fatal("the public provider was not reported ineligible for private exposure")
		}
	})
}

// TestRecommendationAsksProvidersWhichKindsTheyRun pins the removal of a
// hardcoded list that had gone stale.
//
// The engine blocked every kind except service exposure, on the stated grounds
// that nothing else was executable. Portico had been creating and running local
// port forwards for some time, so the engine refused a kind the rest of the
// system supported — and wiring the wizard to it would have started refusing
// connections that work today.
func TestRecommendationAsksProvidersWhichKindsTheyRun(t *testing.T) {
	serviceOnly := core.Capabilities{}
	if !serviceOnly.Executes(core.ConnectionServiceExposure) {
		t.Fatal("a provider declaring no kinds must still run service exposure")
	}
	if serviceOnly.Executes(core.ConnectionPortForward) {
		t.Fatal("a provider declaring no kinds must not claim port forwarding")
	}

	forwarder := core.Capabilities{Kinds: []core.ConnectionKind{core.ConnectionPortForward}}
	if !forwarder.Executes(core.ConnectionPortForward) {
		t.Fatal("a declared kind was not honoured")
	}
	if forwarder.Executes(core.ConnectionServiceExposure) {
		t.Fatal("declaring one kind must not imply another")
	}

	// An unspecified kind constrains nothing.
	if !serviceOnly.Executes("") {
		t.Fatal("an unspecified kind must not block a provider")
	}
}

// TestAnAccountPreferenceBelongsToItsProvider pins that a preference is scored
// against the pair, not the account name.
//
// Account identity is (provider, account). With two providers each holding an
// account called "default", a preference carrying only the account ID credited
// both — so a user who chose one provider's account raised another provider's
// score by the same amount.
func TestAnAccountPreferenceBelongsToItsProvider(t *testing.T) {
	shared := []provider.AccountInfo{{ID: "default", Label: "Default", Status: "authenticated"}}
	caps := core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{Supported: true},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP: {Supported: true, Public: true},
		},
		BuiltInProtection: []core.ProtectionCapability{{Kind: core.ProtectionNone, Supported: true}},
	}
	reg := &snapshotRegistry{snaps: []provider.ProviderSnapshot{
		{ID: "provider-a", DisplayName: "A", Availability: provider.AvailabilityReady,
			Authenticated: true, Accounts: shared, Capabilities: caps},
		{ID: "provider-b", DisplayName: "B", Availability: provider.AvailabilityReady,
			Authenticated: true, Accounts: shared, Capabilities: caps},
	}}
	ctrl := &Controller{registry: reg}

	rec, err := ctrl.Recommend(context.Background(), RecommendationInput{
		Kind: core.ConnectionServiceExposure, SourceKind: core.SourceExisting,
		ExposureMode: core.ExposureTemporary, Protocol: core.ProtocolHTTP,
		PreferredProvider: "provider-b", PreferredAccount: "default",
	})
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Recommended == nil || rec.Recommended.ProviderID != "provider-b" {
		t.Fatalf("recommended %#v, want the provider whose account was preferred", rec.Recommended)
	}

	// The other provider holds an account of the same name and must not be
	// credited for it.
	for _, alt := range rec.Alternatives {
		if alt.ProviderID != "provider-a" {
			continue
		}
		for _, reason := range alt.Strengths {
			if strings.Contains(reason, "account you selected") {
				t.Fatal("a preference for one provider's account credited another's")
			}
		}
		if alt.Score >= rec.Recommended.Score {
			t.Fatalf("scores did not separate: a=%d b=%d", alt.Score, rec.Recommended.Score)
		}
	}
}

// TestAnUnconfiguredProviderIsNotRecommended pins that eligibility and
// selectability are one answer.
//
// An unconfigured provider was eligible: it could be returned as the best
// choice, shown with "before this can be used", and selected — producing a
// connection that could not open. Whether a provider can carry a connection is
// now decided in one place and the engine reads it.
func TestAnUnconfiguredProviderIsNotRecommended(t *testing.T) {
	p := capableProvider("cloudflare", false)
	p.Availability = provider.AvailabilityUnconfigured

	rec := recommendWith(t, []provider.ProviderSnapshot{p},
		RecommendationInput{ExposureMode: core.ExposureTemporary})

	if rec.Recommended != nil {
		t.Fatalf("an unconfigured provider was recommended: %+v", rec.Recommended)
	}
	if len(rec.Alternatives) != 0 {
		t.Fatalf("an unconfigured provider was offered as an alternative: %+v", rec.Alternatives)
	}
	// It must still be reported, with what would make it usable.
	if len(rec.Ineligible) != 1 {
		t.Fatalf("the provider vanished instead of being explained: %+v", rec.Ineligible)
	}
	if len(rec.Ineligible[0].BlockingReasons) == 0 {
		t.Fatal("no reason given for refusing it")
	}
}

// TestAReadyAccountlessProviderIsEligible pins the other direction. Quick
// Tunnels and local port forwards need no account and must not be treated as
// unusable for lacking one.
func TestAReadyAccountlessProviderIsEligible(t *testing.T) {
	p := capableProvider("portforward", false)
	p.Accounts = nil
	p.Availability = provider.AvailabilityReady

	rec := recommendWith(t, []provider.ProviderSnapshot{p},
		RecommendationInput{ExposureMode: core.ExposureTemporary})

	if rec.Recommended == nil {
		t.Fatalf("a ready accountless provider was not recommended; summary=%q", rec.Summary)
	}
	for _, tradeoff := range rec.Recommended.Tradeoffs {
		if strings.Contains(tradeoff, "needs account setup") {
			t.Fatal("a provider that needs no account was said to need one")
		}
	}
}
