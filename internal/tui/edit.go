package tui

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Editing a connection.
//
// The supervisor could edit a connection, and could preview the edit as a plan
// before applying it — a route, a handler, a delta description and a client
// method, all working. Nothing called any of it. A connection created with the
// wrong hostname, or pointing at an account that has since been removed, could
// only be deleted and made again from scratch.
//
// This reuses the existing preview and apply path rather than building a second
// one: an edit produces a plan, and a plan is already something this interface
// knows how to show and approve.

// editableField identifies one property of a connection that can be changed.
type editableField int

const (
	editName editableField = iota
	editHostname
	editProtection
	// editProtectionRules is the identities an email-OTP policy allows. It is
	// a field of its own because protection without identities is not a policy
	// anyone can use — core refuses it, and offering the mode without a way to
	// name anyone was an operation that could not succeed.
	editProtectionRules
	editAutoStart
	editOnDisconnect
	editAccount
	// The properties below are ones the controller has always classified and the
	// screen could not change, so a connection created with the wrong address or
	// the wrong port could only be deleted and made again.
	editSourceAddress
	editSourceProtocol
	editExposureMode
	editLocalPort
	editRemoteHost
	editRemotePort
	editForwardProtocol
	editTunnelID
	editTunnelMCP
	// Private-network fields. The controller has classified changes to the network and
	// the mode since the kind existed, and to the published address since it became
	// deliverable.
	editNetworkMode
	editNetworkAddress
)

// editState holds an edit in progress.
type editState struct {
	connectionID string
	// revision is what the edit was built against. It is sent with the request
	// so an edit computed from a stale view is refused rather than silently
	// overwriting a change made elsewhere.
	revision uint64
	detail   *ipc.ConnectionDetailDTO

	cursor int
	// editing is the field currently being typed into, if any.
	editing  editableField
	typing   bool
	field    textinput.Model
	requests requestTracker
	err      string
	// confirmingDiscard is set when the user has asked to leave an edit that
	// has unsaved changes. Escape used to discard them silently, which throws
	// away work for one keystroke on a screen whose whole purpose is to
	// accumulate it.
	confirmingDiscard bool

	// Pending values. A nil pointer means "unchanged", which is what the
	// update request itself means by an absent field.
	name            *string
	hostname        *string
	protection      *string
	protectionRules *string
	// allowedEmails and allowedDomains are the parsed form of protectionRules,
	// kept so the request carries structure rather than the raw line.
	allowedEmails  []string
	allowedDomains []string
	autoStart      *bool
	onDisconnect   *string
	accountID      *string
	// Service-exposure properties the controller classified and the screen could
	// not change.
	sourceAddress  *string
	sourceProtocol *string
	exposureMode   *string
	// Port-forward properties, likewise.
	localPort       *string
	remoteHost      *string
	remotePort      *string
	forwardProtocol *string
	tunnelID        *string
	tunnelMCP       *string

	// Private-network properties.
	networkMode    *string
	networkAddress *string

	// caps and usable are what the provider declares and what accounts it has,
	// resolved by the root model from the snapshot. They decide which choices the
	// screen offers, so a policy the provider cannot enforce is not offered and an
	// account is named rather than identified.
	caps   *ipc.CapabilitySetDTO
	usable []ipc.ProviderAccountDTO
}

// editRow is one line on the edit screen.
type editRow struct {
	field   editableField
	label   string
	current string
	pending string
	// editable is false for a property this connection kind cannot carry, which
	// is stated rather than hidden so the absence is not mistaken for an
	// oversight.
	editable bool
	reason   string
	// choices are the answers for a property with a fixed set of them, so the
	// screen steps through what is actually available rather than a hardcoded
	// cycle. Empty means the property is a free value, typed into a field.
	choices []string
	// explain says what the property means, in the words a user would use.
	explain string
}

// changed reports whether the row carries an edit.
func (r editRow) changed() bool { return r.pending != "" && r.pending != r.current }

