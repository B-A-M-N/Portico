package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The settings screen.
//
// ScreenSettings existed as a constant with no renderer, no key handling and no
// way to reach it. Meanwhile the only operational choice a user could make —
// launch mode — was buried on the readiness screen behind an undocumented `l`,
// and the supervisor could not persist it, so the interface had to tell the user
// their explicit selection would be forgotten at the next restart.
//
// The TUI does not write the config file. It reads and writes these over IPC,
// because durable state belongs to the supervisor.

// settingsState holds the settings screen's own state.
type settingsState struct {
	settings *ipc.SettingsDTO
	cursor   int
	// saving stops a second keypress sending a second write while the first is
	// still in flight.
	saving bool
	// requests correlates writes, so a reply for a change the user has moved
	// past cannot install itself over a newer one.
	requests requestTracker
	err      string
	// loadErr records a failed read, which must be reported as a failed read
	// rather than rendered as a screenful of defaults.
	loadErr  string
	rotating bool
	rotation string
	// confirmingRotate gates key rotation behind an explicit confirmation.
	// Re-encrypting every credential is a destructive operation, so pressing
	// K previews what will happen and enter — only enter — starts it. The
	// CLI asks the same question before its own rotation.
	confirmingRotate bool
}

// settingRow is one line on the settings screen.
type settingRow struct {
	id    ActionID
	label string
	value string
	// explain says what the setting does, in the words a user would use.
	explain string
	// editable is false for a setting the environment is deciding.
	editable bool
	reason   string
}

// settingsRows describes the settings and their current values.
func (m Model) settingsRows() []settingRow {
	if m.settings == nil || m.settings.settings == nil {
		return nil
	}
	s := m.settings.settings

	launchRow := settingRow{
		id:       ActionLaunchMode,
		label:    "At startup",
		value:    launchModeValue(s.LaunchMode),
		explain:  "Whether Portico opens connections by itself when the supervisor starts.",
		editable: !s.LaunchModePinned,
	}
	if s.LaunchModePinned {
		launchRow.reason = "fixed by " + s.LaunchModePinnedBy + "; unset it to change this here"
	}

	return []settingRow{
		launchRow,
		{
			id:    ActionDefaultAutoStart,
			label: "New connections open",
			value: autoStartValue(s.DefaultAutoStart),
			explain: "What a newly created connection is set to. " +
				"You can change it for any single connection when you make it.",
			editable: true,
		},
		{
			id:    ActionDefaultOnDisconnect,
			label: "When Portico closes",
			value: onDisconnectValue(s.DefaultOnDisconnect),
			explain: "What a new connection does when you quit Portico. " +
				"Leaving them running is why the supervisor outlives the interface.",
			editable: true,
		},
	}
}

// settingsCurrentRow returns the row under the cursor.
func (m Model) settingsCurrentRow() (settingRow, bool) {
	rows := m.settingsRows()
	if m.settings == nil || m.settings.cursor < 0 || m.settings.cursor >= len(rows) {
		return settingRow{}, false
	}
	return rows[m.settings.cursor], true
}

// launchModeValue says what a launch mode does rather than naming the enum.
func launchModeValue(mode string) string {
	if mode == launchManual {
		return "nothing opens by itself"
	}
	return "connections marked to open will open"
}

// autoStartValue describes the AutoStart default in user language.
func autoStartValue(enabled bool) string {
	if enabled {
		return "when the supervisor starts"
	}
	return "only when you ask"
}

// onDisconnectValue describes the disconnect policy in user language.
func onDisconnectValue(policy string) string {
	if policy == "close" {
		return "close with Portico"
	}
	return "keep running"
}

// settingsLoadedMsg carries the settings read from the supervisor.
type settingsLoadedMsg struct {
	Settings *ipc.SettingsDTO
	Err      error
}

// settingsSavedMsg carries the outcome of a settings write.
type settingsSavedMsg struct {
	Generation requestGeneration
	Settings   *ipc.SettingsDTO
	Err        error
}

// loadSettingsCmd reads the settings the supervisor holds.
func (m *Model) loadSettingsCmd() tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return settingsLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		settings, err := client.Settings(reqCtx)
		return settingsLoadedMsg{Settings: settings, Err: err}
	}
}

// saveSettingsCmd writes one changed setting.
//
// Only the changed field is sent, so a second client changing a different
// setting is not clobbered.
func (m *Model) saveSettingsCmd(req ipc.SettingsRequest) tea.Cmd {
	if m.settings == nil {
		return nil
	}
	generation := m.settings.requests.next()
	m.settings.saving = true
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		reply := settingsSavedMsg{Generation: generation}
		if client == nil {
			reply.Err = fmt.Errorf("no supervisor connection")
			return reply
		}
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		reply.Settings, reply.Err = client.UpdateSettings(reqCtx, req)
		return reply
	}
}

// renderSettings draws the operational settings.
func (m *Model) renderSettings() string {
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" SETTINGS "))
	b.WriteString("\n\n")

	if m.settings == nil {
		b.WriteString("Loading settings...\n")
		return b.String()
	}
	if m.settings.loadErr != "" {
		b.WriteString("Portico could not read its settings.\n\n")
		b.WriteString("Reason: " + m.settings.loadErr + "\n\n")
		b.WriteString("This does not mean the settings are at their defaults; " +
			"it means the read failed.\n")
		return b.String()
	}
	if m.settings.settings == nil {
		b.WriteString("Loading settings...\n")
		return b.String()
	}

	b.WriteString(m.theme.Style("muted").Render(
		"These are stored by the supervisor and survive a restart."))
	b.WriteString("\n\n")

	rows := m.settingsRows()
	for i, row := range rows {
		cursor := "  "
		if i == m.settings.cursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s%-22s %s", cursor, row.label+":", row.value)
		if !row.editable {
			b.WriteString(m.theme.Style("muted").Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")

		// The explanation is shown for the highlighted setting only, so the
		// list stays readable.
		if i == m.settings.cursor {
			b.WriteString(m.theme.Style("muted").Render("    " + row.explain))
			b.WriteString("\n")
			if row.reason != "" {
				b.WriteString(m.theme.Style("attention").Render("    " + row.reason))
				b.WriteString("\n")
			}
		}
	}

	if m.settings.saving {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("muted").Render("Saving..."))
		b.WriteString("\n")
	}
	if m.settings.err != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("intervention").Render(m.settings.err))
		b.WriteString("\n")
	}
	if m.settings.confirmingRotate {
		// The warning names the consequence before the key material moves.
		b.WriteString("\n")
		b.WriteString(m.theme.Style("intervention").Render(
			"Rotate the installation encryption key?"))
		b.WriteString("\n")
		b.WriteString(m.theme.Style("muted").Render(
			"Existing credentials are re-encrypted under the new key."))
		b.WriteString("\n")
		b.WriteString(m.theme.Style("attention").Render(
			"enter: rotate    esc: cancel"))
		b.WriteString("\n")
	}
	if m.settings.rotation != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("muted").Render(m.settings.rotation))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(m.actionsFor(ScreenSettings).footer(m.theme, m.width))
	b.WriteString("\n")
	return b.String()
}
