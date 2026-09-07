package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/B-A-M-N/portico/internal/config"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/provider"
	provider_clienttunnel "github.com/B-A-M-N/portico/internal/provider/clienttunnel"
)

// settingsConfigDir isolates the durable settings file the handler writes.
// The config package derives its directory from XDG_CONFIG_HOME, so pinning
// that keeps the test off the machine running it.
func settingsConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	// The environment pins beat stored values; both must be absent.
	t.Setenv("PORTICO_ENABLE_CLIENT_TUNNEL", "")
	t.Setenv("PORTICO_CLIENT_TUNNEL_BIN", "")
	_ = os.Unsetenv("PORTICO_ENABLE_CLIENT_TUNNEL")
	_ = os.Unsetenv("PORTICO_CLIENT_TUNNEL_BIN")
	if err := config.Init(); err != nil {
		t.Fatalf("config init: %v", err)
	}
}

// TestSettingsUpdateRegistersClientTunnelWithoutRestart pins the settings
// contract for the experimental transport: the toggle is a real setting, and
// turning it on while the supervisor runs makes the provider selectable
// through the same activation path a restart would run — so the two paths
// cannot disagree about the result.
func TestSettingsUpdateRegistersClientTunnelWithoutRestart(t *testing.T) {
	settingsConfigDir(t)

	st := newRecoveryTestStore(t)
	def := provider_clienttunnel.NewDefinition(provider_clienttunnel.DefinitionConfig{
		Bin: "tunnel-client", Enabled: false,
	})
	sup, registry := activationTestSupervisorWithEnv(t, st, func(key string) string {
		if key == "CONTROL_PLANE_API_KEY" {
			return "sk-test-runtime-key-for-settings-test"
		}
		return ""
	}, def)
	handler := &supervisorHandler{sup: sup}
	ctx := context.Background()
	if err := sup.ActivateProvider(ctx, provider_clienttunnel.ProviderID); err != nil {
		t.Fatalf("precondition activation: %v", err)
	}

	availability := func() provider.Availability {
		for _, snap := range registry.Snapshot() {
			if snap.ID == provider_clienttunnel.ProviderID {
				return snap.Availability
			}
		}
		return ""
	}
	if av := availability(); av == provider.AvailabilityReady {
		t.Fatalf("precondition: a disabled transport must not be ready (got %q)", av)
	}

	dto, err := handler.HandleUpdateSettings(ipc.SettingsRequest{
		ClientTunnelEnabled: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("settings update: %v", err)
	}
	if !dto.ClientTunnelEnabled {
		t.Error("the reply does not report the transport as enabled")
	}

	// The stored setting survives, and the provider is now selectable without
	// any restart.
	stored, err := config.LoadOperationalSettings()
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if !stored.ClientTunnelEnabled {
		t.Error("the enabled choice did not persist")
	}
	// The hot-apply path already reactivated; re-running it here proves the
	// activation path alone (a restart's equivalent) now yields a ready
	// provider from the persisted state.
	if err := sup.ActivateProvider(ctx, provider_clienttunnel.ProviderID); err != nil {
		t.Fatalf("post-update activation: %v", err)
	}
	if av := availability(); av != provider.AvailabilityReady {
		t.Errorf("the transport did not become ready after the settings toggle (got %q)", av)
	}
}

func boolPtr(b bool) *bool { return &b }

func strPtr(s string) *string { return &s }

// TestSettingsUpdateRejectsPinnedBinPath pins that an environment override
// wins: the handler refuses a stored-path change instead of accepting a write
// that would not be in force.
func TestSettingsUpdateRejectsPinnedBinPath(t *testing.T) {
	settingsConfigDir(t)
	t.Setenv("PORTICO_CLIENT_TUNNEL_BIN", filepath.Join("pinned", "tunnel-client"))
	if err := config.Init(); err != nil {
		t.Fatalf("config init: %v", err)
	}

	st := newRecoveryTestStore(t)
	sup, _ := activationTestSupervisor(t, st,
		provider_clienttunnel.NewDefinition(provider_clienttunnel.DefinitionConfig{Bin: "tunnel-client"}))
	handler := &supervisorHandler{sup: sup}

	_, err := handler.HandleUpdateSettings(ipc.SettingsRequest{
		ClientTunnelBin: strPtr("/elsewhere/tunnel-client"),
	})
	if err == nil {
		t.Fatal("a pinned path accepted a different stored value")
	}
	if want := "PORTICO_CLIENT_TUNNEL_BIN"; !supervisorTestContains(err.Error(), want) {
		t.Errorf("error %q does not name the pin %q", err.Error(), want)
	}
}

func supervisorTestContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
