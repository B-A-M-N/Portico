package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider/tailscale"
)

// A stub observer that returns pre-set statuses and plans like the real
// Tailscale provider.
//
// Repair now asks the provider to plan the desired state rather than
// manufacturing technical parameters itself, so the stub must be able to plan.
// It embeds the real adapter over a scripted command runner: that is what makes
// the test cross the controller→provider boundary instead of stopping at it.
type networkObserver struct {
	*tailscale.Provider
	statuses []core.ObservedResourceStatus
}

func newNetworkObserver(statuses []core.ObservedResourceStatus) *networkObserver {
	runner := &scriptedTailscale{}
	return &networkObserver{
		Provider: tailscale.New(runner),
		statuses: statuses,
	}
}

// scriptedTailscale answers the status query the adapter makes while planning.
type scriptedTailscale struct{}

func (s *scriptedTailscale) Run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "status" {
		return []byte(`{
			"BackendState": "Running",
			"Self": {"ID": "n1234567890123456", "DNSName": "machine.example.", "TailscaleIPs": ["100.1.2.3"], "Online": true},
			"CurrentTailnet": {"Name": "example.com"}
		}`), nil
	}
	return []byte(`{}`), nil
}

func (o *networkObserver) Observe(_ context.Context,
	id core.ConnectionID) (*core.ObservedConnection, error) {
	return &core.ObservedConnection{
		ConnectionID: id, ProviderID: "tailscale",
		ResourceStatuses: o.statuses,
	}, nil
}

// ObserveWithResources must be overridden too: the embedded adapter implements
// core.ResourceAwareObserver, and Controller.Observe prefers that method. Without
// this the promoted method would win and the scripted statuses would be ignored.
func (o *networkObserver) ObserveWithResources(ctx context.Context,
	id core.ConnectionID, _ []core.ProviderResource) (*core.ObservedConnection, error) {
	return o.Observe(ctx, id)
}

// networkController registers the observer and the profile.
//
// It reuses forwardRegistry from the port-forward repair tests rather than adding a second
// one-provider registry: the two need exactly the same thing.
func networkController(t *testing.T, profile *core.ConnectionProfile,
	statuses []core.ObservedResourceStatus) *Controller {
	t.Helper()
	c := &Controller{
		registry: &forwardRegistry{prov: newNetworkObserver(statuses)},
		profiles: map[core.ConnectionID]*core.ConnectionProfile{},
		runtimes: map[core.ConnectionID]*core.ConnectionRuntime{},
	}
	// Observe reads the profile, so the connection under test is registered.
	c.profiles[profile.ID] = profile
	return c
}

// networkProfile is a private-network connection in the given mode.
func networkProfile(id core.ConnectionID, mode core.PrivateNetworkMode, address string) *core.ConnectionProfile {
	spec := &core.PrivateNetworkSpec{
		NetworkID: "example.com", Mode: mode,
		ExposeLocal:  mode == core.PrivateNetworkExpose,
		LocalAddress: address,
	}
	if mode == core.PrivateNetworkExpose {
		spec.LocalProtocol = core.ProtocolHTTP
	}
	return &core.ConnectionProfile{
		ID: id, Name: "network", Kind: core.ConnectionPrivateNetwork,
		Desired: core.DesiredOpen, Revision: 1,
		Driver: core.DriverSelection{ProviderID: "tailscale"},
		Spec:   core.ConnectionSpec{PrivateNetwork: spec},
	}
}

