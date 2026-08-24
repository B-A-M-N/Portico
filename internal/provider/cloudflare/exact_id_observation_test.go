package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"

	"github.com/B-A-M-N/portico/internal/core"
)

// Observing the resource Portico owns, after the supervisor has restarted.
//
// A restarted supervisor has no in-memory record of what it created. It reconstructs
// the adapter and observes from persisted state, and the only thing that makes the
// observation correct is that it asks for the exact provider resource ID it stored.
//
// The alternative — listing resources and matching on a name — is wrong in a way that
// is invisible until it matters: two tunnels called "portico-web" belonging to the same
// account resolve to whichever the provider happens to return first, and Portico then
// manages, reconciles and eventually deletes a resource it does not own.
//
// So these tests reconstruct the adapter over a fake HTTP API that holds two plausible
// resources, and assert on what was requested. The fake fails the test if anything asks
// for a collection.

// recordingAPI is a Cloudflare API that records every path it is asked for.
type recordingAPI struct {
	t        *testing.T
	requests []string
	respond  map[string]func() (int, string)
}

func (f *recordingAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)

	// A request with no resource ID is a listing. Nothing in the observation path may
	// make one: listing is how the wrong resource gets selected.
	if isCollectionRequest(r.URL.Path) {
		f.t.Errorf("the adapter listed a collection instead of asking for an exact ID: %s %s",
			r.Method, r.URL.Path)
	}

	// Longest matching prefix wins. Map iteration order is random, so a shorter
	// prefix would otherwise sometimes shadow a more specific one — which is a bug in
	// the fake that presents as the adapter having asked for the wrong thing.
	bestPrefix, bestLen := "", -1
	for prefix := range f.respond {
		method, path, hasMethod := strings.Cut(prefix, " ")
		if !hasMethod {
			// A bare path matches any method.
			path, method = prefix, r.Method
		}
		if r.Method == method && strings.HasPrefix(r.URL.Path, path) && len(path) > bestLen {
			bestPrefix, bestLen = prefix, len(path)
		}
	}
	if bestLen >= 0 {
		status, body := f.respond[bestPrefix]()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1000,"message":"not found"}]}`))
}

// isCollectionRequest reports whether a path addresses a collection rather than one
// resource.
//
// The Cloudflare paths end in the resource ID when one is addressed, so a path ending
// at the collection segment is a listing.
func isCollectionRequest(path string) bool {
	for _, collection := range []string{"/cfd_tunnel", "/dns_records", "/access/apps"} {
		if strings.HasSuffix(strings.TrimSuffix(path, "/"), collection) {
			return true
		}
	}
	return false
}

// asked reports whether a specific request was made.
func (f *recordingAPI) asked(method, path string) bool {
	for _, request := range f.requests {
		if request == method+" "+path {
			return true
		}
	}
	return false
}

// reconstructedProvider builds an adapter the way a restarted supervisor does: from
// configuration alone, with no memory of what it previously created.
func reconstructedProvider(t *testing.T, respond map[string]func() (int, string)) (*Provider, *recordingAPI) {
	t.Helper()
	fake := &recordingAPI{t: t, respond: respond}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	provider, err := newWithOptions(
		"test-token", "acct-1", "zone-1", "cloudflared", t.TempDir(),
		&fakeConnectorProcessService{},
		cf.BaseURL(server.URL+"/client/v4"),
		cf.UsingRetryPolicy(0, 0, 0),
	)
	if err != nil {
		t.Fatalf("reconstructing the provider: %v", err)
	}
	return provider, fake
}

// The two resources the fake holds. They share a name, which is the case that makes a
// name-based lookup silently wrong.
const (
	ownedTunnelID     = "11111111111111111111111111111111"
	strangerTunnelID  = "22222222222222222222222222222222"
	sharedTunnelName  = "portico-web"
	ownedDNSRecordID  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	strangerDNSRecord = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func tunnelBody(id, name string) string {
	return fmt.Sprintf(`{"success":true,"result":{"id":%q,"name":%q,"status":"healthy"}}`, id, name)
}

func dnsBody(id, name, content string) string {
	return fmt.Sprintf(
		`{"success":true,"result":{"id":%q,"name":%q,"type":"CNAME","content":%q,"proxied":true}}`,
		id, name, content)
}

// persistedResources is what the supervisor stored when it created the connection.
func persistedResources() []core.ProviderResource {
	return []core.ProviderResource{
		{Type: core.ResourceTunnel, ExternalID: ownedTunnelID},
		{Type: core.ResourceDNSRecord, ExternalID: ownedDNSRecordID},
	}
}

// TestObservationAsksForThePersistedIDAfterReconstruction is the central case.
func TestObservationAsksForThePersistedIDAfterReconstruction(t *testing.T) {
	ownedTunnelPath := "/client/v4/accounts/acct-1/cfd_tunnel/" + ownedTunnelID
	ownedDNSPath := "/client/v4/zones/zone-1/dns_records/" + ownedDNSRecordID

	provider, fake := reconstructedProvider(t, map[string]func() (int, string){
		http.MethodGet + " " + ownedTunnelPath: func() (int, string) {
			return http.StatusOK, tunnelBody(ownedTunnelID, sharedTunnelName)
		},
		http.MethodGet + " " + ownedDNSPath: func() (int, string) {
			return http.StatusOK, dnsBody(ownedDNSRecordID, "web.example.com", ownedTunnelID+".cfargotunnel.com")
		},
	})

	obs, err := provider.ObserveWithResources(context.Background(), "conn-1", persistedResources())
	if err != nil {
		t.Fatalf("ObserveWithResources: %v", err)
	}

	// The exact paths were requested. A name-based lookup would not have produced
	// either of these.
	if !fake.asked(http.MethodGet, ownedTunnelPath) {
		t.Errorf("the tunnel was not fetched by its persisted ID; requests were %v", fake.requests)
	}
	if !fake.asked(http.MethodGet, ownedDNSPath) {
		t.Errorf("the DNS record was not fetched by its persisted ID; requests were %v", fake.requests)
	}

	// And the observation describes the resource Portico owns, not the stranger.
	if obs.Tunnel == nil {
		t.Fatal("the observation carries no tunnel")
	}
	if obs.Tunnel.ID != ownedTunnelID {
		t.Fatalf("observed tunnel %q, want the persisted one", obs.Tunnel.ID)
	}
	if len(obs.DNSRecords) != 1 || obs.DNSRecords[0].ID != ownedDNSRecordID {
		t.Fatalf("observed DNS records %+v, want only the persisted one", obs.DNSRecords)
	}
	assertResourcePresent(t, obs, core.ResourceTunnel, ownedTunnelID)
	assertResourcePresent(t, obs, core.ResourceDNSRecord, ownedDNSRecordID)
}

// TestAStrangerWithTheSameNameIsNotAdopted pins the case a listing would get wrong.
//
// The provider holds another tunnel with the same name, belonging to something else.
// Portico's own tunnel has been deleted at the provider. The correct observation is
// that Portico's tunnel is missing — not that a tunnel with the right name was found.
func TestAStrangerWithTheSameNameIsNotAdopted(t *testing.T) {
	ownedTunnelPath := "/client/v4/accounts/acct-1/cfd_tunnel/" + ownedTunnelID
	strangerPath := "/client/v4/accounts/acct-1/cfd_tunnel/" + strangerTunnelID

	provider, fake := reconstructedProvider(t, map[string]func() (int, string){
		// Portico's tunnel is gone.
		http.MethodGet + " " + ownedTunnelPath: func() (int, string) {
			return http.StatusNotFound,
				`{"success":false,"errors":[{"code":1000,"message":"tunnel not found"}]}`
		},
		// A tunnel with the same name still exists, under a different ID.
		http.MethodGet + " " + strangerPath: func() (int, string) {
			return http.StatusOK, tunnelBody(strangerTunnelID, sharedTunnelName)
		},
	})

	obs, err := provider.ObserveWithResources(context.Background(), "conn-1",
		[]core.ProviderResource{{Type: core.ResourceTunnel, ExternalID: ownedTunnelID}})
	if err != nil {
		t.Fatalf("ObserveWithResources: %v", err)
	}

	// The stranger was never asked about, so it cannot have been adopted.
	if fake.asked(http.MethodGet, strangerPath) {
		t.Errorf("the adapter fetched a resource Portico does not own: %v", fake.requests)
	}
	if obs.Tunnel != nil && obs.Tunnel.ID == strangerTunnelID {
		t.Fatal("a tunnel belonging to something else was adopted because its name matched")
	}
	// And the outcome is the honest one: what Portico owned is gone.
	assertResourceStatus(t, obs, core.ResourceTunnel, ownedTunnelID, core.ObservationMissing)
}

// TestAMissingResourceIsReportedMissingNotTransient pins the classification the
// reconcile path branches on.
func TestAMissingResourceIsReportedMissingNotTransient(t *testing.T) {
	provider, _ := reconstructedProvider(t, map[string]func() (int, string){
		"/client/v4/accounts/acct-1/cfd_tunnel/": func() (int, string) {
			return http.StatusNotFound,
				`{"success":false,"errors":[{"code":1000,"message":"tunnel not found"}]}`
		},
		"/client/v4/zones/zone-1/dns_records/": func() (int, string) {
			return http.StatusNotFound,
				`{"success":false,"errors":[{"code":81044,"message":"record does not exist"}]}`
		},
	})

	obs, err := provider.ObserveWithResources(context.Background(), "conn-1", persistedResources())
	if err != nil {
		t.Fatalf("ObserveWithResources: %v", err)
	}
	assertResourceStatus(t, obs, core.ResourceTunnel, ownedTunnelID, core.ObservationMissing)
	assertResourceStatus(t, obs, core.ResourceDNSRecord, ownedDNSRecordID, core.ObservationMissing)
}

// TestAFailedLookupIsNotReportedAsMissing pins the invariant that protects against
// recreating a resource that exists.
//
// A resource reported missing gets recreated. A rate limit or a server error says
// nothing about whether the resource is there, so reporting either as missing would
// create a second tunnel every time the API was busy.
func TestAFailedLookupIsNotReportedAsMissing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "rate limited", status: http.StatusTooManyRequests,
			body: `{"success":false,"errors":[{"code":10000,"message":"rate limited"}]}`},
		{name: "server error", status: http.StatusInternalServerError,
			body: `{"success":false,"errors":[{"code":10000,"message":"boom"}]}`},
		{name: "forbidden", status: http.StatusForbidden,
			body: `{"success":false,"errors":[{"code":10000,"message":"no permission"}]}`},
		{name: "malformed body", status: http.StatusOK,
			body: `<html><body>502 Bad Gateway</body></html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, _ := reconstructedProvider(t, map[string]func() (int, string){
				"/client/v4/accounts/acct-1/cfd_tunnel/": func() (int, string) {
					return tc.status, tc.body
				},
				"/client/v4/zones/zone-1/dns_records/": func() (int, string) {
					return tc.status, tc.body
				},
			})

			obs, err := provider.ObserveWithResources(context.Background(), "conn-1",
				persistedResources())
			if err != nil {
				// An error return is also acceptable: what must not happen is a
				// confident "missing".
				return
			}
			for _, res := range obs.ResourceStatuses {
				if res.Status == core.ObservationMissing {
					t.Errorf("%s was classified as the resource being missing, so "+
						"reconciliation would recreate it: %+v", tc.name, res)
				}
			}
		})
	}
}

