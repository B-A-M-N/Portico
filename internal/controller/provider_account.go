package controller

import (
	"context"
	"fmt"

	"github.com/B-A-M-N/portico/internal/core"
)

// providerForProfile returns a registered provider only when it can safely
// serve the profile's selected provider account. A registry has one provider
// instance per provider ID today, so allowing a mismatched account here would
// direct mutations to whichever account happened to configure that instance.
func (c *Controller) providerForProfile(profile *core.ConnectionProfile) (core.Provider, error) {
	if profile == nil {
		return nil, core.ErrValidation("profile is required")
	}
	provider := profile.GetProvider()
	providerID := provider.ProviderID
	prov := c.registry.Get(providerID)
	if prov == nil {
		return nil, core.ErrProviderNotFound(providerID)
	}

	selectedAccount := provider.AccountID
	if scoped, ok := prov.(core.AccountScopedProvider); ok {
		child, err := scoped.ProviderForAccount(selectedAccount)
		if err != nil {
			return nil, core.ErrProviderAccountUnavailable(providerID, selectedAccount)
		}
		return child, nil
	}
	if selectedAccount == "" {
		return prov, nil
	}
	binding, ok := prov.(core.ProviderAccountBinding)
	if !ok || binding.ProviderAccountID() == "" || binding.ProviderAccountID() != selectedAccount {
		return nil, core.ErrProviderAccountUnavailable(providerID, selectedAccount)
	}
	return prov, nil
}

// validateProviderForProfile verifies that a provider exists, supports the
// connection kind, and satisfies all capability constraints. This is the
// single authority for profile → provider compatibility, used by create,
// update, and edit planning so that creation and editing cannot have
// different provider validity rules.
//
// It checks:
//   - provider exists and account binding is valid
//   - provider executes the connection kind
//   - protocols, exposure modes, protection kinds are supported
//   - structured capability constraints are satisfied
func (c *Controller) validateProviderForProfile(ctx context.Context, profile *core.ConnectionProfile) error {
	if profile == nil {
		return core.ErrValidation("profile is required")
	}
	providerID := profile.Driver.ProviderID
	if providerID == "" {
		return core.ErrValidation("provider ID is required")
	}

	// Resolve the provider (validates account binding).
	prov, err := c.providerForProfile(profile)
	if err != nil {
		return err
	}

	// Query capabilities and verify the provider executes this kind.
	caps, err := prov.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("query provider capabilities: %w", err)
	}
	if !caps.Executes(profile.Kind) {
		return core.ErrValidation(fmt.Sprintf("provider %q does not support connection kind %q", providerID, profile.Kind))
	}

	// Check kind-specific capability requirements.
	if err := checkKindCapabilities(caps, profile, providerID); err != nil {
		return err
	}

	// Apply structured capability constraints.
	constraints := core.CheckConstraints(caps.Constraints, profile)
	if len(constraints) > 0 {
		return core.ErrValidation(fmt.Sprintf("provider %q cannot satisfy profile requirements: %s", providerID, constraints[0].Message))
	}

	return nil
}

// checkKindCapabilities verifies that a provider's capabilities satisfy the
// profile's kind-specific requirements (protocol, exposure, protection, etc.).
func checkKindCapabilities(caps core.Capabilities, profile *core.ConnectionProfile, providerID core.ProviderID) error {
	switch profile.Kind {
	case core.ConnectionPortForward:
		// Port forwards must use a supported protocol. For an executable
		// connection kind, an unstated protocol map means no protocols are
		// supported — it must be explicitly listed.
		spec := profile.Spec.PortForward
		if spec != nil && spec.Protocol != "" {
			pc, ok := caps.Protocols[spec.Protocol]
			if !ok || !pc.Supported {
				return core.ErrValidation(fmt.Sprintf("provider %q does not support protocol %q for port forwards", providerID, spec.Protocol))
			}
		}
	case core.ConnectionServiceExposure:
		// Service exposure: check exposure mode support.
		exposure := profile.GetExposure()
		if exposure.Mode != "" && !caps.PrivateExposure.Supported {
			// Check if the mode is private-only and the provider supports it.
			if exposure.Mode == core.ExposurePrivate && !caps.PrivateExposure.Supported {
				return core.ErrValidation(fmt.Sprintf("provider %q does not support private exposure", providerID))
			}
		}
		// Check protection support.
		protection := profile.GetProtection()
		if protection.Kind != "" && protection.Kind != core.ProtectionNone {
			supported := false
			for _, pc := range caps.BuiltInProtection {
				if pc.Kind == protection.Kind && pc.Supported {
					supported = true
					break
				}
			}
			if !supported {
				return core.ErrValidation(fmt.Sprintf("provider %q does not support protection %q", providerID, protection.Kind))
			}
		}
	}
	return nil
}
