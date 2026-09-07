package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"sync"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// binaryRequirer is an optional capability for a definition whose provider
// cannot run without a client binary.
type binaryRequirer interface {
	RequiredBinary() string
	MissingBinaryEntry() provider.CatalogEntry
}

// gatedDefinition is an optional capability for a definition behind an explicit
// opt-in. The gate is evaluated here rather than inside Activate because it
// reads the supervisor's environment, which cannot change in-process.
type gatedDefinition interface{ Enabled() bool }

// accountlessDefinition is a definition that never consumes accounts. A
// guidance provider is one: it stores nothing, so passing it accounts would
// imply it can use them.
type accountlessDefinition interface{ SetupFlow() core.SetupFlow }

// activationCoordinator turns durable state into installed providers.
//
// It is the only thing that mutates the registry, and it runs the same sequence
// at startup and after an account change. Previously each provider had its own
// registration function implementing its own partial subset of gate evaluation,
// binary lookup, account loading, construction and catalog fallback — which is
// why two Cloudflare construction paths could drift into producing different
// capabilities from the same durable state.
type activationCoordinator struct {
	mu          sync.Mutex
	definitions []provider.Definition
	services    provider.RuntimeServices
}

// SetProviderDefinitions installs the provider composition list and the runtime
// services activation hands to it.
//
// It is a setter rather than a constructor argument so the supervisor can be
// built in tests without a provider catalogue, matching how its other
// collaborators are wired.
func (s *Supervisor) SetProviderDefinitions(defs []provider.Definition, services provider.RuntimeServices) {
	s.activation = &activationCoordinator{definitions: defs, services: services}
}

// definitionFor returns the definition for one provider ID.
func (c *activationCoordinator) definitionFor(id core.ProviderID) provider.Definition {
	// Compatibility for profiles and setup requests created before the
	// workload/transport split. The alias is resolved only at lookup time; the
	// provider registry contains the transport ID alone.
	id = core.NormalizeProviderID(id)
	for _, def := range c.definitions {
		if def.Identity().ID == id {
			return def
		}
	}
	return nil
}

// setupDefinitionFor resolves a provider's setup capability from its
// definition.
//
// Setup requirements are static, so they must not require a constructed
// adapter. Requiring one meant a provider that was switched off, or whose
// client was not installed, could not tell you what it needed — which is the
// moment that information matters most.
func (s *Supervisor) setupDefinitionFor(id string) (provider.SetupDefinition, error) {
	id = string(core.NormalizeProviderID(core.ProviderID(id)))
	var def provider.Definition
	if s.activation != nil {
		def = s.activation.definitionFor(core.ProviderID(id))
	}
	if def == nil {
		// A provider Portico has an adapter for but no definition of is a
		// provider that cannot be configured, not one that does not exist.
		// Saying "not found" about something on screen is wrong.
		if s.registry != nil && s.registry.Get(core.ProviderID(id)) != nil {
			return nil, core.ErrValidation(
				fmt.Sprintf("provider %q cannot be configured through Portico", id))
		}
		if err := s.catalogOnlySetupError(id); err != nil {
			return nil, err
		}
		return nil, core.ErrProviderNotFound(core.ProviderID(id))
	}
	setup, ok := def.(provider.SetupDefinition)
	if !ok {
		return nil, core.ErrValidation(
			fmt.Sprintf("provider %q cannot be configured through Portico", id))
	}
	return setup, nil
}

// setupFlowFor returns a provider's declared setup flow.
func (s *Supervisor) setupFlowFor(id string) (core.SetupFlow, error) {
	setup, err := s.setupDefinitionFor(id)
	if err != nil {
		return core.SetupFlow{}, err
	}
	return setup.SetupFlow(), nil
}

// setupKindFor reports what setting a provider up through Portico does:
// "account" when completing the flow stores a credential, "guidance" when it
// only describes what to do elsewhere, and empty when the provider declares no
// setup flow at all. It reads the provider's own definition — the same source
// the setup endpoint serves fields from — because the alternative was each
// client inferring capability from provider names, which is how "Add account"
// got offered for providers that cannot hold one.
//
// Setup capability is static, so this deliberately does not require a
// constructed adapter: a provider whose client is missing is exactly the one a
// user needs to set up.
func (s *Supervisor) setupKindFor(id string) string {
	setup, err := s.setupDefinitionFor(id)
	if err != nil {
		return ""
	}
	return string(setup.SetupFlow().Kind)
}

