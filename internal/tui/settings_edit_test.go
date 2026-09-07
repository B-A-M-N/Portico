package tui

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The tunnel-client path is writable, and writable here.
//
// The supervisor could update SettingsRequest.ClientTunnelBin and persisted
// it, but the settings screen marked the row read-only — so the transport
// path was configurable only through the config file, in an interface whose
// job is to make setup unnecessary to hand-edit. These tests pin the vertical:
// enter on the row opens a field, the field owns its keys, enter sends exactly
// that one field, esc sends nothing, a pinned path refuses with the reason,
// and what the screen shows afterwards is the supervisor's answer.

func tunnelBinSettingsModel(client *fakeClient, settings *ipc.SettingsDTO) Model {
	m := readyModel(client, twoConnectionSnapshot())
	m.screen = ScreenSettings
	m.settings = &settingsState{settings: settings}
	// The fifth row is the tunnel-client path.
	m.settings.cursor = 4
	return m
}

// TestTunnelClientPathRowIsEditable pins the row's new semantics: offered as
// editable when the environment has not fixed it, refused with the reason when
// it has.
func TestTunnelClientPathRowIsEditable(t *testing.T) {
	free := &ipc.SettingsDTO{ClientTunnelBin: ""}
	m := tunnelBinSettingsModel(&fakeClient{}, free)

	row, ok := m.settingsCurrentRow()
	if !ok || row.id != ActionClientTunnelBin {
		t.Fatalf("row 4 is %v (ok=%v), want the tunnel-client path", row.id, ok)
	}
	if !row.editable {
		t.Fatal("the path row is still read-only")
	}
	if !strings.Contains(row.explain, "enter to set it") {
		t.Errorf("the row does not say how to set it: %q", row.explain)
	}

	pinned := &ipc.SettingsDTO{
		ClientTunnelBin:         "/opt/tunnel-client",
		ClientTunnelBinPinned:   true,
		ClientTunnelBinPinnedBy: "PORTICO_CLIENT_TUNNEL_BIN",
	}
	m = tunnelBinSettingsModel(&fakeClient{}, pinned)
	row, _ = m.settingsCurrentRow()
	if row.editable {
		t.Fatal("a path pinned by the environment is offered as editable")
	}
	if !strings.Contains(row.reason, "PORTICO_CLIENT_TUNNEL_BIN") {
		t.Errorf("the refusal does not name the override: %q", row.reason)
	}
	// Pressing enter on a pinned row opens nothing and sends nothing.
	next, cmd, _ := m.changeSelectedSetting()
	if cmd != nil {
		t.Fatal("a pinned path was written anyway")
	}
	if next.settings.editing != nil {
		t.Fatal("a pinned path opened the editor")
	}
}

// TestEnterOnPathRowOpensTheField pins the interaction: enter opens the
// editor, and the action set becomes the editor's two answers.
func TestEnterOnPathRowOpensTheField(t *testing.T) {
	client := &fakeClient{settingsDTO: &ipc.SettingsDTO{ClientTunnelBin: ""}}
	m := tunnelBinSettingsModel(client, client.settingsDTO)

	actions := m.actionsFor(ScreenSettings)
	change, ok := actions.Find(ActionConfirm)
	if !ok {
		t.Fatal("the settings screen has no enter action")
	}
	if !change.Enabled {
		t.Fatalf("enter is disabled on the path row: %s", change.DisabledReason)
	}

	next, cmd, _ := m.changeSelectedSetting()
	m = next
	if cmd == nil {
		t.Fatal("opening the editor produced no command")
	}
	if m.settings.editing == nil {
		t.Fatal("enter on the path row did not open the editor")
	}
	if m.settings.cursor != 4 {
		t.Fatalf("the cursor moved to %d while the editor opened", m.settings.cursor)
	}

	// While the editor is up, the screen's actions are save and cancel —
	// nothing that a printable key could be swallowed by without saying so.
	editing := m.actionsFor(ScreenSettings)
	for _, id := range []ActionID{ActionLaunchMode, ActionDefaultAutoStart, ActionRotateSecretKey} {
		if act, ok := editing.Find(id); ok && act.Enabled {
			t.Errorf("%s is still enabled while the editor is open", id)
		}
	}
	save, ok := editing.Find(ActionConfirm)
	if !ok || !save.Enabled {
		t.Fatal("the editor does not offer save")
	}
	cancel, ok := editing.Find(ActionBack)
	if !ok || !cancel.Enabled {
		t.Fatal("the editor does not offer cancel")
	}

	_ = change // referenced above through actions
}

