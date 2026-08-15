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
type Definition struct{ cfg DefinitionConfig }

// NewDefinition builds the OpenAI tunnel provider definition.
func NewDefinition(cfg DefinitionConfig) *Definition {
	if cfg.Bin == "" {
		cfg.Bin = ClientBinary
	}
	return &Definition{cfg: cfg}
}

func (d *Definition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID: "openai_tunnel", Name: "openai_tunnel", DisplayName: "OpenAI Secure MCP Tunnel",
	}
}

func (d *Definition) CatalogEntry() provider.CatalogEntry {
	entry := provider.CatalogEntry{
		ID:           "openai_tunnel",
		Name:         "openai_tunnel",
		DisplayName:  "OpenAI Secure MCP Tunnel",
		Availability: provider.AvailabilityExperimental,
		Stability:    core.StabilityExperimental,
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

// RequiredBinary reports the client this provider cannot run without.
func (d *Definition) RequiredBinary() string { return d.cfg.Bin }

// MissingBinaryEntry describes OpenAI when its client is not installed.
func (d *Definition) MissingBinaryEntry() provider.CatalogEntry {
	return d.CatalogEntry()
}

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
func (d *Definition) Activate(ctx context.Context, req provider.ActivationRequest) (provider.Installation, error) {
	entry := d.CatalogEntry()

	// Check for the binary.
	if req.Services.LookPath != nil {
		if _, err := req.Services.LookPath(d.cfg.Bin); err != nil {
			entry.Availability = provider.AvailabilityClientMissing
			entry.Reason = fmt.Sprintf("the %q client was not found on PATH", d.cfg.Bin)
			entry.SetupActions = []string{
				"Install tunnel-client from the OpenAI platform's tunnel settings",
				fmt.Sprintf("Export %s before starting the supervisor", CredentialEnvVar),
			}
			if !d.cfg.Enabled {
				entry.SetupActions = append(entry.SetupActions,
					"Set PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL=1 to register the provider")
			}
			return provider.Installation{Catalog: entry}, nil
		}
	}

	// Check for the credential.
	if req.Services.Getenv != nil && req.Services.Getenv(CredentialEnvVar) == "" {
		entry.Availability = provider.AvailabilityUnconfigured
		entry.Reason = fmt.Sprintf("%s is not set; the tunnel client cannot reach the OpenAI control plane", CredentialEnvVar)
		entry.SetupActions = []string{
			fmt.Sprintf("Export %s before starting the supervisor", CredentialEnvVar),
		}
		if !d.cfg.Enabled {
			entry.SetupActions = append(entry.SetupActions,
				"Set PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL=1 to register the provider")
		}
		return provider.Installation{Catalog: entry}, nil
	}

	adapter := New(d.cfg.Bin, req.Services.Processes)

	// The adapter is selectable when explicitly enabled. The experimental stability
	// warning remains in the catalog Reason so the UI surfaces it.
	if d.cfg.Enabled {
		entry.Availability = provider.AvailabilityReady
	} else {
		entry.Availability = provider.AvailabilityExperimental
	}
	entry.Stability = core.StabilityExperimental

	return provider.Installation{Provider: adapter, Catalog: entry}, nil
}
