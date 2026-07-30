package supervisor

import (
	"context"
	"fmt"
	"strings"

	cf "github.com/cloudflare/cloudflare-go"
)

// AccountValidation is the result of checking a credential against a provider
// before the account is recorded.
type AccountValidation struct {
	// AccountAccessible reports whether the credential can actually reach the
	// named account, not merely that a credential was supplied.
	AccountAccessible bool
	// Zones are the zones the credential can see. A zone is only required for
	// DNS and custom-hostname work, so discovery lets the user pick one instead
	// of copying an ID by hand.
	Zones []ZoneSummary
	// MissingPermissions names the specific permissions the token lacks, so the
	// failure can be acted on rather than merely reported.
	MissingPermissions []string
}

// ZoneSummary is a zone the credential can access.
type ZoneSummary struct {
	ID   string
	Name string
}

// AccountValidator verifies a provider credential before it is persisted.
//
// Recording an account as authenticated without checking is how Portico came to
// report ready providers that could not perform a single operation. Validation
// is an interface so tests do not require live provider credentials.
type AccountValidator interface {
	Validate(ctx context.Context, providerID, accountID, credential string) (*AccountValidation, error)
}

// cloudflareAccountValidator validates against the real Cloudflare API.
type cloudflareAccountValidator struct{}

func (cloudflareAccountValidator) Validate(ctx context.Context, providerID, accountID, credential string) (*AccountValidation, error) {
	if providerID != "cloudflare" {
		return nil, fmt.Errorf("no validator for provider %q", providerID)
	}

	api, err := cf.NewWithAPIToken(credential)
	if err != nil {
		return nil, fmt.Errorf("the API token was rejected: %w", err)
	}

	result := &AccountValidation{}

	accounts, _, err := api.Accounts(ctx, cf.AccountsListParams{})
	if err != nil {
		// A token that cannot list accounts cannot manage tunnels either.
		result.MissingPermissions = append(result.MissingPermissions,
			"Account Settings: Read (required to confirm account access)")
		return result, fmt.Errorf("the token could not read your Cloudflare accounts: %w", err)
	}
	for _, acc := range accounts {
		if acc.ID == accountID {
			result.AccountAccessible = true
			break
		}
	}
	if !result.AccountAccessible {
		visible := make([]string, 0, len(accounts))
		for _, acc := range accounts {
			visible = append(visible, acc.ID)
		}
		if len(visible) == 0 {
			return result, fmt.Errorf("the token is valid but can see no accounts; check that it is an account-scoped token")
		}
		return result, fmt.Errorf("the token is valid but cannot see account %s; it can see: %s",
			accountID, strings.Join(visible, ", "))
	}

	// Zones are optional: they are only needed for DNS and custom hostnames.
	// A token without zone access can still run Quick Tunnels and managed
	// tunnels, so a zone listing failure is reported, not fatal.
	zones, zErr := api.ListZones(ctx)
	if zErr != nil {
		result.MissingPermissions = append(result.MissingPermissions,
			"Zone: Read (required only for permanent hostnames and DNS)")
		return result, nil
	}
	for _, z := range zones {
		result.Zones = append(result.Zones, ZoneSummary{ID: z.ID, Name: z.Name})
	}
	return result, nil
}
