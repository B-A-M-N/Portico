package tui

import (
	"strings"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Friendly names for the things the supervisor identifies by ID.
//
// Providers and accounts were shown by their identifiers on the plan preview,
// the inspect screen and the edit screen. An account ID is a durable identity,
// not a name: it is the right thing to send and the wrong thing to read.

// providerDisplayName is the provider's own display name, falling back to its ID
// when the snapshot does not carry the provider — which happens for a plan
// referring to a provider that has since been uninstalled.
func (m *Model) providerDisplayName(id string) string {
	if id == "" {
		return "no provider"
	}
	for _, p := range m.snapshot.Providers {
		if p.ID == id && p.DisplayName != "" {
			return p.DisplayName
		}
	}
	return id
}

// accountDisplayLabel names an account the way the provider labelled it, with
// its status when the status is something the user should know.
func (m *Model) accountDisplayLabel(providerID, accountID string) string {
	if accountID == "" {
		return "no account"
	}
	account, ok := m.findAccount(providerID, accountID)
	if !ok {
		// An account referenced by a connection and absent from the snapshot has
		// been removed. Saying so is the point: it explains why the connection
		// cannot open.
		return accountID + " (no longer configured)"
	}
	label := account.Label
	if label == "" {
		label = accountID
	}
	if reason := accountStatusNote(account); reason != "" {
		label += " — " + reason
	}
	return label
}

// findAccount locates one account across a provider's usable and pending lists.
func (m *Model) findAccount(providerID, accountID string) (ipc.ProviderAccountDTO, bool) {
	for _, p := range m.snapshot.Providers {
		if p.ID != providerID {
			continue
		}
		for _, account := range p.Accounts {
			if account.ID == accountID {
				return account, true
			}
		}
		for _, account := range p.PendingAccounts {
			if account.ID == accountID {
				return account, true
			}
		}
	}
	return ipc.ProviderAccountDTO{}, false
}

// accountStatusNote says why an account is not usable, when it is not.
//
// A usable account gets no note: annotating the normal case is noise, and it is
// the exceptions a user needs pointed out.
func accountStatusNote(account ipc.ProviderAccountDTO) string {
	switch account.Status {
	case "", "usable", "confirmed", "active":
		return ""
	case "pending":
		return "not verified yet"
	case "unusable":
		if account.UnusableReason != "" {
			return account.UnusableReason
		}
		return "cannot be used"
	default:
		if account.UnusableReason != "" {
			return account.UnusableReason
		}
		return strings.ReplaceAll(account.Status, "_", " ")
	}
}

// renderUserFacingError draws a blocking error as an intervention: what
// happened, why, what to do, and a retry when one applies.
//
// Blocking errors were reduced to a one-line status message, which discarded the
// explanation and the recovery actions the supervisor had already provided.
func (m *Model) renderUserFacingError() string {
	if m.err == nil {
		return ""
	}
	ufe := describeError(m.err)

	var b strings.Builder
	b.WriteString(m.theme.Style("intervention").Render(ufe.Summary))
	b.WriteString("\n")
	if ufe.Explanation != "" {
		b.WriteString("\n" + ufe.Explanation + "\n")
	}
	if len(ufe.NextActions) > 0 {
		b.WriteString("\nWhat you can do:\n")
		for _, action := range ufe.NextActions {
			b.WriteString("  • " + action + "\n")
		}
	}
	if ufe.Retryable {
		b.WriteString("\n" + m.theme.Style("muted").Render(
			"This can be retried.") + "\n")
	}
	// The technical detail and the code are kept and shown last, dimmed: they
	// are what goes into a bug report and are not what the user is being asked
	// to read.
	if ufe.Technical != "" || ufe.Code != "" {
		detail := ufe.Technical
		if ufe.Code != "" {
			detail = strings.TrimSpace(ufe.Code + " " + detail)
		}
		b.WriteString("\n" + m.theme.Style("muted").Render("Technical detail: "+detail) + "\n")
	}
	return b.String()
}
