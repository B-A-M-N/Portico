package cloudflare

import (
	"context"
	"fmt"
	"sort"

	"github.com/B-A-M-N/portico/internal/core"
)

// AccountsProvider is the Cloudflare registry entry when Portico has more
// than one authenticated account. It never executes provider calls itself:
// the controller first resolves a concrete account-bound Provider through
// ProviderForAccount. That prevents mutable adapter state and credentials from
// bleeding between accounts.
type AccountsProvider struct {
	accounts map[core.ProviderAccountID]*Provider
	ordered  []core.ProviderAccountID
}

// NewAccountsProvider constructs an immutable account router. Every child
// must be a Cloudflare adapter bound to the map key's exact account ID.
func NewAccountsProvider(accounts map[core.ProviderAccountID]*Provider) (*AccountsProvider, error) {
	if len(accounts) == 0 {
		return nil, fmt.Errorf("cloudflare accounts provider requires at least one account")
	}
	copyAccounts := make(map[core.ProviderAccountID]*Provider, len(accounts))
	ordered := make([]core.ProviderAccountID, 0, len(accounts))
	for id, child := range accounts {
		if id == "" || child == nil || child.ProviderAccountID() != id {
			return nil, fmt.Errorf("invalid Cloudflare account adapter for %q", id)
		}
		copyAccounts[id] = child
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return &AccountsProvider{accounts: copyAccounts, ordered: ordered}, nil
}

func (p *AccountsProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "cloudflare", Name: "cloudflare", DisplayName: "Cloudflare"}
}

func (p *AccountsProvider) Capabilities(ctx context.Context) (core.Capabilities, error) {
	child, err := p.defaultProvider()
	if err != nil {
		return core.Capabilities{}, err
	}
	return child.Capabilities(ctx)
}

func (p *AccountsProvider) Authenticate(ctx context.Context, req core.AuthRequest) error {
	child, err := p.ProviderForAccount(req.AccountID)
	if err != nil {
		return err
	}
	return child.Authenticate(ctx, req)
}

func (p *AccountsProvider) Plan(ctx context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	if desired.Profile == nil {
		return nil, fmt.Errorf("cloudflare profile is required")
	}
	child, err := p.ProviderForAccount(desired.Profile.Provider.AccountID)
	if err != nil {
		return nil, err
	}
	return child.Plan(ctx, desired)
}

// ExecuteStep and Observe are deliberately unavailable on the router. The
// controller always resolves a profile to one child first; accepting these
// calls would reintroduce account ambiguity after a restart.
func (p *AccountsProvider) ExecuteStep(context.Context, core.ConnectionID, core.PlanStep) (core.StepResult, error) {
	return core.StepResult{}, fmt.Errorf("cloudflare account must be selected before executing a step")
}

func (p *AccountsProvider) Observe(context.Context, core.ConnectionID) (*core.ObservedConnection, error) {
	return nil, fmt.Errorf("cloudflare account must be selected before observing a connection")
}

// ProviderForAccount implements core.AccountScopedProvider. An empty account
// is accepted only when exactly one account is configured, preserving a
// single-account first-run setup without guessing in a multi-account setup.
func (p *AccountsProvider) ProviderForAccount(accountID core.ProviderAccountID) (core.Provider, error) {
	if accountID == "" {
		if len(p.ordered) != 1 {
			return nil, fmt.Errorf("select a Cloudflare account")
		}
		accountID = p.ordered[0]
	}
	child := p.accounts[accountID]
	if child == nil {
		return nil, fmt.Errorf("cloudflare account %q is unavailable", accountID)
	}
	return child, nil
}

func (p *AccountsProvider) defaultProvider() (*Provider, error) {
	if len(p.ordered) == 0 {
		return nil, fmt.Errorf("no Cloudflare accounts configured")
	}
	return p.accounts[p.ordered[0]], nil
}
