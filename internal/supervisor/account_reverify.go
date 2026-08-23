package supervisor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/provider"
)

// Re-verifying and replacing an account's credential.
//
// Re-verification returned "not yet implemented", so an account that had gone
// bad — a revoked token, a permission removed at the provider — could only be
// removed and added again. Removal is refused while a connection depends on the
// account, so the recovery path for the most common credential problem ran
// through deleting connections.
//
// Both operations below preserve the account identity (provider_id, account_id).
// That identity is what every connection stores, so replacing a credential must
// not create a new account: the connections would point at the old one.
//
// The credential itself never leaves this file's local variables. It is not in
// the response, the events, the logs, the plans or the diagnostics.

// HandleReverifyProviderAccount checks a stored credential against the provider
// without changing it, and records what was learned.
func (h *supervisorHandler) HandleReverifyProviderAccount(
	providerID, accountID string, req ipc.ReverifyProviderAccountRequest,
) (*ipc.ReverifyProviderAccountResponse, error) {
	if accountID == "" {
		accountID = strings.TrimSpace(req.AccountID)
	}
	if providerID == "" || accountID == "" {
		return nil, core.ErrValidation("re-verification needs both a provider and an account")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	account, err := h.sup.findStoredAccount(ctx, providerID, accountID)
	if err != nil {
		return nil, err
	}

	// The credential is fetched to be used, not to be shown. It stays in this
	// scope, and the response reports only the outcome.
	credential, err := h.sup.store.LoadProviderCredential(
		ctx, account.Provider, account.CredentialRef)
	if err != nil {
		return &ipc.ReverifyProviderAccountResponse{
			Status:                  string(account.Status),
			VerificationUnavailable: "Portico could not read the stored credential for this account",
		}, fmt.Errorf("read stored credential: %w", err)
	}

	result, verifyErr := h.sup.verifyAccountCredential(ctx, providerID, account, credential)
	resp := &ipc.ReverifyProviderAccountResponse{Validated: result.validated}

	// The durable status is updated either way. An account that has stopped
	// working must stop being selectable, and one that has started working again
	// must become selectable without being re-entered.
	next := core.AccountPending
	if result.validated {
		next = core.AccountAuthenticated
	}
	if result.unavailable != "" {
		resp.VerificationUnavailable = result.unavailable
	}
	if next != account.Status {
		updated := account
		updated.Status = next
		if err := h.sup.store.UpsertProviderAccount(ctx, updated); err != nil {
			return nil, fmt.Errorf("record the verification outcome: %w", err)
		}
		account = updated
	}
	resp.Status = string(account.Status)

	if verifyErr != nil {
		// A verification that failed is not an error in the request: the answer
		// is that the credential does not work, and why.
		resp.VerificationUnavailable = strings.TrimSpace(verifyErr.Error())
	}
	if result.validated {
		// A credential that works again should be usable again in this process,
		// without a restart.
		if activateErr := h.sup.ActivateProvider(ctx, core.ProviderID(providerID)); activateErr != nil {
			resp.VerificationUnavailable = fmt.Sprintf(
				"the credential is valid, but the provider could not be reloaded: %v", activateErr)
		}
	}
	return resp, nil
}

// verificationResult is what a credential check established.
type verificationResult struct {
	validated bool
	// unavailable explains why nothing could be established, which is different
	// from establishing that the credential is bad.
	unavailable string
}

// verifyAccountCredential checks one credential against its provider.
//
// The check is the provider's own: either the account validator for providers
// that have one, or the declared SetupVerifier. A provider with neither cannot
// be checked, and that is reported as unavailable rather than as a pass.
func (s *Supervisor) verifyAccountCredential(
	ctx context.Context, providerID string, account core.ProviderAccount, credential string,
) (verificationResult, error) {
	if s.accountValidator != nil {
		validation, err := s.accountValidator.Validate(ctx, providerID, string(account.ID), credential)
		if err != nil {
			return verificationResult{}, err
		}
		if validation != nil && validation.AccountAccessible {
			return verificationResult{validated: true}, nil
		}
		return verificationResult{}, fmt.Errorf(
			"the credential no longer reaches account %s", account.ID)
	}

	setup, err := s.setupDefinitionFor(providerID)
	if err != nil {
		return verificationResult{
			unavailable: "Portico has no way to check a credential for this provider",
		}, nil
	}
	verifier, ok := setup.(provider.SetupVerifier)
	if !ok {
		return verificationResult{
			unavailable: fmt.Sprintf(
				"Portico cannot check a %s credential, so this account's status is unchanged", providerID),
		}, nil
	}

	prepared := provider.PreparedAccount{Account: account, Secret: []byte(credential)}
	defer zeroBytes(prepared.Secret)
	if _, err := verifier.VerifyAccount(ctx, prepared); err != nil {
		return verificationResult{}, err
	}
	return verificationResult{validated: true}, nil
}

// findStoredAccount locates one account by its durable identity.
func (s *Supervisor) findStoredAccount(
	ctx context.Context, providerID, accountID string,
) (core.ProviderAccount, error) {
	accounts, err := s.store.ListProviderAccounts(ctx)
	if err != nil {
		return core.ProviderAccount{}, fmt.Errorf("read stored accounts: %w", err)
	}
	for _, account := range accounts {
		if string(account.Provider) == providerID && string(account.ID) == accountID {
			return account, nil
		}
	}
	return core.ProviderAccount{}, core.ErrValidation(fmt.Sprintf(
		"no %s account called %q is configured", providerID, accountID))
}