// editRows describes what can be changed about this connection.
//
// The properties are the connection kind's own: only a published service has an
// address or an access policy, and only a forward has a local port. A kind is
// told what it cannot carry rather than being offered a field its spec has
// nowhere to put — and rather than the field being hidden, which makes the
// absence look like an oversight.
func (s *editState) rows() []editRow {
	if s.detail == nil {
		return nil
	}
	summary := s.detail.Summary

	rows := []editRow{{
		field: editName, label: "Name", current: summary.Name,
		pending: derefString(s.name), editable: true,
		explain: "What this connection is called. Changing it affects nothing but the name.",
	}}

	switch {
	case s.detail.DesiredSpec.ServiceExposure != nil:
		rows = append(rows, s.serviceExposureRows()...)
	case s.detail.DesiredSpec.PortForward != nil:
		rows = append(rows, s.portForwardRows()...)
	case s.detail.DesiredSpec.PrivateNetwork != nil:
		rows = append(rows, s.privateNetworkRows()...)
	case s.detail.DesiredSpec.ClientTunnel != nil:
		rows = append(rows, s.clientTunnelRows()...)
	default:
		rows = append(rows, editRow{
			field: editHostname, label: "Address", editable: false,
			reason: "Portico cannot yet change the properties of a " +
				screens.ConnectionKindLabel(summary.Kind),
		})
	}

	rows = append(rows,
		editRow{
			field: editAutoStart, label: "Open at startup",
			current: yesNo(s.detail.Lifecycle.AutoStart), pending: pendingYesNo(s.autoStart),
			editable: true,
			explain:  "Whether this connection opens by itself whenever the supervisor starts.",
		},
		editRow{
			field:    editOnDisconnect,
			label:    "When Portico closes",
			current:  onDisconnectWord(s.detail.Lifecycle.OnDisconnect),
			pending:  onDisconnectWord(derefString(s.onDisconnect)),
			choices:  []string{"keep_alive", "close"},
			editable: true,
			explain:  "Whether this connection keeps running after you quit Portico.",
		},
	)
	rows = append(rows, s.accountRow())
	return rows
}

// onDisconnectWord says what the disconnect policy does rather than naming it.
func onDisconnectWord(policy string) string {
	switch policy {
	case "close":
		return "closes with Portico"
	case "keep_alive":
		return "keeps running"
	default:
		return policy
	}
}

// currentRow returns the row under the cursor, if there is one. Callers that
// need to know whether the highlighted property can be changed read this rather
// than indexing rows() themselves and risking an out-of-range cursor.
func (s *editState) currentRow() (editRow, bool) {
	rows := s.rows()
	if s.cursor < 0 || s.cursor >= len(rows) {
		return editRow{}, false
	}
	return rows[s.cursor], true
}

// dirty reports whether anything would change.
func (s *editState) dirty() bool {
	for _, row := range s.rows() {
		if row.changed() {
			return true
		}
	}
	return false
}