// activateAll installs every known provider.
func (s *Supervisor) activateAll(ctx context.Context) {
	if s.activation == nil {
		return
	}
	for _, def := range s.activation.definitions {
		_ = s.activateDefinition(ctx, def)
	}
}

// ActivateProvider rebuilds one provider from durable state and installs it.
//
// This replaces the provider-specific rebuild that existed only for Cloudflare,
// which is why saving an account for any other provider could only report
// "restart required".
func (s *Supervisor) ActivateProvider(ctx context.Context, id core.ProviderID) error {
	if s.activation == nil {
		return fmt.Errorf("the supervisor is not fully initialised; provider activation is unavailable")
	}
	def := s.activation.definitionFor(id)
	if def == nil {
		return core.ErrProviderNotFound(id)
	}
	return s.activateDefinition(ctx, def)
}

// activateDefinition runs the fixed per-provider sequence.
//
// The order is the contract: the catalog entry is installed first so the
// provider is visible before anything can fail, and the runtime is built fully
// before anything is installed so a failure cannot leave a half-installed
// provider.
func (s *Supervisor) activateDefinition(ctx context.Context, def provider.Definition) error {
	c := s.activation
	c.mu.Lock()
	defer c.mu.Unlock()

	id := def.Identity().ID

	// 1. Visible first. Every later failure updates this entry rather than
	// removing anything, so a provider never disappears.
	entry := def.CatalogEntry()
	if s.registry.Get(id) == nil {
		s.registry.Install(provider.Installation{Catalog: entry})
	}

	// Accounts are loaded before any early return, because a provider that
	// cannot run right now must still show the accounts already configured for
	// it. Omitting them made a user's accounts vanish from the provider screen
	// the moment its client was uninstalled, and made the same durable state
	// project differently at startup than after a live change.
	accounts, unusable, accountsErr := s.activationAccounts(ctx, def)
	defer func() {
		for i := range accounts {
			zeroBytes(accounts[i].Secret)
		}
	}()
	knownAccounts := append(accountInfos(accounts), unusable...)
	if accountsErr != nil {
		failed := entry
		failed.Availability = provider.AvailabilityDegraded
		failed.Reason = "provider activation failed"
		slog.Warn("provider activation failed", "provider", id, "err", accountsErr)
		s.registry.Install(provider.Installation{
			Provider: s.registry.Get(id), Catalog: failed, Accounts: knownAccounts,
		})
		return accountsErr
	}

	// 2. Explicit opt-in.
	if gated, ok := def.(gatedDefinition); ok && !gated.Enabled() {
		s.registry.Install(provider.Installation{Catalog: entry, Accounts: knownAccounts})
		return nil
	}

	// 3. Client binary.
	if req, ok := def.(binaryRequirer); ok {
		if bin := req.RequiredBinary(); bin != "" {
			lookPath := c.services.LookPath
			if lookPath == nil {
				lookPath = exec.LookPath
			}
			if _, err := lookPath(bin); err != nil {
				slog.Info("provider client not found", "provider", id, "bin", bin)
				s.registry.Install(provider.Installation{
					Catalog: req.MissingBinaryEntry(), Accounts: knownAccounts,
				})
				return nil
			}
		}
	}

	// 5. Build, containing a panic so one provider cannot take down startup or
	// an IPC handler goroutine.
	inst, err := activateSafely(ctx, def, provider.ActivationRequest{
		Accounts: accounts,
		Services: c.services,
	})
	if err != nil {
		// The previously installed adapter is retained deliberately: dropping a
		// working adapter because a re-activation failed would break Observe
		// for connections that are currently open.
		failed := entry
		failed.Availability = provider.AvailabilityDegraded
		failed.Reason = "provider activation failed"
		slog.Warn("provider activation failed", "provider", id, "err", err)
		// The account projection is the same either way: a failure must not
		// change which accounts exist, only whether a runtime does.
		s.registry.Install(provider.Installation{
			Provider: s.registry.Get(id), Catalog: failed, Accounts: knownAccounts,
		})
		return err
	}

	// 6. Install. Unusable accounts are carried through so they can be
	// repaired rather than silently disappearing from the provider screen.
	inst.Accounts = append(inst.Accounts, unusable...)
	sort.Slice(inst.Accounts, func(i, j int) bool { return inst.Accounts[i].ID < inst.Accounts[j].ID })
	if inst.Catalog.ID == "" {
		inst.Catalog = entry
	}
	s.registry.Install(inst)
	return nil
}

