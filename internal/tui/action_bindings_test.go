package tui

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// One key, one meaning.
//
// The provider-setup footer once drew "[esc] Cancel setup" and "[esc] Back" on
// the same bar: the form owned Escape, and global navigation advertised a
// second interpretation of the same key. Whichever one the reader believed,
// the other was a lie — and the same structural duplication existed on the
// edit screen while typing ("Cancel value" beside "Back"), in the discard
// confirmations ("Keep editing" beside "Back"), on Help ("Close help" beside
// "Back"), and in navigation's "[q] Quit" beside recovery's own.
//
// These tests pin the property that replaces the defect: for every reachable
// screen state, no key is claimed by two advertised actions, and the
// composition that guarantees it (navigation contributes only unclaimed keys)
// holds without each screen having to remember it.

// bindingStateCase pairs a name with a builder for one reachable state's action
// set — typing, confirming discard, the setup confirmation step, a guidance
// flow. Some states are cheapest to reach through the model, some through the
// composition boundary itself; both must obey the same invariant.
type bindingStateCase struct {
	name string
	// model builds the model sitting on the state under test; the set is read
	// from m.actionsFor(m.screen). Mutually exclusive with set.
	model func(t *testing.T) Model
	// set builds the action set directly. Mutually exclusive with model.
	set func(t *testing.T) ActionSet
}

// actionsFor reads the case's action set through whichever path it declared.
func (tc bindingStateCase) actions(t *testing.T) (ActionSet, ScreenID) {
	t.Helper()
	if tc.set != nil {
		return tc.set(t), ""
	}
	m := tc.model(t)
	return m.actionsFor(m.screen), m.screen
}

func bindingStateCases() []bindingStateCase {
	return []bindingStateCase{
		{
			name: "provider setup, first field",
			model: func(t *testing.T) Model {
				return setupNavFixture(t, ScreenHome, cloudflareNavSetupFlow())
			},
		},
		{
			name: "provider setup, confirmation step",
			model: func(t *testing.T) Model {
				m := setupNavFixture(t, ScreenHome, cloudflareNavSetupFlow())
				m.providerSetupIndex = len(m.providerSetupFields())
				return m
			},
		},
		{
			name: "provider setup, guidance flow",
			model: func(t *testing.T) Model {
				guidance := &ipc.SetupFlowDTO{
					ProviderID: "tailscale", Kind: "guidance",
					Summary:        "Tailscale is configured in its own admin console.",
					GuidanceReason: "Portico holds no Tailscale credential.",
					Fields:         []ipc.SetupFieldDTO{{ID: "credential", Label: "Auth key", Secret: true}},
				}
				return setupNavFixture(t, ScreenProviders, guidance)
			},
		},
		{
			name: "provider setup, loading",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenProviderSetup
				return m
			},
		},
		{
			name: "edit, list",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenEdit
				m.edit = &editState{connectionID: "conn-a"}
				return m
			},
		},
		{
			name: "edit, typing in a field",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenEdit
				m.edit = &editState{connectionID: "conn-a"}
				m.edit.typing = true
				return m
			},
		},
		{
			name: "edit, confirming discard",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenEdit
				m.edit = &editState{connectionID: "conn-a"}
				m.edit.confirmingDiscard = true
				return m
			},
		},
		{
			name: "wizard, opening question",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, testSnapshot())
				next, _ := m.Update(keyMsg("n"))
				nm := next.(Model)
				if nm.wizard == nil {
					t.Fatal("the wizard did not open")
				}
				return nm
			},
		},
		{
			name: "wizard, confirming discard",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, testSnapshot())
				next, _ := m.Update(keyMsg("n"))
				nm := next.(Model)
				nm.wizardConfirmingDiscard = true
				return nm
			},
		},
		{
			name: "wizard action set carrying its own escape",
			set: func(t *testing.T) ActionSet {
				// A wizard question whose own actions bind Escape ("Not now" on
				// the plan preview) is exactly where navigation's Back must not
				// also advertise itself. Wizard steps beyond the opening
				// question cannot be constructed from outside the wizard
				// package, so the composition is exercised at its boundary:
				// wizardActionSet is what the root model composes against, with
				// the same shape Actions() emits for the plan preview.
				set := wizardActionSet(screens.WizardActions{{
					Keys: []string{"enter"}, Label: "Apply", Enabled: true, Primary: true,
				}, {
					Keys: []string{"esc"}, Label: "Not now", Enabled: true, Primary: true,
				}})
				return append(set, navigationActions(ScreenNewConnection, scrollsFreely(ScreenNewConnection)).notClaimedBy(set)...)
			},
		},
		{
			name: "clone, loading",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenClone
				m.clone = &cloneState{sourceID: "conn-a", sourceName: "one"}
				return m
			},
		},
		{
			name: "clone, ready",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenClone
				m.clone = &cloneState{sourceID: "conn-a", sourceName: "one"}
				m.clone.detail = &ipc.ConnectionDetailDTO{Summary: ipc.ConnectionDTO{ID: "conn-a"}}
				return m
			},
		},
		{
			name: "settings, text editor open",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenSettings
				m.settings = &settingsState{settings: &ipc.SettingsDTO{}}
				for _, row := range m.settingsRows() {
					if row.id != ActionClientTunnelBin {
						continue
					}
					next, _, _ := m.beginSettingsEdit(row)
					if next.settings.editing == nil {
						t.Fatal("the settings editor did not open")
					}
					return next
				}
				t.Fatal("no client-tunnel-bin row to open an editor for")
				return m
			},
		},
		{
			name: "settings, confirming key rotation",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenSettings
				m.settings = &settingsState{confirmingRotate: true}
				return m
			},
		},
		{
			name: "help open",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenHome
				next, _ := m.Update(keyMsg("?"))
				nm := next.(Model)
				if nm.screen != ScreenHelp {
					t.Fatalf("screen = %s, want help", nm.screen)
				}
				return nm
			},
		},
		{
			name: "recovery",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenRecovery
				return m
			},
		},
	}
}

