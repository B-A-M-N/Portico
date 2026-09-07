package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Provider setup is a screen, not a keyboard-precedence flag.
//
// The wizard used to advertise "[s] Set up this provider", arm the setup flow
// internally, and keep every subsequent key — and the whole rendered surface —
// on the wizard, because setup was a flag consulted before the current screen
// in key handling but absent from rendering. These tests pin the corrected
// contract: setup is ScreenProviderSetup, it renders the form, it owns keys
// while current, Help opened from it returns to it on the first Escape, and
// every exit path returns to the caller.

// setupNavFixture is a declaring account flow, entered from a given screen.
func setupNavFixture(t *testing.T, from ScreenID, flow *ipc.SetupFlowDTO) Model {
	t.Helper()
	m := readyModel(&fakeClient{setupFlow: flow}, twoConnectionSnapshot())
	m.screen = from
	m.navStack = nil
	next, cmd := m.beginProviderSetup(flow.ProviderID)
	m = next
	if cmd == nil {
		t.Fatal("beginning setup issued no command to load the flow")
	}
	delivered, _ := m.Update(cmd())
	m = delivered.(Model)
	if m.screen != ScreenProviderSetup {
		t.Fatalf("screen = %s, want %s", m.screen, ScreenProviderSetup)
	}
	return m
}

func cloudflareNavSetupFlow() *ipc.SetupFlowDTO {
	return &ipc.SetupFlowDTO{
		ProviderID: "cloudflare", Kind: "account",
		Summary:     "Configure a Cloudflare account.",
		HelpURL:     "https://dash.cloudflare.com/profile/api-tokens",
		SecretField: "credential",
		Fields: []ipc.SetupFieldDTO{
			{ID: "account_id", Label: "Account ID", Required: true},
			{ID: "label", Label: "Label"},
			{ID: "zone_id", Label: "Zone ID"},
			{ID: "credential", Label: "API token", Secret: true, Required: true},
		},
	}
}

// TestWizardSetupRequestShowsTheSetupScreen pins the P0 defect: the wizard's
// setup request must hand the rendered surface and the keyboard to provider
// setup, not arm the flow while leaving the user looking at the wizard.
func TestWizardSetupRequestShowsTheSetupScreen(t *testing.T) {
	m := startSetupWithFlow(t, cloudflareNavSetupFlow())
	// Simulate the wizard having asked from the wizard screen.
	m.screen = ScreenProviderSetup

	view := m.renderScreen()
	if !strings.Contains(view, "SET UP CLOUDFLARE") {
		t.Fatalf("the wizard screen is rendered while setup is armed:\n%s", view)
	}
	if !strings.Contains(view, "Account ID") {
		t.Fatalf("the declared form is not rendered:\n%s", view)
	}

	// The keyboard belongs to the form: a printable key must reach the focused
	// field, not the wizard.
	for _, r := range "acct-7" {
		next, _ := m.Update(keyMsg(string(r)))
		m = next.(Model)
	}
	if got := m.providerSetupValue("account_id"); got != "acct-7" {
		t.Fatalf("typed input did not reach the setup field: %q", got)
	}
	if m.wizard != nil {
		t.Fatal("a wizard model exists without the wizard having been entered")
	}
}

