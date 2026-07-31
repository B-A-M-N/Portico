package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Account lifecycle.
//
// An account could be added and never removed. The supervisor implemented
// removal — carefully, refusing while connections still depend on the account —
// and nothing routed it or called it, so a revoked or mistyped credential
// stayed listed as a working account with no way to say otherwise. The only
// remedy was to configure the provider again and hope the new account replaced
// the old one.

// accountRow is one selectable account on the providers screen.
type accountRow struct {
	ProviderID   string
	ProviderName string
	AccountID    string
	Label        string
	Status       string
	// Pending marks an account whose credential was never confirmed. These are
	// the ones most likely to want removing, so they are selectable too.
	Pending bool
}

// Name is what to call the account when asking about it.
func (r accountRow) Name() string {
	if r.Label != "" {
		return r.Label
	}
	return r.AccountID
}

// accountRows lists every stored account in the order the screen draws them, so
// a cursor index means the same thing to the renderer and to the action.
func (m *Model) accountRows() []accountRow {
	var rows []accountRow
	for _, p := range m.snapshot.Providers {
		for _, account := range p.Accounts {
			rows = append(rows, accountRow{
				ProviderID: p.ID, ProviderName: p.DisplayName,
				AccountID: account.ID, Label: account.Label, Status: account.Status,
			})
		}
		for _, account := range p.PendingAccounts {
			rows = append(rows, accountRow{
				ProviderID: p.ID, ProviderName: p.DisplayName,
				AccountID: account.ID, Label: account.Label, Status: account.Status,
				Pending: true,
			})
		}
	}
	return rows
}

// selectedAccount returns the account the cursor is on.
func (m *Model) selectedAccount() (accountRow, bool) {
	rows := m.accountRows()
	if m.accountSelected < 0 || m.accountSelected >= len(rows) {
		return accountRow{}, false
	}
	return rows[m.accountSelected], true
}

// moveAccountSelection moves the cursor, clamping to the list.
func (m *Model) moveAccountSelection(delta int) {
	rows := m.accountRows()
	if len(rows) == 0 {
		m.accountSelected = 0
		return
	}
	m.accountSelected += delta
	if m.accountSelected < 0 {
		m.accountSelected = 0
	}
	if m.accountSelected >= len(rows) {
		m.accountSelected = len(rows) - 1
	}
}

// accountRemovedMsg reports the outcome of a removal.
type accountRemovedMsg struct {
	// Generation identifies the request this answers, so a reply for an account
	// the user has moved on from cannot report against the one now selected.
	Generation requestGeneration
	ProviderID string
	AccountID  string
	Name       string
	Response   *ipc.RemoveProviderAccountResponse
	Err        error
}

// removeAccountCmd asks the supervisor to remove an account.
func (m *Model) removeAccountCmd(row accountRow) tea.Cmd {
	generation := m.accountRequests.next()
	client := m.client
	rootCtx := m.rootCtx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(rootCtx, 20*time.Second)
		defer cancel()
		response, err := client.RemoveProviderAccount(ctx, row.ProviderID, row.AccountID)
		return accountRemovedMsg{
			Generation: generation, ProviderID: row.ProviderID, AccountID: row.AccountID,
			Name: row.Name(), Response: response, Err: err,
		}
	}
}

// renderAccountRemoval draws the confirmation for removing an account.
//
// Removing an account is not destructive to anything the provider holds — it
// forgets a credential Portico stored — so the confirmation says exactly that
// rather than implying the provider account itself is being deleted.
func (m *Model) renderAccountRemoval() string {
	row := m.accountRemovalTarget
	if row == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" REMOVE ACCOUNT "))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("Remove %s from %s?\n\n", row.Name(), row.ProviderName))
	b.WriteString("Portico will forget the credential it stored for this account.\n")
	b.WriteString("Nothing is deleted at the provider, and you can add it again.\n\n")

	if m.accountRemovalError != "" {
		b.WriteString(m.theme.Style("intervention").Render(m.accountRemovalError))
		b.WriteString("\n")
		for _, dep := range m.accountRemovalDependents {
			b.WriteString(m.theme.Style("muted").Render("  • " + dep))
			b.WriteString("\n")
		}
		b.WriteString("\n[esc] back\n")
		return b.String()
	}

	b.WriteString("[enter] remove    [esc] cancel\n")
	return b.String()
}