// activateSafely converts a panic in provider construction into an error.
func activateSafely(ctx context.Context, def provider.Definition, req provider.ActivationRequest) (
	inst provider.Installation, err error,
) {
	defer func() {
		if r := recover(); r != nil {
			// Panic values can contain credentials or provider response bodies;
			// preserve only the safe classification at this boundary.
			err = fmt.Errorf("provider panicked while starting")
		}
	}()
	return def.Activate(ctx, req)
}

// activationAccounts loads the accounts a definition may use, and describes the
// ones it may not.
//
// An account whose credential cannot be resolved is reported with that reason
// rather than skipped. Skipping it is what made an authenticated account
// indistinguishable from one that was never configured.
func (s *Supervisor) activationAccounts(ctx context.Context, def provider.Definition) (
	[]provider.AccountMaterial, []provider.AccountInfo, error,
) {
	id := def.Identity().ID
	// A provider that stores no accounts is never handed any.
	if flow, ok := def.(accountlessDefinition); ok && !flow.SetupFlow().StoresAccount() {
		return nil, nil, nil
	}

	stored, err := s.store.ListProviderAccounts(ctx)
	if err != nil {
		slog.Warn("list provider accounts", "provider", id, "err", err)
		return nil, nil, fmt.Errorf("list provider accounts: %w", err)
	}

	var usable []provider.AccountMaterial
	var unusable []provider.AccountInfo
	for _, account := range stored {
		if account.Provider != id {
			continue
		}
		info := provider.AccountInfo{
			ID: account.ID, Label: account.Label, Status: string(account.Status),
			// Zone naming metadata rides along so clients can propose a
			// hostname instead of asking the user to retype a domain the
			// supervisor already knows.
			ZoneID:   account.Metadata["zone_id"],
			ZoneName: account.Metadata["zone_name"],
		}
		if !info.Usable() {
			unusable = append(unusable, info)
			continue
		}
		if account.CredentialRef == "" {
			info.UnusableReason = "no credential is recorded for this account"
			unusable = append(unusable, info)
			continue
		}
		token, loadErr := s.store.LoadProviderCredential(ctx, id, account.CredentialRef)
		if loadErr != nil || token == "" {
			// Authenticated but unusable is a different fact from never
			// verified, and the user has to be told which.
			info.UnusableReason = "its stored credential could not be read"
			unusable = append(unusable, info)
			if loadErr != nil {
				return usable, unusable, fmt.Errorf("load credential for account %s: %w", account.ID, loadErr)
			}
			continue
		}
		usable = append(usable, provider.AccountMaterial{Account: account, Secret: []byte(token)})
	}
	sort.Slice(usable, func(i, j int) bool { return usable[i].Account.ID < usable[j].Account.ID })
	return usable, unusable, nil
}

// accountInfos projects account material for display.
func accountInfos(materials []provider.AccountMaterial) []provider.AccountInfo {
	infos := make([]provider.AccountInfo, 0, len(materials))
	for _, m := range materials {
		infos = append(infos, provider.AccountInfo{
			ID:       m.Account.ID,
			Label:    m.Account.Label,
			Status:   string(m.Account.Status),
			ZoneID:   m.Account.Metadata["zone_id"],
			ZoneName: m.Account.Metadata["zone_name"],
		})
	}
	return infos
}

// verifyBootstrapCredential checks an environment credential before it is
// imported as an authenticated account.
//
// It prefers the provider's own verifier and falls back to the supervisor's
// account validator, which is where Cloudflare's check currently lives. A
// provider offering neither returns an error, so nothing unchecked is ever
// recorded as authenticated.
func (s *Supervisor) verifyBootstrapCredential(
	ctx context.Context, providerID core.ProviderID, accountID, token string,
) error {
	if s.activation != nil {
		if def := s.activation.definitionFor(providerID); def != nil {
			if setup, ok := def.(provider.SetupDefinition); ok {
				if verifier, ok := setup.(provider.SetupVerifier); ok {
					_, err := verifier.VerifyAccount(ctx, provider.PreparedAccount{
						Account: core.ProviderAccount{
							ID: core.ProviderAccountID(accountID), Provider: providerID,
						},
						Secret: []byte(token),
					})
					return err
				}
			}
		}
	}

	validator := s.accountValidator
	if validator == nil {
		validator = cloudflareAccountValidator{}
	}
	validation, err := validator.Validate(ctx, string(providerID), accountID, token)
	if err != nil {
		return err
	}
	if validation == nil || !validation.AccountAccessible {
		return fmt.Errorf("the credential could not be confirmed against %s", providerID)
	}
	return nil
}
