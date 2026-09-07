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

// PotentialDefinition is an optional capability for a definition whose
// provider can deliver more after its declared setup flow completes than its
// current adapter reports.
//
// The answer must be static: derivable from the definition alone, with no
// constructed adapter, no account, and no I/O. Cloudflare is the motivating
// case — an accountless activation produces a Quick Tunnel adapter whose
// Capabilities reports no custom hostnames, while the definition knows that
// configuring an account delivers them. Deriving the after-setup answer from
// the live adapter's current answer cannot see that gap, which is how the
// wizard ended up recommending setup actions from providers that could never
// deliver the capability being asked about.
//
// A provider whose capabilities do not change with setup, and a provider
// Portico ships no adapter for, do not implement this.
type PotentialDefinition interface {
	Definition
	PotentialCapabilities(ctx context.Context) (core.Capabilities, error)
}

// VerificationStrength states what a passing VerifyAccount actually proved.
//
// The distinction is load-bearing. A verifier that only checks value shape
// locally — length, charset, no embedded whitespace — has not contacted the
// provider, so a pass must never be recorded as authenticated: that would turn
// "the key looks plausible" into "OpenAI accepted this key", which is false.
// Implement this interface to declare local-only verification and receive a
// provisional account that runtime evidence later promotes or demotes.
type LocalShapeVerifier interface {
	VerificationStrength() VerificationStrength
}

type VerificationStrength string

const (
	// VerificationAuthoritative means VerifyAccount consulted the real
	// provider and its answer is a durable fact about the credential.
	VerificationAuthoritative VerificationStrength = "authoritative"
	// VerificationLocalShape means VerifyAccount checked only the value's
	// plausibility. It rules out truncated pastes; it proves nothing about
	// whether the provider accepts the credential.
	VerificationLocalShape VerificationStrength = "local_shape"
)

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
	Processes core.ConnectorProcessService
	// Gateways is the supervisor-owned gateway lifecycle service used by
	// gateway-fronted transports. It is optional for providers that do not use
	// a gateway.
	Gateways core.GatewayService
	// TunnelCredentials persists per-connection tunnel tokens. It is a narrow
	// capability rather than the store, so a definition can save the tokens it
	// creates without being able to touch provider accounts.
	TunnelCredentials TunnelCredentialStore
	ConnectorDir      string
	LogDir            string
	// LookPath resolves an executable, injectable so a test does not depend on
	// what happens to be installed.
	LookPath func(name string) (string, error)
	// Getenv reads the supervisor's environment, injectable for the same reason.
	Getenv func(key string) string
}

// TunnelCredentialStore is durable storage for tokens a provider creates while
// opening a connection.
//
// It is deliberately not the account store: a provider may persist the tunnel
// token it just minted, and may not read or write provider accounts.
type TunnelCredentialStore interface {
	SaveTunnelCredential(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string, token []byte) error
	LoadTunnelCredentialExact(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string) (string, error)
	DeleteTunnelCredentialExact(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string) error
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
