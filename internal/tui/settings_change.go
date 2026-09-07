package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Changing a setting.
//
// Each row is one field of the settings request, and only the changed field is
// sent: a whole-object write would make every change a full overwrite, so two
// clients changing different settings would clobber each other.

// changeSelectedSetting advances the setting under the cursor.
//
// Every setting here except the tunnel-client path has a small closed set of
// answers, so stepping is the whole interaction. A path is typed, so its enter
// opens the field editor rather than stepping through values nobody wants to
// cycle. A setting the environment has pinned is not editable, and the action
// carries that reason rather than the keystroke silently doing nothing.
func (m Model) changeSelectedSetting() (Model, tea.Cmd, bool) {
	if m.settings == nil || m.settings.settings == nil || m.settings.saving {
		return m, nil, true
	}
	// While an editor is open, enter is the editor's commit, not a re-entry
	// into whatever row the cursor was on.
	if m.settings.editing != nil {
		return m.commitSettingsEdit()
	}
	row, ok := m.settingsCurrentRow()
	if !ok || !row.editable {
		return m, nil, true
	}

	current := m.settings.settings
	var req ipc.SettingsRequest
	switch row.id {
	case ActionLaunchMode:
		mode := launchAuto
		if current.LaunchMode != launchManual {
			mode = launchManual
		}
		req.LaunchMode = &mode
	case ActionDefaultAutoStart:
		next := !current.DefaultAutoStart
		req.DefaultAutoStart = &next
	case ActionDefaultOnDisconnect:
		policy := "keep_alive"
		if current.DefaultOnDisconnect != "close" {
			policy = "close"
		}
		req.DefaultOnDisconnect = &policy
	case ActionClientTunnelEnabled:
		next := !current.ClientTunnelEnabled
		req.ClientTunnelEnabled = &next
	case ActionClientTunnelBin:
		return m.beginSettingsEdit(row)
	default:
		return m, nil, true
	}

	m.settings.err = ""
	return m, m.saveSettingsCmd(req), true
}

// beginSettingsEdit opens the text editor for a free-text setting.
//
// The editor starts empty rather than pre-filled: the row already shows the
// effective value, and pre-filling would invite submitting it back verbatim —
// harmless for a path, but the same shape applied to a secret would be a
// leak. Clearing means "find on PATH again", which is a legitimate answer the
// empty field reaches directly.
func (m Model) beginSettingsEdit(row settingRow) (Model, tea.Cmd, bool) {
	field := screens.NewField()
	field.Placeholder = "e.g. /usr/local/bin/tunnel-client"
	m.settings.editing = &settingsEditState{
		action: row.id,
		field:  field,
		row:    m.settings.cursor,
	}
	m.settings.err = ""
	return m, textinput.Blink, true
}

// commitSettingsEdit sends the typed value.
//
// The empty answer is sent as the empty string, which is the documented way
// to clear the override and return to finding the executable on PATH — not
// a no-op to be silently skipped.
func (m Model) commitSettingsEdit() (Model, tea.Cmd, bool) {
	edit := m.settings.editing
	value := strings.TrimSpace(edit.field.Value())
	m.settings.editing = nil
	m.settings.cursor = edit.row
	req := ipc.SettingsRequest{ClientTunnelBin: &value}
	m.settings.err = ""
	return m, m.saveSettingsCmd(req), true
}

// cancelSettingsEdit closes the editor with nothing sent.
func (m Model) cancelSettingsEdit() (Model, tea.Cmd, bool) {
	edit := m.settings.editing
	m.settings.editing = nil
	m.settings.cursor = edit.row
	return m, nil, true
}

// handleSettingsEditKey routes the keys the open editor owns.
//
// This is checked before the screen's action set, the way the wizard and the
// setup form own their keyboards: while the editor is up, esc is cancel and
// enter is commit, and no other key is a command.
func (m Model) handleSettingsEditKey(key string) (Model, tea.Cmd, bool) {
	if m.settings == nil || m.settings.editing == nil {
		return m, nil, false
	}
	switch key {
	case "esc":
		return m.cancelSettingsEdit()
	case "enter":
		return m.commitSettingsEdit()
	}
	return m, nil, false
}

type secretKeyRotatedMsg struct {
	Result *ipc.RotateSecretKeyDTO
	Err    error
}

func (m *Model) rotateSecretKeyCmd() tea.Cmd {
	if m.settings == nil {
		return nil
	}
	m.settings.rotating = true
	client := m.client
	return func() tea.Msg {
		if client == nil {
			return secretKeyRotatedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		result, err := client.RotateSecretKey(context.Background())
		return secretKeyRotatedMsg{Result: result, Err: err}
	}
}

func (m *Model) applySecretKeyRotated(msg secretKeyRotatedMsg) {
	if m.settings == nil {
		return
	}
	m.settings.rotating = false
	if msg.Err != nil {
		m.settings.err = describeError(msg.Err).Summary
		return
	}
	m.settings.err = ""
	if msg.Result != nil {
		m.settings.rotation = fmt.Sprintf("Installation encryption key rotated to version %d.", msg.Result.Version)
	}
}

// applySettingsLoaded installs the settings the supervisor reported.
func (m *Model) applySettingsLoaded(msg settingsLoadedMsg) {
	if m.settings == nil {
		m.settings = &settingsState{}
	}
	if msg.Err != nil {
		// A failed read must not render as a screenful of defaults: the user
		// would be looking at values that are not in force.
		m.settings.settings = nil
		m.settings.loadErr = describeError(msg.Err).Summary
		return
	}
	m.settings.loadErr = ""
	m.settings.settings = msg.Settings
	m.settings.cursor = clampIndex(m.settings.cursor, len(m.settingsRows()))
}

// applySettingsSaved installs the result of a write.
func (m *Model) applySettingsSaved(msg settingsSavedMsg) {
	if m.settings == nil || !m.settings.requests.accepts(msg.Generation) {
		return
	}
	m.settings.saving = false
	if msg.Err != nil {
		m.settings.err = describeError(msg.Err).Summary
		return
	}
	m.settings.err = ""
	// What is displayed is what the supervisor reports after the write, never
	// the value that was requested: a pinned launch mode legitimately does not
	// change, and echoing the request would claim otherwise.
	if msg.Settings != nil {
		m.settings.settings = msg.Settings
	}
}
