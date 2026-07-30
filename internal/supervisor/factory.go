package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/config"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/process"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/provider/cloudflare"
	"github.com/B-A-M-N/portico/internal/provider/mock"
	"github.com/B-A-M-N/portico/internal/provider/ngrok"
	"github.com/B-A-M-N/portico/internal/provider/openaitunnel"
	"github.com/B-A-M-N/portico/internal/store"
)

// processManagerAdapter wraps *process.Manager to implement core.ConnectorProcessService.
type processManagerAdapter struct {
	mgr *process.Manager
}

func (a *processManagerAdapter) Start(ctx context.Context, cfg core.ProcessConfig) (core.ConnectorHandle, error) {
	mp, err := a.mgr.Start(ctx, process.ProcessConfig{
		ConnectionID: cfg.ConnectionID,
		Spec:         cfg.Spec,
	})
	if err != nil {
		return core.ConnectorHandle{}, err
	}
	pid := 0
	if mp.Cmd != nil && mp.Cmd.Process != nil {
		pid = mp.Cmd.Process.Pid
	}
	return core.ConnectorHandle{
		ConnectionID: mp.ConnectionID,
		Identity:     mp.Identity,
		PID:          pid,
	}, nil
}

func (a *processManagerAdapter) Stop(connectionID core.ConnectionID, timeout time.Duration) error {
	return a.mgr.Stop(connectionID, timeout)
}

func (a *processManagerAdapter) Observe(connectionID core.ConnectionID) (core.ConnectorHandle, bool) {
	return a.mgr.Observe(connectionID)
}

// RunSupervisor constructs and runs the production supervisor.
// This is the single entry point for supervisor mode (called by "portico supervisor run").
func RunSupervisor(ctx context.Context) error {
	paths := app.DefaultPaths()

	// Initialize config (reads config file, env vars).
	if err := config.Init(); err != nil {
		slog.Warn("config init", "err", err)
	}

	// Create provider registry (empty initially).
	reg := provider.NewRegistry()

	// Create process manager for connector lifecycle.
	procMgr := process.NewManager()

	// Open SQLite store and run migrations first.
	st, err := store.Open(paths.DatabasePath)
	if err != nil {
		return fmt.Errorf("store open: %w", err)
	}

	// Register production providers. Persisted account rows are loaded and
	// validated against the concrete adapter during startup; doing it before
	// adapter construction would advertise credentials it cannot actually use.
	registerProviderCatalog(reg)
	registerOpenAITunnel(reg, &processManagerAdapter{mgr: procMgr})
	hasRealProvider := registerCloudflareWithAccounts(reg, paths, &processManagerAdapter{mgr: procMgr}, st)
	hasRealProvider = registerNgrokWithAccounts(reg, paths, &processManagerAdapter{mgr: procMgr}, st) || hasRealProvider

	// Register mock provider only in explicit development mode.
	// Never silently fall back to mock when no real provider is configured,
	// as that would report fake success to production users.
	if isDevMode() {
		if err := reg.Add(mock.New()); err != nil {
			slog.Warn("mock provider register failed", "err", err)
		} else {
			slog.Info("mock provider registered (PORTICO_DEV=true)")
		}
	} else if !hasRealProvider {
		slog.Warn("no real provider configured and PORTICO_DEV not set; connections will be unavailable")
	}

	// Configure production logging
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Create and start the supervisor (shares the same process manager).
	sup, err := New(paths, reg, procMgr, st)
	if err != nil {
		return fmt.Errorf("supervisor init: %w", err)
	}

	slog.Info("Portico supervisor starting", "socket", paths.SocketPath, "db", paths.DatabasePath)
	return sup.Start(ctx)
}

