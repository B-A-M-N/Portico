package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The first run, and the settings screen it points at.
//
// A fresh installation landed on the words "No connections" above a row of
// single letters. Everything Portico could do was reachable and none of it was
// offered, so the user had to already know that n starts a wizard and a scans
// for local services.
//
// ScreenSettings existed as a constant with no renderer and no way to reach it,
// while the only operational choice a user could make was buried on the
// readiness screen behind an undocumented l — and could not be persisted, so the
// interface had to tell them their choice would be forgotten.

// TestEmptyHomeOffersTheWaysIn pins that a fresh install proposes work.
func TestEmptyHomeOffersTheWaysIn(t *testing.T) {
	m := readyModel(&fakeClient{}, ipc.SnapshotDTO{})
	m.width = 100
	m.screen = ScreenHome

	view := m.View().Content
	if strings.Contains(view, "No connections") {
		t.Error("the empty home still reports an absence rather than offering a task")
	}

	// Each task is named, explained, and carries the key that starts it.
	tasks := m.emptyHomeTasks()
	if len(tasks) < 3 {
		t.Fatalf("the empty home offers %d tasks, want at least three", len(tasks))
	}
	for _, task := range tasks {
		if !strings.Contains(view, task.title) {
			t.Errorf("the empty home does not offer %q:\n%s", task.title, view)
		}
		if task.body == "" {
			t.Errorf("the task %q is offered with no explanation of what it does", task.title)
		}
	}

	// Creating and discovering are always among them: neither needs an account.
	offered := map[ActionID]bool{}
	for _, task := range tasks {
		offered[task.action] = true
	}
	for _, want := range []ActionID{ActionNew, ActionDiscover} {
		if !offered[want] {
			t.Errorf("the empty home does not offer %s", want)
		}
	}
}

// TestEmptyHomeDoesNotDemandSetupWhenSomethingWorks pins that setup is offered
// only when it is genuinely required.
//
// Portico can create a temporary public address with no account at all. Telling
// a new user to configure a provider first would be a gate the product does not
// have, and a new user told to do configuration before trying anything stops.
func TestEmptyHomeDoesNotDemandSetupWhenSomethingWorks(t *testing.T) {
	m := readyModel(&fakeClient{}, ipc.SnapshotDTO{})
	m.readiness = &ipc.ReadinessDTO{
		Providers: []ipc.ProviderReadinessDTO{
			{ID: "cloudflare", DisplayName: "Cloudflare", Blocked: false},
			{ID: "ngrok", DisplayName: "ngrok", Blocked: true, Reason: "needs an API key"},
		},
	}

	if required, _ := m.setupRequired(); required {
		t.Error("setup was demanded while a usable provider was available")
	}
	for _, task := range m.emptyHomeTasks() {
		if task.action == ActionSetup {
			t.Error("the empty home offers setup as a task when nothing needs setting up")
		}
	}
}

// TestEmptyHomeExplainsWhatSetupNeeds pins the case where setup is required.
func TestEmptyHomeExplainsWhatSetupNeeds(t *testing.T) {
	m := readyModel(&fakeClient{}, ipc.SnapshotDTO{})
	m.width = 100
	m.readiness = &ipc.ReadinessDTO{
		Providers: []ipc.ProviderReadinessDTO{
			{ID: "cloudflare", DisplayName: "Cloudflare", Blocked: true,
				Reason: "has no account configured"},
		},
	}

	required, reason := m.setupRequired()
	if !required {
		t.Fatal("every provider is blocked and setup was not required")
	}
	if !strings.Contains(reason, "Cloudflare") {
		t.Errorf("the reason does not say which provider needs what: %q", reason)
	}

	view := m.View().Content
	if !strings.Contains(view, "Cloudflare") {
		t.Errorf("the empty home does not say what needs configuring:\n%s", view)
	}
}

