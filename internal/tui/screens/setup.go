package screens

import (
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// SetupModel renders the readiness view: what Portico needs, what is already
// satisfied, and the next thing to do.
//
// This exists because the information was previously spread across the provider
// screen, the connection list and error messages, so working out why nothing
// worked meant assembling the answer yourself. Everything needed to get to a
// working connection is on one screen, in order.
type SetupModel struct {
	Readiness *ipc.ReadinessDTO
	selected  int
	// Err is set when readiness could not be loaded, so the screen can say that
	// rather than appearing to report perfect health.
	Err error
}

// NewSetup creates the setup screen model.
func NewSetup() *SetupModel { return &SetupModel{} }

// rows returns the selectable provider entries.
func (m *SetupModel) rows() []ipc.ProviderReadinessDTO {
	if m.Readiness == nil {
		return nil
	}
	return m.Readiness.Providers
}

// HandleKey moves the selection.
func (m *SetupModel) HandleKey(key string) {
	switch key {
	case "up", "k":
		if m.selected > 0 {
			m.selected--
		}
	case "down", "j":
		if m.selected < len(m.rows())-1 {
			m.selected++
		}
	}
}

// Selected returns the highlighted provider, if any.
func (m *SetupModel) Selected() *ipc.ProviderReadinessDTO {
	rows := m.rows()
	if m.selected < 0 || m.selected >= len(rows) {
		return nil
	}
	return &rows[m.selected]
}

// View renders the screen.
func (m *SetupModel) View() string {
	var b strings.Builder

	if m.Err != nil {
		b.WriteString("Portico could not work out what it needs.\n\n")
		b.WriteString("Reason: " + m.Err.Error() + "\n\n")
		b.WriteString("This does not mean everything is fine; it means the check failed.\n")
		return b.String()
	}
	if m.Readiness == nil {
		b.WriteString("Checking what Portico needs...\n")
		return b.String()
	}

	// The first line answers "can I use this yet?".
	b.WriteString(m.Readiness.Summary)
	b.WriteString("\n\n")

	b.WriteString(fmt.Sprintf("Launch mode: %s\n", launchModeLabel(m.Readiness.LaunchMode)))
	// A mode fixed by the environment cannot be changed from here. Saying so
	// keeps an unresponsive-looking key from reading as a broken one.
	if m.Readiness.LaunchModePinned {
		b.WriteString(fmt.Sprintf("             fixed by %s; unset it to change this here\n",
			m.Readiness.LaunchModePinnedBy))
	} else {
		// The mode is written to the config file the supervisor owns, so an
		// explicit choice survives a restart. This used to say it applied only
		// until the supervisor restarted, which was true before settings were
		// persisted and has been wrong since.
		b.WriteString("             saved, and kept across restarts\n")
	}
	b.WriteString("\n")

	// What Portico can see about this machine. These are the supervisor's own
	// checks — the same ones `portico doctor` prints — so the screen a user opens
	// when something is wrong and the command they run agree about what is wrong.
	if section := m.renderChecks(); section != "" {
		b.WriteString(section)
	}

	b.WriteString("PROVIDERS\n")
	for i, p := range m.Readiness.Providers {
		marker := "  "
		if i == m.selected {
			marker = "> "
		}
		b.WriteString(fmt.Sprintf("%s%s %s\n", marker, readinessMark(p.Blocked), p.DisplayName))
		b.WriteString("      " + p.Summary + "\n")

		// Credential sources are the part people most often already have, so
		// they are shown as findings rather than as questions.
		for _, c := range p.Credentials {
			b.WriteString("      " + credentialLine(c) + "\n")
		}

		// Only the highlighted provider expands into actions, so the list stays
		// readable when several providers are present.
		if i == m.selected && p.Blocked {
			if p.Reason != "" {
				b.WriteString("\n      Why: " + p.Reason + "\n")
			}
			actions := nextActions(p)
			if len(actions) > 0 {
				b.WriteString("\n      Next:\n")
				for _, action := range actions {
					b.WriteString("        • " + action + "\n")
				}
			}
		}
		b.WriteString("\n")
	}

	if len(m.Readiness.Connections) > 0 {
		b.WriteString("CONNECTIONS\n")
		for _, c := range m.Readiness.Connections {
			b.WriteString(fmt.Sprintf("  %s %s", readinessMark(!c.Ready), c.Name))
			if c.AutoStart {
				b.WriteString("  (starts automatically)")
			}
			b.WriteString("\n")
			for _, blocker := range c.Blockers {
				b.WriteString("      " + blocker + "\n")
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("[↑↓] select   [enter] set up   [l] launch mode   [r] refresh   [esc] back\n")
	return b.String()
}

// nextActions returns what to do about a blocked provider, preferring the
// concrete credential action over the generic setup one.
func nextActions(p ipc.ProviderReadinessDTO) []string {
	var actions []string
	for _, c := range p.Credentials {
		if !c.Present && c.Action != "" {
			actions = append(actions, c.Action)
		}
	}
	actions = append(actions, p.SetupActions...)
	return actions
}

// credentialLine describes one credential source in a single line.
func credentialLine(c ipc.CredentialSourceDTO) string {
	if c.Present {
		return fmt.Sprintf("%s found: %s", readinessMark(false), describeLocation(c))
	}
	// A negative result states where Portico looked. "Not set" on its own
	// reads as "you have no credential", when it only means it was not in the
	// places Portico knows to search — a token kept somewhere else is not
	// found, and the user is the only one who can say so.
	if len(c.Searched) > 1 {
		return fmt.Sprintf("%s not found in: %s",
			readinessMark(true), strings.Join(c.Searched, ", "))
	}
	return fmt.Sprintf("%s not set: %s", readinessMark(true), describeLocation(c))
}

func describeLocation(c ipc.CredentialSourceDTO) string {
	switch c.Kind {
	case "environment":
		return "environment variable " + c.Location
	case "client_config":
		return "the provider's own config at " + c.Location
	case "portico":
		return c.Location
	}
	return c.Location
}

func readinessMark(blocked bool) string {
	if blocked {
		return "✗"
	}
	return "✓"
}

func launchModeLabel(mode string) string {
	if mode == "manual" {
		return "manual — nothing opens by itself; you start each connection"
	}
	return "automatic — connections marked to start will open on launch"
}
