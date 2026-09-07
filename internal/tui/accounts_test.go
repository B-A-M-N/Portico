package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

func accountSnapshot() ipc.SnapshotDTO {
	snap := testSnapshot()
	snap.Providers = []ipc.ProviderDTO{
		{
			ID: "cloudflare", DisplayName: "Cloudflare", Authenticated: true,
			Accounts: []ipc.ProviderAccountDTO{
				{ID: "acct-work", Label: "Work", Status: "authenticated"},
				{ID: "acct-personal", Label: "Personal", Status: "authenticated"},
			},
			PendingAccounts: []ipc.ProviderAccountDTO{
				{ID: "acct-stale", Label: "Old token", Status: "unverified"},
			},
		},
	}
	return snap
}

// TestAnAccountCanBeSelectedAndRemoved pins audit finding 10 at the surface a
// user actually touches.
//
// The supervisor implemented removal and refused to strand connections. Nothing
// called it: the providers screen listed accounts with no cursor and offered no
// action, so a revoked credential stayed listed as a working account.
func TestAnAccountCanBeSelectedAndRemoved(t *testing.T) {
	client := &fakeClient{}
	m := readyModel(client, accountSnapshot())
	m.screen = ScreenProviders

	// Move down past the provider header to the first account, then to the second.
	next, _ := m.Update(keyMsg("down"))
	m = next.(Model)
	next, _ = m.Update(keyMsg("down"))
	m = next.(Model)
	next, previewCmd := m.Update(keyMsg("x"))
	m = next.(Model)

	if m.screen != ScreenAccountRemoval {
		t.Fatalf("x did not open the confirmation: screen = %q", m.screen)
	}
	if previewCmd == nil {
		t.Fatal("opening the confirmation did not ask what the removal would do")
	}
	next, _ = m.Update(previewCmd())
	m = next.(Model)
	if m.accountRemovalTarget == nil || m.accountRemovalTarget.AccountID != "acct-personal" {
		t.Fatalf("the confirmation targets %#v, want acct-personal", m.accountRemovalTarget)
	}

	view := m.renderAccountRemoval()
	if !strings.Contains(view, "Personal") || !strings.Contains(view, "Cloudflare") {
		t.Fatalf("the confirmation does not say what is being removed:\n%s", view)
	}

	next, cmd := m.Update(keyMsg("enter"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter did not send the removal")
	}
	msg := cmd()

	next, _ = m.Update(msg)
	m = next.(Model)

	if client.removedAccount != "acct-personal" || client.removedProvider != "cloudflare" {
		t.Fatalf("removed provider %q account %q", client.removedProvider, client.removedAccount)
	}
	if m.screen == ScreenAccountRemoval {
		t.Fatal("the confirmation stayed open after a successful removal")
	}
}

// TestXOnProviderRowDoesNothing verifies the production regression: pressing
// [x] on a provider heading must not silently remove that provider's first
// account. Account removal is too destructive for implicit child selection.
func TestXOnProviderRowDoesNothing(t *testing.T) {
	client := &fakeClient{}
	m := readyModel(client, accountSnapshot())
	m.screen = ScreenProviders

	// Cursor is on the provider row (index 0) by default.
	row, ok := m.selectedScreenRow()
	if !ok {
		t.Fatal("no selected row")
	}
	if row.Kind != rowKindProvider {
		t.Fatalf("expected provider row, got %v", row.Kind)
	}

	// Press [x] — must NOT open account removal or issue a removal command.
	next, previewCmd := m.Update(keyMsg("x"))
	m = next.(Model)

	if m.screen == ScreenAccountRemoval {
		t.Fatal("x on provider row opened account removal screen")
	}
	if previewCmd != nil {
		t.Fatal("x on provider row issued a command")
	}
	if client.removedAccount != "" {
		t.Fatalf("x on provider row removed account %q", client.removedAccount)
	}
}

// TestARefusalNamesWhatMustChangeFirst pins that the connections still using an
// account are shown, not just counted. A user told "3 connections still use
// this" has to go and find them.
func TestARefusalNamesWhatMustChangeFirst(t *testing.T) {
	client := &fakeClient{
		removeAccountErr: errors.New("2 connection(s) still use this account"),
		removeAccountResponse: &ipc.RemoveProviderAccountResponse{
			Removed: false, DependentConnections: []string{"api-staging", "docs-site"},
		},
	}
	m := readyModel(client, accountSnapshot())
	m.screen = ScreenProviders

	// Navigate to the first account row (past the provider heading).
	next, _ := m.Update(keyMsg("down"))
	m = next.(Model)

	// Now press [x] to attempt removal of the account under the cursor.
	next, previewCmd := m.Update(keyMsg("x"))
	m = next.(Model)
	next, _ = m.Update(previewCmd())
	m = next.(Model)
	next, cmd := m.Update(keyMsg("enter"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)

	view := m.renderAccountRemoval()
	for _, name := range []string{"api-staging", "docs-site"} {
		if !strings.Contains(view, name) {
			t.Errorf("the refusal does not name %s:\n%s", name, view)
		}
	}
	if m.screen != ScreenAccountRemoval {
		t.Fatal("a refused removal left the screen, hiding the reason")
	}
}

// TestARefusedRemovalIsNotResent pins that pressing enter again on a refusal
// does not repeat a request whose answer will not change.
func TestARefusedRemovalIsNotResent(t *testing.T) {
	m := readyModel(&fakeClient{}, accountSnapshot())
	m.screen = ScreenAccountRemoval
	row := accountRow{ProviderID: "cloudflare", AccountID: "acct-work", Label: "Work"}
	m.accountRemovalTarget = &row
	m.accountRemovalPreview = &ipc.AccountRemovalPreviewDTO{Removable: true, Fingerprint: "fp"}
	m.accountRemovalError = "2 connection(s) still use this account"

	_, cmd := m.Update(keyMsg("enter"))
	if cmd != nil {
		t.Fatal("enter re-sent a removal that had already been refused")
	}
}

// TestAnUnverifiedAccountCanBeRemoved pins that the accounts most likely to
// need removing are selectable. An account whose credential was never confirmed
// is exactly the one a user wants to clear out.
func TestAnUnverifiedAccountCanBeRemoved(t *testing.T) {
	m := readyModel(&fakeClient{}, accountSnapshot())
	rows := m.accountRows()

	if len(rows) != 3 {
		t.Fatalf("accountRows returned %d rows, want 3", len(rows))
	}
	last := rows[len(rows)-1]
	if last.AccountID != "acct-stale" || !last.Pending {
		t.Fatalf("the unverified account is not selectable: %#v", last)
	}
}

// TestTheCursorStaysInsideTheList pins that removing the last account does not
// leave the cursor pointing past the end.
func TestTheCursorStaysInsideTheList(t *testing.T) {
	m := readyModel(&fakeClient{}, accountSnapshot())
	m.screen = ScreenProviders

	for i := 0; i < 10; i++ {
		m.moveAccountSelection(1)
	}
	if _, ok := m.selectedAccount(); !ok {
		t.Fatal("the cursor ran off the end of the list")
	}

	m.snapshot.Providers = nil
	m.moveAccountSelection(0)
	if _, ok := m.selectedAccount(); ok {
		t.Fatal("an empty list still reports a selected account")
	}
}

// TestALateRemovalReplyDoesNotReportAgainstAnotherAccount pins the correlation
// rule on this path too.
func TestALateRemovalReplyDoesNotReportAgainstAnotherAccount(t *testing.T) {
	m := readyModel(&fakeClient{}, accountSnapshot())
	m.screen = ScreenAccountRemoval
	row := accountRow{ProviderID: "cloudflare", AccountID: "acct-work", Label: "Work"}
	m.accountRemovalTarget = &row
	m.removeAccountCmd(row, "fp-test")
	stale := m.accountRequests.current

	// The user cancels and starts another.
	m.accountRequests.cancel()

	next, _ := m.Update(accountRemovedMsg{
		Generation: stale, ProviderID: "cloudflare", AccountID: "acct-work", Name: "Work",
		Err: errors.New("2 connection(s) still use this account"),
	})
	m = next.(Model)

	if m.accountRemovalError != "" {
		t.Fatalf("an abandoned removal reported against the screen: %q", m.accountRemovalError)
	}
}

// TestTheProvidersScreenSaysHowToRemoveAnAccount pins that the action is
// discoverable, since an action nobody can find is not reachable. The footer
// is the screen's action set, so this reads the action's own label rather than
// a hand-written string that could drift from what dispatch runs.
func TestTheProvidersScreenSaysHowToRemoveAnAccount(t *testing.T) {
	m := readyModel(&fakeClient{}, accountSnapshot())
	m.screen = ScreenProviders
	// Removal targets an account; move the cursor off the provider row onto
	// one, which is the state in which the action must be offered.
	next, _ := m.Update(keyMsg("down"))
	m = next.(Model)

	action, ok := m.actionsFor(ScreenProviders).Find(ActionRemoveAccount)
	if !ok {
		t.Fatal("the providers screen does not offer removal at all")
	}
	if !action.Enabled {
		t.Fatalf("removal is disabled with an account listed: %s", action.DisabledReason)
	}
	view := m.renderProviders()
	if !strings.Contains(view, action.Label) {
		t.Fatalf("the providers screen does not advertise %q:\n%s", action.Label, view)
	}
}

// TestAddingAnAccountUsesTheSelectedProvider pins that provider management is
// provider-neutral.
//
// The providers screen had no provider selection, so adding an account fell
// back to Cloudflare by name — in a codebase whose entire setup mechanism is
// declarative and knows nothing about any specific provider.
func TestAddingAnAccountUsesTheSelectedProvider(t *testing.T) {
	snap := testSnapshot()
	snap.Providers = []ipc.ProviderDTO{
		{ID: "ngrok", DisplayName: "ngrok", Accounts: []ipc.ProviderAccountDTO{
			{ID: "ngrok-default", Label: "Default", Status: "authenticated"},
		}},
		{ID: "cloudflare", DisplayName: "Cloudflare", Accounts: []ipc.ProviderAccountDTO{
			{ID: "cf-work", Label: "Work", Status: "authenticated"},
		}},
	}
	m := readyModel(&fakeClient{}, snap)
	m.transitionTo(ScreenProviders)

	// The cursor starts on ngrok's provider row.
	if got := m.selectedProviderID(); got != "ngrok" {
		t.Fatalf("selected provider = %q, want ngrok", got)
	}

	// Moving down past ngrok's account lands on Cloudflare's provider row.
	next, _ := m.Update(keyMsg("down"))
	m = next.(Model)
	next, _ = m.Update(keyMsg("down"))
	m = next.(Model)
	if got := m.selectedProviderID(); got != "cloudflare" {
		t.Fatalf("after moving, selected provider = %q, want cloudflare", got)
	}
}

// TestTheSelectedAccountIsVisible pins that the screen shows which account an
// action will apply to. It offered "↑↓ select account" and drew no marker, so
// the target was invisible until the confirmation appeared.
func TestTheSelectedAccountIsVisible(t *testing.T) {
	m := readyModel(&fakeClient{}, accountSnapshot())
	// newModel derives useASCII from TERM; pin it so the test asserts the
	// Unicode rendering regardless of the shell that invoked `go test`.
	m.useASCII = false
	m.transitionTo(ScreenProviders)

	view := m.renderProviders()
	if !strings.Contains(view, "▸") {
		t.Fatalf("no account is marked as selected:\n%s", view)
	}

	next, _ := m.Update(keyMsg("down"))
	m = next.(Model)
	moved := m.renderProviders()
	if moved == view {
		t.Fatal("moving the cursor did not change what is marked")
	}
}

// TestProviderMarkerSurvivesPrecedingAccounts pins the coordinate mismatch the
// renderer had: it compared the flattened selection index against a separate
// provider-only counter, so a provider preceded by another provider's accounts
// sat at two different positions and its cursor could never be drawn. The
// flattened row list is now the sole authority for both moving and marking.
func TestProviderMarkerSurvivesPrecedingAccounts(t *testing.T) {
	snap := testSnapshot()
	snap.Providers = []ipc.ProviderDTO{
		{
			ID: "provider-a", DisplayName: "Provider A", Authenticated: true,
			Accounts: []ipc.ProviderAccountDTO{
				{ID: "acct-a1", Label: "A1", Status: "authenticated"},
				{ID: "acct-a2", Label: "A2", Status: "authenticated"},
			},
		},
		{
			ID: "provider-b", DisplayName: "Provider B", Authenticated: true,
		},
	}
	m := readyModel(&fakeClient{}, snap)
	m.useASCII = false
	m.transitionTo(ScreenProviders)

	// Cursor starts on Provider A (flat index 0).
	if view := m.renderProviders(); !strings.Contains(view, "▸ Provider A") {
		t.Fatalf("initial cursor missing from Provider A:\\n%s", view)
	}

	// Walk to Provider B: down past A1, A2, then onto B's row (flat index 3).
	for range 3 {
		next, _ := m.Update(keyMsg("down"))
		m = next.(Model)
	}
	view := m.renderProviders()
	if !strings.Contains(view, "▸ Provider B") {
		t.Fatalf("Provider B marker vanished when preceded by Provider A's accounts:\\n%s", view)
	}
	if strings.Contains(view, "▸ Provider A") {
		t.Fatalf("Provider A kept the marker after the cursor moved to B:\\n%s", view)
	}
}

// TestProviderSectionRendersExactlyOnce pins the N+1 regression the 14fcaaf
// renderer had: the rowKindProvider guard protected only the header, so the
// provider detail and its account list were re-emitted for every account row
// of that provider. A provider with three accounts drew its accounts four
// times. Marker tests passed anyway because they assert Contains, not counts —
// this test counts.
func TestProviderSectionRendersExactlyOnce(t *testing.T) {
	snap := accountSnapshot()
	// Capabilities are declared so the renderer's capability block is also
	// under the once-only contract.
	snap.Providers[0].Capabilities = &ipc.CapabilitySetDTO{
		TemporaryAddresses: true,
		CustomHostnames:    true,
		ProtectionModes:    []string{"email_otp"},
	}
	m := readyModel(&fakeClient{}, snap)
	m.useASCII = false
	m.transitionTo(ScreenProviders)

	view := m.renderProviders()
	// Cloudflare has 2 configured + 1 pending account: each must appear once.
	if got := strings.Count(view, "Work — authenticated"); got != 1 {
		t.Fatalf("account row rendered %d times, want 1:\n%s", got, view)
	}
	if got := strings.Count(view, "Personal — authenticated"); got != 1 {
		t.Fatalf("account row rendered %d times, want 1:\n%s", got, view)
	}
	if got := strings.Count(view, "Old token — unverified"); got != 1 {
		t.Fatalf("pending account row rendered %d times, want 1:\n%s", got, view)
	}
	if got := strings.Count(view, "Permanent hostnames"); got != 1 {
		t.Fatalf("capability line rendered %d times, want 1:\n%s", got, view)
	}
}

// TestProviderRowSelectsNoAccount pins the deliberate safety rule: the cursor
// can rest on a provider row, but that must never imply an account is
// selected — destructive account actions would otherwise target a child the
// user never chose. The action set is the one description: on a provider row
// every account action is disabled with that as its reason, so the footer
// drawn from it cannot claim an account is selected.
func TestProviderRowSelectsNoAccount(t *testing.T) {
	m := readyModel(&fakeClient{}, accountSnapshot())
	m.useASCII = false
	m.transitionTo(ScreenProviders)

	// The initial cursor sits on the provider row.
	if _, ok := m.selectedAccount(); ok {
		t.Fatal("a provider row must not select an account")
	}
	actions := m.actionsFor(ScreenProviders)
	for _, id := range []ActionID{ActionVerifyAccount, ActionReplaceCredential, ActionRemoveAccount} {
		action, ok := actions.Find(id)
		if !ok {
			t.Fatalf("the providers screen does not offer %s", id)
		}
		if action.Enabled {
			t.Fatalf("%s is enabled on a provider row, which selects no account", id)
		}
		if action.DisabledReason == "" {
			t.Fatalf("%s is disabled with no reason, so a dimmed action is a mystery", id)
		}
	}

	// Moving onto an account row selects it — and the same actions must become
	// enabled, so the bar reflects the selection it draws.
	for range 1 {
		next, _ := m.Update(keyMsg("down"))
		m = next.(Model)
	}
	if _, ok := m.selectedAccount(); !ok {
		t.Fatal("an account row must select that account")
	}
	actions = m.actionsFor(ScreenProviders)
	for _, id := range []ActionID{ActionVerifyAccount, ActionReplaceCredential, ActionRemoveAccount} {
		if action, ok := actions.Find(id); ok && !action.Enabled {
			t.Fatalf("%s is still disabled on an account row: %s", id, action.DisabledReason)
		}
	}
}

// TestProviderMarkerSurvivesInASCIITerminal pins the ASCII rendering path:
// TERM=dumb flips newModel to useASCII, so a test that hardcodes ▸ is
// environment-dependent. The ASCII marker is `>`; it must behave identically.
func TestProviderMarkerSurvivesInASCIITerminal(t *testing.T) {
	m := readyModel(&fakeClient{}, accountSnapshot())
	m.useASCII = true
	m.transitionTo(ScreenProviders)

	if view := m.renderProviders(); !strings.Contains(view, "> Cloudflare") {
		t.Fatalf("ASCII mode must draw the provider cursor:\n%s", view)
	}
	next, _ := m.Update(keyMsg("down"))
	m = next.(Model)
	if _, ok := m.selectedAccount(); !ok {
		t.Fatal("ASCII mode must still move the cursor onto accounts")
	}
}
