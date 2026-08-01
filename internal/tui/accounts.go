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

// providerRow is one provider on the providers screen, with its accounts.
type providerRow struct {
	ID       string
	Name     string
	Accounts []accountRow
}

// providerRows lists providers and their accounts in the order drawn.
func (m *Model) providerRows() []providerRow {
	var rows []providerRow
	for _, p := range m.snapshot.Providers {
		row := providerRow{ID: p.ID, Name: p.DisplayName}
		for _, account := range p.Accounts {
			row.Accounts = append(row.Accounts, accountRow{
				ProviderID: p.ID, ProviderName: p.DisplayName,
				AccountID: account.ID, Label: account.Label, Status: account.Status,
			})
		}
		for _, account := range p.PendingAccounts {
			row.Accounts = append(row.Accounts, accountRow{
				ProviderID: p.ID, ProviderName: p.DisplayName,
				AccountID: account.ID, Label: account.Label, Status: account.Status,
				Pending: true,
			})
		}
		rows = append(rows, row)
	}
	return rows
}

// selectedProvider names the provider the cursor is within.
//
// The providers screen had no provider selection at all, so adding an account
// fell back to Cloudflare by name — in a codebase whose whole setup mechanism is
// declarative and provider-neutral.
func (m *Model) selectedProvider() (providerRow, bool) {
	rows := m.providerRows()
	if len(rows) == 0 {
		return providerRow{}, false
	}
	// The cursor addresses accounts; the provider is the one owning the account
	// under it, or the first provider when there are no accounts at all.
	if account, ok := m.selectedAccount(); ok {
		for _, row := range rows {
			if row.ID == account.ProviderID {
				return row, true
			}
		}
	}
	if m.providerSelected >= 0 && m.providerSelected < len(rows) {
		return rows[m.providerSelected], true
	}
	return rows[0], true
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

// accountRemovalPreviewMsg carries what removing an account would do.
type accountRemovalPreviewMsg struct {
	Generation requestGeneration
	ProviderID string
	AccountID  string
	Preview    *ipc.AccountRemovalPreviewDTO
	Err        error
}

// previewAccountRemovalCmd asks the supervisor what removing this would do.
//
// The screen used to describe the removal itself, from a cached row, and so
// promised to forget a credential Portico might not hold. The supervisor
// answers from the same evidence the removal decides on.
func (m *Model) previewAccountRemovalCmd(row accountRow) tea.Cmd {
	generation := m.accountRequests.next()
	client := m.client
	rootCtx := m.rootCtx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(rootCtx, 20*time.Second)
		defer cancel()
		preview, err := client.PreviewProviderAccountRemoval(ctx, row.ProviderID, row.AccountID)
		return accountRemovalPreviewMsg{
			Generation: generation, ProviderID: row.ProviderID, AccountID: row.AccountID,
			Preview: preview, Err: err,
		}
	}
}

// removeAccountCmd asks the supervisor to remove an account.
//
// It carries the fingerprint of the preview the user confirmed, so a removal
// cannot describe one thing and do another.
func (m *Model) removeAccountCmd(row accountRow, fingerprint string) tea.Cmd {
	generation := m.accountRequests.next()
	client := m.client
	rootCtx := m.rootCtx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(rootCtx, 20*time.Second)
		defer cancel()
		response, err := client.RemoveProviderAccount(ctx, row.ProviderID, row.AccountID, fingerprint)
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

	if m.accountRemovalPreview == nil {
		// No preview yet, either because it is still loading or because the
		// request failed. Either way there is nothing true to say about the
		// removal, so nothing is claimed.
		if m.accountRemovalError != "" {
			b.WriteString(m.theme.Style("intervention").Render(m.accountRemovalError))
			b.WriteString("\n")
			// What is in the way, even without a preview: a refusal that names
			// nothing leaves the user to go and find it.
			for _, dep := range m.accountRemovalDependents {
				b.WriteString(m.theme.Style("muted").Render("  • " + dep))
				b.WriteString("\n")
			}
			b.WriteString("\n[esc] back\n")
			return b.String()
		}
		b.WriteString("Checking what this would remove...\n")
		return b.String()
	}
	// Every factual claim here comes from the supervisor, which computed it
	// from the same evidence the removal decides on.
	for _, line := range m.accountRemovalPreview.Consequences {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")

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

	if !m.accountRemovalPreview.Removable {
		b.WriteString(m.theme.Style("intervention").Render("This account cannot be removed yet:"))
		b.WriteString("\n")
		for _, dep := range m.accountRemovalPreview.Dependencies {
			name := dep.Name
			if name == "" {
				name = dep.ID
			}
			b.WriteString(m.theme.Style("muted").Render("  • " + name + " — " + dep.Explanation))
			b.WriteString("\n")
		}
		b.WriteString("\n[esc] back\n")
		return b.String()
	}

	b.WriteString("[enter] remove    [esc] cancel\n")
	return b.String()
}
