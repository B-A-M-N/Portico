package tui

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Verifying and rotating an account's credential, from the screen that lists
// accounts.
//
// Both were implemented below the interface and reachable from nowhere: the
// supervisor could re-check a stored credential and replace one in place, and
// the only account operations the Providers screen offered were add and remove.
// A user whose token had been revoked had to delete the account — which is
// refused while a connection depends on it.
//
// Neither operation shows, logs, or echoes a credential. Verification sends
// nothing at all; replacement sends the new secret once, in a request body, and
// the field is cleared as soon as the command has run.

// accountVerifiedMsg carries the outcome of a re-verification.
type accountVerifiedMsg struct {
	Token    requestToken
	Provider string
	Account  string
	Result   *ipc.ReverifyProviderAccountResponse
	Err      error
}

// credentialReplacedMsg carries the outcome of a rotation.
type credentialReplacedMsg struct {
	Token    requestToken
	Provider string
	Account  string
	Result   *ipc.ReplaceCredentialResponse
	Err      error
}

// reverifyAccountCmd asks the supervisor to check a stored credential.
//
// The credential is not sent: it is already held by the supervisor, which is the
// only component that can read it. This asks for a verdict about it.
func (m *Model) reverifyAccountCmd(row accountRow) tea.Cmd {
	token := m.credentialRequests.start(row.ProviderID + "/" + row.AccountID)
	client := m.client
	ctx := m.rootCtx
	provider, account := row.ProviderID, row.AccountID
	m.status = "Checking the credential for " + row.Label + "..."
	return func() tea.Msg {
		reply := accountVerifiedMsg{Token: token, Provider: provider, Account: account}
		if client == nil {
			reply.Err = fmt.Errorf("no supervisor connection")
			return reply
		}
		reqCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		reply.Result, reply.Err = client.ReverifyProviderAccount(reqCtx, provider, account)
		return reply
	}
}

// applyAccountVerified reports the verdict, and refreshes the snapshot because
// the account's durable status may have changed.
func (m *Model) applyAccountVerified(msg accountVerifiedMsg) tea.Cmd {
	if !m.credentialRequests.accepts(msg.Token) {
		return nil
	}
	switch {
	case msg.Err != nil:
		m.err = msg.Err
		m.status = ""
	case msg.Result == nil:
		m.status = "The supervisor returned no verification result."
	case msg.Result.Validated:
		m.status = "The credential still works. This account is usable."
	case msg.Result.VerificationUnavailable != "":
		m.status = msg.Result.VerificationUnavailable
	default:
		m.status = "The credential could not be confirmed, so this account is not usable."
	}
	// The status the supervisor recorded is authoritative and is read back
	// rather than assumed from the verdict.
	return m.requestSnapshot()
}

// beginCredentialReplacement opens the provider's setup form against an existing
// account, so a rotation collects the new secret the same way the original was
// collected — including the masked field and the same never-echoed handling.
//
// It does not build a second credential form. The provider's declared setup flow
// is the one description of what its credential looks like.
//
// Identity is FIXED during a rotation (audit item 24): the account's label and
// its zone are shown as read-only context, and only the secret is prompted for.
// Fields that identify the account are pre-filled and disabled so the form can
// never read as though an Account ID or Zone ID changes with the secret.
func (m Model) beginCredentialReplacement(row accountRow) (Model, tea.Cmd, bool) {
	next, cmd := m.beginProviderSetup(row.ProviderID)
	next.replacingAccountID = row.AccountID
	for i := range next.providerSetupFlow.Fields {
		f := &next.providerSetupFlow.Fields[i]
		switch f.ID {
		case "account_id", "zone_id", "label":
			f.Required = false
			f.Description = "(unchanged during credential replacement) " + f.Description
		}
	}
	next.status = "Replace the credential for " + row.Label +
		". Its identity stays fixed; only the secret changes. It is checked before it replaces the old one."
	return next, cmd, true
}

// replaceCredentialCmd sends the new secret.
func (m *Model) replaceCredentialCmd(providerID, accountID, credential string) tea.Cmd {
	token := m.credentialRequests.start(providerID + "/" + accountID)
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		reply := credentialReplacedMsg{Token: token, Provider: providerID, Account: accountID}
		if client == nil {
			reply.Err = fmt.Errorf("no supervisor connection")
			return reply
		}
		reqCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		reply.Result, reply.Err = client.ReplaceProviderAccountCredential(
			reqCtx, providerID, accountID, credential)
		return reply
	}
}

// applyCredentialReplaced reports the outcome of a rotation.
func (m *Model) applyCredentialReplaced(msg credentialReplacedMsg) tea.Cmd {
	if !m.credentialRequests.accepts(msg.Token) {
		return nil
	}
	m.providerSetupSubmitting = false
	if msg.Err != nil {
		// Nothing was stored: the supervisor validates before committing, so the
		// account still has the credential it had. Saying so is the point.
		m.providerSetupError = describeError(msg.Err).Summary
		return nil
	}

	m.clearProviderSetupSecret()
	m.providerSetupStep = 0
	m.replacingAccountID = ""
	switch {
	case msg.Result == nil:
		m.status = "The credential was replaced."
	case msg.Result.RestartRequired:
		m.status = "The new credential is stored, but the provider could not be reloaded. " +
			"It takes effect when the supervisor next starts."
	case msg.Result.VerificationUnavailable != "":
		m.status = "The new credential is stored. " + msg.Result.VerificationUnavailable
	case msg.Result.Validated:
		m.status = "The new credential was checked and is now in use. " +
			"Every connection using this account keeps working."
	default:
		m.status = "The credential was replaced."
	}
	return m.requestSnapshot()
}