// request builds the update from the pending values only.
//
// Sending the unchanged values back would make every edit a full overwrite, so
// a field the user did not touch is absent rather than resubmitted.
func (s *editState) request() ipc.UpdateConnectionRequest {
	req := ipc.UpdateConnectionRequest{ExpectedRevision: s.revision}
	if s.name != nil {
		req.Name = s.name
	}
	if s.autoStart != nil || s.onDisconnect != nil {
		lifecycle := ipc.LifecycleDTO{
			AutoStart:    s.detail.Lifecycle.AutoStart,
			OnDisconnect: s.detail.Lifecycle.OnDisconnect,
		}
		if s.autoStart != nil {
			lifecycle.AutoStart = *s.autoStart
		}
		if s.onDisconnect != nil {
			lifecycle.OnDisconnect = *s.onDisconnect
		}
		req.Lifecycle = &lifecycle
	}
	if s.accountID != nil {
		req.Driver = &ipc.DriverSelectionDTO{
			ProviderID: s.detail.Driver.ProviderID,
			AccountID:  *s.accountID,
			Options:    s.detail.Driver.Options,
		}
	}
	if s.detail.DesiredSpec.ServiceExposure != nil &&
		(s.hostname != nil || s.protection != nil || s.protectionRules != nil ||
			s.exposureMode != nil || s.sourceAddress != nil || s.sourceProtocol != nil) {
		// The spec arm is sent whole because the supervisor merges it into the
		// existing profile field by field; the unchanged parts must therefore
		// carry their current values rather than zeroes.
		exposed := *s.detail.DesiredSpec.ServiceExposure
		// The source is not editable here, and the supervisor rebuilds it from
		// whatever the request carries. Sending it back would replace a spec
		// the DTO cannot fully describe — a health check has no wire form at
		// all — with a lesser copy of itself, as a side effect of changing the
		// protection.
		exposed.Source = ipc.SourceDTO{}
		if s.hostname != nil {
			exposed.Exposure.RequestedAddress = *s.hostname
		}
		if s.protection != nil {
			exposed.Protection.Kind = *s.protection
			if *s.protection == "none" {
				// Turning protection off clears the identities. Leaving them
				// would store a list of people against a connection that no
				// longer asks anyone to sign in.
				exposed.Protection.AllowedEmails = nil
				exposed.Protection.AllowedDomains = nil
			}
		}
		if s.effectiveProtection() == "email_otp" && s.protectionRules != nil {
			exposed.Protection.AllowedEmails = s.allowedEmails
			exposed.Protection.AllowedDomains = s.allowedDomains
		}
		if s.exposureMode != nil {
			exposed.Exposure.Mode = *s.exposureMode
		}
		// The source is sent only when it is what changed. It is rebuilt from the
		// request, so sending it back unchanged would replace a spec the DTO cannot
		// fully describe — a health check has no wire form at all — with a lesser
		// copy of itself, as a side effect of changing something else.
		if s.sourceAddress != nil || s.sourceProtocol != nil {
			current := s.detail.DesiredSpec.ServiceExposure.Source.Existing
			if current != nil {
				existing := *current
				if s.sourceAddress != nil {
					existing.Address = *s.sourceAddress
				}
				if s.sourceProtocol != nil {
					existing.Protocol = *s.sourceProtocol
				}
				exposed.Source = ipc.SourceDTO{Kind: "existing_service", Existing: &existing}
			}
		}
		req.Spec = &exposed
	}
	if s.detail.DesiredSpec.PortForward != nil &&
		(s.localPort != nil || s.remoteHost != nil || s.remotePort != nil ||
			s.forwardProtocol != nil) {
		// The forward arm, likewise sent whole: the supervisor merges it field by
		// field, so an unchanged field must carry its current value rather than a
		// zero that would be read as "no change" for a port and as port zero for a
		// value that is genuinely set.
		forward := *s.detail.DesiredSpec.PortForward
		if s.localPort != nil {
			if port, err := strconv.Atoi(*s.localPort); err == nil {
				forward.LocalPort = port
			}
		}
		if s.remoteHost != nil {
			forward.RemoteHost = *s.remoteHost
		}
		if s.remotePort != nil {
			if port, err := strconv.Atoi(*s.remotePort); err == nil {
				forward.RemotePort = port
			}
		}
		if s.forwardProtocol != nil {
			forward.Protocol = *s.forwardProtocol
		}
		req.PortForward = &forward
	}
	if s.detail.DesiredSpec.PrivateNetwork != nil &&
		(s.networkMode != nil || s.networkAddress != nil) {
		// Sent whole, like the forward arm: the supervisor merges field by field, so an
		// unchanged field must carry its current value.
		network := *s.detail.DesiredSpec.PrivateNetwork
		if s.networkMode != nil {
			network.Mode = *s.networkMode
			network.ExposeLocal = network.Mode == "expose"
			if network.Mode == "join" {
				network.LocalAddress = ""
				network.LocalProtocol = ""
			}
		}
		if s.networkAddress != nil && network.Mode != "join" {
			network.LocalAddress = *s.networkAddress
		}
		req.PrivateNetwork = &network
	}
	if s.detail.DesiredSpec.ClientTunnel != nil &&
		(s.tunnelID != nil || s.tunnelMCP != nil) {
		tunnel := *s.detail.DesiredSpec.ClientTunnel
		tunnel.Profile = ""
		if s.tunnelID != nil {
			tunnel.TunnelID = *s.tunnelID
		}
		if s.tunnelMCP != nil {
			if strings.HasPrefix(*s.tunnelMCP, "http://") ||
				strings.HasPrefix(*s.tunnelMCP, "https://") {
				transport := tunnel.MCP.Transport
				if tunnel.MCP.Command != nil || transport == "" {
					transport = "streamable_http"
				}
				tunnel.MCP = ipc.MCPSourceDTO{
					Transport: transport,
					Endpoint:  *s.tunnelMCP,
				}
			} else {
				tunnel.MCP = ipc.MCPSourceDTO{
					Transport: tunnel.MCP.Transport,
					Command: &ipc.CommandSourceDTO{
						Executable: *s.tunnelMCP,
					},
				}
			}
		}
		req.ClientTunnel = &tunnel
	}
	return req
}

