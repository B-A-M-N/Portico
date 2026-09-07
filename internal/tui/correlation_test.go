package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/B-A-M-N/portico/internal/ipc"
)

func twoConnectionSnapshot() ipc.SnapshotDTO {
	snap := testSnapshot()
	snap.Connections = []ipc.ConnectionDTO{
		{ID: "conn-a", Name: "alpha", DesiredState: "closed", UserState: "Closed"},
		{ID: "conn-b", Name: "beta", DesiredState: "closed", UserState: "Closed"},
	}
	return snap
}

// TestAPlanNeverAppearsUnderAnotherConnectionsName pins the worst thing this
// interface could do.
//
// The plan request carried no identity, so a plan prepared for one connection
// that arrived after the cursor moved was previewed under the other's name —
// and the next keypress applied what the plan said, not what the screen said.
func TestAPlanNeverAppearsUnderAnotherConnectionsName(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.selectedID = "conn-a"

	// Ask for a plan for alpha, then move to beta before it arrives.
	cmd := m.planOpenCmd("conn-a")
	generation := m.planRequests.current
	m.selectedID = "conn-b"

	next, _ := m.Update(planLoadedMsg{
		Generation: generation, ConnectionID: "conn-a",
		Plan: &ipc.PlanDTO{ID: "plan-1", ConnectionID: "conn-a", Intent: "open"},
	})
	m = next.(Model)
	_ = cmd

	if m.screen != ScreenPlanPreview {
		t.Fatalf("screen = %q, want the preview", m.screen)
	}
	view := m.View().Content
	if !strings.Contains(view, "alpha") {
		t.Fatalf("the preview does not name the connection the plan is for:\n%s", view)
	}
	if strings.Contains(view, "beta") {
		t.Fatalf("the preview names the selected connection rather than the plan's:\n%s", view)
	}
}

// TestAPlanTheUserMovedOnFromIsDropped covers the abandoned request.
func TestAPlanTheUserMovedOnFromIsDropped(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.selectedID = "conn-a"
	m.planOpenCmd("conn-a")
	stale := m.planRequests.current

	// A second request supersedes the first.
	m.planOpenCmd("conn-b")

	next, _ := m.Update(planLoadedMsg{
		Generation: stale, ConnectionID: "conn-a",
		Plan: &ipc.PlanDTO{ID: "plan-1", ConnectionID: "conn-a", Intent: "delete"},
	})
	m = next.(Model)

	if m.screen == ScreenPlanPreview {
		t.Fatal("an abandoned plan opened a preview")
	}
	if m.plan != nil {
		t.Fatalf("an abandoned plan was installed: %#v", m.plan)
	}
}

// TestDiagnosticsNeverAttachToAnotherConnection pins the same defect in the
// diagnostics path, where it makes the repair flow reason from the wrong
// evidence.
func TestDiagnosticsNeverAttachToAnotherConnection(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.selectedID = "conn-a"
	m.diagnosticsCmd("conn-a")
	stale := m.diagnosticsRequests.current

	m.selectedID = "conn-b"
	m.diagnosticsCmd("conn-b")

	next, _ := m.Update(diagnosticsMsg{
		Generation: stale, ConnectionID: "conn-a",
		Findings: []ipc.DiagnosticDTO{{ID: "f1", Summary: "alpha's problem"}},
	})
	m = next.(Model)

	for _, d := range m.diagnostics {
		if d.Summary == "alpha's problem" {
			t.Fatal("one connection's findings were shown against another")
		}
	}
}

// TestAFailedDiagnosticIsNotAnEmptyResult pins that a check which could not run
// is reported as such rather than as a connection with no problems.
func TestAFailedDiagnosticIsNotAnEmptyResult(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.selectedID = "conn-a"
	m.diagnosticsCmd("conn-a")

	next, _ := m.Update(diagnosticsMsg{
		Generation: m.diagnosticsRequests.current, ConnectionID: "conn-a",
		Err: errors.New("supervisor unreachable"),
	})
	m = next.(Model)

	if m.diagnosticsFailed == nil {
		t.Fatal("a failed check was not recorded as a failure")
	}
	if len(m.diagnostics) != 0 {
		t.Fatal("stale findings were left on screen after a failed check")
	}
}

// TestRepairIsVerifiedByWhichFindingsChanged pins that counting findings cannot
// answer whether a repair worked.
func TestRepairIsVerifiedByWhichFindingsChanged(t *testing.T) {
	before := []ipc.DiagnosticDTO{{ID: "dns-missing", Segment: "dns", Summary: "DNS record missing"}}
	after := []ipc.DiagnosticDTO{{ID: "connector-down", Segment: "connector", Summary: "Connector exited"}}

	resolved, remaining, appeared := compareFindings(before, after)

	if len(resolved) != 1 || resolved[0].ID != "dns-missing" {
		t.Fatalf("resolved = %#v, want the DNS finding", resolved)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining = %#v, want none", remaining)
	}
	if len(appeared) != 1 || appeared[0].ID != "connector-down" {
		t.Fatalf("appeared = %#v, want the connector finding", appeared)
	}
	// Counting would have called this unchanged: one before, one after.
	if len(before) != len(after) {
		t.Fatal("the fixture no longer exercises equal counts")
	}
}

