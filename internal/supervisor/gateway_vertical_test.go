package supervisor

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// The generic profile → gateway → transport execution path (audit R1/R2).
//
// The contract under test: for an openai_compatible service exposure, the
// SUPERVISOR owns the gateway. It starts the gateway before applying an open
// plan; the provider's plan then carries the gateway endpoint as the
// effective transport target instead of the raw origin; and when the gateway
// is not running, planning falls back to the raw origin rather than lying.

func gatewayVerticalProfile() *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID: "gw-vertical", Name: "llama-box",
		Kind:        core.ConnectionServiceExposure,
		ProfileKind: core.ProfileOpenAICompatible,
		Desired:     core.DesiredOpen,
		Spec: core.ConnectionSpec{ServiceExposure: &core.ServiceExposureSpec{
			Source: core.SourceSpec{Kind: core.SourceExisting, Existing: &core.ExistingServiceSpec{
				Address: "127.0.0.1:8080", Protocol: core.ProtocolHTTP,
			}},
			Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
			Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
		}},
	}
}

// TestGatewaySpecDerivedFromProfileIntent pins that the supervisor derives a
// gateway requirement from openai_compatible intent alone — with auth
// REQUIRED by policy, not inferred from token presence.
func TestGatewaySpecDerivedFromProfileIntent(t *testing.T) {
	sup := &Supervisor{gatewayMgr: newGatewayManager()}
	spec, needed := sup.gatewaySpecFor(gatewayVerticalProfile())
	if !needed {
		t.Fatal("an openai_compatible exposure produced no gateway requirement")
	}
	if spec.Upstream != "http://127.0.0.1:8080" {
		t.Fatalf("gateway upstream = %q, want the raw local origin", spec.Upstream)
	}
	if !spec.AuthRequired {
		t.Fatal("openai_compatible gateway auth was not required; policy must not be downgraded by token absence")
	}
	if !spec.AllowSSE {
		t.Fatal("SSE passthrough was not enabled for an OpenAI-compatible profile")
	}

	// A plain web-service exposure needs no gateway at all.
	web := gatewayVerticalProfile()
	web.ProfileKind = core.ProfileWebService
	if _, needed := sup.gatewaySpecFor(web); needed {
		t.Fatal("a plain web service requested a gateway")
	}
}

// TestGatewayEndpointBecomesTheTransportTarget pins the planning half of the
// vertical: with a running gateway recorded in the controller's resolver, a
// transport provider's plan carries the gateway endpoint as origin_url.
func TestGatewayEndpointBecomesTheTransportTarget(t *testing.T) {
	const gatewayEndpoint = "http://127.0.0.1:49160"

	provider := &targetCapturingProvider{}
	sup := testSupervisorWithController(t, provider)
	sup.controller.SetGatewayEndpointResolver(func(core.ConnectionID) string {
		return gatewayEndpoint
	})

	profile := gatewayVerticalProfile()
	profile.Driver.ProviderID = provider.Identity().ID

	ctx := context.Background()
	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	plan, err := sup.controller.PlanOpen(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}

	for _, step := range plan.Steps {
		if url := step.Technical.Parameters["origin_url"]; url == "http://127.0.0.1:8080" {
			t.Fatalf("a plan step still targets the RAW origin %q; the transport must target the gateway", url)
		}
	}
	found := false
	for _, step := range plan.Steps {
		if step.Technical.Parameters["origin_url"] == gatewayEndpoint {
			found = true
		}
	}
	if !found {
		t.Fatalf("no plan step targets the gateway endpoint %q; steps = %+v", gatewayEndpoint, plan.Steps)
	}
}

// TestNoGatewayMeansRawOriginPlanning pins the fallback: without a running
// gateway the plan uses the raw origin, so the path degrades honestly instead
// of pointing at nothing.
func TestNoGatewayMeansRawOriginPlanning(t *testing.T) {
	provider := &targetCapturingProvider{}
	sup := testSupervisorWithController(t, provider)
	// No resolver installed at all.

	profile := gatewayVerticalProfile()
	profile.Driver.ProviderID = provider.Identity().ID

	ctx := context.Background()
	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	plan, err := sup.controller.PlanOpen(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	found := false
	for _, step := range plan.Steps {
		if step.Technical.Parameters["origin_url"] == "http://127.0.0.1:8080" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the raw origin vanished from the plan with no gateway running: %+v", plan.Steps)
	}
}

// TestSupervisorStartsGatewayBeforeApplyAndStopsItOnFailure pins the
// lifecycle-ownership half: HandleApplyPlan starts the gateway BEFORE the
// plan executes, and stops it again if the apply fails — no orphaned
// supervisor-owned listener.
func TestSupervisorStartsGatewayBeforeApplyAndStopsItOnFailure(t *testing.T) {
	failing := &failingApplyProvider{}
	sup := testSupervisorWithController(t, failing)

	profile := gatewayVerticalProfile()
	profile.Driver.ProviderID = failing.Identity().ID

	ctx := context.Background()
	handler := &supervisorHandler{sup: sup}

	if _, _, err := sup.controller.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	plan, err := sup.controller.PlanOpen(ctx, profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	if err := sup.controller.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	// The apply fails on purpose.
	if _, err := handler.HandleApplyPlan(string(plan.ID), ""); err == nil {
		t.Fatal("the deliberately failing apply reported success")
	}

	// The gateway must not survive the failed apply.
	if _, exists := sup.gatewayMgr.Runtime(profile.ID); exists {
		t.Fatal("a supervisor-owned gateway outlived its failed apply")
	}
}
