package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// Repairing a port forward.
//
// CanRepair named service exposure and stopped, so the interface carried a
// permanent "repair is not available for this connection kind" check — for a kind
// whose provider has a start step, a stop step and an Observe that reports whether
// the listener is up. Nothing about the architecture prevented repair; only that
// function did.

// forwardProfile is a saved port forward.
func forwardProfile(id core.ConnectionID) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID:       id,
		Name:     "database",
		Revision: 3,
		Kind:     core.ConnectionPortForward,
		Desired:  core.DesiredOpen,
		Spec: core.ConnectionSpec{
			PortForward: &core.PortForwardSpec{
				LocalPort:  15432,
				RemoteHost: "db.internal",
				RemotePort: 5432,
				Protocol:   core.ProtocolTCP,
				Direction:  core.PortForwardLocal,
			},
		},
	}
}

// TestAPortForwardCanBeRepaired pins that the kind is no longer refused.
func TestAPortForwardCanBeRepaired(t *testing.T) {
	c := &Controller{}
	if !c.CanRepair(forwardProfile("conn-forward")) {
		t.Fatal("a port forward is still reported as unrepairable")
	}
	// And the kinds that genuinely have no repair path still say so, rather than
	// CanRepair becoming a blanket yes.
	for _, kind := range []core.ConnectionKind{
		core.ConnectionPrivateNetwork, core.ConnectionClientTunnel,
	} {
		profile := forwardProfile("conn-x")
		profile.Kind = kind
		if c.CanRepair(profile) {
			t.Errorf("%s is reported repairable with no repair path implemented", kind)
		}
	}
}

