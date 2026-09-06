package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
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
	// ZoneVerified records that the exact supplied zone ID was confirmed
	// against the API. Only a verified zone makes the account DNS-capable.
	ZoneVerified bool
	// MissingPermissions names the specific permissions the token lacks, so the
	// failure can be acted on rather than merely reported.
	MissingPermissions []string
}

// ZoneSummary is a zone the credential can access.
type ZoneSummary struct {
	ID   string
	Name string
}

// AccountSummary is the non-secret identity returned when a credential can
// access more than one provider account.
type AccountSummary struct {
	ID   string
	Name string
}

type accountLister interface {
	ListAccounts(context.Context, string) ([]AccountSummary, error)
}

// AccountValidator verifies a provider credential before it is persisted.
//
// Recording an account as authenticated without checking is how Portico came to
// report ready providers that could not perform a single operation. Validation
// is an interface so tests do not require live provider credentials.
type AccountValidator interface {
	Validate(ctx context.Context, providerID, accountID, credential string) (*AccountValidation, error)
	// VerifyZone performs mandatory exact-zone validation for a supplied zone
	// ID. It is separate from Validate because zones are optional for tunnels
	// but must be proven exact before any DNS capability is granted.
	VerifyZone(ctx context.Context, credential, zoneID string) ([]ZoneSummary, error)
}

// cloudflareAccountValidator validates against the real Cloudflare API using
// least-privilege requests: an exact account-scoped tunnel read instead of a
// broad account listing, and mandatory exact-zone verification for DNS.
type cloudflareAccountValidator struct{}

