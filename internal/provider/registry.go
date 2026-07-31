package provider

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// Registry manages provider instances
type Registry interface {
	Get(id core.ProviderID) core.Provider
	List() []ProviderSnapshot
	Snapshot() []ProviderSnapshot
	Add(provider core.Provider) error
	// Replace installs a provider, superseding any existing adapter with the
	// same ID. It exists so account changes can rebuild an adapter in place
	// rather than requiring a supervisor restart.
	Replace(provider core.Provider)
	// Remove drops a provider, for when its last usable account is gone.
	Remove(id core.ProviderID)
	// AddCatalogEntry keeps a provider visible when no adapter could be
	// constructed for it, so the UI can explain the gap instead of omitting it.
	AddCatalogEntry(entry CatalogEntry)
	// Install replaces adapter, accounts and catalog entry atomically.
	Install(inst Installation)
	DiscoverIdentities(ctx context.Context) []ProviderSnapshot
	SetAccounts(providerID core.ProviderID, accounts []core.ProviderAccountID)
	SetAccountInfo(providerID core.ProviderID, accounts []AccountInfo)
	GetAccounts(providerID core.ProviderID) []core.ProviderAccountID
}

// Availability describes whether a provider can currently be used, and when it
// cannot, why. A provider that Portico knows about must remain visible with a
// reason rather than disappearing from the catalog, otherwise the UI cannot
// distinguish "this provider does not exist" from "its client is not installed".
type Availability string

const (
	// AvailabilityReady means a live adapter is registered and usable.
	AvailabilityReady Availability = "ready"
	// AvailabilityUnconfigured means the adapter exists but has no usable
	// account or credential yet.
	AvailabilityUnconfigured Availability = "unconfigured"
	// AvailabilityClientMissing means the provider's local client binary was
	// not found, so no adapter could be constructed.
	AvailabilityClientMissing Availability = "client_missing"
	// AvailabilityExperimental means the adapter exists but is not
	// lifecycle-complete and is gated behind an explicit opt-in.
	AvailabilityExperimental Availability = "experimental"
	// AvailabilityNotImplemented means Portico names the provider but ships no
	// adapter for it.
	AvailabilityNotImplemented Availability = "not_implemented"
	// AvailabilityDegraded means an adapter is registered but reported an
	// error when queried.
	AvailabilityDegraded Availability = "degraded"
)

// ProviderSnapshot is a frozen provider summary for listing
type ProviderSnapshot struct {
	ID           core.ProviderID
	Name         string
	DisplayName  string
	Capabilities core.Capabilities
	// Authenticated reports that at least one account can actually be used.
	// It is derived from account status, not from how many accounts exist: an
	// account saved without its credential being checked must not make a
	// provider look ready.
	Authenticated bool
	// Accounts are the accounts usable for planning, selection and opening.
	Accounts []AccountInfo
	// PendingAccounts are configured but not usable — saved without
	// verification, expired or revoked. They are reported separately so they
	// can be repaired from the provider screen rather than silently
	// disappearing, and so nothing offers them for work.
	PendingAccounts []AccountInfo

	// Availability and Reason explain why a catalogued provider is not usable.
	// Registered adapters report ready or unconfigured; catalog-only entries
	// report why no adapter exists.
	Availability Availability
	Reason       string
	// SetupActions are the concrete steps a user can take to make this
	// provider usable.
	SetupActions []string
	// CapabilityError records a failed capability query instead of silently
	// presenting a zero-valued capability set as fact.
	CapabilityError string
}

// Usable reports whether an account may be selected, planned against or opened.
//
// The check is an allowlist. "authenticated" is an account whose credential was
// confirmed against the provider; "configured" is the registry's own marker for
// an adapter that reported its own accounts, which exist only because a working
// credential built that adapter. Every other value — pending, expired, revoked,
// or anything unrecognised — is not usable, so a status Portico does not
// understand fails closed rather than being presented as working.
func (a AccountInfo) Usable() bool {
	// A reason recorded at activation time overrides the durable status: an
	// account can be authenticated and still unusable, because its credential
	// could not be resolved.
	if a.UnusableReason != "" {
		return false
	}
	switch a.Status {
	case string(core.AccountAuthenticated), accountStatusConfigured:
		return true
	}
	return false
}

// accountStatusConfigured marks an account reported by a live adapter rather
// than loaded from the account store.
const accountStatusConfigured = "configured"

// CatalogEntry describes a provider Portico knows about but has no live adapter
// for. Registering one keeps the provider visible with an explanation.
type CatalogEntry struct {
	ID           core.ProviderID
	Name         string
	DisplayName  string
	Availability Availability
	Reason       string
	SetupActions []string
}

// AccountInfo describes a configured provider account
type AccountInfo struct {
	ID    core.ProviderAccountID
	Label string
	// Status is the durable account status.
	Status string
	// UnusableReason explains why an account cannot currently be used, when
	// that is not already implied by its status.
	//
	// An authenticated account whose credential will not decrypt is unusable,
	// but that is a different fact from an account whose credential was never
	// verified, and the user needs to be told which. Without this the two
	// collapse into "not available" and the actionable difference is lost.
	UnusableReason string
}

