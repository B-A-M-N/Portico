package supervisor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// Replacing a credential in place.
//
// A token is rotated, revoked, or was entered with a typo. The only remedy was
// to remove the account and add it again — and removal is refused while any
// connection depends on the account, so the recovery path for the commonest
// credential problem ran through deleting the connections that needed it.
//
// The account identity (provider_id, account_id) is preserved, so every
// connection referring to it keeps working. The new credential is validated
// before it replaces the old one: a rotation that installs a broken token would
// take working connections down at their next open.

// HandleReplaceProviderAccountCredential validates a new credential and, only if
// it works, stores it against the existing account.
func (h *supervisorHandler) HandleReplaceProviderAccountCredential(
	providerID, accountID string, req ipc.ReplaceCredentialRequest,
) (*ipc.ReplaceCredentialResponse, error) {
	credential := strings.TrimSpace(req.Credential)
	// The plaintext is zeroed on every path out of here.
	secret := []byte(credential)
	defer zeroBytes(secret)

	if providerID == "" || accountID == "" {
		return nil, core.ErrValidation("replacing a credential needs both a provider and an account")
	}
	if credential == "" {
		return nil, core.ErrValidation("a new credential is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	account, err := h.sup.findStoredAccount(ctx, providerID, accountID)
	if err != nil {
		return nil, err
	}

	// Validated before it is committed. An unvalidatable provider is a
	// different case from a rejected credential: the replacement proceeds, and
	// the account is stored pending with the reason, exactly as first-time
	// configuration does.
	result, verifyErr := h.sup.verifyAccountCredential(ctx, providerID, account, credential)
	resp := &ipc.ReplaceCredentialResponse{}
	if verifyErr != nil {
		// Nothing is written. The old credential is still in place, so whatever
		// was working before still works.
		return nil, core.ErrValidation(fmt.Sprintf(
			"the new credential was not accepted, so the old one is unchanged: %v", verifyErr))
	}

	updated := account
	updated.Status = core.AccountPending
	if result.validated {
		updated.Status = core.AccountAuthenticated
		resp.Validated = true
	}
	if result.unavailable != "" {
		resp.VerificationUnavailable = result.unavailable
	}

	// The identity, label, credential reference and metadata are all carried
	// over: this is the same account with a different secret behind it. The
	// write is one transaction, so a failure cannot leave the account pointing
	// at a credential that was not stored.
	if err := h.sup.store.UpsertProviderAccountCredential(ctx, updated, secret); err != nil {
		return nil, fmt.Errorf("store the new credential: %w", err)
	}
	resp.Status = string(updated.Status)
	resp.AccountID = string(updated.ID)

	// Reload in place so the new credential is in force now. A user who has just
	// rotated a token should not have to restart the supervisor from a shell.
	if activateErr := h.sup.ActivateProvider(ctx, core.ProviderID(providerID)); activateErr != nil {
		resp.RestartRequired = true
		if activationUnavailable(activateErr) {
			return resp, nil
		}
		return resp, fmt.Errorf("activate provider %q after credential replacement: %w", providerID, activateErr)
	}
	return resp, nil
}
