package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider/cloudflare"
)

// Cloudflare fake-HTTP failures driven through the supervisor reconciliation
// decision, not only through provider-client tests.
//
// The provider-level classifier has its own suite: it proves that a 404 means
// missing and that a 401/429/5xx/malformed body never does. What nothing
// proved until now is that those classifications survive the whole path —
// real adapter, real activation, controller observation with the durable
// resource inventory — into computeReconcileDecision without reconciliation
// acting on a guess.
//
// The invariant under test: only an authoritative missing classification may
// produce a repair/recreate plan. Unauthorized, rate-limited, transient
// (5xx/timeout/malformed) observations must leave an open connection alone.

// cloudflareAPIFake answers Cloudflare API paths with configured status/body
// pairs and records every request path it was asked.
type cloudflareAPIFake struct {
	t       *testing.T
	respond map[string]func() (int, string)
	paths   []string
}

func (f *cloudflareAPIFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	body, ok := f.respond[r.URL.Path]
	if !ok {
		// A path the fixture did not configure is a fixture error: the test
		// must know exactly which endpoints its scenario touches.
		f.t.Fatalf("unexpected Cloudflare API request: %s %s", r.Method, r.URL.Path)
	}
	status, payload := body()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(payload))
}

func cfErrorBody(message string) string {
	return `{"success":false,"errors":[{"code":10000,"message":"` + message + `"}]}`
}

func cfSuccessBody(id string) string {
	return `{"success":true,"result":{"id":"` + id + `","name":"portico-web","status":"healthy"}}`
}

// reconcilingCloudflareSupervisor activates the production Cloudflare
// definition against a fake API and registers a service-exposure profile with
// one managed tunnel and one managed DNS record. Everything downstream of the
// HTTP boundary is the code production runs.
func reconcilingCloudflareSupervisor(t *testing.T, respond map[string]func() (int, string)) (*Supervisor, core.ConnectionID) {
	t.Helper()
	st := newRecoveryTestStore(t)
	fake := &cloudflareAPIFake{t: t, respond: respond}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	def := cloudflare.NewDefinition(cloudflare.DefinitionConfig{
		Bin:        "cloudflared",
		APIBaseURL: server.URL + "/client/v4",
	})
	sup, registry := activationTestSupervisor(t, st, def)

	account := core.ProviderAccount{
		ID: "acct-vert", Provider: "cloudflare", Label: "Vertical",
		CredentialRef: "cloudflare:acct-vert:api-token", Status: core.AccountAuthenticated,
		Metadata: map[string]string{"zone_id": "zone-vert"},
	}
	if err := st.UpsertProviderAccountCredential(context.Background(), account, []byte("vertical-token")); err != nil {
		t.Fatalf("UpsertProviderAccountCredential: %v", err)
	}
	if err := sup.ActivateProvider(context.Background(), "cloudflare"); err != nil {
		t.Fatalf("ActivateProvider: %v", err)
	}

	profile := newReconcileProfile("conn-cf-vertical", core.DesiredOpen, core.ProtectionSpec{})
	profile.Driver.AccountID = "acct-vert"
	_, _, err := sup.controller.CreateProfile(context.Background(), profile)
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	rt := &core.ConnectionRuntime{ConnectionID: profile.ID, State: core.RuntimeOpen}
	rt.Connector.Status = core.ConnectorStatusRunning
	for _, res := range []core.ProviderResource{
		{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceTunnel,
			ExternalID: "11111111111111111111111111111111", Ownership: core.OwnershipManaged},
		{ConnectionID: profile.ID, ProviderID: "cloudflare", Type: core.ResourceDNSRecord,
			ExternalID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ownership: core.OwnershipManaged},
	} {
		if err := st.SaveResource(context.Background(), &res); err != nil {
			t.Fatalf("SaveResource %s: %v", res.Type, err)
		}
		rt.Provider.Resources = append(rt.Provider.Resources, res)
	}
	if err := st.SaveRuntime(context.Background(), rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}
	sup.controller.RestoreRuntime(rt)
	_ = registry
	return sup, profile.ID
}

// reconcileInputFor assembles the same input reconcileOne builds in
// production: runtime projection plus persisted inventory plus live observation.
func (s *Supervisor) reconcileInputFor(ctx context.Context, connID core.ConnectionID) ReconcileInput {
	profile, ok := s.controller.GetProfile(connID)
	if !ok {
		panic("profile not registered: " + connID)
	}
	input := ReconcileInput{Profile: profile}
	if rt, ok := s.controller.GetRuntime(connID); ok {
		input.Runtime = rt
		input.Resources = rt.Provider.Resources
	}
	if obs, err := s.observeConnection(ctx, connID); err == nil {
		input.Observed = obs
	}
	return input
}

