package ngrok

import (
	"context"
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// DefinitionConfig carries values the composition root reads from
// configuration.
type DefinitionConfig struct {
	Bin string
	// Enabled reports the experimental opt-in. It is evaluated by the
	// composition root because it reads the supervisor's environment, which
	// cannot change in-process.
	Enabled bool
}

// Definition describes ngrok to Portico before any adapter exists.
type Definition struct{ cfg DefinitionConfig }

// NewDefinition builds the ngrok provider definition.
func NewDefinition(cfg DefinitionConfig) *Definition {
	if strings.TrimSpace(cfg.Bin) == "" {
		cfg.Bin = "ngrok"
	}
	return &Definition{cfg: cfg}
}

func (d *Definition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "ngrok", Name: "ngrok", DisplayName: "ngrok"}
}

func (d *Definition) CatalogEntry() provider.CatalogEntry {
	entry := provider.CatalogEntry{
		ID: "ngrok", Name: "ngrok", DisplayName: "ngrok",
		Availability: provider.AvailabilityExperimental,
		Reason: "ngrok creates real tunnels and rebuilds state after a restart, but Portico " +
			"applies no access protection to ngrok connections, so anyone with the URL can reach them",
		SetupActions: []string{
			"Make sure a token is available: NGROK_AUTHTOKEN, or run: ngrok config add-authtoken <token>",
		},
	}
	if !d.cfg.Enabled {
		entry.Reason = "ngrok is off by default. " + entry.Reason
		entry.SetupActions = append([]string{
			"Set PORTICO_ENABLE_EXPERIMENTAL_NGROK=1 to enable it",
		}, entry.SetupActions...)
	}
	return entry
}

// Enabled reports whether the operator opted in.
func (d *Definition) Enabled() bool { return d.cfg.Enabled }

// RequiredBinary reports the client this provider cannot run without.
func (d *Definition) RequiredBinary() string { return d.cfg.Bin }

// MissingBinaryEntry describes ngrok when its client is not installed.
func (d *Definition) MissingBinaryEntry() provider.CatalogEntry {
	entry := d.CatalogEntry()
	entry.Availability = provider.AvailabilityClientMissing
	entry.Reason = fmt.Sprintf("the %q client was not found on PATH", d.cfg.Bin)
	entry.SetupActions = []string{"Install ngrok and ensure it is on PATH"}
	return entry
}

// Activate builds the ngrok runtime.
//
// ngrok has no account router: one adapter serves one account. The account it
// serves is reported explicitly so the registry does not claim ngrok serves
// accounts that were silently dropped, and any additional account is reported
// with the reason it is not in use rather than disappearing.
func (d *Definition) Activate(_ context.Context, req provider.ActivationRequest) (provider.Installation, error) {
	entry := d.CatalogEntry()

	// With no account configured, the agent's own environment token still
	// works: ngrok reads NGROK_AUTHTOKEN itself when starting, so a machine
	// configured that way is usable without Portico storing anything. Importing
	// that token as an account was never necessary, and Portico cannot check an
	// ngrok token, so an import could only ever claim a verification that had
	// not happened.
	if len(req.Accounts) == 0 {
		if req.Services.Getenv == nil || req.Services.Getenv(AuthTokenEnvVar) == "" {
			entry.Availability = provider.AvailabilityUnconfigured
			entry.Reason = "no ngrok account is configured and " + AuthTokenEnvVar + " is not set"
			return provider.Installation{Catalog: entry}, nil
		}
		adapter, err := New("", d.cfg.Bin, req.Services.Processes)
		if err != nil {
			return provider.Installation{}, fmt.Errorf("build ngrok adapter: %w", err)
		}
		entry.Availability = provider.AvailabilityExperimental
		entry.Reason = "using the token in " + AuthTokenEnvVar + "; " + entry.Reason
		return provider.Installation{Provider: adapter, Catalog: entry}, nil
	}

	serving := req.Accounts[0]
	adapter, err := New(string(serving.Secret), d.cfg.Bin, req.Services.Processes)
	if err != nil {
		return provider.Installation{}, fmt.Errorf("build ngrok adapter: %w", err)
	}

	infos := []provider.AccountInfo{{
		ID: serving.Account.ID, Label: serving.Account.Label, Status: string(serving.Account.Status),
	}}
	// Portico does not route between ngrok accounts, so a second account would
	// be silently ignored. Saying so is better than appearing to support it.
	for _, extra := range req.Accounts[1:] {
		infos = append(infos, provider.AccountInfo{
			ID: extra.Account.ID, Label: extra.Account.Label, Status: string(extra.Account.Status),
			UnusableReason: fmt.Sprintf(
				"Portico runs one ngrok account at a time and is using %q; remove it to use this one instead",
				serving.Account.ID),
		})
	}

	// The adapter stays experimental once enabled. Reporting it as ready would
	// contradict the warning that it applies no access protection.
	entry.Availability = provider.AvailabilityExperimental
	return provider.Installation{Provider: adapter, Catalog: entry, Accounts: infos}, nil
}
