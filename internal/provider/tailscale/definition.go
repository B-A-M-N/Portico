// Package tailscale implements the core.Provider interface for Tailscale.
//
// Tailscale creates a private network overlay between devices. Portico uses
// it to join a tailnet or expose a local service to a tailnet without a
// public address.
//
// This package is currently a SCAFFOLD. It is not wired into the provider
// composition root (see builtin/catalog.go, which keeps Tailscale as
// "not_implemented"). Do not register this adapter until every method below
// is implemented against the real tailscale CLI/LocalAPI.
package tailscale

import (
	"context"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// DefinitionConfig carries values the composition root reads from configuration.
type DefinitionConfig struct {
	Bin string
}

// Definition describes Tailscale to Portico before any adapter exists.
type Definition struct{ cfg DefinitionConfig }

// NewDefinition builds the Tailscale provider definition.
func NewDefinition(cfg DefinitionConfig) *Definition {
	return &Definition{cfg: cfg}
}

func (d *Definition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "tailscale", Name: "tailscale", DisplayName: "Tailscale"}
}

func (d *Definition) CatalogEntry() provider.CatalogEntry {
	return provider.CatalogEntry{
		ID:           "tailscale",
		Name:         "tailscale",
		DisplayName:  "Tailscale",
		Availability: provider.AvailabilityNotImplemented,
		Stability:    core.StabilityExperimental,
		Reason:       "Portico ships no Tailscale adapter yet",
		SetupActions: []string{"Tailscale support is not yet implemented"},
	}
}

// Enabled reports whether Tailscale is always available (it has no opt-in).
func (d *Definition) Enabled() bool { return true }

// RequiredBinary reports the client this provider cannot run without.
func (d *Definition) RequiredBinary() string { return d.cfg.Bin }

// MissingBinaryEntry describes Tailscale when its client is not installed.
func (d *Definition) MissingBinaryEntry() provider.CatalogEntry {
	return d.CatalogEntry()
}

// Activate refuses to build an adapter for an unimplemented provider.
func (d *Definition) Activate(_ context.Context, req provider.ActivationRequest) (provider.Installation, error) {
	entry := d.CatalogEntry()
	entry.Reason = "the Tailscale adapter is a scaffold and cannot be activated yet"
	return provider.Installation{Catalog: entry}, nil
}
