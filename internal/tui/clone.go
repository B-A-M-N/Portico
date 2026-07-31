package tui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Copying a connection.
//
// Making a second connection like an existing one meant walking the whole
// wizard again and retyping every answer — both tedious and the most likely way
// to end up with something subtly different from what was wanted.
//
// The copy is made by the supervisor, which deep-copies the profile. Building a
// create request from the detail DTO instead would have been lossy: the DTO
// omits a command's environment on purpose, and does not carry an existing
// service's health check at all, so the copy would start without them.

// cloneState holds a copy in progress.
type cloneState struct {
	sourceID   string
	sourceName string
	detail     *ipc.ConnectionDetailDTO

	nameField textinput.Model
	// hostField is asked for only when the original holds a permanent hostname,
	// which cannot be shared. The supervisor requires a different one.
	hostField     textinput.Model
	needsHostname bool
	// focus is which field is being typed into.
	focus int
	err   string
	// requests correlates the create with the copy it was started for, and
	// submitting stops a second Enter creating a second connection while the
	// first is still in flight.
	requests   subjectTracker
	submitting bool
}

// beginClone opens the copy prompt for a connection.
func (m Model) beginClone(connID, name string) (Model, tea.Cmd) {
	nameField := screens.NewField()
	nameField.SetValue(suggestedCloneName(name))
	nameField.CursorEnd()

	m.clone = &cloneState{
		sourceID: connID, sourceName: name,
		nameField: nameField, hostField: screens.NewField(),
	}
	m.pushScreen(ScreenClone)
	return m, m.connectionDetailCmd(connID)
}

// suggestedCloneName proposes a name that is not the original's.
//
// Names are the thing a user is most likely to leave as offered, so offering
// the original verbatim would guarantee a rejection on the first press of enter.
func suggestedCloneName(name string) string {
	if name == "" {
		return "copy"
	}
	return name + " copy"
}

// activeCloneField returns the field currently being typed into.
func (s *cloneState) activeField() *textinput.Model {
	if s.needsHostname && s.focus == 1 {
		return &s.hostField
	}
	return &s.nameField
}

// fieldCount is how many questions the copy asks.
func (s *cloneState) fieldCount() int {
	if s.needsHostname {
		return 2
	}
	return 1
}

// connectionClonedMsg reports the outcome of creating a copy.
type connectionClonedMsg struct {
	// Token identifies the copy this answers. A reply for a copy the user has
	// abandoned must not clear the one they have since started.
	Token      requestToken
	Connection *ipc.ConnectionDTO
	Err        error
}

// cloneConnectionCmd asks the supervisor to copy the connection.
func (m *Model) cloneConnectionCmd(sourceID string, req ipc.CloneConnectionRequest) tea.Cmd {
	token := m.clone.requests.start(sourceID)
	m.clone.submitting = true
	client := m.client
	rootCtx := m.rootCtx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(rootCtx, 30*time.Second)
		defer cancel()
		conn, err := client.CloneConnection(ctx, sourceID, req)
		return connectionClonedMsg{Token: token, Connection: conn, Err: err}
	}
}

// renderClone draws the copy prompt.
func (m *Model) renderClone() string {
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" COPY CONNECTION "))
	b.WriteString("\n\n")

	if m.clone == nil {
		return b.String()
	}
	if m.clone.detail == nil {
		if m.clone.err != "" {
			b.WriteString(m.theme.Style("intervention").Render(m.clone.err))
			b.WriteString("\n\n[esc] back\n")
			return b.String()
		}
		b.WriteString("Loading " + m.clone.sourceName + "...\n")
		return b.String()
	}

	if m.clone.submitting {
		b.WriteString("Creating the copy...\n\n")
		b.WriteString("  Please wait; pressing enter again will not create a second one.\n")
		return b.String()
	}

	b.WriteString("Copying " + m.clone.sourceName + ".\n\n")

	nameCursor := "  "
	if m.clone.focus == 0 {
		nameCursor = "> "
	}
	b.WriteString(nameCursor + "Name for the copy:\n")
	b.WriteString("  " + m.clone.nameField.View() + "\n")

	if m.clone.needsHostname {
		hostCursor := "  "
		if m.clone.focus == 1 {
			hostCursor = "> "
		}
		b.WriteString("\n" + hostCursor + "Hostname for the copy:\n")
		b.WriteString("  " + m.clone.hostField.View() + "\n")
		b.WriteString(m.theme.Style("muted").Render(
			"  The original uses " + m.cloneSourceHostname() +
				". Two connections cannot share a hostname, so the copy needs its own."))
		b.WriteString("\n")
	}

	// What the copy carries over is stated before it is made, not discovered
	// afterwards.
	if exposed := m.clone.detail.DesiredSpec.ServiceExposure; exposed != nil {
		if exposed.Protection.Kind != "" && exposed.Protection.Kind != "none" {
			b.WriteString("\n")
			b.WriteString(m.theme.Style("muted").Render(
				"Access protection is copied: the same people will be able to reach it."))
			b.WriteString("\n")
		}
	}
	b.WriteString("\nThe copy is created closed. Nothing is opened until you ask.\n")

	if m.clone.err != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("intervention").Render(m.clone.err))
		b.WriteString("\n")
	}

	if m.clone.fieldCount() > 1 {
		b.WriteString("\n[tab] next field    [enter] create the copy    [esc] cancel\n")
	} else {
		b.WriteString("\n[enter] create the copy    [esc] cancel\n")
	}
	return b.String()
}

// cloneSourceHostname names the address the original holds.
func (m *Model) cloneSourceHostname() string {
	if m.clone == nil || m.clone.detail == nil {
		return ""
	}
	if exposed := m.clone.detail.DesiredSpec.ServiceExposure; exposed != nil {
		return exposed.Exposure.RequestedAddress
	}
	return ""
}

// handleCloneKey drives the copy prompt.
func (m Model) handleCloneKey(key string) (Model, tea.Cmd) {
	if m.clone == nil {
		m.transitionTo(ScreenHome)
		return m, nil
	}
	switch key {
	case "esc":
		// Abandon the request as well as the screen: a create still in flight
		// must not report against a copy the user has since started.
		m.clone.requests.cancel()
		m.clone = nil
		if !m.popScreen() {
			m.transitionTo(ScreenHome)
		}
		return m, nil

	case "tab", "down":
		m.clone.focus = (m.clone.focus + 1) % m.clone.fieldCount()
		return m, nil

	case "shift+tab", "up":
		m.clone.focus = (m.clone.focus - 1 + m.clone.fieldCount()) % m.clone.fieldCount()
		return m, nil

	case "enter":
		// A second Enter would create a second connection. The first has not
		// answered yet, and creation is not something to do twice by accident.
		if m.clone.submitting || m.clone.detail == nil {
			return m, nil
		}
		name := strings.TrimSpace(m.clone.nameField.Value())
		if name == "" {
			m.clone.err = "the copy needs a name"
			return m, nil
		}
		req := ipc.CloneConnectionRequest{Name: name}
		if m.clone.needsHostname {
			hostname := strings.TrimSpace(m.clone.hostField.Value())
			if hostname == "" {
				m.clone.err = "the copy needs its own hostname"
				m.clone.focus = 1
				return m, nil
			}
			req.RequestedAddress = hostname
		}
		m.clone.err = ""
		return m, m.cloneConnectionCmd(m.clone.sourceID, req)
	}
	return m, nil
}