// The protection and disconnect cycles are gone.
//
// They were fixed lists: protection stepped through none and email_otp whatever
// the provider could enforce, so a provider with no access control was offered a
// policy that fails at apply. Each property's answers now come from its row,
// which derives them from what the provider declares — so the choices are the
// ones that exist.

// cycleNext returns the value after the current one, wrapping.
func cycleNext(values []string, current string) string {
	for i, v := range values {
		if v == current {
			return values[(i+1)%len(values)]
		}
	}
	if len(values) > 0 {
		return values[0]
	}
	return current
}

// editPlannedMsg carries the plan produced by previewing an edit.
type editPlannedMsg struct {
	Generation   requestGeneration
	ConnectionID string
	Plan         *ipc.PlanDTO
	Err          error
}

// planEditCmd asks the supervisor what the edit would do, without doing it.
func (m *Model) planEditCmd() tea.Cmd {
	if m.edit == nil {
		return nil
	}
	generation := m.edit.requests.next()
	connID := m.edit.connectionID
	req := m.edit.request()
	client := m.client
	rootCtx := m.rootCtx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(rootCtx, 20*time.Second)
		defer cancel()
		plan, err := client.PlanEdit(ctx, connID, req)
		return editPlannedMsg{Generation: generation, ConnectionID: connID, Plan: plan, Err: err}
	}
}

// renderEdit draws the properties of a connection and which of them would change.
func (m *Model) renderEdit() string {
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" EDIT CONNECTION "))
	b.WriteString("\n\n")

	if m.edit == nil || m.edit.detail == nil {
		b.WriteString("Loading the connection...\n")
		return b.String()
	}
	b.WriteString(m.edit.detail.Summary.Name + "\n\n")

	// The discard confirmation replaces the form. Drawing the properties behind
	// the question would invite answering it by editing something else.
	if m.edit.confirmingDiscard {
		b.WriteString(m.theme.Style("attention").Render(
			"You have unsaved changes to this connection."))
		b.WriteString("\n\n")
		for _, row := range m.edit.rows() {
			if !row.changed() {
				continue
			}
			b.WriteString("  " + row.label + ": " + row.current + " → " + row.pending + "\n")
		}
		b.WriteString("\n")
		b.WriteString("Leaving now discards them. Nothing has been saved: an edit is only " +
			"applied after you preview it.\n\n")
		b.WriteString(m.actionsFor(ScreenEdit).footer(m.theme, m.width))
		b.WriteString("\n")
		return b.String()
	}

	rows := m.edit.rows()
	for i, row := range rows {
		cursor := "  "
		if i == m.edit.cursor {
			cursor = "> "
		}

		value := row.current
		if value == "" {
			value = "(not set)"
		}
		line := fmt.Sprintf("%s%-18s %s", cursor, row.label+":", value)

		switch {
		case m.edit.typing && m.edit.editing == row.field && i == m.edit.cursor:
			b.WriteString(fmt.Sprintf("%s%-18s %s\n", cursor, row.label+":", m.edit.field.View()))
			continue
		case row.changed():
			b.WriteString(m.theme.Style("stable").Render(
				fmt.Sprintf("%s%-18s %s → %s", cursor, row.label+":", value, row.pending)))
			b.WriteString("\n")
			continue
		case !row.editable:
			b.WriteString(m.theme.Style("muted").Render(line))
			b.WriteString("\n")
			if i == m.edit.cursor && row.reason != "" {
				b.WriteString(m.theme.Style("muted").Render("    " + row.reason))
				b.WriteString("\n")
			}
			continue
		}
		b.WriteString(line + "\n")
	}

	if m.edit.err != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("intervention").Render(m.edit.err))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if !m.edit.dirty() {
		b.WriteString(m.theme.Style("muted").Render("Nothing has been changed yet.") + "\n")
	}
	// The footer is the screen's own action set, so it cannot advertise a key the
	// screen does not accept nor omit one it does. It previously hardcoded two
	// different strings, neither of which mentioned the list keys.
	b.WriteString(m.actionsFor(ScreenEdit).footer(m.theme, m.width))
	b.WriteString("\n")
	return b.String()
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func pendingYesNo(p *bool) string {
	if p == nil {
		return ""
	}
	return yesNo(*p)
}