// registerProviderCatalog records the providers Portico names but ships no
// adapter for. Without these entries the UI cannot distinguish a provider that
// does not exist from one that is merely unconfigured, and documentation
// claiming support for them has nothing to contradict it.
//
// Entries are added before adapter construction so a successful registration
// supersedes them.
func registerProviderCatalog(reg provider.Registry) {
	for _, entry := range []provider.CatalogEntry{
		{
			ID: "tailscale", Name: "tailscale", DisplayName: "Tailscale",
			Availability: provider.AvailabilityNotImplemented,
			Reason:       "Portico ships no Tailscale adapter yet",
		},
		{
			ID: "zrok", Name: "zrok", DisplayName: "zrok",
			Availability: provider.AvailabilityNotImplemented,
			Reason:       "Portico ships no zrok adapter yet",
		},
		{
			ID: "openai_tunnel", Name: "openai_tunnel", DisplayName: "OpenAI Secure MCP Tunnel",
			Availability: provider.AvailabilityExperimental,
			Reason: "the adapter has not been exercised against a live tunnel; " +
				"it can start and observe the client but does not create tunnels or verify ChatGPT app registration",
			SetupActions: []string{
				"Install tunnel-client from the OpenAI platform's tunnel settings",
				"Create a tunnel there and note its ID; Portico does not create tunnels",
				"Export CONTROL_PLANE_API_KEY before starting the supervisor",
				"Set PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL=1 to register the provider",
			},
		},
	} {
		reg.AddCatalogEntry(entry)
	}
}

// registerOpenAITunnel registers the Secure MCP Tunnel provider behind an
// explicit opt-in. The adapter is experimental and has not been run against a
// live tunnel, so it stays out of standard flows for the same reason ngrok
// does.
func registerOpenAITunnel(reg provider.Registry, procMgr core.ConnectorProcessService) bool {
	if os.Getenv("PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL") != "1" {
		return false
	}
	if err := reg.Add(openaitunnel.New("", procMgr)); err != nil {
		slog.Warn("OpenAI tunnel provider register failed", "err", err)
		return false
	}
	slog.Warn("registering EXPERIMENTAL OpenAI Secure MCP Tunnel provider; " +
		"it has not been exercised against a live tunnel")
	return true
}

