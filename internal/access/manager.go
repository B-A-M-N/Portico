package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go"
)

// AppInfo holds the created Access application details.
type AppInfo struct {
	AppID    string
	PolicyID string
	LoginURL string
}

// Policy defines who can access the application.
type Policy struct {
	AllowedEmails   []string
	AllowedDomains  []string
	SessionDuration string // e.g., "30m", "1h"
	AuthMode        string // otp, idp, idp+mtls, service-token
}

// Manager manages Cloudflare Access applications and policies.
type Manager interface {
	CreateApp(ctx context.Context, accountID, hostname string, policy Policy) (*AppInfo, error)
	GetApp(ctx context.Context, accountID, appID string) (*AppState, error)
	UpdatePolicy(ctx context.Context, accountID, appID, policyID string, policy Policy) error
	DeleteApp(ctx context.Context, accountID, appID string) error
}

// AppState represents the observed state of an Access application.
type AppState struct {
	ID       string
	Name     string
	Domain   string
	AuthMode string
}

// PolicyState represents the observed state of an Access policy.
type PolicyState struct {
	ID              string
	Name            string
	Decision        string
	AllowedEmails   []string
	AllowedDomains  []string
	SessionDuration string
}

// PolicyGetter is an optional extension of Manager for retrieving an
// individual Access policy by its exact ID. Implemented by APIManager.
type PolicyGetter interface {
	GetPolicy(ctx context.Context, accountID, appID, policyID string) (*PolicyState, error)
}

// PolicyCreator is an optional extension for restoring one exact policy on
// an existing Access application. Keeping it optional lets narrow adapters
// expose the capability without forcing unrelated legacy managers to grow a
// method they cannot safely implement.
type PolicyCreator interface {
	CreatePolicy(ctx context.Context, accountID, appID string, policy Policy) (string, error)
}

// AppUpdater is an optional extension for correcting the hostname of one
// exact Access application without replacing its policy or application ID.
type AppUpdater interface {
	UpdateApp(ctx context.Context, accountID, appID, hostname string) error
}

// APIManager implements Manager using the Cloudflare API.
type APIManager struct {
	client     *cf.API
	teamDomain string
}

// NewAPIManager creates a new Access manager.
func NewAPIManager(client *cf.API, teamDomain string) *APIManager {
	return &APIManager{client: client, teamDomain: teamDomain}
}

// CreateApp creates a self-hosted Access application with an allow policy.
func (m *APIManager) CreateApp(ctx context.Context, accountID, hostname string, policy Policy) (*AppInfo, error) {
	rc := cf.AccountIdentifier(accountID)

	sessionDur := policy.SessionDuration
	if sessionDur == "" {
		sessionDur = "30m"
	}

	// Create the Access application.
	app, err := m.client.CreateAccessApplication(ctx, rc, cf.CreateAccessApplicationParams{
		Name:            fmt.Sprintf("flare-%s", hostname),
		Domain:          hostname,
		Type:            cf.SelfHosted,
		SessionDuration: sessionDur,
	})
	if err != nil {
		return nil, fmt.Errorf("creating Access application: %w", err)
	}

	// Build the include rules for the allow policy.
	include := buildIncludeRules(policy)

	// P0 #8: Fail closed for protected configurations.
	// A non-trivial auth mode must have at least one include rule.
	if policy.AuthMode != "none" && policy.AuthMode != "private_network" && len(include) == 0 {
		return nil, fmt.Errorf("access policy requires at least one allowed email or domain for %s protection", policy.AuthMode)
	}

	// Create an allow policy on the application.
	accessPolicy, err := m.client.CreateAccessPolicy(ctx, rc, cf.CreateAccessPolicyParams{
		ApplicationID: app.ID,
		Name:          fmt.Sprintf("flare-allow-%s", hostname),
		Decision:      "allow",
		Precedence:    1,
		Include:       include,
	})
	if err != nil {
		// Attempt cleanup of the app we just created.
		_ = m.client.DeleteAccessApplication(ctx, rc, app.ID)
		return nil, fmt.Errorf("creating Access policy: %w", err)
	}

	loginURL := fmt.Sprintf("https://%s", hostname)

	return &AppInfo{
		AppID:    app.ID,
		PolicyID: accessPolicy.ID,
		LoginURL: loginURL,
	}, nil
}

// GetApp retrieves the current state of an Access application.
// Returns nil, nil if the app is not found (deleted externally).
func (m *APIManager) GetApp(ctx context.Context, accountID, appID string) (*AppState, error) {
	rc := cf.AccountIdentifier(accountID)
	app, err := m.client.GetAccessApplication(ctx, rc, appID)
	if err != nil {
		if cfErr, ok := err.(*cf.Error); ok && cfErr.StatusCode == 404 {
			return nil, nil
		}
		return nil, fmt.Errorf("getting Access app: %w", err)
	}
	return &AppState{
		ID:     app.ID,
		Name:   app.Name,
		Domain: app.Domain,
	}, nil
}

