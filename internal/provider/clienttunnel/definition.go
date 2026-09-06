// Package clienttunnel is the canonical name for Portico's client-mediated
// tunnel transport. The implementation remains in openaitunnel for source
// compatibility with older imports; these aliases make the canonical package
// usable without maintaining two adapters.
package clienttunnel

import (
	"github.com/B-A-M-N/portico/internal/core"
	legacy "github.com/B-A-M-N/portico/internal/provider/openaitunnel"
)

type Definition = legacy.Definition
type DefinitionConfig = legacy.DefinitionConfig
type Provider = legacy.Provider

const ProviderID = legacy.ProviderID

func NewDefinition(cfg DefinitionConfig) *Definition { return legacy.NewDefinition(cfg) }

func runtimeCredentialAccountID() core.ProviderAccountID {
	return core.ProviderAccountID(ProviderID)
}