// TestEveryReachableStateHasUniqueBindings drives every reachable screen state
// through the invariant: no key is claimed by two actions in the set the
// screen draws and dispatches from.
func TestEveryReachableStateHasUniqueBindings(t *testing.T) {
	for _, screen := range productScreens {
		t.Run(string(screen), func(t *testing.T) {
			m := actionModelFor(t, screen)
			if err := m.actionsFor(screen).ValidateUniqueBindings(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range bindingStateCases() {
		t.Run(tc.name, func(t *testing.T) {
			set, _ := tc.actions(t)
			if err := set.ValidateUniqueBindings(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestEscapeMeansOneThingPerState pins the audit's concrete instance: Escape
// appears at most once in every footer, under one label.
func TestEscapeMeansOneThingPerState(t *testing.T) {
	for _, tc := range bindingStateCases() {
		t.Run(tc.name, func(t *testing.T) {
			set, _ := tc.actions(t)
			escapes := 0
			for _, action := range set.Advertised() {
				for _, key := range action.Keys {
					if key == "esc" {
						escapes++
					}
				}
			}
			if escapes > 1 {
				t.Fatalf("escape is advertised %d times on %s", escapes, tc.name)
			}
		})
	}
}

// TestNavigationYieldsClaimedKeys pins the composition rule itself: when a
// screen's own set binds a navigation key, the navigation entry for it is
// absent rather than advertised a second time.
func TestNavigationYieldsClaimedKeys(t *testing.T) {
	t.Run("provider setup owns escape, not back", func(t *testing.T) {
		m := setupNavFixture(t, ScreenHome, cloudflareNavSetupFlow())
		actions := m.actionsFor(ScreenProviderSetup)
		if _, ok := actions.FindByKey("esc"); !ok {
			t.Fatal("provider setup does not answer esc at all")
		}
		esc, _ := actions.FindByKey("esc")
		if esc.Label == "Back" && esc.Help != "" && !esc.Primary {
			t.Fatalf("navigation's generic Back survived the form's own escape: %+v", esc)
		}
	})
	t.Run("edit while typing owns escape as cancel value", func(t *testing.T) {
		m := readyModel(&fakeClient{}, twoConnectionSnapshot())
		m.screen = ScreenEdit
		m.edit = &editState{connectionID: "conn-a", typing: true}
		actions := m.actionsFor(ScreenEdit)
		esc, ok := actions.FindByKey("esc")
		if !ok {
			t.Fatal("edit typing does not answer esc")
		}
		if esc.Label != "Cancel value" {
			t.Fatalf("esc is %q, want the form's own Cancel value", esc.Label)
		}
	})
	t.Run("recovery keeps exactly one advertised quit", func(t *testing.T) {
		m := readyModel(&fakeClient{}, twoConnectionSnapshot())
		m.screen = ScreenRecovery
		// The ctrl+c alias is not a second advertisement: it carries no label
		// and is described once in Help. What must not duplicate is a labelled
		// "[q] Quit" beside recovery's own.
		quits := 0
		for _, action := range m.actionsFor(ScreenRecovery).Advertised() {
			if action.ID != ActionQuit {
				continue
			}
			quits++
		}
		if quits != 1 {
			t.Fatalf("recovery advertises %d quit actions, want 1", quits)
		}
	})
}
