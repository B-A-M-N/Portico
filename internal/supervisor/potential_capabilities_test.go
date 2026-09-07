package supervisor

import (
	"context"
	"slices"
	"testing"

	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/provider/builtin"
	"github.com/B-A-M-N/portico/internal/provider/cloudflare"
	"github.com/B-A-M-N/portico/internal/provider/mock"
	"github.com/B-A-M-N/portico/internal/provider/portforward"
)

// Potential capabilities are the after-setup contract, served beside the
// current one. These tests pin that the served answer comes from the
// definition statically — never from the live adapter's current answer,
// which is how setup advice ended up naming providers that could never
// deliver what was asked about.

func potentialByProvider(t *testing.T, defs ...provider.Definition) map[string]*struct {
	Hostnames bool
	OTP       bool
} {
	t.Helper()
	st := newRecoveryTestStore(t)
	sup, _ := activationTestSupervisor(t, st, defs...)
	sup.activateAll(context.Background())
	snapshot, err := (&supervisorHandler{sup: sup}).HandleSnapshot()
	if err != nil {
		t.Fatalf("HandleSnapshot: %v", err)
	}
	got := map[string]*struct {
		Hostnames bool
		OTP       bool
	}{}
	for i, p := range snapshot.Providers {
		if p.Capabilities == nil {
			continue
		}
		got[p.ID] = &struct {
			Hostnames bool
			OTP       bool
		}{
			Hostnames: p.Capabilities.PotentialCustomHostnames,
			OTP:       slices.Contains(p.Capabilities.PotentialProtectionModes, "email_otp"),
		}
		_ = i
	}
	return got
}

// TestSnapshotCarriesPotentialCapabilitiesFromDefinition pins the accountless
// Cloudflare case that motivated the field: the adapter is Quick-Tunnel-only,
// reports no custom hostnames, and the definition still declares them as
// achievable after setup — alongside email OTP, which only a configured
// account delivers.
func TestSnapshotCarriesPotentialCapabilitiesFromDefinition(t *testing.T) {
	got := potentialByProvider(t,
		mock.NewDefinition(),
		portforward.NewDefinition(),
		cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}),
	)

	cf, ok := got["cloudflare"]
	if !ok {
		t.Fatalf("no cloudflare entry: %v", got)
	}
	if !cf.Hostnames {
		t.Error("accountless cloudflare reports no potential custom hostnames")
	}
	if !cf.OTP {
		t.Error("accountless cloudflare reports no potential email OTP")
	}

	// The local port forward's reach is intrinsic; setup would widen nothing.
	if pf := got["portforward"]; pf != nil && pf.Hostnames {
		t.Error("portforward declares potential custom hostnames")
	}
	if m := got["mock"]; m == nil || !m.Hostnames {
		t.Error("mock does not carry its declared potential custom hostnames")
	}
}

// TestPotentialCapabilitiesSurviveAFailedActivation pins that the answer is
// static: a provider whose adapter could not be constructed still declares
// what setup could deliver, because a switched-off provider is the one a user
// needs advice about.
func TestPotentialCapabilitiesSurviveAFailedActivation(t *testing.T) {
	st := newRecoveryTestStore(t)
	sup, _ := activationTestSupervisor(t, st, cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}))
	// Deliberately no activateAll: the snapshot must still carry the
	// definition's potential contract for the catalogued provider.
	_ = sup.activateDefinition(context.Background(), cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: "cloudflared"}))

	snapshot, err := (&supervisorHandler{sup: sup}).HandleSnapshot()
	if err != nil {
		t.Fatalf("HandleSnapshot: %v", err)
	}
	for _, p := range snapshot.Providers {
		if p.ID != "cloudflare" {
			continue
		}
		if p.Capabilities == nil || !p.Capabilities.PotentialCustomHostnames {
			t.Fatalf("cloudflare potential custom hostnames lost with a dead adapter: %+v", p.Capabilities)
		}
	}
}

// TestUnimplementedProviderServesNoPotential pins that zrok — Portico names it
// but ships no adapter — carries no potential contract, so it can never be
// recommended as a way to get a capability it cannot deliver.
func TestUnimplementedProviderServesNoPotential(t *testing.T) {
	got := potentialByProvider(t, builtin.Definitions(builtin.Config{})...)
	if z, ok := got["zrok"]; ok && (z.Hostnames || z.OTP) {
		t.Fatalf("zrok declares potential capabilities it has no adapter for: %+v", z)
	}
	// And the real composition root's cloudflare still declares its after-setup
	// contract even in a bare installation with no account.
	if cf, ok := got["cloudflare"]; !ok || !cf.Hostnames {
		t.Fatalf("builtin cloudflare lost its potential custom hostnames: %+v", got["cloudflare"])
	}
}
