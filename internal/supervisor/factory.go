package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/paoloanzn/portico/internal/app"
	"github.com/paoloanzn/portico/internal/config"
	"github.com/paoloanzn/portico/internal/core"
	"github.com/paoloanzn/portico/internal/process"
	"github.com/paoloanzn/portico/internal/provider"
	"github.com/paoloanzn/portico/internal/provider/cloudflare"
	"github.com/paoloanzn/portico/internal/provider/mock"
	"github.com/paoloanzn/portico/internal/store"
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

	// Load provider accounts from store.
	accounts, err := st.ListProviderAccounts(ctx)
	if err != nil {
		slog.Warn("provider accounts not available", "err", err)
	} else {
		// Group account IDs by provider
		type providerGroup struct {
			providerID core.ProviderID
			ids        []core.ProviderAccountID
		}
		groups := make(map[core.ProviderID]*providerGroup)
		for _, a := range accounts {
			g, ok := groups[a.Provider]
			if !ok {
				g = &providerGroup{providerID: a.Provider}
				groups[a.Provider] = g
			}
			g.ids = append(g.ids, a.ID)
		}

		// Register accounts with each provider
		for provID, group := range groups {
			reg.SetAccounts(provID, group.ids)
			slog.Info("provider accounts loaded", "provider", provID, "count", len(group.ids))
		}
	}

	// Register production providers using loaded accounts.
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

	// Get accounts for Cloudflare from the store
	accounts := reg.GetAccounts("cloudflare")
	hasAPIAccount := len(accounts) > 0

	var apiToken, accountID, zoneID string

	if hasAPIAccount {
		// Use the first account's credentials
		// In a real implementation, we'd resolve the credential reference
		// For now, fall back to env vars for actual token values
		apiToken = config.APIToken()
		if apiToken != "" {
			accountID = os.Getenv("CLOUDFLARE_ACCOUNT_ID")
			zoneID = os.Getenv("CLOUDFLARE_ZONE_ID")
		}
	}

	if apiToken != "" {
		// Full provider with API access
		cf, err := cloudflare.New(apiToken, accountID, zoneID, cloudflaredBin, paths.ConnectorDir, procMgr)
		if err != nil {
			slog.Warn("cloudflare provider init failed", "err", err)
			return false
		}

		// Wire credential store for durable tunnel token storage (P0 #4).
		cf.SetCredentialStore(st)

		if err := reg.Add(cf); err != nil {
			slog.Warn("cloudflare provider register failed", "err", err)
			return false
		}

		slog.Info("cloudflare provider registered (full API access)")
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
