package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The readiness screen, and the one operational choice it carries.
//
// Launch mode was reachable only through an undocumented `l`, the export was
// implemented and never advertised, and the mode could not be persisted — so the
// screen had to tell the user their explicit choice would be forgotten.

// TestSetupAdvertisesEverythingItDoes pins the drift item 2 names.
//
// `E` produced a diagnostic report and was advertised nowhere, which is a feature
// that exists for a user who is stuck and cannot be found by one.
func TestSetupAdvertisesEverythingItDoes(t *testing.T) {
	m := openSetupWith(t, &fakeClient{}, &ipc.ReadinessDTO{
		Summary:    "one provider is ready",
		LaunchMode: "auto",
		Providers: []ipc.ProviderReadinessDTO{
			{ID: "cloudflare", DisplayName: "Cloudflare", Availability: "ready"},
		},
	})
	m.width = 120

	actions := m.actionsFor(ScreenSetup)
	for _, id := range []ActionID{
		ActionConfigureProvider, ActionLaunchMode, ActionRefresh, ActionSupportExport,
	} {
		action, ok := actions.Find(id)
		if !ok {
			t.Errorf("the setup screen does not offer %s", id)
			continue
		}
		if action.Label == "" {
			t.Errorf("%s is offered with no label, so nothing can advertise it", id)
		}
		if action.Help == "" {
			t.Errorf("%s is offered with no explanation", id)
		}
	}

	// The export says what it does and does not contain, because a user is being
	// asked to attach it to a bug report.
	export, _ := actions.Find(ActionSupportExport)
	if !strings.Contains(export.Help, "credential") {
		t.Errorf("the report action does not say what it excludes: %q", export.Help)
	}
}

// TestTheLaunchModeKeyAsksTheSupervisor pins that the toggle reaches durable state.
func TestTheLaunchModeKeyAsksTheSupervisor(t *testing.T) {
	fake := &fakeClient{}
	m := openSetupWith(t, fake, &ipc.ReadinessDTO{LaunchMode: "auto"})

	action, ok := m.actionsFor(ScreenSetup).Find(ActionLaunchMode)
	if !ok || !action.Enabled {
		t.Fatal("the setup screen does not offer a launch-mode change")
	}

	_, cmd := press(t, m, action.primaryKey())
	if cmd == nil {
		t.Fatal("changing the launch mode sent nothing to the supervisor")
	}
	cmd()

	if len(fake.launchModeAsked) != 1 {
		t.Fatalf("the toggle made %d requests, want one", len(fake.launchModeAsked))
	}
	// Showing auto, the toggle asks for manual. That is the safe direction:
	// manual arms nothing.
	if fake.launchModeAsked[0] != "manual" {
		t.Fatalf("the toggle asked for %q while showing auto", fake.launchModeAsked[0])
	}
}

// TestAPinnedLaunchModeIsNotOfferedAsChangeable pins that an override is
// explained rather than silently swallowing the keystroke.
func TestAPinnedLaunchModeIsNotOfferedAsChangeable(t *testing.T) {
	fake := &fakeClient{}
	m := openSetupWith(t, fake, &ipc.ReadinessDTO{
		LaunchMode:         "manual",
		LaunchModePinned:   true,
		LaunchModePinnedBy: "PORTICO_LAUNCH_MODE",
	})
	m.width = 120

	action, ok := m.actionsFor(ScreenSetup).Find(ActionLaunchMode)
	if !ok {
		t.Fatal("the launch-mode action disappeared entirely when pinned")
	}
	if action.Enabled {
		t.Fatal("a pinned launch mode is offered as changeable")
	}
	if action.DisabledReason == "" {
		t.Fatal("a pinned launch mode gives no reason")
	}
	if !strings.Contains(action.Help, "environment variable") {
		t.Errorf("the help does not explain the override: %q", action.Help)
	}

	// Pressing the key changes nothing and asks for nothing.
	next, cmd := press(t, m, action.primaryKey())
	if cmd != nil {
		t.Fatal("a pinned launch mode was changed anyway")
	}
	if len(fake.launchModeAsked) != 0 {
		t.Fatal("a pinned launch mode sent a request")
	}
	// The refusal is stated rather than silent.
	if next.status == "" {
		t.Error("pressing a disabled action's key said nothing")
	}
}