// TestWizardSetupHandoffPreservesWizardAndReturns pins the vertical handoff:
// setup entered from a live wizard renders setup as the current screen, and
// cancelling returns to the wizard with its answers intact.
func TestWizardSetupHandoffPreservesWizardAndReturns(t *testing.T) {
	m := readyModel(&fakeClient{setupFlow: cloudflareNavSetupFlow()}, twoConnectionSnapshot())
	m.width = 100
	m, _ = press(t, m, "n")
	m, _ = press(t, m, "enter")
	for _, key := range []string{"w", "e", "b"} {
		m, _ = press(t, m, key)
	}
	m, _ = press(t, m, "enter")

	// The wizard records the request; the root model performs it.
	m.wizard.RequestProviderSetup("cloudflare")
	next, setupCmd, started := m.wizardProviderSetupCmd()
	m = next
	if !started || setupCmd == nil {
		t.Fatal("the wizard's setup request was not picked up")
	}
	if m.screen != ScreenProviderSetup {
		t.Fatalf("setup did not become the current screen: %s", m.screen)
	}
	if m.wizard == nil {
		t.Fatal("entering setup tore down the wizard")
	}
	if got := m.wizard.NameAnswer(); got != "web" {
		t.Fatalf("the wizard's answer did not survive entering setup: %q", got)
	}

	// Cancel at the first field: esc leaves setup, back to the wizard.
	delivered, _ := m.Update(keyMsg("esc"))
	m = delivered.(Model)
	if m.screen != ScreenNewConnection {
		t.Fatalf("cancelling landed on %s, want the wizard screen:\n%s",
			m.screen, m.renderScreen())
	}
	if m.providerSetupProviderID != "" {
		t.Fatalf("cancelling kept the setup provider: %q", m.providerSetupProviderID)
	}
	if m.wizard == nil {
		t.Fatal("cancelling tore down the wizard")
	}
	if got := m.wizard.NameAnswer(); got != "web" {
		t.Fatalf("the wizard's answer did not survive cancelling setup: %q", got)
	}
}

// TestHelpFromProviderSetupReturnsOnOneEscape pins the P0-2 defect: Help
// opened from setup must return to setup on the first Escape, with the form
// state untouched. Under the old flag model the Escape was intercepted by the
// setup handler and tore the form down behind Help.
func TestHelpFromProviderSetupReturnsOnOneEscape(t *testing.T) {
	m := setupNavFixture(t, ScreenProviders, cloudflareNavSetupFlow())
	// Type into the first field so "state unchanged" is observable.
	for _, r := range "acct-9" {
		next, _ := m.Update(keyMsg(string(r)))
		m = next.(Model)
	}

	// ? opens Help from the setup screen.
	next, _ := m.Update(keyMsg("?"))
	m = next.(Model)
	if m.screen != ScreenHelp {
		t.Fatalf("? from setup landed on %s, want Help", m.screen)
	}

	// One Escape returns to setup — not further down the stack.
	next, _ = m.Update(keyMsg("esc"))
	m = next.(Model)
	if m.screen != ScreenProviderSetup {
		t.Fatalf("one esc from Help landed on %s, want %s (form state: %+v)",
			m.screen, ScreenProviderSetup, m.providerSetupValues)
	}
	if got := m.providerSetupValue("account_id"); got != "acct-9" {
		t.Fatalf("form state did not survive the Help round trip: %q", got)
	}
	if m.providerSetupIndex != 0 {
		t.Fatalf("form position moved during the Help round trip: %d", m.providerSetupIndex)
	}

	// And Escape from setup now leaves it, back to the caller.
	next, _ = m.Update(keyMsg("esc"))
	m = next.(Model)
	if m.screen == ScreenProviderSetup {
		t.Fatal("esc after Help did not leave setup")
	}
	if m.screen != ScreenProviders {
		t.Fatalf("cancelling setup landed on %s, want the Providers caller", m.screen)
	}
}

// TestHelpFromGuidanceSetupReturnsOnOneEscape repeats the Help round trip for
// a guidance-only flow, where the old behaviour could silently discard the
// guide behind the Help screen.
func TestHelpFromGuidanceSetupReturnsOnOneEscape(t *testing.T) {
	guidance := &ipc.SetupFlowDTO{
		ProviderID: "tailscale", Kind: "guidance",
		Summary:        "Tailscale is configured in its own admin console.",
		GuidanceReason: "Portico holds no Tailscale credential.",
		Fields:         []ipc.SetupFieldDTO{{ID: "credential", Label: "Auth key", Secret: true}},
	}
	m := setupNavFixture(t, ScreenProviders, guidance)

	next, _ := m.Update(keyMsg("?"))
	m = next.(Model)
	if m.screen != ScreenHelp {
		t.Fatalf("? from guidance setup landed on %s, want Help", m.screen)
	}
	next, _ = m.Update(keyMsg("esc"))
	m = next.(Model)
	if m.screen != ScreenProviderSetup {
		t.Fatalf("one esc from Help landed on %s, want the guidance screen", m.screen)
	}
	if m.providerSetupProviderID != "tailscale" {
		t.Fatalf("the guide was discarded behind Help: provider=%q", m.providerSetupProviderID)
	}

	// enter or esc on a guidance screen closes it, back to the caller.
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)
	if m.screen == ScreenProviderSetup {
		t.Fatal("enter on a guidance screen did not close it")
	}
	if m.screen != ScreenProviders {
		t.Fatalf("closing the guide landed on %s, want the Providers caller", m.screen)
	}
}

