package provider

import (
	"context"

	"github.com/B-A-M-N/portico/internal/core"
)

// Unimplemented is a provider Portico names but ships no adapter for.
//
// It exists so such a provider is a definition like any other rather than a
// special case in the coordinator. Without it the UI cannot distinguish "this
// provider does not exist" from "its client is not installed", and
// documentation claiming support has nothing to contradict it.
type Unimplemented struct {
	id     core.ProviderID
	name   string
	reason string
}

// NewUnimplemented describes a named provider with no adapter.
func NewUnimplemented(id core.ProviderID, displayName, reason string) *Unimplemented {
	return &Unimplemented{id: id, name: displayName, reason: reason}
}

func (u *Unimplemented) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: u.id, Name: string(u.id), DisplayName: u.name}
}

func (u *Unimplemented) CatalogEntry() CatalogEntry {
	return CatalogEntry{
		ID: u.id, Name: string(u.id), DisplayName: u.name,
		Availability: AvailabilityNotImplemented,
		Reason:       u.reason,
	}
}

// Activate installs nothing. Returning an adapter here would make an
// unimplemented provider selectable, which is the failure the catalog exists to
// prevent.
func (u *Unimplemented) Activate(context.Context, ActivationRequest) (Installation, error) {
	return Installation{Catalog: u.CatalogEntry()}, nil
}