// FilteredProvider is a provider that was filtered out with a reason
type FilteredProvider struct {
	Provider core.ProviderID
	Reason   string
}

// registry is the default in-memory registry
type registry struct {
	mu          sync.RWMutex
	providers   map[core.ProviderID]core.Provider
	accounts    map[core.ProviderID][]core.ProviderAccountID
	accountInfo map[core.ProviderID][]AccountInfo
	catalog     map[core.ProviderID]CatalogEntry
}

// NewRegistry creates a new provider registry
func NewRegistry() *registry {
	return &registry{
		providers:   make(map[core.ProviderID]core.Provider),
		accounts:    make(map[core.ProviderID][]core.ProviderAccountID),
		accountInfo: make(map[core.ProviderID][]AccountInfo),
		catalog:     make(map[core.ProviderID]CatalogEntry),
	}
}

// AddCatalogEntry records a provider that Portico knows about but cannot
// currently instantiate. A registered adapter always takes precedence, so an
// entry added before registration succeeds is superseded rather than
// duplicated.
func (r *registry) AddCatalogEntry(entry CatalogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry.Name == "" {
		entry.Name = string(entry.ID)
	}
	if entry.DisplayName == "" {
		entry.DisplayName = entry.Name
	}
	r.catalog[entry.ID] = entry
}

// Get returns a provider by ID
func (r *registry) Get(id core.ProviderID) core.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[id]
}

// Snapshot returns all providers in deterministic ID order (alias for List)
func (r *registry) Snapshot() []ProviderSnapshot {
	return r.snapshot(context.Background())
}

// List returns all providers in deterministic ID order
func (r *registry) List() []ProviderSnapshot {
	return r.snapshot(context.Background())
}

// capabilityQueryTimeout bounds a single provider capability query. Capabilities
// are meant to be deterministic and local, but an adapter is free to do work
// here, and the registry must not be held hostage by one.
const capabilityQueryTimeout = 5 * time.Second

// snapshot builds the provider catalog.
//
// Capability queries deliberately run after the registry lock is released. The
// lock previously spanned p.Capabilities(context.Background()), so any adapter
// that performed I/O there would block every other registry reader and writer
// for the duration, with no timeout. References are copied under the lock and
// queried outside it, with a bounded context per provider.
func (r *registry) snapshot(ctx context.Context) []ProviderSnapshot {
	type held struct {
		provider core.Provider
		accounts []AccountInfo
	}

	r.mu.RLock()
	live := make(map[core.ProviderID]held, len(r.providers))
	for id, p := range r.providers {
		accounts := append([]AccountInfo(nil), r.accountInfo[id]...)
		if len(accounts) == 0 {
			for _, accID := range r.accounts[id] {
				accounts = append(accounts, AccountInfo{ID: accID, Label: string(accID), Status: "configured"})
			}
		}
		live[id] = held{provider: p, accounts: accounts}
	}
	catalog := make(map[core.ProviderID]CatalogEntry, len(r.catalog))
	maps.Copy(catalog, r.catalog)
	r.mu.RUnlock()

	ids := make([]core.ProviderID, 0, len(live)+len(catalog))
	for id := range live {
		ids = append(ids, id)
	}
	for id := range catalog {
		if _, registered := live[id]; !registered {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return string(ids[i]) < string(ids[j]) })

	result := make([]ProviderSnapshot, 0, len(ids))
	for _, id := range ids {
		h, registered := live[id]
		if !registered {
			entry := catalog[id]
			result = append(result, ProviderSnapshot{
				ID:           entry.ID,
				Name:         entry.Name,
				DisplayName:  entry.DisplayName,
				Availability: entry.Availability,
				Reason:       entry.Reason,
				SetupActions: append([]string(nil), entry.SetupActions...),
			})
			continue
		}

		ident := h.provider.Identity()
		capCtx, cancel := context.WithTimeout(ctx, capabilityQueryTimeout)
		caps, capErr := h.provider.Capabilities(capCtx)
		cancel()

		// An account is only usable if its credential was actually accepted.
		// Counting rows here is what let an unverified account make a provider
		// look ready after a restart.
		var usable, pending []AccountInfo
		for _, account := range h.accounts {
			if account.Usable() {
				usable = append(usable, account)
			} else {
				pending = append(pending, account)
			}
		}

		snap := ProviderSnapshot{
			ID:              ident.ID,
			Name:            ident.Name,
			DisplayName:     ident.DisplayName,
			Capabilities:    caps,
			Authenticated:   len(usable) > 0,
			Accounts:        usable,
			PendingAccounts: pending,
		}
		switch {
		case capErr != nil:
			// A failed capability query must not be reported as an empty but
			// authoritative capability set.
			snap.Availability = AvailabilityDegraded
			snap.CapabilityError = capErr.Error()
			snap.Reason = "provider capabilities could not be read: " + capErr.Error()
		case len(usable) > 0:
			snap.Availability = AvailabilityReady
		case len(pending) > 0:
			// Distinguishing this from "no account" is the difference between
			// "add one" and "finish the one you started".
			snap.Availability = AvailabilityUnconfigured
			snap.Reason = fmt.Sprintf(
				"%d account(s) are saved but not usable yet; their credentials were never confirmed",
				len(pending))
		default:
			snap.Availability = AvailabilityUnconfigured
		}
		// A catalog entry may still carry setup guidance for a registered but
		// unconfigured provider.
		if entry, ok := catalog[id]; ok {
			snap.SetupActions = append([]string(nil), entry.SetupActions...)
			if snap.Reason == "" {
				snap.Reason = entry.Reason
			}
			if entry.Availability == AvailabilityExperimental {
				snap.Availability = AvailabilityExperimental
			}
		}
		result = append(result, snap)
	}
	return result
}