// TestSetupEnteredFromSetupScreenReturnsThere pins the third entry point: a
// setup opened from the readiness screen returns there, not to Providers.
func TestSetupEnteredFromSetupScreenReturnsThere(t *testing.T) {
	m := setupNavFixture(t, ScreenSetup, cloudflareNavSetupFlow())
	next, _ := m.Update(keyMsg("esc"))
	m = next.(Model)
	if m.screen != ScreenSetup {
		t.Fatalf("cancelling setup landed on %s, want the Setup caller", m.screen)
	}
}

// TestFinishedSetupReturnsToCallerScreen pins the success path for a non-wizard
// entry: after the configured reply, the setup screen is closed and the caller
// is current.
func TestFinishedSetupReturnsToCallerScreen(t *testing.T) {
	m := setupNavFixture(t, ScreenProviders, cloudflareNavSetupFlow())

	m.finishProviderSetup()
	if m.screen != ScreenProviders {
		t.Fatalf("finishing setup landed on %s, want the Providers caller", m.screen)
	}
	if m.providerSetupProviderID != "" {
		t.Fatalf("finishing kept the form: provider=%q", m.providerSetupProviderID)
	}
}

// TestWizardSetupCancelConsumesResumePath pins that the wizard resume path is
// consumed when the form is abandoned, so a stale one cannot reroute a later
// exit.
func TestWizardSetupCancelConsumesResumePath(t *testing.T) {
	m := setupNavFixture(t, ScreenNewConnection, cloudflareNavSetupFlow())
	m.resumeWizardAfterSetup = "cloudflare"

	delivered, _ := m.Update(keyMsg("esc"))
	m = delivered.(Model)
	if m.screen != ScreenNewConnection {
		t.Fatalf("cancelling a wizard setup landed on %s, want the wizard", m.screen)
	}
	if m.resumeWizardAfterSetup != "" {
		t.Fatal("the resume path survived cancellation and could fire again")
	}
}

// TestSetupActionsDescribeTheForm pins the single-action-authority contract for
// the new screen: the action set the footer draws matches what the keyboard
// handles, for each form phase.
func TestSetupActionsDescribeTheForm(t *testing.T) {
	m := setupNavFixture(t, ScreenProviders, cloudflareNavSetupFlow())

	first := m.actionsFor(ScreenProviderSetup)
	if _, ok := first.Lookup("esc"); !ok {
		t.Errorf("the first field's action set does not advertise esc:\n%s", first.footer(DefaultTheme, 100))
	}

	// Reach the confirmation step.
	for _, r := range "acct-1" {
		next, _ := m.Update(keyMsg(string(r)))
		m = next.(Model)
	}
	for range 3 {
		next, _ := m.Update(keyMsg("enter"))
		m = next.(Model)
	}
	confirm := m.actionsFor(ScreenProviderSetup)
	if _, ok := confirm.Lookup("enter"); !ok {
		t.Errorf("the confirmation action set does not advertise enter:\n%s", confirm.footer(DefaultTheme, 100))
	}
	if _, ok := confirm.Lookup("esc"); !ok {
		t.Errorf("the confirmation action set does not advertise esc:\n%s", confirm.footer(DefaultTheme, 100))
	}
}
