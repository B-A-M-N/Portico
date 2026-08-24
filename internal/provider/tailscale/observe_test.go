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
// — a fresh Provider with an empty serving map — and observes from the resources it
// persisted. Every test here builds a new provider rather than reusing one that executed
// the steps, because reusing it would prove only that the adapter remembers its own work.

// reconstructed builds a provider the way a restarted supervisor does.
func reconstructed(runner CommandRunner) *Provider { return New(runner) }

// persistedJoin is what the supervisor stored for a join connection.
func persistedJoin() []core.ProviderResource {
	return []core.ProviderResource{{
		Type:       core.ResourceTailnetMembership,
		ExternalID: "workstation.tail0abc.ts.net",
		Ownership:  core.OwnershipAdopted,
	}}
}

// persistedServe is what the supervisor stored for a serve connection.
func persistedServe(target string) []core.ProviderResource {
	return []core.ProviderResource{{
		Type:       core.ResourceTailnetServe,
		ExternalID: target,
		Ownership:  core.OwnershipManaged,
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

// TestAJoinIsObservedFromThePersistedMachineName pins exact-identity observation.
func TestAJoinIsObservedFromThePersistedMachineName(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusRunning, nil)
	obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
		"conn-join", persistedJoin())
	if err != nil {
		t.Fatalf("ObserveWithResources: %v", err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetMembership, "workstation.tail0abc.ts.net")
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

	observed := statusOf(t, obs, core.ResourceTailnetMembership, "workstation.tail0abc.ts.net")
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

	observed := statusOf(t, obs, core.ResourceTailnetMembership, "workstation.tail0abc.ts.net")
	if observed.Status == core.ObservationMissing {
		t.Fatal("an unreachable client was reported as the machine having left the network")
	}
	if observed.Status != core.ObservationTransient {
		t.Fatalf("observed as %q, want transient", observed.Status)
	}
}

// TestAServeIsObservedFromTheClientNotFromMemory is the central restart case.
//
// The provider observing has never served anything: its map is empty. The only input is
// the persisted target, and the only source of truth is the client's own configuration.
func TestAServeIsObservedFromTheClientNotFromMemory(t *testing.T) {
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve status --json", serveHTTP("127.0.0.1:3000"), nil)

	provider := reconstructed(runner)
	if got := provider.servedTarget("conn-serve"); got != "" {
		t.Fatalf("a reconstructed provider already remembers %q", got)
	}

	obs, err := provider.ObserveWithResources(context.Background(), "conn-serve",
		persistedServe("127.0.0.1:3000"))
	if err != nil {
		t.Fatalf("ObserveWithResources: %v", err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetServe, "127.0.0.1:3000")
	if observed.Status != core.ObservationPresent {
		t.Fatalf("the serve observed as %q: %s", observed.Status, observed.Detail)
	}
	// Observing also re-establishes what to withdraw on close, so a connection created
	// before a restart can still be closed cleanly.
	if got := provider.servedTarget("conn-serve"); got != "127.0.0.1:3000" {
		t.Errorf("observation did not recover the withdrawal target: %q", got)
	}
}

// TestAWithdrawnServeIsObservedMissing pins the drift case reconciliation exists for.
func TestAWithdrawnServeIsObservedMissing(t *testing.T) {
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve status --json", serveNothing, nil)

	obs, err := reconstructed(runner).ObserveWithResources(context.Background(), "conn-serve",
		persistedServe("127.0.0.1:3000"))
	if err != nil {
		t.Fatal(err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetServe, "127.0.0.1:3000")
	if observed.Status != core.ObservationMissing {
		t.Fatalf("a serve that is gone observed as %q, want missing", observed.Status)
	}
	if observed.Detail == "" {
		t.Error("the missing serve is reported without saying what is not being served")
	}
}

// TestSomeoneElsesServeIsNotAdopted pins that observation does not claim what it did not
// create.
//
// The client is serving a different address. Portico's own is gone. Reporting present
// because *something* is served would adopt a configuration the user made by hand, and
// closing the connection would then withdraw it.
func TestSomeoneElsesServeIsNotAdopted(t *testing.T) {
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve status --json", serveHTTP("127.0.0.1:9999"), nil)

	obs, err := reconstructed(runner).ObserveWithResources(context.Background(), "conn-serve",
		persistedServe("127.0.0.1:3000"))
	if err != nil {
		t.Fatal(err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetServe, "127.0.0.1:3000")
	if observed.Status == core.ObservationPresent {
		t.Fatal("a serve for a different address was adopted as this connection's")
	}
}

// TestAnAddressSpelledDifferentlyStillMatches pins the normalisation.
//
// The client echoes back what it chose to store, which may be localhost where Portico
// recorded 127.0.0.1, or carry a scheme and a path. Two spellings of one address would
// make observation report the serve missing, and a missing serve is one reconciliation
// recreates — so it would republish an address that is already published.
func TestAnAddressSpelledDifferentlyStillMatches(t *testing.T) {
	for _, reported := range []string{
		"127.0.0.1:3000",
		"http://127.0.0.1:3000",
		"http://localhost:3000",
		"http://127.0.0.1:3000/",
	} {
		runner := newFakeRunner().
			on("status --json", statusRunning, nil).
			on("serve status --json", serveHTTP(""), nil)
		// Script the exact reported form.
		runner.on("serve status --json", `{"Web":{"h:443":{"Handlers":{"/":{"Proxy":"`+
			reported+`"}}}}}`, nil)

		obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
			"conn-serve", persistedServe("127.0.0.1:3000"))
		if err != nil {
			t.Fatal(err)
		}
		observed := statusOf(t, obs, core.ResourceTailnetServe, "127.0.0.1:3000")
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
	for attempt := 1; attempt <= 3; attempt++ {
		runner := newFakeRunner().
			on("status --json", statusRunning, nil).
			on("serve status --json", serveHTTP("127.0.0.1:3000"), nil)

		obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
			"conn-serve", persistedServe("127.0.0.1:3000"))
		if err != nil {
			t.Fatalf("reconstruction %d: %v", attempt, err)
		}
		observed := statusOf(t, obs, core.ResourceTailnetServe, "127.0.0.1:3000")
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
	observed := statusOf(t, obs, core.ResourceTailnetMembership, "workstation.tail0abc.ts.net")
	if observed.Status != core.ObservationTransient {
		t.Fatalf("a stopped client observed as %q, want transient", observed.Status)
	}
}