// TestAMissingListenerIsRepairedByRestartingTheForward pins the commonest failure.
func TestAMissingListenerIsRepairedByRestartingTheForward(t *testing.T) {
	c := newForwardController(t, observation{listening: false})
	profile := forwardProfile("conn-forward")

	steps, err := c.portForwardRepairSteps(context.Background(), profile, nil)
	if err != nil {
		t.Fatalf("portForwardRepairSteps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("a forward with no listener produced no repair")
	}

	// The target is checked before the listener is started: a listener that
	// starts and then refuses everything is worse than a stated reason.
	if steps[0].Kind != core.StepVerifyOrigin {
		t.Fatalf("the first step is %s, want a target check", steps[0].Kind)
	}
	if !strings.Contains(steps[0].Summary, "db.internal:5432") {
		t.Errorf("the check does not name the target: %q", steps[0].Summary)
	}

	last := steps[len(steps)-1]
	if last.Kind != core.StepStartConnector {
		t.Fatalf("the repair does not restart the forward: last step is %s", last.Kind)
	}
	if got := last.Technical.Parameters["local_port"]; got != "15432" {
		t.Errorf("the restart carries local port %q, want 15432", got)
	}
	if got := last.Technical.Parameters["target"]; got != "db.internal:5432" {
		t.Errorf("the restart carries target %q", got)
	}
}

// TestAStoppedForwardIsClearedBeforeRestarting pins the ordering.
//
// A stale registration left in place makes the restart fail on an address already
// in use, which reports as a repair that did not work rather than as a repair that
// was applied in the wrong order.
func TestAStoppedForwardIsClearedBeforeRestarting(t *testing.T) {
	c := newForwardController(t, observation{listening: false})
	profile := forwardProfile("conn-forward")
	// The runtime believes the forward is running; the observation says it is not.
	rt := &core.ConnectionRuntime{
		Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
	}

	steps, err := c.portForwardRepairSteps(context.Background(), profile, rt)
	if err != nil {
		t.Fatalf("portForwardRepairSteps: %v", err)
	}

	var stopAt, startAt = -1, -1
	for i, step := range steps {
		switch step.Kind {
		case core.StepStopConnector:
			stopAt = i
		case core.StepStartConnector:
			startAt = i
		}
	}
	if stopAt < 0 {
		t.Fatal("a stale forward was not cleared before the restart")
	}
	if startAt < 0 {
		t.Fatal("the forward was cleared and never restarted")
	}
	if stopAt > startAt {
		t.Fatal("the restart is planned before the stale forward is cleared")
	}
}

// TestAnInconsistentRuntimeIsNotRepairedByRestarting pins the case where the
// record is wrong and the forward is fine.
//
// Restarting a working forward drops every connection currently using it. What is
// wrong is the runtime's record, and applying a plan re-observes — so the check
// alone reconciles it.
func TestAnInconsistentRuntimeIsNotRepairedByRestarting(t *testing.T) {
	c := newForwardController(t, observation{listening: true})
	profile := forwardProfile("conn-forward")
	rt := &core.ConnectionRuntime{
		Connector: core.ConnectorRuntime{Status: core.ConnectorStatusStopped},
	}

	steps, err := c.portForwardRepairSteps(context.Background(), profile, rt)
	if err != nil {
		t.Fatalf("portForwardRepairSteps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("a runtime disagreeing with the listener produced no repair at all")
	}
	for _, step := range steps {
		if step.Kind == core.StepStartConnector {
			t.Fatal("a working forward was restarted, dropping its live connections")
		}
		if step.Kind == core.StepStopConnector {
			t.Fatal("a working forward was stopped")
		}
	}
}

// TestAHealthyForwardNeedsNoRepair pins that nothing wrong produces nothing.
//
// A plan whose only step is a check is a plan that does nothing, and offering one
// tells the user something is wrong when nothing is.
func TestAHealthyForwardNeedsNoRepair(t *testing.T) {
	c := newForwardController(t, observation{listening: true})
	profile := forwardProfile("conn-forward")
	rt := &core.ConnectionRuntime{
		Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning},
	}

	steps, err := c.portForwardRepairSteps(context.Background(), profile, rt)
	if err != nil {
		t.Fatalf("portForwardRepairSteps: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("a healthy forward produced %d repair steps", len(steps))
	}
}

// TestAForwardWithNoSpecIsRefused pins that a malformed profile is reported
// rather than repaired into something.
func TestAForwardWithNoSpecIsRefused(t *testing.T) {
	c := newForwardController(t, observation{listening: false})
	profile := forwardProfile("conn-forward")
	profile.Spec.PortForward = nil

	if _, err := c.portForwardRepairSteps(context.Background(), profile, nil); err == nil {
		t.Fatal("a forward with no spec was repaired anyway")
	}
}

// --------------- fixtures ---------------

// newForwardController builds a controller whose only provider reports the given
// observation, so a repair is planned against a stated listener state rather than
// against a real socket.
func newForwardController(t *testing.T, obs observation) *Controller {
	t.Helper()
	c := &Controller{
		registry: &forwardRegistry{prov: forwardObserver{obs: obs}},
		profiles: map[core.ConnectionID]*core.ConnectionProfile{},
		runtimes: map[core.ConnectionID]*core.ConnectionRuntime{},
	}
	// Observe reads the profile, so the forward under test is registered.
	c.profiles["conn-forward"] = forwardProfile("conn-forward")
	return c
}

// forwardRegistry hands out one provider: the observer under test.
type forwardRegistry struct {
	prov core.Provider
}

func (r *forwardRegistry) Get(core.ProviderID) core.Provider                      { return r.prov }
func (r *forwardRegistry) List() []provider.ProviderSnapshot                      { return nil }
func (r *forwardRegistry) Snapshot() []provider.ProviderSnapshot                  { return nil }
func (r *forwardRegistry) Add(core.Provider) error                                { return nil }
func (r *forwardRegistry) AddCatalogEntry(provider.CatalogEntry)                  {}
func (r *forwardRegistry) Install(provider.Installation)                          {}
func (r *forwardRegistry) Replace(core.Provider)                                  {}
func (r *forwardRegistry) Remove(core.ProviderID)                                 {}
func (r *forwardRegistry) SetAccounts(core.ProviderID, []core.ProviderAccountID)  {}
func (r *forwardRegistry) SetAccountInfo(core.ProviderID, []provider.AccountInfo) {}
func (r *forwardRegistry) GetAccounts(core.ProviderID) []core.ProviderAccountID   { return nil }
func (r *forwardRegistry) DiscoverIdentities(context.Context) []provider.ProviderSnapshot {
	return nil
}

// observation is what the fake provider reports about the listener.
type observation struct {
	listening bool
	err       error
}

// forwardObserver is a provider that reports only what a repair needs.
type forwardObserver struct {
	core.Provider
	obs observation
}

func (f forwardObserver) Observe(_ context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	if f.obs.err != nil {
		return nil, f.obs.err
	}
	status := string(core.ConnectorStatusStopped)
	if f.obs.listening {
		status = string(core.ConnectorStatusRunning)
	}
	return &core.ObservedConnection{
		ConnectionID: id,
		ProviderID:   "portforward",
		Connector:    &core.ObservedConnector{Status: status},
	}, nil
}

var _ = errors.New