// handleEditKey drives the edit screen.
//
// It is a screen-local handler rather than more cases in the global switch,
// which is what let navigation keys leak into screens that were asking a
// question.
func (m Model) handleEditKey(key string) (Model, tea.Cmd) {
	if m.edit == nil {
		m.transitionTo(ScreenHome)
		return m, nil
	}
	rows := m.edit.rows()

	// A discard confirmation is up: those are the only two answers, and every
	// other key is ignored rather than acting on the edit behind the question.
	if m.edit.confirmingDiscard {
		switch key {
		case "y", "Y":
			m.edit.requests.cancel()
			m.edit = nil
			if !m.popScreen() {
				m.transitionTo(ScreenHome)
			}
		case "n", "N", "esc":
			m.edit.confirmingDiscard = false
		}
		return m, nil
	}

	// While typing, the field owns everything except committing and abandoning.
	if m.edit.typing {
		switch key {
		case "enter":
			m.commitEditField(m.edit.field.Value())
			// The field stays open when the value was refused. Closing it would
			// throw away what the user typed and put them back on a list, with
			// the reason on screen and nothing to correct — so the next Enter
			// reopened an empty field and the refusal looked like it had been
			// forgotten.
			if m.edit.err == "" {
				m.edit.typing = false
			}
		case "esc":
			m.edit.typing = false
			m.edit.err = ""
		}
		return m, nil
	}

	switch key {
	case "up", "k":
		if m.edit.cursor > 0 {
			m.edit.cursor--
		}
	case "down", "j":
		if m.edit.cursor < len(rows)-1 {
			m.edit.cursor++
		}

	case "enter":
		if m.edit.cursor >= len(rows) {
			return m, nil
		}
		row := rows[m.edit.cursor]
		if !row.editable {
			// The row already says why it cannot be changed; repeating it as
			// an error would be noise on top of an answer already on screen.
			return m, nil
		}
		return m.beginEditingField(row), nil

	case "p":
		if !m.edit.dirty() {
			return m, nil
		}
		// Refused here, where the values are still on screen and editable,
		// rather than by core validation after a round trip.
		if ok, reason := m.edit.protectionIsUsable(); !ok {
			m.edit.err = reason
			return m, nil
		}
		m.edit.err = ""
		return m, m.planEditCmd()

	case "esc":
		// An edit is an accumulation of decisions. Throwing it away for one
		// keystroke destroys exactly the work this screen exists to collect, so
		// leaving with unsaved changes asks first.
		if m.edit.dirty() {
			m.edit.confirmingDiscard = true
			return m, nil
		}
		m.edit.requests.cancel()
		m.edit = nil
		if !m.popScreen() {
			m.transitionTo(ScreenHome)
		}
	}
	return m, nil
}

// beginEditingField opens the right editor for a property: a text field for a
// free value, and an immediate step for a property with a fixed set of answers.
// beginEditingField opens the right editor for a property: a step through the
// available answers for one with a fixed set, and a text field for a free value.
//
// The choices come from the row, which derives them from what the provider
// declares. Protection was a fixed cycle through three modes — offered to
// providers that cannot enforce any of them — and the account was a cycle through
// opaque IDs.
func (m Model) beginEditingField(row editRow) Model {
	m.edit.editing = row.field
	m.edit.err = ""

	// A yes/no property is its own thing: two answers, and the row renders them
	// as words rather than as a list to step through.
	if row.field == editAutoStart {
		current := m.edit.detail.Lifecycle.AutoStart
		if m.edit.autoStart != nil {
			current = *m.edit.autoStart
		}
		next := !current
		m.edit.autoStart = &next
		return m
	}

	// A property with a declared set of answers steps to the next one.
	if len(row.choices) > 0 {
		if len(row.choices) == 1 {
			// One answer is not a choice. Saying so is better than a keystroke
			// that appears to do nothing.
			m.edit.err = "there is only one available answer for " + row.label
			return m
		}
		next := cycleNext(row.choices, m.edit.currentChoice(row.field))
		m.edit.setChoice(row.field, next)
		return m
	}

	// Everything else is a free value, typed into a field prefilled with what it
	// currently is — so correcting a long address does not mean retyping it.
	m.edit.typing = true
	m.edit.field = screens.NewField()
	m.edit.field.SetValue(m.edit.currentText(row))
	m.edit.field.CursorEnd()
	return m
}

