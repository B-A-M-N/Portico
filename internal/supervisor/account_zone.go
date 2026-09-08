package supervisor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// HandleSelectProviderAccountZone applies a zone to an existing account after
// setup returned the provider's visible choices. The credential stays in the
// supervisor: this follow-up only carries the selected zone ID over IPC.
func (h *supervisorHandler) HandleSelectProviderAccountZone(
	providerID, accountID string, req ipc.SelectProviderAccountZoneRequest,
) (*ipc.ConfigureProviderAccountResponse, error) {
	return h.HandleSelectProviderAccountZoneContext(context.Background(), providerID, accountID, req)
}

func (h *supervisorHandler) HandleSelectProviderAccountZoneContext(
	parent context.Context, providerID, accountID string, req ipc.SelectProviderAccountZoneRequest,
) (*ipc.ConfigureProviderAccountResponse, error) {
	release, err := h.sup.admitMutation()
	if err != nil {
		return nil, err
	}
	defer release()

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	providerID = strings.TrimSpace(providerID)
	accountID = strings.TrimSpace(accountID)
	zoneID := strings.TrimSpace(req.ZoneID)
	if providerID == "" || accountID == "" || zoneID == "" {
		return nil, core.ErrValidation("provider, account, and zone are required")
	}
	if providerID != "cloudflare" {
		return nil, core.ErrValidation(fmt.Sprintf("provider %q does not support zone selection", providerID))
	}

	var account *core.ProviderAccount
	accounts, err := h.sup.store.ListProviderAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list provider accounts: %w", err)
	}
	for i := range accounts {
		if string(accounts[i].Provider) == providerID && string(accounts[i].ID) == accountID {
			candidate := accounts[i]
			account = &candidate
			break
		}
	}
	if account == nil {
		return nil, core.ErrValidation("provider account was not found")
	}
	if account.CredentialRef == "" {
		return nil, core.ErrValidation("provider account has no credential")
	}
	credential, err := h.sup.store.LoadProviderCredential(ctx, account.Provider, account.CredentialRef)
	if err != nil {
		return nil, fmt.Errorf("load provider credential: %w", err)
	}
	secret := []byte(credential)
	defer zeroBytes(secret)

	validator := h.sup.accountValidator
	if validator == nil {
		validator = cloudflareAccountValidator{}
	}
	zones, err := validator.VerifyZone(ctx, string(secret), zoneID)
	if err != nil {
		return nil, core.ErrValidation(err.Error())
	}
	var selected ipc.ZoneDTO
	for _, zone := range zones {
		if zone.ID == zoneID {
			selected = ipc.ZoneDTO{ID: zone.ID, Name: zone.Name}
			break
		}
	}
	if selected.ID == "" {
		return nil, core.ErrValidation("the selected zone could not be verified for this account")
	}

	if account.Metadata == nil {
		account.Metadata = map[string]string{}
	}
	account.Metadata["zone_id"] = selected.ID
	account.Metadata["zone_name"] = selected.Name
	account.Metadata["zone_verified"] = "true"
	account.Status = core.AccountAuthenticated
	if err := h.sup.store.UpsertProviderAccountCredential(ctx, *account, secret); err != nil {
		return nil, fmt.Errorf("save provider account zone: %w", err)
	}
	resp := &ipc.ConfigureProviderAccountResponse{
		AccountID:       accountID,
		RestartRequired: false,
		Committed:       true,
		Validated:       true,
		CapabilityLevel: "tunnels_with_dns",
		Zones:           []ipc.ZoneDTO{selected},
		Status:          string(account.Status),
	}
	if activateErr := h.sup.ActivateProvider(ctx, core.ProviderID(providerID)); activateErr != nil {
		resp.Degraded = true
		resp.RestartRequired = true
		resp.ActivationError = activationDegradedMessage(providerID)
		return resp, fmt.Errorf("activate provider %q after selecting zone: %w", providerID, activateErr)
	}
	return resp, nil
}

// HandleListProviderAccountZones returns every DNS zone an account's stored
// credential can see. It never exposes the credential: the supervisor decrypts
// it, lists against the provider, and returns only zone names/IDs. The wizard
// uses it to offer a per-connection zone for a permanent Cloudflare connection
// (finding 7), which the account's single selected default zone cannot express.
func (h *supervisorHandler) HandleListProviderAccountZones(providerID, accountID string) (*ipc.ListProviderAccountZonesResponse, error) {
	release, err := h.sup.admitMutation()
	if err != nil {
		return nil, err
	}
	defer release()

	ctx := context.Background()
	providerID = strings.TrimSpace(providerID)
	accountID = strings.TrimSpace(accountID)
	if providerID == "" || accountID == "" {
		return nil, core.ErrValidation("provider and account are required")
	}
	if providerID != "cloudflare" {
		return nil, core.ErrValidation(fmt.Sprintf("provider %q does not expose zones", providerID))
	}

	var account *core.ProviderAccount
	accounts, err := h.sup.store.ListProviderAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list provider accounts: %w", err)
	}
	for i := range accounts {
		if string(accounts[i].Provider) == providerID && string(accounts[i].ID) == accountID {
			candidate := accounts[i]
			account = &candidate
			break
		}
	}
	if account == nil {
		return nil, core.ErrValidation("provider account was not found")
	}
	if account.CredentialRef == "" {
		return nil, core.ErrValidation("provider account has no credential")
	}
	credential, err := h.sup.store.LoadProviderCredential(ctx, account.Provider, account.CredentialRef)
	if err != nil {
		return nil, fmt.Errorf("load provider credential: %w", err)
	}
	secret := []byte(credential)
	defer zeroBytes(secret)

	validator := h.sup.accountValidator
	if validator == nil {
		validator = cloudflareAccountValidator{}
	}
	zones, err := validator.ListZones(ctx, string(secret))
	if err != nil {
		return nil, core.ErrValidation(err.Error())
	}
	resp := &ipc.ListProviderAccountZonesResponse{
		ProviderID: providerID,
		AccountID:  accountID,
		Zones:      make([]ipc.ZoneDTO, 0, len(zones)),
	}
	for _, zone := range zones {
		resp.Zones = append(resp.Zones, ipc.ZoneDTO{ID: zone.ID, Name: zone.Name})
	}
	return resp, nil
}

func activationDegradedMessage(providerID string) string {
	return fmt.Sprintf("account saved, but %s could not be activated; restart the supervisor or retry activation", providerID)
}
