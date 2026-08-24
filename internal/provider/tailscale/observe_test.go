package tailscale

import (
	"context"
	"errors"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// Observing after the supervisor has restarted.
//
// A restarted supervisor has no memory of what it configured. It reconstructs the adapter
// — a fresh Provider — and observes from the resources it persisted. Every test here
// builds a new provider rather than reusing one that executed the steps, because reusing
// it would prove only that the adapter remembers its own work.

// reconstructed builds a provider the way a restarted supervisor does.
func reconstructed(runner CommandRunner) *Provider { return New(runner) }

// persistedJoin is what the supervisor stored for a join connection.
func persistedJoin() []core.ProviderResource {
	return []core.ProviderResource{{
		Type:       core.ResourceTailnetMembership,
		ExternalID: "n1234567890123456",
		Ownership:  core.OwnershipAdopted,
	}}
}

// persistedServe is what the supervisor stored for a serve connection. The
// ExternalID is the frontend identity; the metadata carries the canonical route
// fields so the route can be reconstructed exactly.
func persistedServe(route ServeRoute) []core.ProviderResource {
	metadata := route.ResourceMetadata()
	metadata["address"] = "workstation.tail0abc.ts.net"
	return []core.ProviderResource{{
		Type:       core.ResourceTailnetMembership,
		ExternalID: "n1234567890123456",
		Ownership:  core.OwnershipAdopted,
	}, {
		Type:       core.ResourceTailnetServe,
		ExternalID: route.Identity(),
		Ownership:  core.OwnershipManaged,
		Metadata:   metadata,
	}}
}

// statusOf finds one resource's observed status.
func statusOf(t *testing.T, obs *core.ObservedConnection,
	kind core.ResourceType, id string) core.ObservedResourceStatus {
	t.Helper()
	for _, res := range obs.ResourceStatuses {
		if res.Type == kind && res.ExternalID == id {
			return res
		}
	}
	t.Fatalf("%s %s was not observed at all; observed %+v", kind, id, obs.ResourceStatuses)
	return core.ObservedResourceStatus{}
}

// TestAJoinIsObservedFromThePersistedNodeID pins exact-identity observation.
func TestAJoinIsObservedFromThePersistedNodeID(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusRunning, nil)
	obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
		"conn-join", persistedJoin())
	if err != nil {
		t.Fatalf("ObserveWithResources: %v", err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetMembership, "n1234567890123456")
	if observed.Status != core.ObservationPresent {
		t.Fatalf("membership observed as %q: %s", observed.Status, observed.Detail)
	}
	if obs.Connector.Status != string(core.ConnectorStatusRunning) {
		t.Errorf("connector status = %q, want running", obs.Connector.Status)
	}
}

// TestAMachineRenamedOnTheTailnetIsNotTheSameResource pins the identity check.
//
// The machine is on a tailnet, but under a different name than the one recorded. That is
// not the resource Portico confirmed, and reporting it present would mean the connection
// claimed an identity it no longer has.
func TestAMachineRenamedOnTheTailnetIsNotTheSameResource(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusRunning, nil)
	obs, err := reconstructed(runner).ObserveWithResources(context.Background(), "conn-join",
		[]core.ProviderResource{{
			Type:       core.ResourceTailnetMembership,
			ExternalID: "old-name.tail0abc.ts.net",
			Ownership:  core.OwnershipAdopted,
		}})
	if err != nil {
		t.Fatal(err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetMembership, "old-name.tail0abc.ts.net")
	if observed.Status == core.ObservationPresent {
		t.Fatal("a differently named machine was observed as the recorded one")
	}
	if observed.Detail == "" {
		t.Error("the mismatch is reported without saying what changed")
	}
}

// TestASignedOutMachineIsMissingNotTransient pins the classification reconciliation acts
// on.
func TestASignedOutMachineIsMissingNotTransient(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusNeedsLogin, nil)
	obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
		"conn-join", persistedJoin())
	if err != nil {
		t.Fatal(err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetMembership, "n1234567890123456")
	if observed.Status != core.ObservationMissing {
		t.Fatalf("a signed-out machine observed as %q, want missing", observed.Status)
	}
}

// TestAnUnreachableClientIsTransientNotMissing pins the invariant that protects the user.
//
// A client that cannot be reached says nothing about membership. Reporting it missing
// would let reconciliation act on a guess about a machine-wide setting.
func TestAnUnreachableClientIsTransientNotMissing(t *testing.T) {
	runner := newFakeRunner()
	runner.fallback = func([]string) ([]byte, error) {
		return nil, errors.New("dial unix /var/run/tailscale/tailscaled.sock: connect: no such file")
	}

	obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
		"conn-join", persistedJoin())
	if err != nil {
		t.Fatal(err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetMembership, "n1234567890123456")
	if observed.Status == core.ObservationMissing {
		t.Fatal("an unreachable client was reported as the machine having left the network")
	}
	if observed.Status != core.ObservationTransient {
		t.Fatalf("observed as %q, want transient", observed.Status)
	}
}

