package cloudflare

import (
	"context"
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
)

// ReadinessSnapshot is the account-scoped view of what a Cloudflare adapter
// can use right now. It deliberately does not replace Capabilities: the latter
// is the provider's intrinsic feature set, while this view applies the
// account's configured zone and runtime mode.
type ReadinessSnapshot struct {
	AccountID         core.ProviderAccountID
	Ready             bool
	AccountConfigured bool
	ZoneConfigured    bool
	Capabilities      core.Capabilities
	Notes             []string
}

type dnsConfigurationError struct{ message string }

func (e *dnsConfigurationError) Error() string { return e.message }

// cloudflareIntrinsicCapabilities asks a full Cloudflare adapter for the
// static capability contract without selecting an account. AccountsProvider
// uses this instead of the first sorted child, because child order must never
// decide the capability answer for a multi-account installation.
func cloudflareIntrinsicCapabilities(ctx context.Context) (core.Capabilities, error) {
	return (&Provider{mode: ProviderModeFull}).Capabilities(ctx)
}

// AccountCapabilities returns the effective capabilities for this configured
// account. Unlike Capabilities, it is allowed to reflect account-local facts
// such as whether a zone was configured.
func (p *Provider) AccountCapabilities(ctx context.Context) (core.Capabilities, error) {
	snapshot, err := p.ReadinessSnapshot(ctx)
	if err != nil {
		return core.Capabilities{}, err
	}
	return snapshot.Capabilities, nil
}

// ReadinessSnapshot returns the local readiness facts for this adapter. It is
// intentionally non-I/O: credential verification belongs to account setup and
// activation, while this snapshot answers the stable no-zone/zone distinction.
func (p *Provider) ReadinessSnapshot(ctx context.Context) (ReadinessSnapshot, error) {
	if p == nil {
		return ReadinessSnapshot{}, fmt.Errorf("cloudflare provider is nil")
	}
	intrinsic, err := p.Capabilities(ctx)
	if err != nil {
		return ReadinessSnapshot{}, err
	}

	snapshot := ReadinessSnapshot{
		AccountID:         core.ProviderAccountID(p.accountID),
		AccountConfigured: strings.TrimSpace(p.accountID) != "",
		ZoneConfigured:    strings.TrimSpace(p.zoneID) != "",
		Capabilities:      intrinsic,
		Ready:             p.mode == ProviderModeQuickOnly || (p.apiClient != nil && strings.TrimSpace(p.accountID) != ""),
	}

	if p.mode == ProviderModeQuickOnly {
		snapshot.Notes = []string{"only temporary Quick Tunnel addresses are available; no Cloudflare account is selected"}
		return snapshot, nil
	}
	if !snapshot.AccountConfigured {
		snapshot.Ready = false
		snapshot.Notes = []string{"a Cloudflare account is required for managed tunnels"}
		return snapshot, nil
	}
	if !snapshot.ZoneConfigured {
		// These are intrinsic Cloudflare features, but this account cannot
		// execute them until a zone is configured. Keep the distinction visible
		// in the snapshot rather than weakening the provider-wide contract.
		snapshot.Capabilities.CustomHostnames.Supported = false
		snapshot.Capabilities.CustomHostnames.Notes = []string{"this account has no configured Cloudflare zone"}
		snapshot.Capabilities.ManagedDNS.Supported = false
		snapshot.Capabilities.ManagedDNS.Notes = []string{"this account has no configured Cloudflare zone"}
		for i := range snapshot.Capabilities.BuiltInProtection {
			if snapshot.Capabilities.BuiltInProtection[i].Kind == core.ProtectionEmailOTP {
				snapshot.Capabilities.BuiltInProtection[i].Supported = false
				snapshot.Capabilities.BuiltInProtection[i].Notes = []string{"Cloudflare Access requires a configured zone"}
			}
		}
		snapshot.Notes = []string{"managed tunnels and temporary addresses are available; a zone is required for permanent hostnames, DNS and Access"}
	} else {
		snapshot.Notes = []string{"managed tunnels, permanent hostnames, DNS and configured Access protection are available"}
	}
	return snapshot, nil
}