func (cloudflareAccountValidator) ListAccounts(ctx context.Context, credential string) ([]AccountSummary, error) {
	api, err := cf.NewWithAPIToken(credential)
	if err != nil {
		return nil, fmt.Errorf("the API token was rejected: %w", err)
	}
	accounts, _, err := api.Accounts(ctx, cf.AccountsListParams{})
	if err != nil {
		return nil, fmt.Errorf("could not discover Cloudflare accounts: %w", err)
	}
	result := make([]AccountSummary, 0, len(accounts))
	for _, account := range accounts {
		result = append(result, AccountSummary{ID: account.ID, Name: account.Name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// devAccountValidator answers credential validation with canned results.
//
// It exists for the same reason the mock provider does: the hermetic TUI gate
// must be able to walk the real setup path — field → IPC → supervisor →
// validator → discovered choices — without a live Cloudflare credential, and
// a developer without one must still be able to exercise the form. It is
// wired only under an explicit development opt-in and never in production,
// where cloudflareAccountValidator keeps checking the real API.
type devAccountValidator struct{}

func (devAccountValidator) ListAccounts(_ context.Context, credential string) ([]AccountSummary, error) {
	if strings.TrimSpace(credential) == "" {
		return nil, errors.New("a Cloudflare API token is required")
	}
	return []AccountSummary{{ID: "acct-dev-1", Name: "Dev Account"}}, nil
}

func (devAccountValidator) Validate(_ context.Context, _ string, accountID, credential string) (*AccountValidation, error) {
	if strings.TrimSpace(credential) == "" {
		return nil, errors.New("a Cloudflare API token is required")
	}
	if strings.TrimSpace(accountID) == "" {
		return nil, errors.New("a Cloudflare account ID is required")
	}
	return &AccountValidation{AccountAccessible: true}, nil
}

func (devAccountValidator) VerifyZone(_ context.Context, credential, zoneID string) ([]ZoneSummary, error) {
	if strings.TrimSpace(credential) == "" {
		return nil, errors.New("a Cloudflare API token is required")
	}
	if strings.TrimSpace(zoneID) == "" {
		return nil, errors.New("a zone ID is required")
	}
	return []ZoneSummary{{ID: zoneID, Name: "dev.example"}}, nil
}

func (cloudflareAccountValidator) Validate(ctx context.Context, providerID, accountID, credential string) (*AccountValidation, error) {
	if providerID != "cloudflare" {
		return nil, fmt.Errorf("no validator for provider %q", providerID)
	}
	if strings.TrimSpace(accountID) == "" {
		return nil, fmt.Errorf("a Cloudflare account ID is required")
	}

	api, useErr := cf.NewWithAPIToken(credential)
	if useErr != nil {
		return nil, fmt.Errorf("the API token was rejected: %w", useErr)
	}

	result := &AccountValidation{}

	// Exact account verification: one non-mutating tunnel list scoped to the
	// supplied account. 403/401 here means insufficient tunnel permission —
	// classified distinctly from a bad token or a transient failure.
	rc := cf.AccountIdentifier(accountID)
	_, _, listErr := api.ListTunnels(ctx, rc, cf.TunnelListParams{
		IsDeleted: cf.BoolPtr(false),
	})
	if listErr != nil {
		var cfErr *cf.Error
		switch {
		case errors.As(listErr, &cfErr) && (cfErr.StatusCode == http.StatusUnauthorized):
			result.MissingPermissions = append(result.MissingPermissions,
				"The API token itself is invalid or expired")
			return result, fmt.Errorf("the API token was rejected by Cloudflare")
		case errors.As(listErr, &cfErr) && cfErr.StatusCode == http.StatusForbidden:
			result.MissingPermissions = append(result.MissingPermissions,
				"Cloudflare Tunnel: Edit (required to create and manage tunnels)")
			return result, fmt.Errorf("the token cannot manage tunnels in account %s", accountID)
		case errors.As(listErr, &cfErr) && cfErr.StatusCode == http.StatusTooManyRequests:
			return result, fmt.Errorf("the request was rate limited by Cloudflare; retry shortly")
		default:
			return result, fmt.Errorf("could not reach Cloudflare to verify the account: %w", listErr)
		}
	}
	result.AccountAccessible = true
	return result, nil
}

// VerifyZone performs mandatory exact-zone validation for a supplied zone ID.
//
// This replaces the fail-open condition where a zone was accepted whenever
// zone listing happened to return rows: listing is optional discovery, but a
// user-supplied zone ID used for DNS must be proven to belong to this account
// before the account becomes DNS-capable. The check is an exact lookup of
// that zone ID — empty result means not visible to this token.
func (cloudflareAccountValidator) VerifyZone(ctx context.Context, credential, zoneID string) ([]ZoneSummary, error) {
	api, useErr := cf.NewWithAPIToken(credential)
	if useErr != nil {
		return nil, fmt.Errorf("the API token was rejected: %w", useErr)
	}

	zones, err := api.ListZones(ctx, zoneID)
	if err != nil {
		var cfErr *cf.Error
		switch {
		case errors.As(err, &cfErr) && (cfErr.StatusCode == http.StatusForbidden || cfErr.StatusCode == http.StatusNotFound):
			return nil, fmt.Errorf(
				"zone %s is not accessible with this token; verify the zone ID belongs to this account and the token has Zone: Read for it", zoneID)
		case errors.As(err, &cfErr) && cfErr.StatusCode == http.StatusTooManyRequests:
			return nil, fmt.Errorf("the request was rate limited by Cloudflare; retry shortly")
		default:
			return nil, fmt.Errorf("could not reach Cloudflare to verify zone %s: %w", zoneID, err)
		}
	}
	if len(zones) == 0 {
		return nil, fmt.Errorf(
			"zone %s is not visible to this token; verify the zone ID and that the token has Zone: Read for it", zoneID)
	}
	return summarizeZones(zones), nil
}

func summarizeZones(zones []cf.Zone) []ZoneSummary {
	out := make([]ZoneSummary, 0, len(zones))
	for _, z := range zones {
		out = append(out, ZoneSummary{ID: z.ID, Name: z.Name})
	}
	return out
}
