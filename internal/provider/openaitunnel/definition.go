package openaitunnel

import (
	"context"
	"fmt"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// DefinitionConfig carries values the composition root reads from the
// environment.
type DefinitionConfig struct {
	Bin string
	// Enabled reports the experimental opt-in.
	Enabled bool
}

// Definition describes the OpenAI Secure MCP Tunnel to Portico.
//
// This provider is accountless by nature: its client reads the control-plane
// key from the supervisor's own environment, so there is nothing for Portico to
// store and its setup flow is guidance. Keeping that on the definition rather
// than the adapter means the guidance is still retrievable when the adapter is
// switched off, which is exactly when a user needs it.
type Definition struct{ cfg DefinitionConfig }

// NewDefinition builds the OpenAI tunnel provider definition.
func NewDefinition(cfg DefinitionConfig) *Definition { return &Definition{cfg: cfg} }

func (d *Definition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID: "openai_tunnel", Name: "openai_tunnel", DisplayName: "OpenAI Secure MCP Tunnel",
	}
}

func (d *Definition) CatalogEntry() provider.CatalogEntry {
	entry := provider.CatalogEntry{
		ID: "openai_tunnel", Name: "openai_tunnel", DisplayName: "OpenAI Secure MCP Tunnel",
		Availability: provider.AvailabilityExperimental,
		Reason: "the adapter has not been exercised against a live tunnel; it can start and " +
			"observe the client but does not create tunnels or verify ChatGPT app registration",
		SetupActions: []string{
			"Install tunnel-client from the OpenAI platform's tunnel settings",
			"Create a tunnel there and note its ID; Portico does not create tunnels",
			fmt.Sprintf("Export %s before starting the supervisor", CredentialEnvVar),
		},
	}
	if !d.cfg.Enabled {
		entry.SetupActions = append(entry.SetupActions,
			"Set PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL=1 to register the provider")
	}
	return entry
}

// Enabled reports whether the operator opted in.
func (d *Definition) Enabled() bool { return d.cfg.Enabled }

// SetupFlow declares guidance. It is served from the definition so it remains
// available when no adapter is installed.
func (d *Definition) SetupFlow() core.SetupFlow { return openAITunnelSetupFlow() }

// PrepareAccount always refuses. A guidance provider has no account to prepare,
// and returning one would let the supervisor persist a row nothing reads.
func (d *Definition) PrepareAccount(map[string]string) (provider.PreparedAccount, error) {
	return provider.PreparedAccount{}, fmt.Errorf(
		"the OpenAI tunnel is configured outside Portico; its client reads %s from the "+
			"supervisor's environment, so there is nothing here to save", CredentialEnvVar)
}

// Activate builds the accountless runtime. It ignores req.Accounts because the
// coordinator never passes accounts to a provider that stores none.
func (d *Definition) Activate(_ context.Context, req provider.ActivationRequest) (provider.Installation, error) {
	entry := d.CatalogEntry()
	adapter := New(d.cfg.Bin, req.Services.Processes)
	return provider.Installation{Provider: adapter, Catalog: entry}, nil
}
