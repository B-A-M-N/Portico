package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The action model is the one description of what a screen can do.
//
// Key handling, the footer and Help were three separate descriptions and they
// had drifted in every direction the audit found: Inspect executed Space, Repair
// and Delete while advertising none of them; Setup implemented a support export
// and never said so; the scroll keys worked everywhere and were documented
// nowhere; the Providers help omitted removal.
//
// These tests are about the property that replaces those defects, not about the
// individual instances of them. A new screen, or a new action on an existing
// screen, is covered the moment it is added.

// productScreens are the screens a user navigates. Boot and Quit are transient
// states with no actions of their own.
var productScreens = []ScreenID{
	ScreenHome, ScreenInspect, ScreenNewConnection, ScreenEdit, ScreenClone,
	ScreenPlanPreview, ScreenOperationProgress, ScreenOperations, ScreenRepair,
	ScreenDiscovery, ScreenProviders, ScreenSetup, ScreenSettings,
	ScreenAccountRemoval, ScreenRecovery, ScreenHelp,
}

// TestEveryAdvertisedActionIsDispatchable pins that an advertised action is one
// the screen actually accepts.
//
// An action drawn in the footer whose key nothing handles is worse than a
// missing action: the user presses it and the interface does nothing, with no
// indication whether they mistyped or the feature is broken.
func TestEveryAdvertisedActionIsDispatchable(t *testing.T) {
	for _, screen := range productScreens {
		m := actionModelFor(t, screen)
		actions := m.actionsFor(screen)

		for _, action := range actions.Advertised() {
			for _, key := range action.Keys {
				found, ok := actions.FindByKey(key)
				if !ok {
					t.Errorf("%s advertises %q for %q, which the screen does not accept",
						screen, key, action.Label)
					continue
				}
				if found.ID != action.ID {
					t.Errorf("%s: key %q is advertised for %q but dispatches %q",
						screen, key, action.ID, found.ID)
				}
			}
		}
	}
}

// TestAdvertisedActionsAreNamed pins that anything drawn has something to draw.
func TestAdvertisedActionsAreNamed(t *testing.T) {
	for _, screen := range productScreens {
		m := actionModelFor(t, screen)
		for _, action := range m.actionsFor(screen).Advertised() {
			if action.primaryKey() == "" {
				t.Errorf("%s advertises %q with no key, so it cannot be invoked",
					screen, action.Label)
			}
			if action.Help == "" {
				t.Errorf("%s advertises %q with no explanation, so Help cannot describe it",
					screen, action.Label)
			}
		}
	}
}

// TestDisabledActionsCannotExecute pins the enablement check.
//
// The reason an action carries its own enabled state is that the alternative was
// each dispatch site re-deriving it — which is how Inspect came to accept `r` on
// a connection kind the backend refuses, reporting the refusal only afterwards.
func TestDisabledActionsCannotExecute(t *testing.T) {
	for _, screen := range productScreens {
		m := actionModelFor(t, screen)
		actions := m.actionsFor(screen)

		for _, action := range actions {
			if action.Enabled {
				continue
			}
			for _, key := range action.Keys {
				// Lookup is what dispatch uses. A disabled action must not be
				// reachable through it.
				if resolved, ok := actions.Lookup(key); ok && resolved.ID == action.ID {
					t.Errorf("%s: disabled action %q is still dispatchable via %q",
						screen, action.ID, key)
				}
			}
			// A disabled action a user can see needs to say why.
			if action.Label != "" && action.DisabledReason == "" {
				t.Errorf("%s advertises %q as unavailable with no reason given",
					screen, action.Label)
			}
		}
	}
}

// TestDisabledActionKeypressExplainsItself pins that pressing the key of a
// visibly-unavailable action produces the reason rather than silence.
func TestDisabledActionKeypressExplainsItself(t *testing.T) {
	// Home with nothing selected: the lifecycle actions are advertised and
	// disabled, which is deliberate — the screen must not change shape as the
	// first connection appears.
	m := readyModel(&fakeClient{}, ipc.SnapshotDTO{})
	m.screen = ScreenHome
	m.selectedID = ""

	action, ok := m.actionsFor(ScreenHome).Find(ActionEdit)
	if !ok || action.Enabled {
		t.Skip("edit is not disabled on an empty home; nothing to assert here")
	}

	next, _ := press(t, m, "e")
	if next.status == "" {
		t.Fatal("pressing a disabled action's key said nothing at all")
	}
	if !strings.Contains(next.status, action.DisabledReason) {
		t.Fatalf("the refusal does not give the reason %q: %q",
			action.DisabledReason, next.status)
	}
	if next.screen != ScreenHome {
		t.Fatalf("a disabled action navigated to %s", next.screen)
	}
}

// TestEveryScreenHasContextualHelp pins that Help describes the current task.
//
// Help was a single hand-written switch with no entry for the wizard, Edit,
// Clone, Setup, Settings, account removal or the plan preview — the screens where
// a user is most likely to be stuck.
func TestEveryScreenHasContextualHelp(t *testing.T) {
	for _, screen := range productScreens {
		if screen == ScreenHelp {
			continue
		}
		title, body := screenHelp(screen)
		if title == "" || title == "Portico" {
			t.Errorf("%s has no contextual help title", screen)
		}
		if len(body) == 0 {
			t.Errorf("%s has a help title but no description of the task", screen)
		}
	}
}

// TestHelpListsTheScreensOwnActions pins that Help is generated from the action
// set rather than written beside it.
func TestHelpListsTheScreensOwnActions(t *testing.T) {
	for _, screen := range productScreens {
		if screen == ScreenHelp {
			continue
		}
		m := actionModelFor(t, screen)
		m.prevScreen = screen
		m.screen = ScreenHelp
		help := m.renderHelp()

		for _, action := range m.actionsFor(screen).Advertised() {
			if !strings.Contains(help, action.Label) {
				t.Errorf("%s help omits the advertised action %q", screen, action.Label)
			}
		}
	}
}

// actionModelFor builds a model sitting on one screen with enough state that the
// screen's actions are meaningful.
func actionModelFor(t *testing.T, screen ScreenID) Model {
	t.Helper()
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.selectedID = "conn-a"
	m.screen = screen

	switch screen {
	case ScreenEdit:
		m.edit = &editState{connectionID: "conn-a"}
	case ScreenClone:
		m.clone = &cloneState{}
	case ScreenSettings:
		m.settings = &settingsState{}
	}
	return m
}
