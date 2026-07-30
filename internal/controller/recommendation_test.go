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

	rec := recommendWith(t, []provider.ProviderSnapshot{unauth, auth},
		RecommendationInput{ExposureMode: core.ExposureTemporary, PreferredAccount: "acct-zzz"})

	if rec.Recommended == nil || rec.Recommended.ProviderID != "zzz" {
		t.Fatalf("expected the authenticated provider with the selected account, got %+v", rec.Recommended)
	}
	if rec.Recommended.AccountID != "acct-zzz" {
		t.Fatalf("account = %q, want acct-zzz", rec.Recommended.AccountID)
	}
	if len(rec.Recommended.Strengths) == 0 {
		t.Fatal("recommendation gives no reasons")
	}
	// The unauthenticated provider is still eligible, but must be listed as an
	// alternative with a setup action rather than presented as ready.
	if len(rec.Alternatives) != 1 || rec.Alternatives[0].ProviderID != "aaa" {
		t.Fatalf("alternatives = %+v", rec.Alternatives)
	}
	if len(rec.Alternatives[0].SetupActions) == 0 {
		t.Fatal("unauthenticated alternative offers no setup action")
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