// TestAGuidanceFlowCollectsNothing pins that a screen saying Portico cannot
// store a credential does not then collect one.
func TestAGuidanceFlowCollectsNothing(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenProviderSetup
	m.providerSetupProviderID = "openai_tunnel"
	m.providerSetupFlow = &ipc.SetupFlowDTO{
		ProviderID: "openai_tunnel", Kind: "guidance",
		Fields: []ipc.SetupFieldDTO{{ID: "credential", Label: "Key", Secret: true}},
	}

	for _, key := range []string{"s", "e", "c", "r", "e", "t"} {
		next, _ := m.Update(keyMsg(key))
		m = next.(Model)
	}
	if len(m.providerSetupValues) != 0 {
		t.Fatalf("a guidance screen collected values: %#v", m.providerSetupValues)
	}

	next, cmd := m.Update(keyMsg("enter"))
	m = next.(Model)
	if cmd != nil {
		t.Fatal("a guidance screen submitted an account configuration")
	}
	if m.screen == ScreenProviderSetup {
		t.Fatalf("enter did not leave the guidance screen: screen=%s", m.screen)
	}
}

// TestACredentialIsNotSubmittedTwice pins that a second Enter does not send it
// again while the first is in flight.
func TestACredentialIsNotSubmittedTwice(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.providerSetupSubmitting = true
	m.providerSetupProviderID = "cloudflare"
	m.providerSetupFlow = &ipc.SetupFlowDTO{
		ProviderID: "cloudflare", Kind: "account",
		Fields: []ipc.SetupFieldDTO{{ID: "account_id", Label: "Account"}},
	}
	m.screen = ScreenProviderSetup
	m.providerSetupIndex = 1 // the confirmation step

	_, cmd := m.Update(keyMsg("enter"))
	if cmd != nil {
		t.Fatal("a second enter sent the credential again while the first was in flight")
	}
}

// TestACredentialCanBePasted pins audit finding 16 on the field it matters most
// for. An API token is the value a user is most likely to have on the clipboard
// and least likely to retype correctly, and paste did not reach the form at all.
func TestACredentialCanBePasted(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenProviderSetup
	m.providerSetupProviderID = "cloudflare"
	m.providerSetupFlow = &ipc.SetupFlowDTO{
		ProviderID: "cloudflare", Kind: "account",
		Fields: []ipc.SetupFieldDTO{{ID: "api_token", Label: "API token", Secret: true, Required: true}},
	}
	m.providerSetupIndex = 0
	m.focusProviderSetupField()

	next, _ := m.Update(tea.PasteMsg{Content: "cf-token-pasted"})
	m = next.(Model)

	if got := m.providerSetupValue("api_token"); got != "cf-token-pasted" {
		t.Fatalf("pasted credential = %q, want it accepted", got)
	}
}

// TestQuittingOutranksTyping pins the ordering defect that routing text entry
// first introduced: the field consumed ctrl+c and appended the literal string
// "ctrl+c" to the credential being typed, instead of shutting down.
func TestQuittingOutranksTyping(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenProviderSetup
	m.providerSetupProviderID = "cloudflare"
	m.providerSetupFlow = &ipc.SetupFlowDTO{
		ProviderID: "cloudflare", Kind: "account",
		Fields: []ipc.SetupFieldDTO{{ID: "api_token", Label: "API token", Secret: true}},
	}
	m.providerSetupIndex = 0
	m.focusProviderSetupField()
	m.setProviderSetupValue("api_token", "cf-token-secret")

	next, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = next.(Model)

	if cmd == nil {
		t.Fatal("ctrl+c did not quit")
	}
	if got := m.providerSetupValue("api_token"); strings.Contains(got, "ctrl") {
		t.Fatalf("ctrl+c was typed into the credential: %q", got)
	}
	if m.providerSetupValue("api_token") != "" {
		t.Fatalf("the credential survived shutdown: %q", m.providerSetupValue("api_token"))
	}
}

// TestAGuidanceFlowStillCollectsNothingWhenPasted covers the read-only guard on
// the paste path as well as the key path.
func TestAGuidanceFlowStillCollectsNothingWhenPasted(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenProviderSetup
	m.providerSetupProviderID = "openai_tunnel"
	m.providerSetupFlow = &ipc.SetupFlowDTO{
		ProviderID: "openai_tunnel", Kind: "guidance",
		Fields: []ipc.SetupFieldDTO{{ID: "credential", Label: "Key", Secret: true}},
	}
	m.providerSetupIndex = 0

	next, _ := m.Update(tea.PasteMsg{Content: "secret-key"})
	m = next.(Model)

	if len(m.providerSetupValues) != 0 {
		t.Fatalf("a guidance screen accepted a paste: %#v", m.providerSetupValues)
	}
}