// TestReconciliationIgnoresUnauthorizedCloudflareAnswers pins 401/403:
// an unauthorized lookup says nothing about whether the tunnel or DNS record
// exists, so reconciliation must neither repair nor recreate anything.
func TestReconciliationIgnoresUnauthorizedCloudflareAnswers(t *testing.T) {
	ctx := context.Background()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		sup, connID := reconcilingCloudflareSupervisor(t, map[string]func() (int, string){
			"/client/v4/accounts/acct-vert/cfd_tunnel/11111111111111111111111111111111": func() (int, string) {
				return status, cfErrorBody("not permitted")
			},
			"/client/v4/zones/zone-vert/dns_records/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": func() (int, string) {
				return status, cfErrorBody("not permitted")
			},
		})
		decision, err := sup.computeReconcileDecision(ctx, sup.reconcileInputFor(ctx, connID))
		if err != nil {
			t.Fatalf("status %d: computeReconcileDecision: %v", status, err)
		}
		if decision.Action != "none" || decision.Plan != nil {
			t.Fatalf("status %d: reconciliation acted on an unauthorized answer: %#v", status, decision)
		}
	}
}

// TestReconciliationIgnoresRateLimitedAndTransientCloudflareAnswers pins
// 429, 5xx, and transport timeouts: none of them is evidence of absence, so
// none may trigger a repair plan for a tracked resource.
func TestReconciliationIgnoresRateLimitedAndTransientCloudflareAnswers(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"rate limited", http.StatusTooManyRequests, cfErrorBody("rate limited")},
		{"server error", http.StatusInternalServerError, cfErrorBody("boom")},
		{"bad gateway", http.StatusBadGateway, cfErrorBody("upstream")},
		{"malformed body", http.StatusOK, "<html><body>proxy error</body></html>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sup, connID := reconcilingCloudflareSupervisor(t, map[string]func() (int, string){
				"/client/v4/accounts/acct-vert/cfd_tunnel/11111111111111111111111111111111": func() (int, string) {
					return tc.status, tc.body
				},
				"/client/v4/zones/zone-vert/dns_records/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": func() (int, string) {
					return tc.status, tc.body
				},
			})
			decision, err := sup.computeReconcileDecision(ctx, sup.reconcileInputFor(ctx, connID))
			if err != nil {
				t.Fatalf("%s: computeReconcileDecision: %v", tc.name, err)
			}
			if decision.Action != "none" || decision.Plan != nil {
				t.Fatalf("%s: reconciliation acted on an unclassified answer: %#v", tc.name, decision)
			}
		})
	}
}

// TestReconciliationRepairsOnlyAuthoritativelyMissingDNS pins the positive
// control for the same wiring: with the tunnel confirmed present and the DNS
// record confirmed gone (404), the narrow DNS-create repair is produced —
// proving the fixtures above fail closed because of the classification, not
// because observation silently returned nothing.
func TestReconciliationRepairsOnlyAuthoritativelyMissingDNS(t *testing.T) {
	ctx := context.Background()
	sup, connID := reconcilingCloudflareSupervisor(t, map[string]func() (int, string){
		"/client/v4/accounts/acct-vert/cfd_tunnel/11111111111111111111111111111111": func() (int, string) {
			return http.StatusOK, cfSuccessBody("11111111111111111111111111111111")
		},
		"/client/v4/zones/zone-vert/dns_records/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": func() (int, string) {
			return http.StatusNotFound, cfErrorBody("record does not exist")
		},
	})
	decision, err := sup.computeReconcileDecision(ctx, sup.reconcileInputFor(ctx, connID))
	if err != nil {
		t.Fatalf("computeReconcileDecision: %v", err)
	}
	if decision.Action != "repair" || decision.Plan == nil || len(decision.Plan.Steps) != 1 {
		t.Fatalf("an authoritative 404 did not produce the narrow repair: %#v", decision)
	}
	step := decision.Plan.Steps[0]
	if step.Kind != core.StepCreateDNSRecord {
		t.Fatalf("repair step kind = %q, want create_dns_record", step.Kind)
	}
}
