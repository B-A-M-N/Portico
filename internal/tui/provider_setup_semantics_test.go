package tui

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// The configure action is gated by what the provider declares, not by whether
// its name is on screen.
//
// `configurable := provider.ID != ""` enabled "Add account" for every
// catalogued provider — including ones with no setup flow at all — so the
// screen promised an action whose submission the supervisor was guaranteed to
// refuse. The gate now reads the supervisor's SetupKind, which is derived from
// the provider's own Definition: "account" stores a credential, "guidance"
// shows instructions, and empty means there is nothing to configure.

func providersSemanticsSnapshot() ipc.SnapshotDTO {
	snap := testSnapshot()
	snap.Providers = []ipc.ProviderDTO{
		{
			ID: "cloudflare", DisplayName: "Cloudflare",
			Availability: "unconfigured", Readiness: "needs_config",
			SetupKind: "account",
		},
		{
			ID: "tailscale", DisplayName: "Tailscale",
			Availability: "unconfigured", Readiness: "needs_config",
			SetupKind: "guidance",
		},
		{
			ID: "local_port_forward", DisplayName: "Local port forward",
			Availability: "ready", Readiness: "ready", Selectable: true,
			// No SetupKind: the provider has no setup flow at all.
		},
		{
			ID: "zrok", DisplayName: "zrok",
			Availability: "not_implemented", Readiness: "not_implemented",
			// No SetupKind: Portico names it but ships no adapter.
		},
	}
	return snap
}

func semanticsModel(t *testing.T) Model {
	t.Helper()
	m := readyModel(&fakeClient{}, providersSemanticsSnapshot())
	m.screen = ScreenProviders
	return m
}

// cursorTo walks the cursor to the provider row whose header is name, pressing
// down through the list the way the keyboard does.
func cursorTo(t *testing.T, m Model, name string) Model {
	t.Helper()
	for range 10 {
		if provider, ok := m.selectedProvider(); ok && provider.ID == name {
			return m
		}
		next, _ := m.Update(keyMsg("down"))
		m = next.(Model)
	}
	t.Fatalf("could not reach the %s row in ten steps", name)
	return m
}

// TestAddAccountIsOfferedOnlyWhereAnAccountCanBeAdded pins the gate per kind.
func TestAddAccountIsOfferedOnlyWhereAnAccountCanBeAdded(t *testing.T) {
	cases := []struct {
		provider   string
		wantLabel  string
		wantEnable bool
		wantReason string
	}{
		{"cloudflare", "Add account", true, ""},
		{"tailscale", "Set up", true, ""},
		{"local_port_forward", "Set up", false, "this provider has nothing to configure here"},
		{"zrok", "Set up", false, "this provider has nothing to configure here"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			m := cursorTo(t, semanticsModel(t), tc.provider)
			action, ok := m.actionsFor(ScreenProviders).Find(ActionConfigureProvider)
			if !ok {
				t.Fatalf("%s: the providers screen does not offer a configure action", tc.provider)
			}
			if action.Label != tc.wantLabel {
				t.Errorf("%s: label = %q, want %q", tc.provider, action.Label, tc.wantLabel)
			}
			if action.Enabled != tc.wantEnable {
				if tc.wantEnable {
					t.Errorf("%s: configure is disabled: %s", tc.provider, action.DisabledReason)
				} else {
					t.Errorf("%s: configure is enabled for a provider with no account flow", tc.provider)
				}
			}
			if action.DisabledReason != tc.wantReason {
				t.Errorf("%s: reason = %q, want %q", tc.provider, action.DisabledReason, tc.wantReason)
			}
		})
	}
}

// TestPressingAOnASetuplessProviderSendsNothing pins the failure the gate
// exists to prevent: the keystroke must not fire a request the supervisor will
// refuse. An enabled-looking action that always fails is the exact "feature
// exists but does not work" shape the audit found.
func TestPressingAOnASetuplessProviderSendsNothing(t *testing.T) {
	for _, id := range []string{"local_port_forward", "zrok"} {
		t.Run(id, func(t *testing.T) {
			client := &fakeClient{}
			m := readyModel(client, providersSemanticsSnapshot())
			m.screen = ScreenProviders
			m = cursorTo(t, m, id)

			next, cmd := m.Update(keyMsg("a"))
			m = next.(Model)
			if cmd != nil {
				t.Fatalf("%s: pressing a sent a setup request that cannot succeed", id)
			}
			if m.screen == ScreenProviderSetup {
				t.Fatalf("%s: pressing a opened a setup form for a provider with no flow", id)
			}
			if len(client.setupFlowAsked) != 0 {
				t.Fatalf("%s: the press reached the supervisor %d times", id, len(client.setupFlowAsked))
			}
		})
	}
}

// TestGuidanceConfigureNamesWhatItDoes pins that a guidance flow's action
// reads as instructions rather than promising a stored account.
func TestGuidanceConfigureNamesWhatItDoes(t *testing.T) {
	m := cursorTo(t, semanticsModel(t), "tailscale")
	action, _ := m.actionsFor(ScreenProviders).Find(ActionConfigureProvider)
	if action.Help == "" {
		t.Fatal("the guidance configure action carries no explanation")
	}
	for _, forbidden := range []string{"stored encrypted"} {
		if action.Help == forbidden {
			t.Errorf("the guidance help claims a stored credential: %q", action.Help)
		}
	}
}

// TestSetupScreenEnterFollowsTheDeclaredFlow pins the readiness screen's side
// of the same contract: enter is offered for a provider with a flow and
// refuses with a reason for one without.
func TestSetupScreenEnterFollowsTheDeclaredFlow(t *testing.T) {
	client := &fakeClient{readiness: &ipc.ReadinessDTO{
		Providers: []ipc.ProviderReadinessDTO{
			{ID: "cloudflare", DisplayName: "Cloudflare", SetupKind: "account"},
			{ID: "local_port_forward", DisplayName: "Local port forward"},
		},
	}}
	m := readyModel(client, testSnapshot())
	m.screen = ScreenSetup
	m.setup = screens.NewSetup()
	m.setup.Readiness = client.readiness

	// Cursor starts on the first row (Cloudflare, which declares a flow).
	action, ok := m.actionsFor(ScreenSetup).Find(ActionConfigureProvider)
	if !ok {
		t.Fatal("the setup screen does not offer a configure action at all")
	}
	if !action.Enabled {
		t.Fatalf("enter is disabled for a provider that declares a flow: %s", action.DisabledReason)
	}

	// Move to the local-forward row: no flow, so enter is refused with why.
	next, _ := m.Update(keyMsg("down"))
	m = next.(Model)
	action, _ = m.actionsFor(ScreenSetup).Find(ActionConfigureProvider)
	if action.Enabled {
		t.Fatal("enter is enabled for a provider with no setup flow")
	}
	if action.DisabledReason != "this provider has nothing to configure here" {
		t.Fatalf("the refusal does not say why: %q", action.DisabledReason)
	}
}
