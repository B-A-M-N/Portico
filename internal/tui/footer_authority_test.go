package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The rendered footer is the screen's action set, not a hand-written line
// beside it.
//
// The Providers screen ran Verify and Replace credential while a hardcoded
// footer advertised three of its six actions; Repair, Discovery, the operation
// progress screen, the removal form, Clone and provider setup each carried
// their own copy of "what you can press" that had already dropped actions the
// screen executes. These tests pin the property that replaces those defects:
// for every screen and every state it renders, the bar drawn at the bottom is
// exactly what actionsFor draws, so an action that runs is one that was
// advertised and an advertised key is one that runs.

// footerAuthorityCase pairs a screen with a model builder that puts that
// screen into a state worth checking — plain, or with findings, an account
// under the cursor, an operation in flight. The footer must match the action
// set in every one of them, because each is a state where the set's shape
// differs.
type footerAuthorityCase struct {
	name string
	// model builds the model sitting on the screen in the state under test.
	model func(t *testing.T) Model
	// screen is the screen the model sits on, whose action set is compared.
	screen ScreenID
	// render draws the screen.
	render func(m Model) string
}

func footerAuthorityCases() []footerAuthorityCase {
	renderProviders := func(m Model) string { return m.renderProviders() }
	renderRepair := func(m Model) string { return m.renderRepair() }
	renderDiscovery := func(m Model) string { return m.renderDiscovery() }
	return []footerAuthorityCase{
		{
			name: "providers, provider row",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, accountSnapshot())
				m.screen = ScreenProviders
				return m
			},
			screen: ScreenProviders,
			render: renderProviders,
		},
		{
			name: "providers, account row",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, accountSnapshot())
				m.screen = ScreenProviders
				// On an account row the actions whose enablement depends on the
				// selection — Verify, Replace, Remove — are exactly the ones the
				// hardcoded copy got wrong.
				next, _ := m.Update(keyMsg("down"))
				nm := next.(Model)
				return nm
			},
			screen: ScreenProviders,
			render: renderProviders,
		},
		{
			name: "repair, no findings",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenRepair
				return m
			},
			screen: ScreenRepair,
			render: renderRepair,
		},
		{
			name: "repair, findings present",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenRepair
				// The preview action becomes enabled here; this is the state
				// whose hand-written line named it and the others dropped it.
				m.diagnostics = []ipc.DiagnosticDTO{{
					Segment: "origin", Severity: "error", Summary: "nothing is listening",
				}}
				return m
			},
			screen: ScreenRepair,
			render: renderRepair,
		},
		{
			name: "repair, verification view",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenRepair
				// The post-repair comparison view is the third shape the screen
				// renders, and had its own hand-written bar.
				m.preRepairDiagnostics = []ipc.DiagnosticDTO{{
					Segment: "origin", Severity: "error", Summary: "nothing is listening",
				}}
				m.repairConnectionID = "conn-a"
				m.diagnostics = nil
				return m
			},
			screen: ScreenRepair,
			render: renderRepair,
		},
		{
			name: "discovery, empty",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenDiscovery
				return m
			},
			screen: ScreenDiscovery,
			render: renderDiscovery,
		},
		{
			name: "discovery, results",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenDiscovery
				m.discovery = []ipc.DiscoveredServiceDTO{{
					Address: "127.0.0.1:4180", Protocol: "http",
					Confidence: "likely", Selectable: true,
				}}
				m.discoverySelected = 0
				return m
			},
			screen: ScreenDiscovery,
			render: renderDiscovery,
		},
		{
			name: "operation progress, nothing running",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenOperationProgress
				return m
			},
			screen: ScreenOperationProgress,
			render: func(m Model) string { return m.renderOperationProgress() },
		},
		{
			name: "operation progress, running",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenOperationProgress
				m.operation = &ipc.OperationDTO{
					ID: "op-1", State: "running",
					Steps: []ipc.StepDTO{{Summary: "Create tunnel", State: "succeeded"}},
				}
				return m
			},
			screen: ScreenOperationProgress,
			render: func(m Model) string { return m.renderOperationProgress() },
		},
		{
			name: "inspect fallback",
			model: func(t *testing.T) Model {
				// No InspectModel: the fallback basic rendering, which had its
				// own hand-written line.
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.screen = ScreenInspect
				return m
			},
			screen: ScreenInspect,
			render: func(m Model) string { return m.renderInspect() },
		},
	}
}

// TestFooterIsRenderedFromTheActionSet pins that each screen's rendering ends
// with exactly the bar its action set draws.
func TestFooterIsRenderedFromTheActionSet(t *testing.T) {
	for _, tc := range footerAuthorityCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.model(t)

			want := m.actionsFor(tc.screen).footer(m.theme, m.width)
			if want == "" {
				t.Fatal("the action set drew no bar; the case proves nothing")
			}
			got := tc.render(m)
			if !strings.HasSuffix(strings.TrimRight(got, "\n"), strings.TrimRight(want, "\n")) {
				t.Fatalf("the rendered screen does not end with the action set's footer\n--- action set draws ---\n%s\n--- screen drew ---\n%s",
					want, got)
			}
		})
	}
}