// TestObservationSurvivesReconstructionTwice pins that observing is repeatable.
//
// A supervisor may restart more than once, and each reconstruction must ask the same
// question and reach the same answer rather than accumulating state.
func TestObservationSurvivesReconstructionTwice(t *testing.T) {
	ownedTunnelPath := "/client/v4/accounts/acct-1/cfd_tunnel/" + ownedTunnelID
	ownedDNSPath := "/client/v4/zones/zone-1/dns_records/" + ownedDNSRecordID
	respond := map[string]func() (int, string){
		http.MethodGet + " " + ownedTunnelPath: func() (int, string) {
			return http.StatusOK, tunnelBody(ownedTunnelID, sharedTunnelName)
		},
		http.MethodGet + " " + ownedDNSPath: func() (int, string) {
			return http.StatusOK, dnsBody(ownedDNSRecordID, "web.example.com", ownedTunnelID+".cfargotunnel.com")
		},
	}

	for attempt := 1; attempt <= 2; attempt++ {
		provider, fake := reconstructedProvider(t, respond)
		obs, err := provider.ObserveWithResources(context.Background(), "conn-1",
			persistedResources())
		if err != nil {
			t.Fatalf("reconstruction %d: %v", attempt, err)
		}
		if obs.Tunnel == nil || obs.Tunnel.ID != ownedTunnelID {
			t.Fatalf("reconstruction %d observed %+v", attempt, obs.Tunnel)
		}
		if !fake.asked(http.MethodGet, ownedTunnelPath) {
			t.Fatalf("reconstruction %d did not fetch by the persisted ID: %v",
				attempt, fake.requests)
		}
	}
}

