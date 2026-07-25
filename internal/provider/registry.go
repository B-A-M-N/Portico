package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/paoloanzn/portico/internal/core"
)

// Registry manages provider instances
type Registry interface {
	Get(id core.ProviderID) core.Provider
	List() []ProviderSnapshot
	Snapshot() []ProviderSnapshot
	Add(provider core.Provider) error
	DiscoverIdentities(ctx context.Context) []ProviderSnapshot
	SetAccounts(providerID core.ProviderID, accounts []core.ProviderAccountID)
	GetAccounts(providerID core.ProviderID) []core.ProviderAccountID
}

// ProviderSnapshot is a frozen provider summary for listing
type ProviderSnapshot struct {
	ID            core.ProviderID
	Name          string
	DisplayName   string
	Capabilities  core.Capabilities
	Authenticated bool
	Accounts      []AccountInfo
}

// AccountInfo describes a configured provider account
type AccountInfo struct {
	ID     core.ProviderAccountID
	Label  string
	Status string
}

// FilteredProvider is a provider that was filtered out with a reason
type FilteredProvider struct {
	Provider core.ProviderID
	Reason   string
}

// registry is the default in-memory registry
type registry struct {
	mu        sync.RWMutex
	providers map[core.ProviderID]core.Provider
	accounts  map[core.ProviderID][]core.ProviderAccountID
}

// NewRegistry creates a new provider registry
func NewRegistry() *registry {
	return &registry{
		providers: make(map[core.ProviderID]core.Provider),
		accounts:  make(map[core.ProviderID][]core.ProviderAccountID),
	}
}

// Get returns a provider by ID
func (r *registry) Get(id core.ProviderID) core.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[id]
}

// Snapshot returns all providers in deterministic ID order (alias for List)
func (r *registry) Snapshot() []ProviderSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listInternalLocked()
}

func (r *registry) listInternalLocked() []ProviderSnapshot {
	// Sort provider IDs for deterministic ordering
	ids := make([]core.ProviderID, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return string(ids[i]) < string(ids[j])
	})

	result := make([]ProviderSnapshot, 0, len(ids))
	for _, id := range ids {
		p := r.providers[id]
		ident := p.Identity()

		// Populate capabilities by calling the provider.
		// Capabilities should be deterministic and not require network calls
		// — I/O from this path would block the registry lock.
		caps, _ := p.Capabilities(context.Background())

		// Get account info
		accountIDs := r.accounts[id]
		accounts := make([]AccountInfo, len(accountIDs))
		for i, accID := range accountIDs {
			accounts[i] = AccountInfo{
				ID:     accID,
				Label:  string(accID),
				Status: "configured",
			}
		}

		result = append(result, ProviderSnapshot{
			ID:            ident.ID,
			Name:          ident.Name,
			DisplayName:   ident.DisplayName,
			Capabilities:  caps,
			Authenticated: len(accounts) > 0,
			Accounts:      accounts,
		})
	}
	return result
}

// List returns all providers in deterministic ID order
func (r *registry) List() []ProviderSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listInternalLocked()
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

// SetAccounts associates accounts with a provider
func (r *registry) SetAccounts(providerID core.ProviderID, accounts []core.ProviderAccountID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[providerID] = accounts
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

// DiscoverIdentities returns providers with account information.
func (r *registry) DiscoverIdentities(ctx context.Context) []ProviderSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snaps := r.listInternalLocked()
	for i := range snaps {
		if p := r.providers[snaps[i].ID]; p != nil {
			if caps, err := p.Capabilities(ctx); err == nil {
				snaps[i].Capabilities = caps
			}
		}
	}
	return snaps
}