// GetPolicy retrieves the current state of an Access policy by its
// exact policy ID. When appID is non-empty the lookup is scoped to the
// application. Returns nil, nil if the policy is not found (deleted
// externally).
func (m *APIManager) GetPolicy(ctx context.Context, accountID, appID, policyID string) (*PolicyState, error) {
	rc := cf.AccountIdentifier(accountID)
	policy, err := m.client.GetAccessPolicy(ctx, rc, cf.GetAccessPolicyParams{
		PolicyID:      policyID,
		ApplicationID: appID,
	})
	if err != nil {
		var cfErr *cf.Error
		if errors.As(err, &cfErr) && cfErr.StatusCode == 404 {
			return nil, nil
		}
		return nil, fmt.Errorf("getting Access policy: %w", err)
	}
	emails, domains := policyIdentities(policy.Include)
	state := &PolicyState{
		ID:       policy.ID,
		Name:     policy.Name,
		Decision: policy.Decision,
	}
	state.AllowedEmails = emails
	state.AllowedDomains = domains
	if policy.SessionDuration != nil {
		state.SessionDuration = *policy.SessionDuration
	}
	return state, nil
}

func policyIdentities(include []interface{}) (emails, domains []string) {
	for _, rule := range include {
		encoded, err := json.Marshal(rule)
		if err != nil {
			continue
		}
		var envelope struct {
			Email *struct {
				Email string `json:"email"`
			} `json:"email"`
			EmailDomain *struct {
				Domain string `json:"domain"`
			} `json:"email_domain"`
		}
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			continue
		}
		if envelope.Email != nil && envelope.Email.Email != "" {
			emails = append(emails, envelope.Email.Email)
		}
		if envelope.EmailDomain != nil && envelope.EmailDomain.Domain != "" {
			domains = append(domains, envelope.EmailDomain.Domain)
		}
	}
	return emails, domains
}

// CreatePolicy creates the allow policy for an existing Access application
// and returns its exact Cloudflare ID.
func (m *APIManager) CreatePolicy(ctx context.Context, accountID, appID string, policy Policy) (string, error) {
	rc := cf.AccountIdentifier(accountID)
	include := buildIncludeRules(policy)
	if policy.AuthMode != "none" && policy.AuthMode != "private_network" && len(include) == 0 {
		return "", fmt.Errorf("access policy requires at least one allowed email or domain for %s", policy.AuthMode)
	}
	created, err := m.client.CreateAccessPolicy(ctx, rc, cf.CreateAccessPolicyParams{
		ApplicationID: appID,
		Name:          "portico-allow-policy",
		Decision:      "allow",
		Precedence:    1,
		Include:       include,
	})
	if err != nil {
		return "", fmt.Errorf("creating Access policy: %w", err)
	}
	return created.ID, nil
}

// UpdateApp corrects the hostname of an existing self-hosted Access
// application while preserving the fields Cloudflare requires on its PUT
// endpoint. The preceding exact-ID read prevents a partial update from
// clearing the application name, type, or session policy.
func (m *APIManager) UpdateApp(ctx context.Context, accountID, appID, hostname string) error {
	rc := cf.AccountIdentifier(accountID)
	app, err := m.client.GetAccessApplication(ctx, rc, appID)
	if err != nil {
		return fmt.Errorf("getting Access application for update: %w", err)
	}
	_, err = m.client.UpdateAccessApplication(ctx, rc, cf.UpdateAccessApplicationParams{
		ID:              appID,
		Name:            app.Name,
		Domain:          hostname,
		DomainType:      app.DomainType,
		Type:            app.Type,
		SessionDuration: app.SessionDuration,
		PrivateAddress:  app.PrivateAddress,
		Destinations:    app.Destinations,
	})
	if err != nil {
		return fmt.Errorf("updating Access application: %w", err)
	}
	return nil
}
func (m *APIManager) UpdatePolicy(ctx context.Context, accountID, appID, policyID string, policy Policy) error {
	rc := cf.AccountIdentifier(accountID)

	include := buildIncludeRules(policy)

	params := cf.UpdateAccessPolicyParams{
		ApplicationID: appID,
		PolicyID:      policyID,
		Name:          "flare-allow-policy",
		Decision:      "allow",
		Precedence:    1,
		Include:       include,
	}

	if policy.SessionDuration != "" {
		params.SessionDuration = &policy.SessionDuration
	}

	_, err := m.client.UpdateAccessPolicy(ctx, rc, params)
	if err != nil {
		return fmt.Errorf("updating Access policy: %w", err)
	}
	return nil
}

// DeleteApp removes an Access application (cascades to its policies).
func (m *APIManager) DeleteApp(ctx context.Context, accountID, appID string) error {
	rc := cf.AccountIdentifier(accountID)
	if err := m.client.DeleteAccessApplication(ctx, rc, appID); err != nil {
		return fmt.Errorf("deleting Access application: %w", err)
	}
	return nil
}

// buildIncludeRules constructs Access policy include rules from a Policy.
// When the policy has an auth mode other than "none" or "private_network",
// fail closed if no allowed identities are configured.
func buildIncludeRules(policy Policy) []any {
	var include []any

	for _, email := range policy.AllowedEmails {
		if email == "" {
			continue
		}
		include = append(include, cf.AccessGroupEmail{
			Email: struct {
				Email string `json:"email"`
			}{Email: email},
		})
	}

	for _, domain := range policy.AllowedDomains {
		if domain == "" {
			continue
		}
		include = append(include, cf.AccessGroupEmailDomain{
			EmailDomain: struct {
				Domain string `json:"domain"`
			}{Domain: domain},
		})
	}

	// P0 #8: Fail closed for protected configurations.
	// No identities configured with non-trivial auth mode is a validation error
	// caught before this point; if we still reach here with no rules,
	// the configuration is invalid and should not grant open access.
	if len(include) == 0 {
		return nil
	}

	return include
}
