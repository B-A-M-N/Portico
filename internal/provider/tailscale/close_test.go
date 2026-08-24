package tailscale

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// Closing, restart reconstruction, and foreign-collision refusal.
//
// The invariant under test: what Portico withdraws comes from the durable
// ProviderResource it persisted, never from the current profile and never from
// adapter memory. The profile is what the user wants now; the resource is what
// exists remotely. Deletion must target the second one.

// runtimeWithServe builds a runtime carrying one live managed Serve resource for
// the given route — what the supervisor would have persisted after a successful
// serve.
func runtimeWithServe(route ServeRoute, ownership core.ResourceOwnership) *core.ConnectionRuntime {
	metadata := route.ResourceMetadata()
	metadata["address"] = "workstation.tail0abc.ts.net"
	return &core.ConnectionRuntime{
		ConnectionID: "conn-serve",
		State:        core.RuntimeOpen,
		Provider: core.ProviderRuntime{
			Resources: []core.ProviderResource{{
				ConnectionID: "conn-serve", ProviderID: "tailscale",
				Type: core.ResourceTailnetServe, ExternalID: route.Identity(),
				Ownership: ownership, Lifecycle: core.LifecyclePresent,
				Metadata: metadata,
			}},
		},
	}
}

// TestClosingUsesThePersistedRouteNotTheProfile is the central close invariant.
//
// The profile now says 4000, but Portico actually published 3000. Closing must
// withdraw 3000 — the route it owns — or it would leave the real route published
// and issue a withdrawal for something it never created.
func TestClosingUsesThePersistedRouteNotTheProfile(t *testing.T) {
	persisted := ServeRoute{
		FrontendProtocol: "http", FrontendPort: "3000", FrontendPath: "/",
		BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000",
	}

	// The profile has drifted to a different address.
	profile := exposeProfile(core.DesiredClosed, "127.0.0.1:4000")

	plan, err := New(newFakeRunner()).Plan(context.Background(), core.DesiredConnection{
		Profile: profile,
		Runtime: runtimeWithServe(persisted, core.OwnershipManaged),
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	step := stepFor(t, plan, "unserve")
	route, err := serveRouteFromStep(step.Technical.Parameters)
	if err != nil {
		t.Fatalf("the close step carries no reconstructable route: %v", err)
	}
	if !route.Equal(persisted) {
		t.Fatalf("the close targets %s, want the persisted route %s",
			route.Identity(), persisted.Identity())
	}
	if route.FrontendPort == "4000" {
		t.Fatal("the close targeted the profile's new address rather than what Portico owns")
	}
}

// TestClosingAfterRestartReconstructsFromTheResourceAlone pins that a connection
// created before a restart can still be closed exactly.
//
// The adapter is fresh — it has no memory of serving anything. The only input is
// the persisted resource.
func TestClosingAfterRestartReconstructsFromTheResourceAlone(t *testing.T) {
	persisted := ServeRoute{
		FrontendProtocol: "tcp", FrontendPort: "5432", FrontendPath: "/",
		BackendProtocol: "tcp", BackendHost: "127.0.0.1", BackendPort: "5432",
	}

	runner := newFakeRunner().on("serve --bg --tcp=5432 off", "", nil)
	// A freshly reconstructed provider, as a restarted supervisor builds it.
	provider := New(runner)

	plan, err := provider.Plan(context.Background(), core.DesiredConnection{
		Profile: exposeProfileWithProtocol(core.DesiredClosed, "127.0.0.1:5432", core.ProtocolTCP),
		Runtime: runtimeWithServe(persisted, core.OwnershipManaged),
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Execute the withdrawal through the reconstructed adapter.
	result, err := provider.ExecuteStep(context.Background(), "conn-serve",
		stepFor(t, plan, "unserve"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded {
		t.Fatalf("the withdrawal failed: %v", result.Error)
	}
	if !runner.called("serve --bg --tcp=5432 off") {
		t.Fatalf("the exact persisted route was not withdrawn: %v", runner.calls())
	}
}

// TestClosingRefusesACorruptPersistedRoute pins that a close fails closed rather
// than guessing.
//
// If the managed resource's metadata cannot be parsed, deriving a plausible route
// could withdraw somebody else's Serve configuration. Failing is correct.
func TestClosingRefusesACorruptPersistedRoute(t *testing.T) {
	runtime := &core.ConnectionRuntime{
		ConnectionID: "conn-serve",
		State:        core.RuntimeOpen,
		Provider: core.ProviderRuntime{
			Resources: []core.ProviderResource{{
				ConnectionID: "conn-serve", ProviderID: "tailscale",
				Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
				Ownership: core.OwnershipManaged, Lifecycle: core.LifecyclePresent,
				// Corrupt: no backend at all.
				Metadata: map[string]string{"frontend_protocol": "http"},
			}},
		},
	}

	runner := newFakeRunner()
	provider := New(runner)
	plan, err := provider.Plan(context.Background(), core.DesiredConnection{
		Profile: exposeProfile(core.DesiredClosed, "127.0.0.1:3000"),
		Runtime: runtime,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// The step must not carry a plausible-but-invented route.
	step := stepFor(t, plan, "unserve")
	if _, err := serveRouteFromStep(step.Technical.Parameters); err == nil {
		t.Fatal("a corrupt persisted resource produced a usable route; Portico invented one")
	}

	// And executing it fails rather than withdrawing a guess.
	result, err := provider.ExecuteStep(context.Background(), "conn-serve", step)
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded {
		t.Fatal("a corrupt persisted route was withdrawn anyway")
	}
	if len(runner.calls()) != 0 {
		t.Fatalf("a client command ran against an invented route: %v", runner.calls())
	}
}

// TestPlanningRefusesAForeignRouteAtTheSameFrontend pins the ownership boundary.
//
// A live resource occupies the desired frontend but Portico does not manage it.
// Overwriting it would take over configuration the user made by hand.
func TestPlanningRefusesAForeignRouteAtTheSameFrontend(t *testing.T) {
	desired := ServeRoute{
		FrontendProtocol: "http", FrontendPort: "3000", FrontendPath: "/",
		BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000",
	}

	runner := newFakeRunner().on("status --json", statusRunning, nil)
	_, err := New(runner).Plan(context.Background(), core.DesiredConnection{
		Profile: exposeProfile(core.DesiredOpen, "127.0.0.1:3000"),
		// Adopted, not managed: Portico did not create it.
		Runtime: runtimeWithServe(desired, core.OwnershipAdopted),
	})
	if err == nil {
		t.Fatal("a foreign route at the desired frontend was planned over")
	}
	if !strings.Contains(err.Error(), "will not overwrite") {
		t.Errorf("the refusal does not explain the ownership boundary: %v", err)
	}
}

// TestPlanningProceedsWhenPorticoOwnsTheFrontend pins the other side of that
// boundary: a route Portico manages is not a collision.
func TestPlanningProceedsWhenPorticoOwnsTheFrontend(t *testing.T) {
	desired := ServeRoute{
		FrontendProtocol: "http", FrontendPort: "3000", FrontendPath: "/",
		BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000",
	}

	runner := newFakeRunner().on("status --json", statusRunning, nil)
	plan, err := New(runner).Plan(context.Background(), core.DesiredConnection{
		Profile: exposeProfile(core.DesiredOpen, "127.0.0.1:3000"),
		Runtime: runtimeWithServe(desired, core.OwnershipManaged),
	})
	if err != nil {
		t.Fatalf("Portico refused to re-plan a route it owns: %v", err)
	}
	if _, err := serveRouteFromStep(stepFor(t, plan, "serve").Technical.Parameters); err != nil {
		t.Fatalf("the serve step carries no route: %v", err)
	}
}

// TestPlanningProceedsForADifferentFrontend pins that two Portico Serve
// connections can coexist: a different frontend identity is not a collision,
// even when it shares the same backend.
func TestPlanningProceedsForADifferentFrontend(t *testing.T) {
	existing := ServeRoute{
		FrontendProtocol: "tcp", FrontendPort: "5432", FrontendPath: "/",
		BackendProtocol: "tcp", BackendHost: "127.0.0.1", BackendPort: "5432",
	}

	runner := newFakeRunner().on("status --json", statusRunning, nil)
	// Desired is http:3000 — a different frontend than the existing tcp:5432.
	if _, err := New(runner).Plan(context.Background(), core.DesiredConnection{
		Profile: exposeProfile(core.DesiredOpen, "127.0.0.1:3000"),
		Runtime: runtimeWithServe(existing, core.OwnershipAdopted),
	}); err != nil {
		t.Fatalf("a different frontend was refused as a collision: %v", err)
	}
}

// TestPlanningRefusesWhenOwnershipCannotBeVerified pins the fail-closed case.
//
// A live tracked resource whose metadata cannot be parsed might BE the occupant of
// the desired frontend — Portico cannot tell. Skipping it would let a corrupt row
// read as "nothing is there", and Portico would overwrite a binding it could not
// establish ownership of. Refusing is correct.
func TestPlanningRefusesWhenOwnershipCannotBeVerified(t *testing.T) {
	runtime := &core.ConnectionRuntime{
		ConnectionID: "conn-serve",
		State:        core.RuntimeOpen,
		Provider: core.ProviderRuntime{
			Resources: []core.ProviderResource{{
				ConnectionID: "conn-serve", ProviderID: "tailscale",
				Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
				// Managed, but the route cannot be reconstructed from this.
				Ownership: core.OwnershipManaged, Lifecycle: core.LifecyclePresent,
				Metadata: map[string]string{"frontend_protocol": "http"},
			}},
		},
	}

	runner := newFakeRunner().on("status --json", statusRunning, nil)
	_, err := New(runner).Plan(context.Background(), core.DesiredConnection{
		Profile: exposeProfile(core.DesiredOpen, "127.0.0.1:3000"),
		Runtime: runtime,
	})
	if err == nil {
		t.Fatal("planning proceeded over a resource whose ownership could not be verified")
	}
	if !strings.Contains(err.Error(), "cannot parse") {
		t.Errorf("the refusal does not explain that ownership is unverifiable: %v", err)
	}
	// And no client command ran: the refusal happens before any mutation.
	if len(runner.calls()) > 1 {
		t.Errorf("a mutation was attempted before ownership was established: %v", runner.calls())
	}
}

// TestAStatusCommandFailureIsTransientNotMissing pins the classification that
// stops reconciliation acting on a false absence.
//
// `serve status` failing for permission, daemon, or version reasons says nothing
// about whether the route exists. Reporting missing would make reconciliation
// recreate infrastructure based on an unknown.
func TestAStatusCommandFailureIsTransientNotMissing(t *testing.T) {
	route := ServeRoute{
		FrontendProtocol: "http", FrontendPort: "3000", FrontendPath: "/",
		BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000",
	}

	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		// Non-zero exit, and it still printed parseable JSON. The exit code wins.
		on("serve status --json", serveNothing, errors.New("permission denied"))

	obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
		"conn-serve", persistedServe(route))
	if err != nil {
		t.Fatal(err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetServe, "http:3000:/")
	if observed.Status == core.ObservationMissing {
		t.Fatal("a failed status command was reported as the route being gone")
	}
	if observed.Status != core.ObservationTransient {
		t.Fatalf("observed as %q, want transient", observed.Status)
	}
}

// TestMalformedServeConfigurationIsNotMissing pins fail-closed parsing.
//
// A route Portico cannot interpret must not read as absent. "I could not
// understand this route" and "this route is missing" lead to opposite actions.
func TestMalformedServeConfigurationIsNotMissing(t *testing.T) {
	route := ServeRoute{
		FrontendProtocol: "http", FrontendPort: "3000", FrontendPath: "/",
		BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000",
	}

	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		// A web frontend whose backend cannot be read.
		on("serve status --json", serveHTTPProxy("not-an-address"), nil)

	obs, err := reconstructed(runner).ObserveWithResources(context.Background(),
		"conn-serve", persistedServe(route))
	if err != nil {
		t.Fatal(err)
	}

	observed := statusOf(t, obs, core.ResourceTailnetServe, "http:3000:/")
	if observed.Status == core.ObservationMissing {
		t.Fatal("an uninterpretable serve configuration was reported as the route being gone")
	}
}
