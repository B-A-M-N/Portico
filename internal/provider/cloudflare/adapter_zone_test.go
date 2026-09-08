package cloudflare

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// zoneProfile builds a service-exposure profile whose connection carries its
// own DNS zone via Driver.Options["zone_id"] — the finding-7 slot. A hostname
// in a non-default zone is supplied separately.
func zoneProfile(id, name, hostname, zoneID string) *core.ConnectionProfile {
	p := cfTestProfile(core.ExposurePermanent, hostname, core.ProtectionSpec{Kind: core.ProtectionNone})
	p.ID = core.ConnectionID(id)
	p.Name = name
	if zoneID != "" {
		if p.Driver.Options == nil {
			p.Driver.Options = map[string]string{}
		}
		p.Driver.Options["zone_id"] = zoneID
	}
	return p
}

// planStepByID returns the plan step with the given ID, or "" id missing.
func planStepByID(t *testing.T, steps []core.PlanStep, id string) core.PlanStep {
	t.Helper()
	for _, s := range steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("plan has no step %q:\n%s", id, formatSteps(steps))
	return core.PlanStep{}
}

// planDNSZone extracts the zone a plan's cf-dns step targets.
func planDNSZone(t *testing.T, p *Provider, profile *core.ConnectionProfile) string {
	t.Helper()
	plan, err := p.Plan(context.Background(), core.DesiredConnection{
		Profile: profile,
		Origin:  &core.ResolvedOrigin{URL: "http://127.0.0.1:3000"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return planStepByID(t, plan.Steps, "cf-dns").Technical.Parameters["zone_id"]
}

// TestZoneIsConnectionScopedForTwoZones is the finding-7 acceptance: two
// connections under one provider account plan and execute against different
// zones. Each connection's own zone must reach the DNS step parameter and the
// CreateCNAME call, independent of the password-pinned account zone.
func TestZoneIsConnectionScopedForTwoZones(t *testing.T) {
	p := cfPlanTestProvider() // account default zone "zone-test"
	connA := zoneProfile("conn-a", "a", "a.example.com", "zone-A")
	connB := zoneProfile("conn-b", "b", "b.otherdomain.net", "zone-B")

	if got := planDNSZone(t, p, connA); got != "zone-A" {
		t.Fatalf("connection A planned against zone %q, want zone-A", got)
	}
	if got := planDNSZone(t, p, connB); got != "zone-B" {
		t.Fatalf("connection B planned against zone %q, want zone-B", got)
	}

	// Executing A's DNS step must call CreateCNAME with zone-A, not the
	// account default.
	dns, _ := p.dns.(*fakeDNSManager)
	step := core.PlanStep{ID: "cf-dns", Kind: core.StepCreateDNSRecord, Technical: core.TechnicalOperation{
		Provider: "cloudflare", Type: "create_dns",
		Parameters: map[string]string{"hostname": "a.example.com", "tunnel_id": "tun-a", "zone_id": "zone-A"},
	}}
	if _, err := p.ExecuteStep(context.Background(), "conn-a", step); err != nil {
		t.Fatalf("ExecuteStep CreateDNSRecord: %v", err)
	}
	if dns.zoneID != "zone-A" {
		t.Fatalf("CreateCNAME called with zone %q, want zone-A", dns.zoneID)
	}
}

// TestAccountZoneChangeDoesNotRetargetConnection proves updating the
// account-default zone does not silently retarget a connection that carries
// its own zone — the audit's "updating account credentials must not silently
// retarget either connection." The adapter's account zone is mutated to a new
// default; the connection's own zone must still win.
func TestAccountZoneChangeDoesNotRetargetConnection(t *testing.T) {
	p := cfPlanTestProvider()
	p.zoneID = "new-default" // account default moved after the connection existed

	conn := zoneProfile("conn-a", "a", "a.example.com", "zone-A")
	if got := planDNSZone(t, p, conn); got != "zone-A" {
		t.Fatalf("connection planned against zone %q after account-default change, want zone-A", got)
	}
}

// TestZoneIDIsTrimmedConsistently proves a whitespace-padded Options zone is
// trimmed in Plan exactly as stepZone trims it in ExecuteStep, so the zone
// Plan verifies is the zone ExecuteStep targets (no Plan/Execute asymmetry).
func TestZoneIDIsTrimmedConsistently(t *testing.T) {
	p := cfPlanTestProvider()
	p.zoneID = "zone-test"
	conn := zoneProfile("conn-a", "a", "a.example.com", "  zone-A  ")

	if got := planDNSZone(t, p, conn); got != "zone-A" {
		t.Fatalf("Plan carried zone %q, want trimmed zone-A", got)
	}
}

// TestLegacyConnectionFallsBackToAccountZone proves a connection without its
// own zone (created before finding-7) keeps planning against the account zone
// — the fallback is the migration, so no data copy is needed.
func TestLegacyConnectionFallsBackToAccountZone(t *testing.T) {
	p := cfPlanTestProvider() // account default "zone-test"
	legacy := zoneProfile("conn-old", "old", "old.example.com", "")

	if got := planDNSZone(t, p, legacy); got != "zone-test" {
		t.Fatalf("legacy connection planned against zone %q, want the account default zone-test", got)
	}

	// And executing its DNS step calls CreateCNAME with the account zone.
	dns, _ := p.dns.(*fakeDNSManager)
	step := core.PlanStep{ID: "cf-dns", Kind: core.StepCreateDNSRecord, Technical: core.TechnicalOperation{
		Provider: "cloudflare", Type: "create_dns",
		Parameters: map[string]string{"hostname": "old.example.com", "tunnel_id": "tun-old"},
	}}
	if _, err := p.ExecuteStep(context.Background(), "conn-old", step); err != nil {
		t.Fatalf("ExecuteStep CreateDNSRecord: %v", err)
	}
	if dns.zoneID != "zone-test" {
		t.Fatalf("legacy CreateCNAME called with zone %q, want account zone-test", dns.zoneID)
	}
}

// TestPermanentConnectionOwnZoneOnQuickOnlyAccount proves a permanent
// connection bearing its own zone can be planned on an account that has no
// default zone at all (a Quick-Tunnel-only account). Before finding-7 this
// returned "permanent exposure requires a configured Cloudflare zone".
func TestPermanentConnectionOwnZoneOnQuickOnlyAccount(t *testing.T) {
	p := cfPlanTestProvider()
	p.zoneID = "" // no account default

	conn := zoneProfile("conn-a", "a", "a.example.com", "zone-A")
	if got := planDNSZone(t, p, conn); got != "zone-A" {
		t.Fatalf("own-zone connection on a quick-only account planned against zone %q, want zone-A", got)
	}
}

// TestPlanTemporaryConnectionCarriesNoZone proves a Quick Tunnel connection
// never carries a zone.
func TestPlanTemporaryConnectionCarriesNoZone(t *testing.T) {
	p := cfPlanTestProvider()
	quick := cfTestProfile(core.ExposureTemporary, "", core.ProtectionSpec{Kind: core.ProtectionNone})
	plan, err := p.Plan(context.Background(), core.DesiredConnection{
		Profile: quick,
		Origin:  &core.ResolvedOrigin{URL: "http://127.0.0.1:3000"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, s := range plan.Steps {
		if v := s.Technical.Parameters["zone_id"]; v != "" {
			t.Fatalf("temporary step %s carries zone_id %q; Quick Tunnels must not carry a zone", s.ID, v)
		}
	}
	if !strings.Contains(plan.Expected.PublicAddress, "temporary") {
		t.Fatalf("temporary plan does not declare a temporary address: %q", plan.Expected.PublicAddress)
	}
}
