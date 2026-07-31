package provider

import (
	"context"

	"github.com/B-A-M-N/portico/internal/core"
)

// Definition is what Portico knows about a provider before any adapter exists.
//
// It is separate from the adapter on purpose. An adapter is a constructed
// runtime that can plan, execute and observe; a definition is the static,
// provider-owned description of how to build one and what to say when it cannot
// be built. Fusing them is why a provider with no constructed adapter could not
// report its own setup requirements, and why every activation path had to be
// written by hand in the supervisor.
type Definition interface {
	// Identity names the provider. The identity of an adapter returned by
	// Activate must match it.
	Identity() core.ProviderIdentity
	// CatalogEntry describes the provider when no adapter is installed, so it
	// stays visible with a reason rather than disappearing.
	CatalogEntry() CatalogEntry
	// Activate builds the runtime for a set of usable accounts.
	//
	// It must not mutate durable state. It receives already-decrypted
	// credentials and no store handle, so this is structural rather than a
	// rule to remember.
	Activate(ctx context.Context, req ActivationRequest) (Installation, error)
}

// SetupDefinition is an optional capability for a provider that can be
// configured through Portico.
//
// PrepareAccount belongs to the provider because only the provider knows which
// declared field identifies an account, what its label should default to, which
// values are metadata, and how its credential reference is derived. The
// supervisor previously guessed all four.
type SetupDefinition interface {
	Definition
	SetupFlow() core.SetupFlow
	PrepareAccount(values map[string]string) (PreparedAccount, error)
}

// SetupVerifier is an optional capability for a provider that can check a
// credential against the real service.
//
// It is the only thing that may promote an account to authenticated. A provider
// that does not implement it stores accounts that stay pending, and the caller
// must say so rather than implying the credential works.
type SetupVerifier interface {
	VerifyAccount(ctx context.Context, account PreparedAccount) (core.SetupValidation, error)
}

// PreparedAccount is a provider's canonical account, built from submitted setup
// values. The status is deliberately absent: assigning it is the supervisor's
// job, and a provider cannot declare its own credential verified.
type PreparedAccount struct {
	Account core.ProviderAccount
	Secret  []byte
}

// AccountMaterial is one usable account with its decrypted credential.
type AccountMaterial struct {
	Account core.ProviderAccount
	// Secret is zeroed by the coordinator once Activate returns, so a
	// definition must not retain the slice.
	Secret []byte
}

// ActivationRequest carries everything needed to build a runtime.
type ActivationRequest struct {
	// Accounts are usable accounts only. A pending, revoked or expired account
	// never reaches a definition.
	Accounts []AccountMaterial
	Services RuntimeServices
}

// RuntimeServices are the process and filesystem dependencies a provider needs.
//
// These are interfaces and plain strings rather than concrete types. In
// particular there is no *store.Store and no app.Paths: app imports the TUI,
// and a store handle would make "activation never writes" a convention instead
// of an impossibility.
type RuntimeServices struct {
	Processes    core.ConnectorProcessService
	ConnectorDir string
	LogDir       string
	// LookPath resolves an executable, injectable so a test does not depend on
	// what happens to be installed.
	LookPath func(name string) (string, error)
	// Getenv reads the supervisor's environment, injectable for the same reason.
	Getenv func(key string) string
}

// Installation is the complete result of activating a provider: the runtime,
// what to display, and which accounts it actually serves.
//
// It is applied to the registry as one operation. Applying the adapter, the
// account projection and the catalog entry separately let a concurrent reader
// observe a new adapter with a stale account list.
type Installation struct {
	// Provider is nil when the provider cannot currently be used. The catalog
	// entry then carries the reason.
	Provider core.Provider
	Catalog  CatalogEntry
	// Accounts are the accounts the runtime serves, plus any that are
	// configured but unusable. The registry splits them; both must be passed
	// or a pending account becomes invisible after a restart.
	Accounts []AccountInfo
}