// Add adds a provider to the registry
func (r *registry) Add(provider core.Provider) error {
	identity := provider.Identity()

	r.mu.Lock()
	defer r.mu.Unlock()

	// Reject duplicate provider IDs
	if _, exists := r.providers[identity.ID]; exists {
		return fmt.Errorf("provider %s already registered", identity.ID)
	}

	r.providers[identity.ID] = provider
	return nil
}

// Replace installs a provider, superseding any existing adapter with the same
// ID.
func (r *registry) Replace(provider core.Provider) {
	identity := provider.Identity()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[identity.ID] = provider
}

// Install replaces a provider's adapter, account projection and catalog entry
// as one operation.
//
// Doing these as three calls takes three locks, so a concurrent snapshot can
// observe a new adapter alongside the previous account list, or a catalog
// reason that contradicts the installed adapter. Callers see one state or the
// other and never a mixture.
//
// A nil Provider installs the catalog entry alone, which is how a provider that
// cannot currently be used stays visible with a reason instead of vanishing.
func (r *registry) Install(inst Installation) {
	id := inst.Catalog.ID
	if inst.Provider != nil {
		id = inst.Provider.Identity().ID
	}
	if id == "" {
		return
	}

	entry := inst.Catalog
	entry.ID = id
	if entry.Name == "" {
		entry.Name = string(id)
	}
	if entry.DisplayName == "" {
		entry.DisplayName = entry.Name
	}

	infos := make([]AccountInfo, 0, len(inst.Accounts))
	ids := make([]core.ProviderAccountID, 0, len(inst.Accounts))
	for _, account := range inst.Accounts {
		if account.Label == "" {
			account.Label = string(account.ID)
		}
		if account.Status == "" {
			account.Status = accountStatusConfigured
		}
		infos = append(infos, account)
		ids = append(ids, account.ID)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.catalog[id] = entry
	if inst.Provider == nil {
		delete(r.providers, id)
	} else {
		r.providers[id] = inst.Provider
	}
	r.accounts[id] = ids
	r.accountInfo[id] = infos
}

// Remove drops a provider and the account projection that went with it.
func (r *registry) Remove(id core.ProviderID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.providers, id)
	delete(r.accounts, id)
	delete(r.accountInfo, id)
}

// SetAccounts associates accounts with a provider
func (r *registry) SetAccounts(providerID core.ProviderID, accounts []core.ProviderAccountID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[providerID] = append([]core.ProviderAccountID(nil), accounts...)
	infos := make([]AccountInfo, len(accounts))
	for i, accountID := range accounts {
		infos[i] = AccountInfo{ID: accountID, Label: string(accountID), Status: "configured"}
	}
	r.accountInfo[providerID] = infos
}

// SetAccountInfo associates non-secret display metadata with configured
// accounts. It also replaces the account ID projection used for validation.
func (r *registry) SetAccountInfo(providerID core.ProviderID, accounts []AccountInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	infos := append([]AccountInfo(nil), accounts...)
	ids := make([]core.ProviderAccountID, 0, len(infos))
	for i := range infos {
		if infos[i].Label == "" {
			infos[i].Label = string(infos[i].ID)
		}
		if infos[i].Status == "" {
			infos[i].Status = "configured"
		}
		ids = append(ids, infos[i].ID)
	}
	r.accounts[providerID] = ids
	r.accountInfo[providerID] = infos
}

// GetAccounts returns the accounts associated with a provider
func (r *registry) GetAccounts(providerID core.ProviderID) []core.ProviderAccountID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	accounts := r.accounts[providerID]
	// Return a copy to prevent external mutation
	result := make([]core.ProviderAccountID, len(accounts))
	copy(result, accounts)
	return result
}

// DiscoverIdentities returns providers with account information, using the
// caller's context to bound capability queries.
func (r *registry) DiscoverIdentities(ctx context.Context) []ProviderSnapshot {
	return r.snapshot(ctx)
}