// TestNoResourcesMeansNoProviderCalls pins that a connection with nothing persisted
// does not go looking.
//
// This is the case where a listing would be most tempting and most wrong: with no
// stored ID there is nothing to observe, and searching for something that looks like
// Portico's is how an unrelated resource gets adopted.
func TestNoResourcesMeansNoProviderCalls(t *testing.T) {
	provider, fake := reconstructedProvider(t, map[string]func() (int, string){})

	if _, err := provider.ObserveWithResources(context.Background(), "conn-1", nil); err != nil {
		t.Fatalf("ObserveWithResources: %v", err)
	}
	for _, request := range fake.requests {
		if strings.Contains(request, "cfd_tunnel") || strings.Contains(request, "dns_records") {
			t.Errorf("the adapter went looking for resources with none persisted: %s", request)
		}
	}
}

// assertResourcePresent fails unless the named resource was observed as present.
func assertResourcePresent(t *testing.T, obs *core.ObservedConnection, kind core.ResourceType, id string) {
	t.Helper()
	assertResourceStatus(t, obs, kind, id, core.ObservationPresent)
}

// assertResourceStatus fails unless the named resource has the expected status.
func assertResourceStatus(t *testing.T, obs *core.ObservedConnection,
	kind core.ResourceType, id string, want core.ObservationStatus) {
	t.Helper()
	for _, res := range obs.ResourceStatuses {
		if res.Type == kind && res.ExternalID == id {
			if res.Status != want {
				t.Errorf("%s %s observed as %q, want %q (detail: %s)",
					kind, id, res.Status, want, res.Detail)
			}
			return
		}
	}
	encoded, _ := json.Marshal(obs.ResourceStatuses)
	t.Errorf("%s %s was not observed at all; observed: %s", kind, id, encoded)
}