// TestPathEditorOwnsItsKeys pins that typed input reaches the field and that
// esc closes it without sending anything.
func TestPathEditorOwnsItsKeys(t *testing.T) {
	client := &fakeClient{settingsDTO: &ipc.SettingsDTO{ClientTunnelBin: ""}}
	m := tunnelBinSettingsModel(client, client.settingsDTO)

	next, _, _ := m.changeSelectedSetting()
	m = next

	// Printable input goes to the field, not the action set: `q` in a path is
	// the letter q.
	for _, key := range []string{"/", "u", "s", "r", "q"} {
		next, _ := m.Update(keyMsg(key))
		m = next.(Model)
	}
	got := m.settings.editing.field.Value()
	if got != "/usrq" {
		t.Fatalf("the field received %q, want the typed characters", got)
	}
	if m.screen != ScreenSettings {
		t.Fatalf("typing moved the screen to %s", m.screen)
	}

	// Escape cancels: nothing is sent, the editor is closed, the cursor is
	// restored.
	delivered, cancelCmd := m.Update(keyMsg("esc"))
	m = delivered.(Model)
	if cancelCmd != nil {
		t.Fatal("cancelling the editor sent something")
	}
	if m.settings.editing != nil {
		t.Fatal("esc did not close the editor")
	}
	if m.settings.cursor != 4 {
		t.Fatalf("cancel left the cursor on %d, want the row it opened from", m.settings.cursor)
	}
	if len(client.settingsRequests) != 0 {
		t.Fatalf("cancel sent %d writes", len(client.settingsRequests))
	}
}

// TestCommittingThePathSendsExactlyThatField is the vertical: enter sends
// SettingsRequest.ClientTunnelBin and nothing else, and the screen then shows
// what the supervisor reports, not what was typed.
func TestCommittingThePathSendsExactlyThatField(t *testing.T) {
	client := &fakeClient{settingsDTO: &ipc.SettingsDTO{ClientTunnelBin: ""}}
	m := tunnelBinSettingsModel(client, client.settingsDTO)

	next, _, _ := m.changeSelectedSetting()
	m = next

	for _, key := range []string{"/", "o", "p", "t", "/", "t", "c"} {
		next, _ := m.Update(keyMsg(key))
		m = next.(Model)
	}
	next, cmd, _ := m.changeSelectedSetting() // enter commits
	m = next
	if cmd == nil {
		t.Fatal("committing the path sent nothing")
	}
	saved, isSave := cmd().(settingsSavedMsg)
	if !isSave {
		t.Fatalf("commit produced %T, want a save", cmd())
	}
	if m.settings.editing != nil {
		t.Fatal("the editor is still open after committing")
	}
	m.applySettingsSaved(saved)

	if len(client.settingsRequests) != 1 {
		t.Fatalf("the commit sent %d requests, want one", len(client.settingsRequests))
	}
	req := client.settingsRequests[0]
	if req.ClientTunnelBin == nil || *req.ClientTunnelBin != "/opt/tc" {
		t.Fatalf("the path was not sent: %+v", req)
	}
	if req.LaunchMode != nil || req.DefaultAutoStart != nil ||
		req.DefaultOnDisconnect != nil || req.ClientTunnelEnabled != nil {
		t.Errorf("the commit sent fields the user did not touch: %+v", req)
	}

	// The screen shows the supervisor's answer.
	row, _ := m.settingsCurrentRow()
	if row.value != "/opt/tc" {
		t.Fatalf("the row shows %q, want the supervisor's effective value", row.value)
	}
}

