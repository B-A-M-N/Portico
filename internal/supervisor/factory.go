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
	hasRealProvider := registerCloudflareWithAccounts(reg, paths, &processManagerAdapter{mgr: procMgr}, st)

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

// registerCloudflareWithAccounts attempts to register the Cloudflare provider
// using accounts loaded from the store. Returns true if the provider was
// successfully registered.
func registerCloudflareWithAccounts(reg provider.Registry, paths app.Paths, procMgr core.ConnectorProcessService, st *store.Store) bool {
	cloudflaredBin := config.CloudflaredBin()
	if cloudflaredBin == "" {
		cloudflaredBin = "cloudflared"
	}

	// Check if cloudflared is available
	if _, err := exec.LookPath(cloudflaredBin); err != nil {
		slog.Info("cloudflared not found, skipping Cloudflare provider", "bin", cloudflaredBin)
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
		reg.SetAccounts("cloudflare", accountIDs)
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

// isDevMode returns true when PORTICO_DEV=true is set.
func isDevMode() bool {
	v, _ := strconv.ParseBool(os.Getenv("PORTICO_DEV"))
	return v
}
