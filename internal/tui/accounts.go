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
	// still listed but visually distinct.
	Pending bool
}

// providerRow is one provider on the providers screen, with its accounts.
type providerRow struct {
	ID           string
	Name         string
	DisplayName  string
	Accounts     []accountRow
	HasSetupFlow bool
	// SetupKind is the supervisor's answer to what setting this provider up
	// does: "account", "guidance", or empty for a provider with no flow at
	// all. It comes from the provider's own declaration, so an action whose
	// label promises a stored account is one that stores one — not a guess
	// from the provider's name that the supervisor then had to refuse.
	SetupKind string
	// NoAccounts is true when the provider has no accounts yet.
	NoAccounts bool
}

// rowKind identifies the type of a providers screen row.
type rowKind int

const (
	rowKindProvider rowKind = iota
	rowKindAccount
)

// screenRow is one renderable + selectable row on the providers screen.
type screenRow struct {
	Kind     rowKind
	Provider providerRow
	Account  *accountRow
	// FlatIndex is the position in the flattened row list.
	FlatIndex int
}

// buildScreenRows flattens providers and accounts into a selectable row list.
func (m *Model) buildScreenRows() []screenRow {
	var rows []screenRow
	flatIdx := 0
	for _, p := range m.providerRows() {
		rows = append(rows, screenRow{
			Kind:      rowKindProvider,
			Provider:  p,
			FlatIndex: flatIdx,
		})
		flatIdx++
		for i := range p.Accounts {
			rows = append(rows, screenRow{
				Kind:      rowKindAccount,
				Provider:  p,
				Account:   &p.Accounts[i],
				FlatIndex: flatIdx,
			})
			flatIdx++
		}
	}
	return rows
}

// selectedScreenRow returns the currently selected screen row.
func (m *Model) selectedScreenRow() (screenRow, bool) {
	rows := m.buildScreenRows()
	if m.cursorIndex < 0 || m.cursorIndex >= len(rows) {
		return screenRow{}, false
	}
	return rows[m.cursorIndex], true
}

// selectedProvider returns the provider the cursor is within.
func (m *Model) selectedProvider() (providerRow, bool) {
	row, ok := m.selectedScreenRow()
	if !ok {
		rows := m.providerRows()
		if len(rows) == 0 {
			return providerRow{}, false
		}
		return rows[0], true
	}
	return row.Provider, true
}

// selectedAccount returns the account the cursor is on (if any).
// If the cursor is on a provider row, returns nil — account removal is too
// destructive for implicit child selection. Callers must explicitly navigate
// to an account row before account actions apply.
func (m *Model) selectedAccount() (*accountRow, bool) {
	row, ok := m.selectedScreenRow()
	if !ok {
		return nil, false
	}
	if row.Kind == rowKindAccount {
		return row.Account, true
	}
	return nil, false
}

// moveCursor moves the cursor by delta, clamping to the list.
func (m *Model) moveCursor(delta int) {
	rows := m.buildScreenRows()
	if len(rows) == 0 {
		m.cursorIndex = 0
		return
	}
	m.cursorIndex += delta
	if m.cursorIndex < 0 {
		m.cursorIndex = 0
	}
	if m.cursorIndex >= len(rows) {
		m.cursorIndex = len(rows) - 1
	}
}

// moveAccountSelection is an alias for moveCursor for backward compatibility.
func (m *Model) moveAccountSelection(delta int) {
	m.moveCursor(delta)
}

// providerRows returns all providers with their accounts.
func (m *Model) providerRows() []providerRow {
	var rows []providerRow
	for _, p := range m.snapshot.Providers {
		row := providerRow{
			ID:           p.ID,
			Name:         p.Name,
			DisplayName:  p.DisplayName,
			HasSetupFlow: len(p.SetupActions) > 0,
			SetupKind:    p.SetupKind,
		}
		for _, account := range p.Accounts {
			row.Accounts = append(row.Accounts, accountRow{
				ProviderID:   p.ID,
				ProviderName: p.DisplayName,
				AccountID:    account.ID,
				Label:        account.Label,
				Status:       account.Status,
			})
		}
		for _, account := range p.PendingAccounts {
			row.Accounts = append(row.Accounts, accountRow{
				ProviderID:   p.ID,
				ProviderName: p.DisplayName,
				AccountID:    account.ID,
				Label:        account.Label,
				Status:       account.Status,
				Pending:      true,
			})
		}
		if len(row.Accounts) == 0 {
			row.NoAccounts = true
		}
		rows = append(rows, row)
	}
	return rows
}

// accountRows returns all accounts in display order (backward compatibility).
func (m *Model) accountRows() []accountRow {
	var rows []accountRow
	for _, p := range m.providerRows() {
		rows = append(rows, p.Accounts...)
	}
	return rows
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
			Response: response, Err: err,
		}
	}
}

// renderAccountRemoval renders the account removal confirmation screen.
func (m *Model) renderAccountRemoval() string {
	var b strings.Builder

	if m.accountRemovalTarget == nil {
		b.WriteString("No account selected.\n\n")
		b.WriteString(m.actionsFor(ScreenAccountRemoval).footer(m.theme, m.width))
		return b.String()
	}

	target := m.accountRemovalTarget
	b.WriteString(m.theme.Style("header").Render(" REMOVE ACCOUNT "))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("Remove %s from %s?\n\n", target.Label, target.ProviderName))

	if m.accountRemovalError != "" {
		b.WriteString(m.theme.Style("intervention").Render(m.accountRemovalError))
		b.WriteString("\n")
		for _, dep := range m.accountRemovalDependents {
			b.WriteString(m.theme.Style("muted").Render("  • " + dep))
			b.WriteString("\n")
		}
		// The refusal's escape is the screen's own action set; the confirm
		// action there is disabled with the reason, so the bar says blocked
		// rather than offering a key that answers "the supervisor refused".
		b.WriteString("\n")
		b.WriteString(m.actionsFor(ScreenAccountRemoval).footer(m.theme, m.width))
		return b.String()
	}

	if m.accountRemovalPreview != nil {
		if !m.accountRemovalPreview.Removable {
			b.WriteString(m.theme.Style("intervention").Render("This account cannot be removed yet:"))
			b.WriteString("\n")
			for _, dep := range m.accountRemovalPreview.Dependencies {
				b.WriteString(m.theme.Style("muted").Render("  • " + dep.Name))
				b.WriteString("\n")
			}
			// The not-removable refusal is also the screen's action set: confirm
			// is disabled there with the dependency as its reason, so the bar and
			// the dependency list say the same thing.
			b.WriteString("\n")
			b.WriteString(m.actionsFor(ScreenAccountRemoval).footer(m.theme, m.width))
			return b.String()
		}
		for _, line := range m.accountRemovalPreview.Consequences {
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	// The footer is the screen's action set, so the confirm action's own
	// enabled state — carrying the removal's removability — is what draws the
	// bar, not a second copy of the answer beside it.
	b.WriteString(m.actionsFor(ScreenAccountRemoval).footer(m.theme, m.width))
	return b.String()
}
