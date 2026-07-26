package controller

import "github.com/B-A-M-N/portico/internal/core"

// providerForProfile returns a registered provider only when it can safely
// serve the profile's selected provider account. A registry has one provider
// instance per provider ID today, so allowing a mismatched account here would
// direct mutations to whichever account happened to configure that instance.
func (c *Controller) providerForProfile(profile *core.ConnectionProfile) (core.Provider, error) {
	if profile == nil {
		return nil, core.ErrValidation("profile is required")
	}
	providerID := profile.Provider.ProviderID
	prov := c.registry.Get(providerID)
	if prov == nil {
		return nil, core.ErrProviderNotFound(providerID)
	}

	selectedAccount := profile.Provider.AccountID
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