// TestAWithdrawnServeIsRepublished pins the drift Portico owns.
func TestAWithdrawnServeIsRepublished(t *testing.T) {
	profile := networkProfile("conn-serve", core.PrivateNetworkExpose, "127.0.0.1:3000")
	c := networkController(t, profile, []core.ObservedResourceStatus{
		{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
			Status: core.ObservationPresent},
		{Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
			Status: core.ObservationMissing},
	})

	steps, err := c.privateNetworkRepairSteps(context.Background(), profile)
	if err != nil {
		t.Fatalf("privateNetworkRepairSteps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("a withdrawn serve produced no repair")
	}

	var types []string
	for _, step := range steps {
		types = append(types, step.Technical.Type)
	}
	// The origin is verified before republishing: publishing an address nothing is
	// listening on produces a name on the network that refuses every connection.
	want := []string{"verify_membership", "verify_origin", "serve", "verify_serve"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", types, want)
	}
}

// TestADriftedServeIsRepublished pins that a drifted serve (same frontend, different
// backend) is also repaired — Portico owns the frontend and may restore it.
func TestADriftedServeIsRepublished(t *testing.T) {
	profile := networkProfile("conn-serve", core.PrivateNetworkExpose, "127.0.0.1:3000")
	c := networkController(t, profile, []core.ObservedResourceStatus{
		{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
			Status: core.ObservationPresent},
		{Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
			Status: core.ObservationDrifted},
	})

	steps, err := c.privateNetworkRepairSteps(context.Background(), profile)
	if err != nil {
		t.Fatalf("privateNetworkRepairSteps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("a drifted serve produced no repair")
	}
}

// TestAnIntactServeNeedsNoRepair pins that repair does not act when nothing is wrong.
func TestAnIntactServeNeedsNoRepair(t *testing.T) {
	profile := networkProfile("conn-serve", core.PrivateNetworkExpose, "127.0.0.1:3000")
	c := networkController(t, profile, []core.ObservedResourceStatus{
		{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
			Status: core.ObservationPresent},
		{Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
			Status: core.ObservationPresent},
	})

	steps, err := c.privateNetworkRepairSteps(context.Background(), profile)
	if err != nil {
		t.Fatalf("privateNetworkRepairSteps: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("a healthy connection produced %d repair steps", len(steps))
	}
}

// TestAJoinWithIntactMembershipNeedsNoRepair pins that a join has nothing to repair.
func TestAJoinWithIntactMembershipNeedsNoRepair(t *testing.T) {
	profile := networkProfile("conn-join", core.PrivateNetworkJoin, "")
	c := networkController(t, profile, []core.ObservedResourceStatus{
		{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
			Status: core.ObservationPresent},
	})

	steps, err := c.privateNetworkRepairSteps(context.Background(), profile)
	if err != nil {
		t.Fatalf("privateNetworkRepairSteps: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("a join produced %d repair steps; it creates nothing to repair", len(steps))
	}
}

// TestALeftNetworkIsReportedRatherThanRepaired pins the honest refusal.
//
// The machine is off the network. The fix is to sign it back in, which needs a credential
// Portico does not hold — so there is no step it could run, and saying so is more use than
// a repair that would fail.
func TestALeftNetworkIsReportedRatherThanRepaired(t *testing.T) {
	for _, mode := range []core.PrivateNetworkMode{
		core.PrivateNetworkJoin, core.PrivateNetworkExpose,
	} {
		profile := networkProfile("conn-x", mode, "127.0.0.1:3000")
		c := networkController(t, profile, []core.ObservedResourceStatus{
			{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
				Status: core.ObservationMissing},
		})

		steps, err := c.privateNetworkRepairSteps(context.Background(), profile)
		if err == nil {
			t.Fatalf("%s: leaving the network produced %d steps rather than a reason",
				mode, len(steps))
		}
		if !strings.Contains(err.Error(), "cannot sign it back in") {
			t.Errorf("%s: the refusal does not say why Portico cannot fix it: %v", mode, err)
		}
		if !strings.Contains(err.Error(), "recover on its own") {
			t.Errorf("%s: the refusal does not say what happens after signing in: %v", mode, err)
		}
	}
}

// TestATransientObservationIsNotRepaired pins the invariant that protects the machine.
//
// A client that could not be reached says nothing about membership. Repairing on that would
// act on a guess about a machine-wide setting, and for a publish it would republish an
// address that may already be published.
func TestATransientObservationIsNotRepaired(t *testing.T) {
	profile := networkProfile("conn-serve", core.PrivateNetworkExpose, "127.0.0.1:3000")
	c := networkController(t, profile, []core.ObservedResourceStatus{
		{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
			Status: core.ObservationTransient, Detail: "the client did not answer"},
		{Type: core.ResourceTailnetServe, ExternalID: "http:3000:/",
			Status: core.ObservationTransient},
	})

	steps, err := c.privateNetworkRepairSteps(context.Background(), profile)
	if err != nil {
		t.Fatalf("a transient observation produced an error rather than no repair: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("a transient observation produced %d repair steps", len(steps))
	}
}

// TestAServeWithNoRecordedAddressIsRefused pins that repair does not invent a target.
func TestAServeWithNoRecordedAddressIsRefused(t *testing.T) {
	profile := networkProfile("conn-serve", core.PrivateNetworkExpose, "")
	c := networkController(t, profile, []core.ObservedResourceStatus{
		{Type: core.ResourceTailnetMembership, ExternalID: "machine.example",
			Status: core.ObservationPresent},
		// Missing, and with no external ID to recover the target from.
		{Type: core.ResourceTailnetServe, ExternalID: "", Status: core.ObservationMissing},
	})

	if _, err := c.privateNetworkRepairSteps(context.Background(), profile); err == nil {
		t.Fatal("a serve with no recorded address was repaired against an invented target")
	}
}
