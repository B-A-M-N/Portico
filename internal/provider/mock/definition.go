package mock

import (
	"context"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// Definition is the static, provider-owned description of the mock provider.
// The real composition root registers the mock as a bare adapter in dev mode,
// but setup (provider login, TUI provider setup) resolves through definitions:
// a setup-capable adapter with no definition reported "cannot be configured
// through Portico" — exactly the component/production disconnect the audit
// exists to catch. This definition makes the mock's declared setup flow
// reachable through the same path production providers use.
type Definition struct{}

// NewDefinition builds the mock provider definition.
func NewDefinition() *Definition { return &Definition{} }

func (d *Definition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "mock", Name: "mock", DisplayName: "Mock Provider"}
}

func (d *Definition) CatalogEntry() provider.CatalogEntry {
	return provider.CatalogEntry{
		ID:           "mock",
		Name:         "mock",
		DisplayName:  "Mock Provider",
		Availability: provider.AvailabilityReady,
		Stability:    core.StabilityStable,
	}
}

// Activate installs the mock adapter itself: the adapter is its own runtime.
// The usable accounts are projected into the installation the same way every
// real definition does — dropping them made accounts disappear from the
// Providers screen the moment a credential replacement re-activated the
// provider.
func (d *Definition) Activate(_ context.Context, req provider.ActivationRequest) (provider.Installation, error) {
	infos := make([]provider.AccountInfo, 0, len(req.Accounts))
	for _, material := range req.Accounts {
		infos = append(infos, provider.AccountInfo{
			ID:     material.Account.ID,
			Label:  material.Account.Label,
			Status: string(material.Account.Status),
		})
	}
	return provider.Installation{
		// The scoped wrapper keeps the account-scoped create path working:
		// named accounts resolve to the adapter, matching dev-mode create
		// with --account-id.
		Provider: NewScoped(),
		Catalog:  d.CatalogEntry(),
		Accounts: infos,
	}, nil
}

// SetupFlow mirrors the adapter's declared flow so both paths agree.
func (d *Definition) SetupFlow() core.SetupFlow { return New().SetupFlow() }

// PrepareAccount mirrors the adapter's account preparation.
func (d *Definition) PrepareAccount(values map[string]string) (provider.PreparedAccount, error) {
	return New().PrepareAccount(values)
}
