package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// setupHelpFlowFixture is a declaring provider's flow with a help URL.
func setupHelpFlowFixture() *ipc.SetupFlowDTO {
	return &ipc.SetupFlowDTO{
		ProviderID: "cloudflare", Kind: "account",
		Summary:     "Configure a Cloudflare account.",
		HelpURL:     "https://dash.cloudflare.com/profile/api-tokens",
		SecretField: "credential",
		Fields: []ipc.SetupFieldDTO{
			{ID: "credential", Label: "API token", Secret: true, Required: true},
		},
	}
}

// TestProviderSetupRendersDeclaredHelpURL pins that the help address the
// provider declares is offered on the setup screen, with the h key advertised.
// A URL the user is never shown cannot help them.
func TestProviderSetupRendersDeclaredHelpURL(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.screen = ScreenProviders
	m.providerSetupProviderID = "cloudflare"
	m.providerSetupFlow = setupHelpFlowFixture()

	view := m.renderProviderSetup()
	if !strings.Contains(view, "https://dash.cloudflare.com/profile/api-tokens") {
		t.Errorf("the declared help URL is not rendered:\n%s", view)
	}
	if !strings.Contains(view, "h") {
		t.Errorf("the help key is not advertised:\n%s", view)
	}
}

// TestProviderSetupWithoutHelpURLOffersNothing pins that a provider which
// declares no help URL gets no help hint — the screen must not invent one.
func TestProviderSetupWithoutHelpURLOffersNothing(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.screen = ScreenProviders
	m.providerSetupProviderID = "portforward"
	flow := setupHelpFlowFixture()
	flow.HelpURL = ""
	m.providerSetupFlow = flow

	view := m.renderProviderSetup()
	if strings.Contains(view, "dash.cloudflare.com") {
		t.Errorf("a provider without a help URL rendered one anyway:\n%s", view)
	}
}

// TestOpenSetupHelpLaunchesBrowserAndReportsFallback pins the two help
// outcomes through the injected launcher: a successful launch says so, and a
// failed one prints the address instead of leaving the keystroke silent.
func TestOpenSetupHelpLaunchesBrowserAndReportsFallback(t *testing.T) {
	previous := browserLauncher
	t.Cleanup(func() { browserLauncher = previous })

	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.providerSetupProviderID = "cloudflare"
	m.providerSetupFlow = setupHelpFlowFixture()

	browserLauncher = func(string) bool { return true }
	m.applySetupHelpLaunched(setupHelpLaunchedMsg{
		URL: m.providerSetupFlow.HelpURL, Opened: true,
	})
	if !strings.Contains(m.setupHelpStatus, "Opened") {
		t.Errorf("a successful launch is not reported: %q", m.setupHelpStatus)
	}

	browserLauncher = func(string) bool { return false }
	m.applySetupHelpLaunched(setupHelpLaunchedMsg{
		URL: m.providerSetupFlow.HelpURL, Opened: false,
	})
	if !strings.Contains(m.setupHelpStatus, "No browser available") ||
		!strings.Contains(m.setupHelpStatus, m.providerSetupFlow.HelpURL) {
		t.Errorf("a failed launch does not print the URL: %q", m.setupHelpStatus)
	}
}

// TestHKeyOnSetupScreenTriggersHelp pins that pressing h on the setup screen
// produces the help command when a URL is declared, and nothing when not.
func TestHKeyOnSetupScreenTriggersHelp(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.screen = ScreenProviders
	m.providerSetupProviderID = "cloudflare"
	m.providerSetupFlow = setupHelpFlowFixture()

	// The field step is where a user looks for the credential instructions.
	if _, cmd := m.handleProviderSetupKey("h"); cmd == nil {
		t.Error("h on the setup screen produced no help command")
	}

	flow := setupHelpFlowFixture()
	flow.HelpURL = ""
	m.providerSetupFlow = flow
	if _, cmd := m.handleProviderSetupKey("h"); cmd != nil {
		t.Error("h produced a help command with no URL declared")
	}
}