// currentChoice is the value a choice-based property would have after this edit.
func (s *editState) currentChoice(field editableField) string {
	switch field {
	case editProtection:
		return s.effectiveProtection()
	case editOnDisconnect:
		if s.onDisconnect != nil {
			return *s.onDisconnect
		}
		return s.detail.Lifecycle.OnDisconnect
	case editExposureMode:
		return s.effectiveExposureMode()
	case editSourceProtocol:
		if s.sourceProtocol != nil {
			return *s.sourceProtocol
		}
		if exposed := s.detail.DesiredSpec.ServiceExposure; exposed != nil && exposed.Source.Existing != nil {
			return exposed.Source.Existing.Protocol
		}
		return ""
	case editForwardProtocol:
		if s.forwardProtocol != nil {
			return *s.forwardProtocol
		}
		if forward := s.detail.DesiredSpec.PortForward; forward != nil {
			return forward.Protocol
		}
		return ""
	case editAccount:
		return s.effectiveAccount()
	case editNetworkMode:
		if s.networkMode != nil {
			return *s.networkMode
		}
		if network := s.detail.DesiredSpec.PrivateNetwork; network != nil {
			return network.Mode
		}
		return ""
	default:
		return ""
	}
}

// setChoice records the new answer for a choice-based property.
//
// Invalidation happens here: an answer that makes another answer meaningless
// clears it, so a combination core would refuse is never carried to apply. The
// alternative is a plan that fails a long way from the decision that caused it.
func (s *editState) setChoice(field editableField, value string) {
	switch field {
	case editProtection:
		s.protection = &value
		if value != "email_otp" {
			// The identities belong to the policy that needed them.
			s.protectionRules = nil
			s.allowedEmails = nil
			s.allowedDomains = nil
		}
	case editOnDisconnect:
		s.onDisconnect = &value
	case editExposureMode:
		s.exposureMode = &value
		if value != "permanent_public" {
			// A hostname is only meaningful for a stable address. Keeping it
			// would send a requested address the mode has nowhere to put.
			s.hostname = nil
		}
	case editSourceProtocol:
		s.sourceProtocol = &value
	case editForwardProtocol:
		s.forwardProtocol = &value
	case editAccount:
		s.accountID = &value
	case editNetworkMode:
		s.networkMode = &value
		if value == "join" {
			// A join publishes nothing, so an address entered for a publish is dropped
			// rather than sent with a mode that has nowhere to put it.
			s.networkAddress = nil
		}
	}
}

// currentText is the value a free-text property currently has, used to prefill
// the field.
func (s *editState) currentText(row editRow) string {
	if row.pending != "" {
		return row.pending
	}
	return row.current
}

func (m *Model) commitEditField(value string) {
	value = strings.TrimSpace(value)
	switch m.edit.editing {
	case editName:
		if value == "" {
			m.edit.err = "a connection needs a name"
			return
		}
		m.edit.name = &value
	case editHostname:
		m.edit.hostname = &value
	case editProtectionRules:
		emails, domains, err := screens.ParseProtectionRules(value)
		if err != nil {
			m.edit.err = err.Error()
			return
		}
		if len(emails) == 0 && len(domains) == 0 {
			m.edit.err = "name at least one person or domain, or set protection to none"
			return
		}
		m.edit.protectionRules = &value
		m.edit.allowedEmails = emails
		m.edit.allowedDomains = domains
	case editAccount:
		m.edit.accountID = &value
	case editSourceAddress:
		if value == "" {
			m.edit.err = "a published service needs an address to publish"
			return
		}
		m.edit.sourceAddress = &value
	case editLocalPort, editRemotePort:
		// Validated here, where the value is still on screen and editable, rather
		// than by the supervisor after a round trip.
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			m.edit.err = "a port is a number between 1 and 65535"
			return
		}
		if m.edit.editing == editLocalPort {
			m.edit.localPort = &value
		} else {
			m.edit.remotePort = &value
		}
	case editRemoteHost:
		if value == "" {
			m.edit.err = "a forward needs a host to forward to"
			return
		}
		m.edit.remoteHost = &value
	case editNetworkAddress:
		if value == "" {
			m.edit.err = "publishing a service needs the address it is listening on"
			return
		}
		if !strings.Contains(value, ":") {
			m.edit.err = "the network needs to know which port to reach, as host:port"
			return
		}
		m.edit.networkAddress = &value
	case editTunnelID:
		if err := core.ValidateTunnelID(value); err != nil {
			m.edit.err = err.Error()
			return
		}
		m.edit.tunnelID = &value
	case editTunnelMCP:
		if value == "" {
			m.edit.err = "the tunnel needs somewhere to forward to"
			return
		}
		if validTunnelMCPEndpoint(value) {
			m.edit.tunnelMCP = &value
		} else if looksLikeURL(value) {
			m.edit.err = "the tunnel forwards to an absolute http:// or https:// address"
			return
		} else {
			m.edit.tunnelMCP = &value
		}
	}
	m.edit.err = ""
}