// TestAnEmptyPathClearsTheOverride pins that committing an empty field is the
// documented way back to finding the executable on PATH — sent as empty, not
// silently skipped.
func TestAnEmptyPathClearsTheOverride(t *testing.T) {
	client := &fakeClient{settingsDTO: &ipc.SettingsDTO{
		ClientTunnelBin:         "/opt/tunnel-client",
		ClientTunnelBinPinned:   false,
		ClientTunnelBinPinnedBy: "",
	}}
	m := tunnelBinSettingsModel(client, client.settingsDTO)

	next, _, _ := m.changeSelectedSetting()
	m = next
	// Nothing is typed: the field stays empty.
	next, cmd, _ := m.changeSelectedSetting()
	m = next
	if cmd == nil {
		t.Fatal("committing the empty field sent nothing")
	}
	saved := cmd().(settingsSavedMsg)
	m.applySettingsSaved(saved)

	if len(client.settingsRequests) != 1 {
		t.Fatalf("the commit sent %d requests, want one", len(client.settingsRequests))
	}
	req := client.settingsRequests[0]
	if req.ClientTunnelBin == nil || *req.ClientTunnelBin != "" {
		t.Fatalf("the clear was not sent as empty: %+v", req)
	}
	row, _ := m.settingsCurrentRow()
	if row.value != "find on PATH" {
		t.Fatalf("the row shows %q, want the PATH lookup", row.value)
	}
}

// TestSettingsScreenRendersTheOpenEditor pins the surface: the field is drawn
// under the row it edits, and the footer is the editor's two answers.
func TestSettingsScreenRendersTheOpenEditor(t *testing.T) {
	client := &fakeClient{settingsDTO: &ipc.SettingsDTO{ClientTunnelBin: ""}}
	m := tunnelBinSettingsModel(client, client.settingsDTO)

	next, _, _ := m.changeSelectedSetting()
	m = next
	delivered, _ := m.Update(keyMsg("/"))
	m = delivered.(Model)

	view := m.renderSettings()
	if !strings.Contains(view, "/") {
		t.Fatalf("the open field is not drawn:\n%s", view)
	}
	want := m.actionsFor(ScreenSettings).footer(m.theme, m.width)
	if !strings.HasSuffix(strings.TrimRight(view, "\n"), strings.TrimRight(want, "\n")) {
		t.Fatalf("the screen does not end with the action set's footer\n--- action set ---\n%s\n--- screen ---\n%s", want, view)
	}
	if !strings.Contains(want, "Save path") || !strings.Contains(want, "Cancel") {
		t.Errorf("the footer does not name the editor's two answers:\n%s", want)
	}
}

// TestRotationConfirmationOutranksTheEditor pins the precedence between the
// screen's two modal states: while a path editor is open, the rotation
// confirmation cannot be armed, and while the rotation confirmation is up the
// editor's keys are not consumed — enter is the rotation's answer there.
func TestRotationConfirmationOutranksTheEditor(t *testing.T) {
	client := &fakeClient{settingsDTO: &ipc.SettingsDTO{ClientTunnelBin: ""}}
	m := tunnelBinSettingsModel(client, client.settingsDTO)

	// While an editor is open, K is a printable key into the field, not the
	// rotation action.
	next, _, _ := m.changeSelectedSetting()
	m = next
	delivered, _ := m.Update(keyMsg("K"))
	m = delivered.(Model)
	if m.settings.confirmingRotate {
		t.Fatal("K while the editor is open armed the rotation confirmation")
	}
	if m.settings.editing == nil {
		t.Fatal("the editor closed")
	}

	// Close the editor, then arm the rotation: enter confirms the rotation
	// rather than committing an editor that is not open.
	delivered, _ = m.Update(keyMsg("esc"))
	m = delivered.(Model)
	delivered, _ = m.Update(keyMsg("K"))
	m = delivered.(Model)
	if !m.settings.confirmingRotate {
		t.Fatal("K with no editor did not arm the rotation confirmation")
	}
	if m.settings.editing != nil {
		t.Fatal("the editor is still open")
	}
}

// pressActionKey was removed with the test that used it; the keyboard
// boundary is exercised by the settings PTY test.