// TestProviderSetupFooterMatchesItsStepActions covers the setup screen: its
// action set changes per step, so each step's rendering is checked against that
// step's own set.
func TestProviderSetupFooterMatchesItsStepActions(t *testing.T) {
	steps := []struct {
		name string
		to   func(m *Model)
	}{
		{"first field", func(m *Model) {}},
		{"confirmation", func(m *Model) {
			m.providerSetupIndex = len(m.providerSetupFields())
		}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			m := setupNavFixture(t, ScreenHome, cloudflareNavSetupFlow())
			step.to(&m)

			want := m.actionsFor(ScreenProviderSetup).footer(m.theme, m.width)
			if want == "" {
				t.Fatal("the action set drew no bar")
			}
			got := m.renderProviderSetup()
			if !strings.HasSuffix(strings.TrimRight(got, "\n"), strings.TrimRight(want, "\n")) {
				t.Fatalf("the rendered form does not end with the action set's footer\n--- action set draws ---\n%s\n--- screen drew ---\n%s",
					want, got)
			}
		})
	}
}

// TestEarlyReturnFootersComeFromTheActionSet covers the states whose rendering
// returns early: a guidance flow, clone while loading, clone in error, and a
// refused or blocked account removal. An early return is where a hardcoded
// line survives a cleanup that only fixed the main path.
func TestEarlyReturnFootersComeFromTheActionSet(t *testing.T) {
	cases := []struct {
		name   string
		model  func(t *testing.T) Model
		render func(m Model) string
		screen ScreenID
	}{
		{
			name: "guidance setup",
			model: func(t *testing.T) Model {
				guidance := &ipc.SetupFlowDTO{
					ProviderID: "tailscale", Kind: "guidance",
					Summary:        "Tailscale is configured in its own admin console.",
					GuidanceReason: "Portico holds no Tailscale credential.",
					Fields:         []ipc.SetupFieldDTO{{ID: "credential", Label: "Auth key", Secret: true}},
				}
				return setupNavFixture(t, ScreenProviders, guidance)
			},
			render: func(m Model) string { return m.renderProviderSetup() },
			screen: ScreenProviderSetup,
		},
		{
			name: "clone while loading",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.clone = &cloneState{sourceID: "conn-a", sourceName: "one"}
				m.screen = ScreenClone
				return m
			},
			render: func(m Model) string { return m.renderClone() },
			screen: ScreenClone,
		},
		{
			name: "clone with an error",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, twoConnectionSnapshot())
				m.clone = &cloneState{sourceID: "conn-a", sourceName: "one", err: "the supervisor refused"}
				m.screen = ScreenClone
				return m
			},
			render: func(m Model) string { return m.renderClone() },
			screen: ScreenClone,
		},
		{
			name: "account removal refused",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, accountSnapshot())
				m.screen = ScreenAccountRemoval
				row := accountRow{ProviderID: "cloudflare", AccountID: "acct-work", Label: "Work"}
				m.accountRemovalTarget = &row
				m.accountRemovalError = "2 connection(s) still use this account"
				return m
			},
			render: func(m Model) string { return m.renderAccountRemoval() },
			screen: ScreenAccountRemoval,
		},
		{
			name: "account removal not removable",
			model: func(t *testing.T) Model {
				m := readyModel(&fakeClient{}, accountSnapshot())
				m.screen = ScreenAccountRemoval
				row := accountRow{ProviderID: "cloudflare", AccountID: "acct-work", Label: "Work"}
				m.accountRemovalTarget = &row
				m.accountRemovalPreview = &ipc.AccountRemovalPreviewDTO{
					Removable:    false,
					Dependencies: []ipc.AccountDependencyDTO{{Name: "api-staging"}},
					Fingerprint:  "fp",
					Consequences: []string{"The stored credential is forgotten."},
				}
				return m
			},
			render: func(m Model) string { return m.renderAccountRemoval() },
			screen: ScreenAccountRemoval,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.model(t)
			want := m.actionsFor(tc.screen).footer(m.theme, m.width)
			if want == "" {
				t.Fatal("the action set drew no bar; the case proves nothing")
			}
			got := tc.render(m)
			if !strings.HasSuffix(strings.TrimRight(got, "\n"), strings.TrimRight(want, "\n")) {
				t.Fatalf("the rendered screen does not end with the action set's footer\n--- action set draws ---\n%s\n--- screen drew ---\n%s",
					want, got)
			}
		})
	}
}