// TestEmptyHomeMakesNoClaimBeforeReadinessIsKnown pins that Portico does not
// guess.
//
// Claiming setup is required before asking would be a guess; so would claiming
// it is not. Neither is claimed, and the providers screen is offered instead.
func TestEmptyHomeMakesNoClaimBeforeReadinessIsKnown(t *testing.T) {
	m := readyModel(&fakeClient{}, ipc.SnapshotDTO{})
	m.readiness = nil

	if required, _ := m.setupRequired(); required {
		t.Error("setup was demanded before readiness had been read")
	}
	if !m.readinessForEmptyHome() {
		t.Error("an empty home with no readiness does not ask for it")
	}

	// Once it is known, it is not asked for again: this is not a poll.
	m.storeReadiness(&ipc.ReadinessDTO{})
	if m.readinessForEmptyHome() {
		t.Error("readiness is requested again after it has been read")
	}
}

// TestSettingsIsReachableAndReadsFromTheSupervisor pins that ScreenSettings is
// no longer dead, and that the values shown are the supervisor's.
func TestSettingsIsReachableAndReadsFromTheSupervisor(t *testing.T) {
	client := &fakeClient{settingsDTO: &ipc.SettingsDTO{
		LaunchMode:          "manual",
		DefaultAutoStart:    true,
		DefaultOnDisconnect: "close",
	}}
	m := readyModel(client, twoConnectionSnapshot())
	m.width = 100
	m.screen = ScreenHome

	// Reached from Home through its advertised action.
	action, ok := m.actionsFor(ScreenHome).Find(ActionSettings)
	if !ok || !action.Enabled {
		t.Fatal("home does not offer settings")
	}
	m, cmd := press(t, m, action.primaryKey())
	if m.screen != ScreenSettings {
		t.Fatalf("screen = %q, want settings", m.screen)
	}
	if cmd == nil {
		t.Fatal("opening settings did not ask the supervisor for them")
	}

	// The reply installs what the supervisor holds.
	msg := cmd()
	loaded, isLoad := msg.(settingsLoadedMsg)
	if !isLoad {
		t.Fatalf("opening settings produced %T, want settingsLoadedMsg", msg)
	}
	m.applySettingsLoaded(loaded)

	rows := m.settingsRows()
	if len(rows) != 5 {
		t.Fatalf("settings offers %d rows, want five (three operational + client-tunnel transport)", len(rows))
	}
	view := m.renderSettings()
	// Values are described in user language, not as wire enums.
	for _, forbidden := range []string{"keep_alive", "auto_start", "launch_mode"} {
		if strings.Contains(view, forbidden) {
			t.Errorf("the settings screen shows the internal name %q:\n%s", forbidden, view)
		}
	}
}

// TestChangingASettingSendsOnlyThatField pins that a write does not clobber.
//
// Sending a whole settings object would make every change a full overwrite, so
// two clients changing different settings would undo each other.
func TestChangingASettingSendsOnlyThatField(t *testing.T) {
	client := &fakeClient{settingsDTO: &ipc.SettingsDTO{
		LaunchMode: "auto", DefaultAutoStart: false, DefaultOnDisconnect: "keep_alive",
	}}
	m := readyModel(client, twoConnectionSnapshot())
	m.screen = ScreenSettings
	m.settings = &settingsState{settings: client.settingsDTO}

	// The second row is the AutoStart default.
	m.settings.cursor = 1
	next, cmd, _ := m.changeSelectedSetting()
	if cmd == nil {
		t.Fatal("changing a setting sent nothing")
	}
	m = next
	saved, isSave := cmd().(settingsSavedMsg)
	if !isSave {
		t.Fatal("changing a setting did not produce a save")
	}
	m.applySettingsSaved(saved)

	if len(client.settingsRequests) != 1 {
		t.Fatalf("the change sent %d requests, want one", len(client.settingsRequests))
	}
	req := client.settingsRequests[0]
	if req.DefaultAutoStart == nil {
		t.Fatal("the changed field was not sent")
	}
	if !*req.DefaultAutoStart {
		t.Error("the field was sent with its old value")
	}
	// Nothing else was touched.
	if req.LaunchMode != nil || req.DefaultOnDisconnect != nil {
		t.Errorf("the change sent fields the user did not touch: %+v", req)
	}
}