// registerCloudflareWithAccounts attempts to register the Cloudflare provider
// using accounts loaded from the store. Returns true if the provider was
// successfully registered.
func registerCloudflareWithAccounts(reg provider.Registry, paths app.Paths, procMgr core.ConnectorProcessService, st *store.Store) bool {
	cloudflaredBin := config.CloudflaredBin()
	if cloudflaredBin == "" {
		cloudflaredBin = "cloudflared"
	}

	// Check if cloudflared is available. A missing client is a setup gap, not a
	// reason for the provider to vanish: record it in the catalog so the UI can
	// say "install cloudflared" instead of silently omitting Cloudflare.
	if _, err := exec.LookPath(cloudflaredBin); err != nil {
		slog.Info("cloudflared not found, Cloudflare provider unavailable", "bin", cloudflaredBin)
		reg.AddCatalogEntry(provider.CatalogEntry{
			ID:           "cloudflare",
			Name:         "cloudflare",
			DisplayName:  "Cloudflare",
			Availability: provider.AvailabilityClientMissing,
			Reason:       fmt.Sprintf("the %q client was not found on PATH", cloudflaredBin),
			SetupActions: []string{
				"Install cloudflared from https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/",
				"Ensure cloudflared is on PATH, or set the cloudflared binary path in Portico's configuration",
			},
		})
		return false
	}

	// Environment/configured token credentials are a one-time bootstrap path.
	// Move them to the supervisor's encrypted account store before constructing
	// adapters, so subsequent operations resolve the account selected by the
	// profile rather than a process-global environment token.
	apiToken := config.APIToken()
	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	if accountID == "" {
		accountID = config.AccountID()
	}
	zoneID := os.Getenv("CLOUDFLARE_ZONE_ID")
	if zoneID == "" {
		zoneID = config.ZoneID()
	}

	if apiToken != "" && accountID != "" && zoneID != "" {
		credentialRef := fmt.Sprintf("cloudflare:%s:api-token", accountID)
		if err := st.SaveProviderCredential(context.Background(), "cloudflare", credentialRef, []byte(apiToken)); err != nil {
			slog.Warn("persist Cloudflare bootstrap credential", "err", err)
		} else if err := st.UpsertProviderAccount(context.Background(), core.ProviderAccount{
			ID:            core.ProviderAccountID(accountID),
			Provider:      "cloudflare",
			Label:         accountID,
			CredentialRef: credentialRef,
			Metadata:      map[string]string{"zone_id": zoneID},
			Status:        core.AccountAuthenticated,
		}); err != nil {
			slog.Warn("persist Cloudflare bootstrap account", "err", err)
		}
	}

	accounts, err := st.ListProviderAccounts(context.Background())
	if err != nil {
		slog.Warn("list Cloudflare accounts", "err", err)
		accounts = nil
	}
	children := make(map[core.ProviderAccountID]*cloudflare.Provider)
	accountDetails := make(map[core.ProviderAccountID]core.ProviderAccount)
	for _, account := range accounts {
		if account.Provider != "cloudflare" || account.Status != core.AccountAuthenticated {
			continue
		}
		zone := strings.TrimSpace(account.Metadata["zone_id"])
		if account.CredentialRef == "" || zone == "" {
			slog.Warn("Cloudflare account is missing a credential reference or zone", "account", account.ID)
			continue
		}
		token, loadErr := st.LoadProviderCredential(context.Background(), "cloudflare", account.CredentialRef)
		if loadErr != nil || token == "" {
			slog.Warn("Cloudflare account credential is unavailable", "account", account.ID, "err", loadErr)
			continue
		}
		child, newErr := cloudflare.New(token, string(account.ID), zone, cloudflaredBin, paths.ConnectorDir, procMgr)
		if newErr != nil {
			slog.Warn("Cloudflare account adapter init failed", "account", account.ID, "err", newErr)
			continue
		}
		child.SetCredentialStore(st)
		children[account.ID] = child
		accountDetails[account.ID] = account
	}
	if len(children) > 0 {
		accountsProvider, newErr := cloudflare.NewAccountsProvider(children)
		if newErr != nil {
			slog.Warn("Cloudflare multi-account provider init failed", "err", newErr)
			return false
		}
		if err := reg.Add(accountsProvider); err != nil {
			slog.Warn("Cloudflare provider register failed", "err", err)
			return false
		}
		accountIDs := make([]core.ProviderAccountID, 0, len(children))
		for id := range children {
			accountIDs = append(accountIDs, id)
		}
		sort.Slice(accountIDs, func(i, j int) bool { return accountIDs[i] < accountIDs[j] })
		infos := make([]provider.AccountInfo, 0, len(accountIDs))
		for _, id := range accountIDs {
			account := accountDetails[id]
			infos = append(infos, provider.AccountInfo{ID: id, Label: account.Label, Status: string(account.Status)})
		}
		reg.SetAccountInfo("cloudflare", infos)
		slog.Info("Cloudflare provider registered", "accounts", len(accountIDs))
		return true
	}

	// No API token — register for Quick Tunnels only
	cf, err := cloudflare.NewQuickTunnel(cloudflaredBin, paths.ConnectorDir, procMgr)
	if err != nil {
		slog.Warn("cloudflare quick tunnel provider init failed", "err", err)
		return false
	}

	// Wire credential store for durable tunnel token storage (P0 #4).
	cf.SetCredentialStore(st)

	if err := reg.Add(cf); err != nil {
		slog.Warn("cloudflare provider register failed", "err", err)
		return false
	}

	slog.Info("cloudflare provider registered (Quick Tunnels only — no API token)")
	return true
}

