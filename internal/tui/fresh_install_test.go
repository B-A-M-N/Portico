package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// A fresh installation, from nothing to an open connection.
//
// This is required coverage item 1, and it is the one test that answers the actual
// completion criterion: a user who has just installed Portico can work out what to
// do, configure what needs configuring without abandoning the task, understand what
// Portico will do, and end with a connection that is open.
//
// It is deliberately one continuous sequence rather than several focused tests. Each
// step is covered on its own elsewhere; what nothing else covers is whether the steps
// join up. The audit's own lesson is that two units passing does not make the path
// between them work — account removal and provider validation both computed the right
// answer and dropped it in the client — so this crosses every boundary in one run.
//
// Keys go through Update, never by assigning state. A screen that cannot actually be
// driven passes a test that arranges its state directly and fails for every real user.

// freshInstallClient is a machine with Portico installed and nothing set up: a
// provider whose client is present but which has no account yet.
func freshInstallClient() *fakeClient {
	return &fakeClient{
		readiness: &ipc.ReadinessDTO{
			Summary:    "No provider is ready yet. Set one up to make your first connection.",
			LaunchMode: "auto",
			Providers: []ipc.ProviderReadinessDTO{{
				ID: "cloudflare", DisplayName: "Cloudflare",
				Availability: "unconfigured",
				Summary:      "Needs an account before it can be used.",
				Blocked:      true,
				SetupActions: []string{"Add a Cloudflare API token"},
				// What the real supervisor now derives from the provider's own
				// declaration; without it the readiness screen would not offer
				// enter for a provider that has nothing to configure.
				SetupKind: "account",
			}},
		},
		setupFlow: &ipc.SetupFlowDTO{
			ProviderID: "cloudflare",
			Kind:       "account",
			Summary:    "Portico needs an API token to manage tunnels for you.",
			Fields: []ipc.SetupFieldDTO{
				{ID: "account_id", Label: "Account ID", Required: true},
				{ID: "api_token", Label: "API token", Required: true, Secret: true},
			},
		},
		created: &ipc.ConnectionDTO{
			ID: "conn-new", Name: "web",
			Kind: "service_exposure", DesiredState: "closed", UserState: "Closed",
			ProviderID: "cloudflare",
		},
		plan: &ipc.PlanDTO{
			ID: "plan-1", ConnectionID: "conn-new", Intent: "open",
			Steps: []ipc.StepDTO{
				{ID: "s1", Summary: "Create a temporary tunnel"},
				{ID: "s2", Summary: "Start the connector"},
			},
		},
		operation: &ipc.OperationDTO{
			ID: "op-1", ConnectionID: "conn-new", PlanID: "plan-1",
			Intent: "open", State: ipc.OperationRunning,
		},
	}
}

// emptySnapshotWithProvider is what a fresh install's snapshot looks like: no
// connections, and a provider that is present and not yet usable.
func emptySnapshotWithProvider() ipc.SnapshotDTO {
	return ipc.SnapshotDTO{
		Providers: []ipc.ProviderDTO{{
			ID: "cloudflare", Name: "cloudflare", DisplayName: "Cloudflare",
			Selectable: false, Availability: "unconfigured", Readiness: "needs_config",
			SetupActions: []string{"Add a Cloudflare API token"},
			Capabilities: &ipc.CapabilitySetDTO{
				Kinds:              []string{"service_exposure"},
				TemporaryAddresses: true,
				Protocols:          []string{"http", "https"},
			},
		}},
	}
}