// TestAPinnedLaunchModeCannotBeChangedHere pins that an override is reported
// rather than silently losing the user's keystroke.
func TestAPinnedLaunchModeCannotBeChangedHere(t *testing.T) {
	pinned := &ipc.SettingsDTO{
		LaunchMode:         "manual",
		LaunchModePinned:   true,
		LaunchModePinnedBy: "PORTICO_LAUNCH_MODE",
	}
	m := readyModel(&fakeClient{settingsDTO: pinned}, twoConnectionSnapshot())
	m.screen = ScreenSettings
	m.settings = &settingsState{settings: pinned, cursor: 0}

	row, ok := m.settingsCurrentRow()
	if !ok {
		t.Fatal("no setting under the cursor")
	}
	if row.editable {
		t.Fatal("a pinned launch mode is offered as editable")
	}
	if !strings.Contains(row.reason, "PORTICO_LAUNCH_MODE") {
		t.Errorf("the reason does not name the override to unset: %q", row.reason)
	}

	// Pressing the key changes nothing and sends nothing.
	_, cmd, _ := m.changeSelectedSetting()
	if cmd != nil {
		t.Fatal("a pinned setting was written anyway")
	}
}

// TestChangingClientTunnelSettingSendsOnlyThatField pins that the
// experimental-transport toggle follows the same write discipline as every
// other setting: one field per request, nothing else touched.
func TestChangingClientTunnelSettingSendsOnlyThatField(t *testing.T) {
	client := &fakeClient{settingsDTO: &ipc.SettingsDTO{
		LaunchMode: "auto", DefaultAutoStart: false, DefaultOnDisconnect: "keep_alive",
		ClientTunnelEnabled: false,
	}}
	m := readyModel(client, twoConnectionSnapshot())
	m.screen = ScreenSettings
	m.settings = &settingsState{settings: client.settingsDTO}

	// The fourth row is the client-tunnel opt-in.
	m.settings.cursor = 3
	row, ok := m.settingsCurrentRow()
	if !ok || row.id != ActionClientTunnelEnabled {
		t.Fatalf("row 3 is %v (ok=%v), want the client-tunnel toggle", row.id, ok)
	}
	next, cmd, _ := m.changeSelectedSetting()
	if cmd == nil {
		t.Fatal("changing the transport setting sent nothing")
	}
	m = next
	saved, isSave := cmd().(settingsSavedMsg)
	if !isSave {
		t.Fatalf("changing the transport setting produced %T, want a save", cmd())
	}
	m.applySettingsSaved(saved)

	if len(client.settingsRequests) != 1 {
		t.Fatalf("the change sent %d requests, want one", len(client.settingsRequests))
	}
	req := client.settingsRequests[0]
	if req.ClientTunnelEnabled == nil || !*req.ClientTunnelEnabled {
		t.Errorf("the toggle was not sent as on: %+v", req)
	}
	if req.LaunchMode != nil || req.DefaultAutoStart != nil || req.DefaultOnDisconnect != nil {
		t.Errorf("the change sent fields the user did not touch: %+v", req)
	}
}

// TestClientTunnelSettingRowsDescribeTheirState pins that the settings screen
// reports the transport rows in user language and explains what a change
// affects, including the experimental boundary.
func TestClientTunnelSettingRowsDescribeTheirState(t *testing.T) {
	m := readyModel(&fakeClient{settingsDTO: &ipc.SettingsDTO{
		LaunchMode: "auto", DefaultOnDisconnect: "keep_alive",
		ClientTunnelEnabled: true,
	}}, twoConnectionSnapshot())
	m.screen = ScreenSettings
	m.settings = &settingsState{settings: &ipc.SettingsDTO{
		LaunchMode: "auto", DefaultOnDisconnect: "keep_alive",
		ClientTunnelEnabled: true,
	}}

	view := m.renderSettings()
	if !strings.Contains(view, "Client-mediated MCP") {
		t.Errorf("the transport row is not rendered:\n%s", view)
	}
	if !strings.Contains(view, "on") {
		t.Errorf("the enabled transport does not render as on:\n%s", view)
	}
	if !strings.Contains(view, "tunnel-client") {
		t.Errorf("the executable row is not rendered:\n%s", view)
	}
}
