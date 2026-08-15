package portforward

import (
	"context"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// Definition describes local port forwarding, which needs no account, no
// credential and no client binary. It is the simplest shape a definition can
// take and is useful as a reference.
type Definition struct{}

// NewDefinition builds the port forward provider definition.
func NewDefinition() *Definition { return &Definition{} }

func (d *Definition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID: "portforward", Name: "portforward", DisplayName: "Local port forward",
	}
}

func (d *Definition) CatalogEntry() provider.CatalogEntry {
	return provider.CatalogEntry{
		ID:           "portforward",
		Name:         "portforward",
		DisplayName:  "Local port forward",
		Availability: provider.AvailabilityReady,
		Stability:    core.StabilityStable,
	}
}

// Activate always succeeds. There is nothing to configure and nothing to fail.
func (d *Definition) Activate(context.Context, provider.ActivationRequest) (provider.Installation, error) {
	return provider.Installation{Provider: New(), Catalog: d.CatalogEntry()}, nil
}
