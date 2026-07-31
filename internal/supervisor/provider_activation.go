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

// activateAll installs every known provider.
func (s *Supervisor) activateAll(ctx context.Context) {
	if s.activation == nil {
		return
	}
	for _, def := range s.activation.definitions {
		s.activateDefinition(ctx, def)
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
	s.activateDefinition(ctx, def)
	return nil
}

// activateDefinition runs the fixed per-provider sequence.
//
// The order is the contract: the catalog entry is installed first so the
// provider is visible before anything can fail, and the runtime is built fully
// before anything is installed so a failure cannot leave a half-installed
// provider.
func (s *Supervisor) activateDefinition(ctx context.Context, def provider.Definition) {
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

	// 2. Explicit opt-in.
	if gated, ok := def.(gatedDefinition); ok && !gated.Enabled() {
		s.registry.Install(provider.Installation{Catalog: entry})
		return
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
				s.registry.Install(provider.Installation{Catalog: req.MissingBinaryEntry()})
				return
			}
		}
	}

	// 4. Usable accounts, with their credentials resolved.
	accounts, unusable := s.activationAccounts(ctx, def)
	defer func() {
		for i := range accounts {
			zeroBytes(accounts[i].Secret)
		}
	}()

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
		failed.Reason = "this provider could not be started: " + err.Error()
		slog.Warn("provider activation failed", "provider", id, "err", err)
		if existing := s.registry.Get(id); existing != nil {
			s.registry.Install(provider.Installation{
				Provider: existing, Catalog: failed,
				Accounts: append(accountInfos(accounts), unusable...),
			})
			return
		}
		s.registry.Install(provider.Installation{Catalog: failed})
		return
	}

	// 6. Install. Unusable accounts are carried through so they can be
	// repaired rather than silently disappearing from the provider screen.
	inst.Accounts = append(inst.Accounts, unusable...)
	sort.Slice(inst.Accounts, func(i, j int) bool { return inst.Accounts[i].ID < inst.Accounts[j].ID })
	if inst.Catalog.ID == "" {
		inst.Catalog = entry
	}
	s.registry.Install(inst)
}

// activateSafely converts a panic in provider construction into an error.
func activateSafely(ctx context.Context, def provider.Definition, req provider.ActivationRequest) (
	inst provider.Installation, err error,
) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("provider panicked while starting: %v", r)
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
	[]provider.AccountMaterial, []provider.AccountInfo,
) {
	id := def.Identity().ID
	// A provider that stores no accounts is never handed any.
	if flow, ok := def.(accountlessDefinition); ok && !flow.SetupFlow().StoresAccount() {
		return nil, nil
	}

	stored, err := s.store.ListProviderAccounts(ctx)
	if err != nil {
		slog.Warn("list provider accounts", "provider", id, "err", err)
		return nil, nil
	}

	var usable []provider.AccountMaterial
	var unusable []provider.AccountInfo
	for _, account := range stored {
		if account.Provider != id {
			continue
		}
		info := provider.AccountInfo{
			ID: account.ID, Label: account.Label, Status: string(account.Status),
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
			continue
		}
		usable = append(usable, provider.AccountMaterial{Account: account, Secret: []byte(token)})
	}
	sort.Slice(usable, func(i, j int) bool { return usable[i].Account.ID < usable[j].Account.ID })
	return usable, unusable
}

// accountInfos projects account material for display.
func accountInfos(materials []provider.AccountMaterial) []provider.AccountInfo {
	infos := make([]provider.AccountInfo, 0, len(materials))
	for _, m := range materials {
		infos = append(infos, provider.AccountInfo{
			ID: m.Account.ID, Label: m.Account.Label, Status: string(m.Account.Status),
		})
	}
	return infos
}