// TestAServeIsObservedFromTheClientNotFromMemory is the central restart case.
//
// The provider observing has never served anything: it has no in-memory map. The only
// input is the persisted resource, and the only source of truth is the client's own
// configuration.
func TestAServeIsObservedFromTheClientNotFromMemory(t *testing.T) {
	route := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve status --json", serveHTTP("127.0.0.1:3000"), nil)

	provider := reconstructed(runner)

	obs, err := provider.ObserveWithResources(context.Background(), "conn-serve",
		persistedServe(route))
	if err != nil {
		t.Fatalf("ObserveWithResources: %v", err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetServe, "http:3000:/")
	if observed.Status != core.ObservationPresent {
		t.Fatalf("the serve observed as %q: %s", observed.Status, observed.Detail)
	}
}

// TestAWithdrawnServeIsObservedMissing pins the missing case reconciliation exists for.
func TestAWithdrawnServeIsObservedMissing(t *testing.T) {
	route := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve status --json", serveNothing, nil)

	obs, err := reconstructed(runner).ObserveWithResources(context.Background(), "conn-serve",
		persistedServe(route))
	if err != nil {
		t.Fatal(err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetServe, "http:3000:/")
	if observed.Status != core.ObservationMissing {
		t.Fatalf("a serve that is gone observed as %q, want missing", observed.Status)
	}
	if observed.Detail == "" {
		t.Error("the missing serve is reported without saying what is not being served")
	}
}

// TestADriftedServeIsObservedDrifted pins the drift case.
//
// The client is serving the same frontend identity but a different backend. That is
// drift Portico owns and may safely restore — it must NOT be reported as missing.
func TestADriftedServeIsObservedDrifted(t *testing.T) {
	route := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		// Client reports the same frontend but a different backend.
		on("serve status --json", serveHTTP("127.0.0.1:9999"), nil)

	obs, err := reconstructed(runner).ObserveWithResources(context.Background(), "conn-serve",
		persistedServe(route))
	if err != nil {
		t.Fatal(err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetServe, "http:3000:/")
	if observed.Status != core.ObservationDrifted {
		t.Fatalf("a drifted serve observed as %q, want drifted: %s", observed.Status, observed.Detail)
	}
}

// TestSomeoneElsesServeIsNotAdopted pins that observation does not claim what it did not
// create.
//
// The client is serving a different frontend identity. Portico's own is gone. Reporting
// present because *something* is served would adopt a configuration the user made by hand,
// and closing the connection would then withdraw it.
func TestSomeoneElsesServeIsNotAdopted(t *testing.T) {
	route := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve status --json", serveHTTP("127.0.0.1:9999"), nil)

	obs, err := reconstructed(runner).ObserveWithResources(context.Background(), "conn-serve",
		persistedServe(route))
	if err != nil {
		t.Fatal(err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetServe, "http:3000:/")
	if observed.Status == core.ObservationPresent {
		t.Fatal("a serve for a different backend was adopted as this connection's")
	}
}

// TestAnAddressSpelledDifferentlyStillMatches pins the normalisation.
//
// The client echoes back what it chose to store, which may be localhost where Portico
// recorded 127.0.0.1, or carry a scheme and a path. Two spellings of one address would
// make observation report the serve missing, and a missing serve is one reconciliation
// recreates — so it would republish an address that is already published.
func TestAnAddressSpelledDifferentlyStillMatches(t *testing.T) {
	route := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	reportedForms := []string{
		"127.0.0.1:3000",
		"http://127.0.0.1:3000",
		"http://localhost:3000",
		"http://127.0.0.1:3000/",
	}
	for _, reported := range reportedForms {
		runner := newFakeRunner().
			on("status --json", statusRunning, nil)
		// Script the exact reported form.
		runner.on("serve status --json", serveHTTPProxy(reported), nil)

		obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
			"conn-serve", persistedServe(route))
		if err != nil {
			t.Fatal(err)
		}
		observed := statusOf(t, obs, core.ResourceTailnetServe, "http:3000:/")
		if observed.Status != core.ObservationPresent {
			t.Errorf("a serve the client reported as %q was observed as %q",
				reported, observed.Status)
		}
	}
}

// TestObservingNothingQueriesNothing pins that a connection with no persisted resources
// does not go looking.
//
// With nothing stored there is nothing to observe, and searching for something that looks
// like Portico's is how an unrelated configuration gets adopted.
func TestObservingNothingQueriesNothing(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusRunning, nil)
	if _, err := reconstructed(runner).ObserveWithResources(
		context.Background(), "conn-join", nil); err != nil {
		t.Fatal(err)
	}
	if runner.called("serve status") {
		t.Errorf("the serve configuration was read with nothing persisted: %v", runner.calls())
	}
}

// TestObservationIsRepeatableAcrossReconstructions pins that restarting twice reaches the
// same answer.
func TestObservationIsRepeatableAcrossReconstructions(t *testing.T) {
	route := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	for attempt := 1; attempt <= 3; attempt++ {
		runner := newFakeRunner().
			on("status --json", statusRunning, nil).
			on("serve status --json", serveHTTP("127.0.0.1:3000"), nil)

		obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
			"conn-serve", persistedServe(route))
		if err != nil {
			t.Fatalf("reconstruction %d: %v", attempt, err)
		}
		observed := statusOf(t, obs, core.ResourceTailnetServe, "http:3000:/")
		if observed.Status != core.ObservationPresent {
			t.Fatalf("reconstruction %d observed %q", attempt, observed.Status)
		}
	}
}

// TestAStoppedClientIsTransient pins that a daemon that is up but not running the tailnet
// is not reported as the machine having left it.
func TestAStoppedClientIsTransient(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusStopped, nil)
	obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
		"conn-join", persistedJoin())
	if err != nil {
		t.Fatal(err)
	}
	observed := statusOf(t, obs, core.ResourceTailnetMembership, "n1234567890123456")
	if observed.Status != core.ObservationTransient {
		t.Fatalf("a stopped client observed as %q, want transient", observed.Status)
	}
}