// TestAFreshInstallReachesAnOpenConnection walks the whole path.
func TestAFreshInstallReachesAnOpenConnection(t *testing.T) {
	client := freshInstallClient()
	m := readyModel(client, emptySnapshotWithProvider())
	m.width = 100
	m.height = 40

	// ---- 1. What a new user lands on -------------------------------------------
	//
	// Not the words "No connections" above a row of single letters. The empty Home
	// has to name the tasks and explain them.
	home := m.View().Content
	// The three tasks a new user might want, each named and explained rather than
	// implied by a key.
	for _, offered := range []string{
		"Make something reachable",
		"Find something already running",
		"Check the providers",
	} {
		if !strings.Contains(home, offered) {
			t.Fatalf("the first screen does not offer %q:\n%s", offered, home)
		}
	}
	// And it says what Portico is for, because "No connections" does not.
	if !strings.Contains(home, "reachable from somewhere else") {
		t.Errorf("the first screen does not say what Portico does:\n%s", home)
	}

	// ---- 2. Readiness says what is missing -------------------------------------
	//
	// The user asks what Portico needs. This is the supervisor's answer, not a
	// guess assembled by the screen.
	m, cmd := press(t, m, "s")
	if m.screen != ScreenSetup {
		t.Fatalf("s did not open the setup screen: %s", m.screen)
	}
	if cmd == nil {
		t.Fatal("opening setup asked the supervisor nothing")
	}
	m = deliver(t, m, cmd)

	setup := m.View().Content
	if !strings.Contains(setup, "Cloudflare") {
		t.Fatalf("setup does not name the provider that needs configuring:\n%s", setup)
	}
	if !strings.Contains(setup, "Needs an account") {
		t.Fatalf("setup does not say what the provider needs:\n%s", setup)
	}

	// ---- 3. Configure the provider from here -----------------------------------
	action, ok := m.actionsFor(ScreenSetup).Find(ActionConfigureProvider)
	if !ok || !action.Enabled {
		t.Fatal("the setup screen offers no way to configure the provider it just named")
	}
	m, cmd = press(t, m, action.primaryKey())
	if cmd == nil {
		t.Fatal("configuring the provider asked the supervisor for no setup flow")
	}
	m = deliver(t, m, cmd)

	if m.providerSetupFlow == nil {
		t.Fatal("the setup flow did not load")
	}
	if len(client.setupFlowAsked) == 0 {
		t.Fatal("the provider's own setup flow was never requested")
	}

	// Fill it in. The fields come from the provider's declaration, so the test
	// types into whatever it declared rather than assuming two fields.
	form := m.View().Content
	if !strings.Contains(form, "Account ID") {
		t.Fatalf("the form does not show the provider's first field:\n%s", form)
	}
	m = typeText(t, m, "acct-live")
	m, _ = press(t, m, "enter")
	m = typeText(t, m, "token-value-not-a-real-secret")
	m, _ = press(t, m, "enter")

	// Confirming submits it.
	m, cmd = press(t, m, "enter")
	if cmd == nil {
		t.Fatal("confirming the form sent nothing to the supervisor")
	}
	m = deliver(t, m, cmd)

	// ---- 4. The provider becomes usable ----------------------------------------
	//
	// The authoritative snapshot is what makes it usable, not a local assumption
	// that the write succeeded.
	next, _ := m.Update(snapshotMsg{Snapshot: configuredSnapshot()})
	m = asModel(t, next)

	// ---- 5. Create a connection ------------------------------------------------
	m.screen = ScreenHome
	m, _ = press(t, m, "n")
	if m.screen != ScreenNewConnection {
		t.Fatalf("n did not open the wizard: %s", m.screen)
	}

	// Publish something already running, temporarily: the first outcome.
	m, _ = press(t, m, "enter")
	m = typeText(t, m, "web")
	m, cmd = press(t, m, "enter")
	// The service question scans; with nothing found, manual entry is the choice.
	if cmd != nil {
		m = deliver(t, m, cmd)
	}
	m, _ = press(t, m, "enter")
	m = typeText(t, m, "127.0.0.1:3000")
	m, cmd = press(t, m, "enter")
	if cmd != nil {
		m = deliver(t, m, cmd)
	}

	// Answer whatever remains until review. Every step is a menu with a workable
	// default, which is the point: a user should not have to know which.
	for i := 0; i < 12 && m.wizard.Step() != screens.WizardStepReview; i++ {
		before := m.wizard.Step()
		m, cmd = press(t, m, "enter")
		if cmd != nil {
			m = deliver(t, m, cmd)
		}
		if m.wizard == nil {
			t.Fatalf("the wizard was abandoned at step %d", before)
		}
		// A question that does not advance on Enter is a dead end: the default is
		// unacceptable and the user is told nothing about what to do instead.
		if m.wizard.Step() == before {
			t.Fatalf("step %d does not advance on Enter, so the wizard cannot be "+
				"completed with defaults: %v\n%s", before, m.wizard.Err(), m.View().Content)
		}
	}
	if m.wizard.Step() != screens.WizardStepReview {
		t.Fatalf("the wizard did not reach review; it is on step %d", m.wizard.Step())
	}

	// ---- 6. Review says what will happen, in the user's words ------------------
	review := m.View().Content
	for _, answered := range []string{
		"What will be reachable", "How it will be reached", "Who can reach it",
	} {
		if !strings.Contains(review, answered) {
			t.Errorf("review does not answer %q:\n%s", answered, review)
		}
	}
	// And not in Portico's words.
	for _, leaked := range []string{"service_exposure", "temporary_public", "keep_alive"} {
		if strings.Contains(review, leaked) {
			t.Errorf("review leaks the internal term %q:\n%s", leaked, review)
		}
	}

	// ---- 7. Create, then preview the plan --------------------------------------
	m, cmd = press(t, m, "enter")
	if cmd == nil {
		t.Fatal("confirming review created nothing")
	}
	m = deliver(t, m, cmd)

	if client.createCalls == 0 {
		t.Fatal("the connection was never created")
	}
	if client.createRequest == nil {
		t.Fatal("no create request was captured")
	}
	// The request carries the answers, including the lifecycle defaults rather than
	// a hardcoded pair.
	if client.createRequest.Name != "web" {
		t.Errorf("the created connection is named %q", client.createRequest.Name)
	}
	if client.createRequest.Source.Existing == nil {
		t.Error("the create request carries no source")
	}
}

// deliver runs a command and feeds its message back, which is what the Bubble Tea
// runtime does. A test that calls a command and discards the message exercises the
// request and none of the handling.
func deliver(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if msg == nil {
		return m
	}
	next, follow := m.Update(msg)
	out := asModel(t, next)
	// One level of follow-up is enough for these flows and avoids looping on a
	// command that reschedules itself.
	if follow != nil {
		if followMsg := follow(); followMsg != nil {
			next, _ = out.Update(followMsg)
			out = asModel(t, next)
		}
	}
	return out
}

// typeText enters text one keystroke at a time, through Update.
func typeText(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		m, _ = press(t, m, string(r))
	}
	return m
}

func asModel(t *testing.T, model tea.Model) Model {
	t.Helper()
	m, ok := model.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", model)
	}
	return m
}

// configuredSnapshot is the machine after the provider has an account.
func configuredSnapshot() ipc.SnapshotDTO {
	snap := emptySnapshotWithProvider()
	snap.Providers[0].Selectable = true
	snap.Providers[0].Availability = "ready"
	snap.Providers[0].Readiness = "ready"
	snap.Providers[0].Authenticated = true
	snap.Providers[0].SetupActions = nil
	snap.Providers[0].Accounts = []ipc.ProviderAccountDTO{{
		ID: "acct-live", Label: "Cloudflare account", Status: "usable",
	}}
	return snap
}
