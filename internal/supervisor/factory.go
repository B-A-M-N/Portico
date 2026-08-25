package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/config"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/process"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/provider/builtin"
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

	// Configure production logging
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Create the supervisor (shares the same process manager).
	sup, err := New(paths, reg, procMgr, st)
	if err != nil {
		return fmt.Errorf("supervisor init: %w", err)
	}

	// Providers are named in one composition list and activated by one path,
	// so adding a provider needs no supervisor change and a live account change
	// produces the same result as a restart.
	services := provider.RuntimeServices{
		Processes:         &processManagerAdapter{mgr: procMgr},
		Gateways:          sup.gatewayMgr,
		TunnelCredentials: st,
		ConnectorDir:      paths.ConnectorDir,
		LogDir:            paths.LogDir,
		LookPath:          exec.LookPath,
		Getenv:            os.Getenv,
	}
	definitions := builtin.Definitions(builtin.Config{
		CloudflaredBin:      config.CloudflaredBin(),
		NgrokBin:            config.NgrokBin(),
		NgrokEnabled:        os.Getenv("PORTICO_ENABLE_EXPERIMENTAL_NGROK") == "1",
		OpenAITunnelEnabled: os.Getenv("PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL") == "1",
		TailscaleBin:        config.TailscaleBin(),
	})
	// The mock provider is never a silent fallback: it appears only on an
	// explicit development opt-in, because reporting fake success to a
	// production user is worse than reporting no provider at all.
	if isDevMode() {
		if err := reg.Add(mock.New()); err != nil {
			slog.Warn("mock provider register failed", "err", err)
		} else {
			slog.Info("mock provider registered (PORTICO_DEV=true)")
		}
	}
	sup.SetProviderDefinitions(definitions, services)

	// Environment credentials are imported before activation, so an import and
	// a restart produce the same durable state in either order.
	seedBootstrapAccounts(ctx, st, sup.verifyBootstrapCredential)
	sup.activateAll(ctx)

	slog.Info("Portico supervisor starting", "socket", paths.SocketPath, "db", paths.DatabasePath)
	return sup.Start(ctx)
}

// seedBootstrapAccounts imports provider credentials found in the supervisor's
// environment.
//
// This is a durable write and is deliberately not part of activation, which is
// a pure read. Keeping them separate is what lets activation carry no store
// handle at all.
func seedBootstrapAccounts(ctx context.Context, st *store.Store, verify bootstrapVerifier) {
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
		seedBootstrapAccount(ctx, st, "cloudflare", accountID, apiToken,
			map[string]string{"zone_id": zoneID}, verify)
	}

	// ngrok is deliberately absent. Its agent reads NGROK_AUTHTOKEN from the
	// environment itself, so importing that token as an account was never
	// necessary — and because Portico cannot check an ngrok token, importing it
	// could only ever produce an account claiming to be verified when it was
	// not. The definition activates from the environment instead.
}

// isDevMode returns true when PORTICO_DEV=true is set.
func isDevMode() bool {
	v, _ := strconv.ParseBool(os.Getenv("PORTICO_DEV"))
	return v
}