// beginEdit opens the edit screen for a connection, loading its current state.
//
// The edit is built against a specific revision, so it starts from what the
// supervisor holds now rather than from whatever the list last showed.
func (m Model) beginEdit(connID string) (Model, tea.Cmd) {
	m.edit = &editState{connectionID: connID}
	m.pushScreen(ScreenEdit)
	return m, m.connectionDetailCmd(connID)
}

// effectiveProtection is the protection this edit would end with.
func (s *editState) effectiveProtection() string {
	if s.protection != nil {
		return *s.protection
	}
	if s.detail != nil && s.detail.DesiredSpec.ServiceExposure != nil {
		return s.detail.DesiredSpec.ServiceExposure.Protection.Kind
	}
	return ""
}

// protectionIsUsable reports whether the pending protection can actually be
// applied. An email-OTP policy naming nobody is refused by core validation, so
// offering it as a change that could be previewed was a false affordance.
func (s *editState) protectionIsUsable() (bool, string) {
	if s.effectiveProtection() != "email_otp" {
		return true, ""
	}
	emails, domains := s.effectiveIdentities()
	if len(emails) == 0 && len(domains) == 0 {
		return false, "an email sign-in policy has to name at least one person or domain"
	}
	return true, ""
}

// effectiveIdentities is who would be allowed after this edit.
func (s *editState) effectiveIdentities() ([]string, []string) {
	if s.protectionRules != nil {
		return s.allowedEmails, s.allowedDomains
	}
	if s.detail != nil && s.detail.DesiredSpec.ServiceExposure != nil {
		p := s.detail.DesiredSpec.ServiceExposure.Protection
		return p.AllowedEmails, p.AllowedDomains
	}
	return nil, nil
}

// usableAccounts lists the accounts this connection could be moved to.
//
// Only accounts the supervisor reports as usable are offered: an account it
// lists as pending has an unconfirmed credential, and moving a connection onto
// one would produce a connection that cannot open.
func (m *Model) usableAccounts() []ipc.ProviderAccountDTO {
	if m.edit == nil || m.edit.detail == nil {
		return nil
	}
	providerID := m.edit.detail.Driver.ProviderID
	for _, p := range m.snapshot.Providers {
		if p.ID == providerID {
			return p.Accounts
		}
	}
	return nil
}

// effectiveAccount is the account this edit would end with.
func (s *editState) effectiveAccount() string {
	if s.accountID != nil {
		return *s.accountID
	}
	if s.detail != nil {
		return s.detail.Summary.ProviderAccountID
	}
	return ""
}

// validTunnelMCPEndpoint refuses an endpoint the client-tunnel spec cannot
// carry. The supervisor checks again; keeping the same rule here catches a typo
// while it is still on screen.
func validTunnelMCPEndpoint(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.User != nil {
		return false
	}
	return parsed.IsAbs() && parsed.Host != "" &&
		(parsed.Scheme == "http" || parsed.Scheme == "https")
}

// looksLikeURL distinguishes a malformed endpoint from an executable name. Both
// are editable values, but only the malformed endpoint can be diagnosed here.
func looksLikeURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.IsAbs() || parsed.Scheme != "" || strings.Contains(value, "://"))
}
