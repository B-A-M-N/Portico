package cloudflare

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// DefinitionConfig carries the values the composition root reads from
// configuration, so this package does not depend on the config system.
type DefinitionConfig struct {
	// Bin is the cloudflared executable name or path.
	Bin string
}

// Definition describes Cloudflare to Portico before any adapter exists.
type Definition struct {
	cfg DefinitionConfig
}

// NewDefinition builds the Cloudflare provider definition.
func NewDefinition(cfg DefinitionConfig) *Definition {
	if strings.TrimSpace(cfg.Bin) == "" {
		cfg.Bin = "cloudflared"
	}
	return &Definition{cfg: cfg}
}

func (d *Definition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "cloudflare", Name: "cloudflare", DisplayName: "Cloudflare"}
}

func (d *Definition) CatalogEntry() provider.CatalogEntry {
	return provider.CatalogEntry{
		ID: "cloudflare", Name: "cloudflare", DisplayName: "Cloudflare",
		Availability: provider.AvailabilityUnconfigured,
		SetupActions: []string{"Add a Cloudflare account"},
	}
}

// RequiredBinary reports the client this provider cannot run without.
func (d *Definition) RequiredBinary() string { return d.cfg.Bin }

// MissingBinaryEntry describes Cloudflare when cloudflared is not installed. A
// missing client is a setup gap, not a reason for the provider to disappear.
func (d *Definition) MissingBinaryEntry() provider.CatalogEntry {
	entry := d.CatalogEntry()
	entry.Availability = provider.AvailabilityClientMissing
	entry.Reason = fmt.Sprintf("the %q client was not found on PATH", d.cfg.Bin)
	entry.SetupActions = []string{
		"Install cloudflared from https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/",
		"Ensure cloudflared is on PATH, or set the cloudflared binary path in Portico's configuration",
	}
	return entry
}

// SetupFlow declares what Cloudflare needs in order to configure an account.
func (d *Definition) SetupFlow() core.SetupFlow { return cloudflareSetupFlow() }

// PrepareAccount turns submitted setup values into Cloudflare's canonical
// account.
//
// The mapping lives here because only Cloudflare knows that the zone is
// optional metadata rather than identity, and that the credential reference is
// derived from the account ID. The supervisor used to assume every field it did
// not recognise was metadata, which is adequate scaffolding and not a contract.
func (d *Definition) PrepareAccount(values map[string]string) (provider.PreparedAccount, error) {
	accountID := strings.TrimSpace(values["account_id"])
	if accountID == "" {
		return provider.PreparedAccount{}, fmt.Errorf("a Cloudflare account ID is required")
	}
	credential := strings.TrimSpace(values["credential"])
	if credential == "" {
		return provider.PreparedAccount{}, fmt.Errorf("a Cloudflare API token is required")
	}
	label := strings.TrimSpace(values["label"])
	if label == "" {
		label = accountID
	}
	metadata := map[string]string{}
	// A zone is needed only for DNS and custom hostnames. An account without
	// one is still usable for tunnels, so its absence is not an error.
	if zone := strings.TrimSpace(values["zone_id"]); zone != "" {
		metadata["zone_id"] = zone
	}
	return provider.PreparedAccount{
		Account: core.ProviderAccount{
			ID:            core.ProviderAccountID(accountID),
			Provider:      "cloudflare",
			Label:         label,
			CredentialRef: fmt.Sprintf("cloudflare:%s:api-token", accountID),
			Metadata:      metadata,
		},
		Secret: []byte(credential),
	}, nil
}

// Activate builds the runtime from the usable accounts.
//
// With no usable account it returns the Quick Tunnel adapter rather than
// nothing. That is deliberate and is the resolution of a real divergence:
// startup registration fell back to Quick Tunnels while the rebuild path
// removed Cloudflare entirely, so the same durable state produced different
// capabilities depending on how it was reached. A Quick Tunnel needs no
// account and works; withdrawing it because an account was deleted is a
// regression.
func (d *Definition) Activate(_ context.Context, req provider.ActivationRequest) (provider.Installation, error) {
	entry := d.CatalogEntry()
	children := make(map[core.ProviderAccountID]*Provider)
	infos := make([]provider.AccountInfo, 0, len(req.Accounts))

	for _, material := range req.Accounts {
		account := material.Account
		zone := strings.TrimSpace(account.Metadata["zone_id"])
		child, err := New(string(material.Secret), string(account.ID), zone,
			d.cfg.Bin, req.Services.ConnectorDir, req.Services.Processes)
		if err != nil {
			// One unusable account must not cost the others their adapter.
			infos = append(infos, provider.AccountInfo{
				ID: account.ID, Label: account.Label, Status: string(account.Status),
				UnusableReason: "its adapter could not be built: " + err.Error(),
			})
			continue
		}
		if req.Services.TunnelCredentials != nil {
			child.SetCredentialStore(req.Services.TunnelCredentials)
		}
		children[account.ID] = child
		infos = append(infos, provider.AccountInfo{
			ID: account.ID, Label: account.Label, Status: string(account.Status),
		})
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })

	if len(children) > 0 {
		accountsProvider, err := NewAccountsProvider(children)
		if err != nil {
			return provider.Installation{}, fmt.Errorf("build Cloudflare account router: %w", err)
		}
		entry.Availability = provider.AvailabilityReady
		return provider.Installation{Provider: accountsProvider, Catalog: entry, Accounts: infos}, nil
	}

	quick, err := NewQuickTunnel(d.cfg.Bin, req.Services.ConnectorDir, req.Services.Processes)
	if err != nil {
		return provider.Installation{}, fmt.Errorf("build Cloudflare quick tunnel adapter: %w", err)
	}
	if req.Services.TunnelCredentials != nil {
		quick.SetCredentialStore(req.Services.TunnelCredentials)
	}
	entry.Availability = provider.AvailabilityReady
	entry.Reason = "no account is configured, so only temporary Quick Tunnel addresses are available"
	return provider.Installation{Provider: quick, Catalog: entry, Accounts: infos}, nil
}
