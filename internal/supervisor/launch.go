package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/config"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/store"
)

// providerCredentialRef derives the credential reference for one account.
//
// Every writer must derive it here. Two writers building the reference
// differently for the same account produced two encrypted credential rows, and
// because provider_credentials is keyed on the reference while deletion removes
// only the one the account currently points at, the other survived the account
// being deleted — a secret left in the database after the user was told it was
// gone, reachable by nothing.
func providerCredentialRef(providerID, accountID string) string {
	return fmt.Sprintf("%s:%s:api-token", providerID, accountID)
}

// bootstrapVerifier checks an environment credential before Portico records an
// account as authenticated. A nil verifier means no check is available.
type bootstrapVerifier func(ctx context.Context, providerID core.ProviderID, accountID, token string) error

// seedBootstrapAccount imports a credential found in the supervisor's
// environment, and only when the provider has no account under that ID.
//
// This runs on every supervisor start, so it must never overwrite anything an
// operator established. The account row and the secret are written as one unit,
// because guarding the row alone let a stale environment token silently replace
// a validated one while the row kept its label, zone and status.
//
// An account created here is verified first. Recording an unchecked token as
// authenticated is the same defect this audit corrected elsewhere, and it was
// reachable here because adapters are built only from authenticated accounts —
// so writing pending would have disabled environment setup outright. Verifying
// the import once closes it without making every boot depend on the network:
// existing accounts are trusted as previously verified and are never re-checked.
//
// If verification cannot complete, nothing is written and the import is retried
// on a later start. That is deliberate: an account Portico could not confirm
// must not become the thing every connection is planned against.
func seedBootstrapAccount(
	ctx context.Context, st *store.Store, providerID core.ProviderID, accountID, token string,
	metadata map[string]string, verify bootstrapVerifier,
) {
	credentialRef := providerCredentialRef(string(providerID), accountID)

	// Only a new import is verified, so a machine that is offline keeps working
	// with the accounts it already has.
	if existing, err := st.ListProviderAccounts(ctx); err == nil {
		for _, account := range existing {
			if account.Provider == providerID && string(account.ID) == accountID {
				return
			}
		}
	}

	if verify == nil {
		slog.Warn("not importing an environment credential that cannot be checked",
			"provider", providerID, "account", accountID)
		return
	}
	// Bounded, and derived from the caller's context. This runs before the IPC
	// listener binds, so an unbounded network call here does not fail slowly —
	// it prevents the supervisor from ever starting, on a network that
	// blackholes rather than refuses, with no way to interrupt it.
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := verify(verifyCtx, providerID, accountID, token); err != nil {
		slog.Warn("environment credential was not imported because it could not be confirmed",
			"provider", providerID, "account", accountID, "err", err)
		return
	}

	created, err := st.CreateProviderAccountCredentialIfAbsent(ctx, core.ProviderAccount{
		ID:            core.ProviderAccountID(accountID),
		Provider:      providerID,
		Label:         accountID,
		CredentialRef: credentialRef,
		Metadata:      metadata,
		// Confirmed just above, so this claim is now earned.
		Status: core.AccountAuthenticated,
	}, []byte(token))
	if err != nil {
		slog.Warn("import bootstrap provider account", "provider", providerID, "err", err)
		return
	}
	if created {
		slog.Info("imported and confirmed a provider account from the environment",
			"provider", providerID, "account", accountID)
	}
}

// Launch modes.
const (
	// LaunchManual starts nothing automatically. Connections are opened one at
	// a time, deliberately.
	LaunchManual = "manual"
	// LaunchAuto honours each connection's own AutoStart setting.
	LaunchAuto = "auto"
)

// launchModeEnv overrides the launch mode without editing anything.
const launchModeEnv = "PORTICO_LAUNCH_MODE"

// launchMode reports whether the supervisor arms connections at startup.
//
// It is a single gate in front of the per-connection AutoStart flag rather than
// a replacement for it: manual means nothing starts by itself whatever the
// connections say, and auto honours each connection's own choice. That gives one
// switch for "don't touch anything yet" without losing per-connection control.
//
// The default is auto so an existing setup keeps behaving as it did.
func (s *Supervisor) launchMode() string {
	if mode, pinned := launchModeOverride(); pinned {
		return mode
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.launch == LaunchManual {
		return LaunchManual
	}
	return LaunchAuto
}

// launchModeOverride reports the mode forced by the environment, if any.
//
// This is separated from launchMode so callers can tell "the mode is auto"
// apart from "the mode is auto and nothing you do here will change it". A
// toggle that silently fails to take effect reads as a bug; one that says it is
// pinned, and by what, is actionable.
func launchModeOverride() (string, bool) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(launchModeEnv)))
	if mode == "" {
		return "", false
	}
	if mode == LaunchManual {
		return LaunchManual, true
	}
	return LaunchAuto, true
}

// ValidLaunchMode reports whether a mode name is one Portico accepts.
//
// Callers at a boundary must reject an unrecognised mode rather than letting
// SetLaunchMode's fallback coerce it: a typo silently arming every connection
// is the opposite of what this gate is for.
func ValidLaunchMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case LaunchManual, LaunchAuto:
		return true
	}
	return false
}

// SetLaunchMode changes the gate and persists it.
//
// The choice now survives a supervisor restart, which is what a user selecting
// it expects. It was previously runtime-only, so the interface had to report
// Persistent=false and tell the user their explicit selection would be
// forgotten — a setting that does not persist is a session preference wearing a
// setting's label.
//
// An environment override is not written: it is not the user's stored
// preference, and persisting it would leave the override's value behind after it
// was unset.
func (s *Supervisor) SetLaunchMode(mode string) error {
	normalized := LaunchAuto
	if strings.EqualFold(strings.TrimSpace(mode), LaunchManual) {
		normalized = LaunchManual
	}

	s.mu.Lock()
	s.launch = normalized
	s.mu.Unlock()

	if _, pinned := launchModeOverride(); pinned {
		// The environment decides. The stored value is left alone so unsetting
		// the override restores whatever the user had chosen.
		return nil
	}
	return config.SaveLaunchMode(normalized)
}

// LaunchModePersistent reports whether the mode in effect is stored.
//
// A pinned mode is not stored, so it does not survive unsetting the override;
// everything else is written to the config file and does survive a restart.
func LaunchModePersistent() bool {
	_, pinned := launchModeOverride()
	return !pinned
}
