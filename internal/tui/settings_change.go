package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Changing a setting.
//
// Each row is one field of the settings request, and only the changed field is
// sent: a whole-object write would make every change a full overwrite, so two
// clients changing different settings would clobber each other.

// changeSelectedSetting advances the setting under the cursor.
//
// Every setting here has a small closed set of answers, so stepping is the whole
// interaction. A setting the environment has pinned is not editable, and the
// action carries that reason rather than the keystroke silently doing nothing.
func (m Model) changeSelectedSetting() (Model, tea.Cmd, bool) {
	if m.settings == nil || m.settings.settings == nil || m.settings.saving {
		return m, nil, true
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
	default:
		return m, nil, true
	}

	m.settings.err = ""
	return m, m.saveSettingsCmd(req), true
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