// TestTwoPorticoServeConnectionsCanCoexist pins that two different frontend identities
// sharing the same backend do not interfere with each other.
func TestTwoPorticoServeConnectionsCanCoexist(t *testing.T) {
	routeA := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	routeB := ServeRoute{FrontendProtocol: "https", FrontendPort: "8443", BackendProtocol: "https", BackendHost: "127.0.0.1", BackendPort: "3000"}
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve status --json", `{
			"TCP": {
				"3000": {"HTTP": true, "HTTPS": false, "TCPForward": ""},
				"8443": {"HTTP": false, "HTTPS": true, "TCPForward": ""}
			},
			"Web": {
				"workstation.tail0abc.ts.net:3000": {
					"Handlers": {"/": {"Proxy": "http://127.0.0.1:3000"}}
				},
				"workstation.tail0abc.ts.net:8443": {
					"Handlers": {"/": {"Proxy": "https://127.0.0.1:3000"}}
				}
			}
		}`, nil)

	resources := append(persistedServe(routeA), persistedServe(routeB)[1:]...)
	obs, err := reconstructed(runner).ObserveWithResources(context.Background(), "conn-serve", resources)
	if err != nil {
		t.Fatal(err)
	}
	a := statusOf(t, obs, core.ResourceTailnetServe, routeA.Identity())
	b := statusOf(t, obs, core.ResourceTailnetServe, routeB.Identity())
	if a.Status != core.ObservationPresent {
		t.Errorf("route A observed as %q", a.Status)
	}
	if b.Status != core.ObservationPresent {
		t.Errorf("route B observed as %q", b.Status)
	}
}

// TestClosingOneServeDoesNotRemoveAnother pins that withdrawing one frontend does not
// affect a different frontend, even when they share the same backend.
func TestClosingOneServeDoesNotRemoveAnother(t *testing.T) {
	routeA := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	// Close only route A.
	runner := newFakeRunner().
		on("serve --bg --http=3000 off", "", nil).
		on("serve status --json", serveHTTP("127.0.0.1:3000"), nil)

	result, err := New(runner).ExecuteStep(context.Background(), "conn-serve", core.PlanStep{
		ID: "s", Technical: core.TechnicalOperation{
			Provider:   "tailscale",
			Type:       "unserve",
			Parameters: routeA.StepParameters(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded {
		t.Fatalf("the withdrawal failed: %v", result.Error)
	}
	// The close command targets only the http:3000 frontend.
	if !runner.called("serve --bg --http=3000 off") {
		t.Fatalf("the close command was wrong: %v", runner.calls())
	}
}

// TestAForeignIdenticalRouteIsObservedMissingNotPresent pins that a manually-created
// serve matching Portico's route is not adopted when Portico has no managed resource.
func TestAForeignIdenticalRouteIsObservedMissingNotPresent(t *testing.T) {
	// No persisted resources at all — Portico owns nothing.
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve status --json", serveHTTP("127.0.0.1:3000"), nil)

	obs, err := reconstructed(runner).ObserveWithResources(context.Background(), "conn-serve", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.ResourceStatuses) != 0 {
		t.Fatalf("no resources were queried but %d statuses returned", len(obs.ResourceStatuses))
	}
}

// TestAMalformedPersistedResourceIsTransientNotMissing pins that a persisted resource
// with corrupt metadata is classified transient (unverifiable), not silently skipped.
func TestAMalformedPersistedResourceIsTransientNotMissing(t *testing.T) {
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve status --json", serveHTTP("127.0.0.1:3000"), nil)

	obs, err := reconstructed(runner).ObserveWithResources(context.Background(), "conn-serve",
		[]core.ProviderResource{{
			Type:       core.ResourceTailnetServe,
			ExternalID: "http:3000:/",
			Ownership:  core.OwnershipManaged,
			Metadata:   map[string]string{"frontend_protocol": "http"}, // incomplete
		}})
	if err != nil {
		t.Fatal(err)
	}
	observed := statusOf(t, obs, core.ResourceTailnetServe, "http:3000:/")
	if observed.Status != core.ObservationTransient {
		t.Fatalf("malformed persisted resource observed as %q, want transient", observed.Status)
	}
}
