package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

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
}

// changed reports whether the row carries an edit.
func (r editRow) changed() bool { return r.pending != "" && r.pending != r.current }

// editRows describes what can be changed about this connection.
//
// Only a published service has an address or an access policy, so a port
// forward is told that rather than offered a hostname field that its spec has
// nowhere to put.
func (s *editState) rows() []editRow {
	if s.detail == nil {
		return nil
	}
	summary := s.detail.Summary
	exposed := s.detail.DesiredSpec.ServiceExposure

	rows := []editRow{{
		field: editName, label: "Name", current: summary.Name,
		pending: derefString(s.name), editable: true,
	}}

	if exposed != nil {
		hostname := exposed.Exposure.RequestedAddress
		if exposed.Exposure.Mode != "permanent_public" {
			rows = append(rows, editRow{
				field: editHostname, label: "Hostname", current: hostname,
				editable: false,
				reason:   "this connection uses a temporary address, which the provider assigns",
			})
		} else {
			rows = append(rows, editRow{
				field: editHostname, label: "Hostname", current: hostname,
				pending: derefString(s.hostname), editable: true,
			})
		}
		rows = append(rows, editRow{
			field: editProtection, label: "Protection", current: exposed.Protection.Kind,
			pending: derefString(s.protection), editable: true,
		})
		if s.effectiveProtection() == "email_otp" {
			rows = append(rows, editRow{
				field: editProtectionRules, label: "Who can sign in",
				current: screens.ProtectionRulesInput(
					exposed.Protection.AllowedEmails, exposed.Protection.AllowedDomains),
				pending:  derefString(s.protectionRules),
				editable: true,
			})
		}
	} else {
		rows = append(rows, editRow{
			field: editHostname, label: "Hostname", editable: false,
			reason: "a " + screens.ConnectionKindLabel(summary.Kind) + " has no public address",
		}, editRow{
			field: editProtection, label: "Protection", editable: false,
			reason: "access is not controlled by a policy for this kind of connection",
		})
	}

	rows = append(rows,
		editRow{
			field: editAutoStart, label: "Open at startup",
			current: yesNo(s.detail.Lifecycle.AutoStart), pending: pendingYesNo(s.autoStart),
			editable: true,
		},
		editRow{
			field: editOnDisconnect, label: "On disconnect",
			current: s.detail.Lifecycle.OnDisconnect, pending: derefString(s.onDisconnect),
			editable: true,
		},
		editRow{
			field: editAccount, label: "Account", current: summary.ProviderAccountID,
			pending: derefString(s.accountID), editable: true,
		},
	)
	return rows
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
	if (s.hostname != nil || s.protection != nil || s.protectionRules != nil) &&
		s.detail.DesiredSpec.ServiceExposure != nil {
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
		req.Spec = &exposed
	}
	return req
}

// protectionCycle is the order the protection choice steps through.
var protectionCycle = []string{"none", "email_otp"}

// disconnectCycle is the order the disconnect policy steps through.
var disconnectCycle = []string{"keep_alive", "close"}

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
			m.edit.typing = false
		case "esc":
			m.edit.typing = false
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
func (m Model) beginEditingField(row editRow) Model {
	m.edit.editing = row.field
	m.edit.err = ""

	switch row.field {
	case editAutoStart:
		current := m.edit.detail.Lifecycle.AutoStart
		if m.edit.autoStart != nil {
			current = *m.edit.autoStart
		}
		next := !current
		m.edit.autoStart = &next
		return m

	case editProtection:
		current := m.edit.detail.DesiredSpec.ServiceExposure.Protection.Kind
		if m.edit.protection != nil {
			current = *m.edit.protection
		}
		next := cycleNext(protectionCycle, current)
		m.edit.protection = &next
		if next == "none" {
			// The identities go with the policy that needed them.
			m.edit.protectionRules = nil
			m.edit.allowedEmails = nil
			m.edit.allowedDomains = nil
		}
		return m

	case editOnDisconnect:
		current := m.edit.detail.Lifecycle.OnDisconnect
		if m.edit.onDisconnect != nil {
			current = *m.edit.onDisconnect
		}
		next := cycleNext(disconnectCycle, current)
		m.edit.onDisconnect = &next
		return m
	}

	if row.field == editAccount {
		// A selector, not free text. Typing an account ID that does not exist
		// saved a profile that failed the next time it was opened — and for a
		// closed connection the edit plan may never consult the provider, so
		// nothing refused it at the time.
		accounts := m.usableAccounts()
		if len(accounts) == 0 {
			m.edit.err = "this provider has no usable account to move the connection to"
			return m
		}
		current := m.edit.effectiveAccount()
		next := accounts[0].ID
		for i, account := range accounts {
			if account.ID == current {
				next = accounts[(i+1)%len(accounts)].ID
				break
			}
		}
		m.edit.accountID = &next
		return m
	}

	m.edit.typing = true
	m.edit.field = screens.NewField()
	value := row.pending
	if value == "" {
		value = row.current
	}
	m.edit.field.SetValue(value)
	m.edit.field.CursorEnd()
	return m
}

// commitEditField records a typed value.
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