// registerNgrokWithAccounts attempts to register the Ngrok provider
// using accounts loaded from the store. Returns true if the provider was
// successfully registered.
func registerNgrokWithAccounts(reg provider.Registry, paths app.Paths, procMgr core.ConnectorProcessService, st *store.Store) bool {
	// The Ngrok adapter is experimental and is not lifecycle-complete: it
	// synthesises tunnel identifiers rather than creating provider resources,
	// targets a hardcoded local port, cannot reconstruct observed state after a
	// supervisor restart, and implements delete and protection as no-ops. It
	// must therefore stay out of standard flows until it is rebuilt, and is
	// registered only when the operator opts in explicitly.
	if os.Getenv("PORTICO_ENABLE_EXPERIMENTAL_NGROK") != "1" {
		slog.Info("Ngrok provider is experimental and disabled; " +
			"set PORTICO_ENABLE_EXPERIMENTAL_NGROK=1 to register it")
		reg.AddCatalogEntry(provider.CatalogEntry{
			ID:           "ngrok",
			Name:         "ngrok",
			DisplayName:  "ngrok",
			Availability: provider.AvailabilityExperimental,
			Reason: "the ngrok adapter is experimental and not lifecycle-complete: " +
				"it does not create real provider resources, targets a fixed local port, " +
				"cannot rebuild observed state after a restart, and applies no protection",
			SetupActions: []string{
				"Set PORTICO_ENABLE_EXPERIMENTAL_NGROK=1 to register it anyway",
				"Do not rely on it for connections that matter",
			},
		})
		return false
	}

	ngrokBin := config.NgrokBin()
	if ngrokBin == "" {
		ngrokBin = "ngrok"
	}

	// Check if ngrok is available
	if _, err := exec.LookPath(ngrokBin); err != nil {
		slog.Info("ngrok not found, Ngrok provider unavailable", "bin", ngrokBin)
		reg.AddCatalogEntry(provider.CatalogEntry{
			ID:           "ngrok",
			Name:         "ngrok",
			DisplayName:  "ngrok",
			Availability: provider.AvailabilityClientMissing,
			Reason:       fmt.Sprintf("the %q client was not found on PATH", ngrokBin),
			SetupActions: []string{"Install ngrok and ensure it is on PATH"},
		})
		return false
	}

	slog.Warn("registering EXPERIMENTAL Ngrok provider; " +
		"it is not lifecycle-complete and must not be relied on")

	// Environment/configured token credentials are a one-time bootstrap path.
	// Move them to the supervisor's encrypted account store before constructing
	// adapters, so subsequent operations resolve the account selected by the
	// profile rather than a process-global environment token.
	apiToken := config.NgrokAPIToken()
	accountID := os.Getenv("NGROK_ACCOUNT_ID")
	if accountID == "" {
		accountID = config.NgrokAccountID()
	}

	if apiToken != "" && accountID != "" {
		credentialRef := fmt.Sprintf("ngrok:%s:api-token", accountID)
		if err := st.SaveProviderCredential(context.Background(), "ngrok", credentialRef, []byte(apiToken)); err != nil {
			slog.Warn("persist Ngrok bootstrap credential", "err", err)
		} else if err := st.UpsertProviderAccount(context.Background(), core.ProviderAccount{
			ID:            core.ProviderAccountID(accountID),
			Provider:      "ngrok",
			Label:         accountID,
			CredentialRef: credentialRef,
			Metadata:      map[string]string{},
			Status:        core.AccountAuthenticated,
		}); err != nil {
			slog.Warn("persist Ngrok bootstrap account", "err", err)
		}
	}

	accounts, err := st.ListProviderAccounts(context.Background())
	if err != nil {
		slog.Warn("list Ngrok accounts", "err", err)
		accounts = nil
	}

	for _, account := range accounts {
		if account.Provider != "ngrok" || account.Status != core.AccountAuthenticated {
			continue
		}
		if account.CredentialRef == "" {
			slog.Warn("Ngrok account is missing a credential reference", "account", account.ID)
			continue
		}
		token, loadErr := st.LoadProviderCredential(context.Background(), "ngrok", account.CredentialRef)
		if loadErr != nil || token == "" {
			slog.Warn("Ngrok account credential is unavailable", "account", account.ID, "err", loadErr)
			continue
		}
		child, newErr := ngrok.New(token, ngrokBin, procMgr)
		if newErr != nil {
			slog.Warn("Ngrok account adapter init failed", "account", account.ID, "err", newErr)
			continue
		}
		// Ngrok doesn't need multi-account routing like Cloudflare
		if err := reg.Add(child); err != nil {
			slog.Warn("Ngrok provider register failed", "err", err)
			return false
		}
		reg.SetAccountInfo("ngrok", []provider.AccountInfo{{ID: account.ID, Label: account.Label, Status: string(account.Status)}})
		slog.Info("Ngrok provider registered", "account", account.ID)
		return true
	}

	// No API token — register with default config (will fail at Authenticate)
	slog.Info("Ngrok provider registered without API token (requires configuration)")
	return false
}

// isDevMode returns true when PORTICO_DEV=true is set.
func isDevMode() bool {
	v, _ := strconv.ParseBool(os.Getenv("PORTICO_DEV"))
	return v
}
