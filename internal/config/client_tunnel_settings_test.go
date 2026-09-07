package config

import (
	"testing"

	"github.com/spf13/viper"
)

// TestClientTunnelSettingsRoundTrip pins the persistence contract for the
// client-tunnel transport settings: the supervisor persists them through the
// same operational-settings path as every other setting, and a save of an
// unrelated field must not silently downgrade a tunnel enabled through the
// legacy key.
func TestClientTunnelSettingsRoundTrip(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	settings, err := LoadOperationalSettings()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if settings.ClientTunnelEnabled {
		t.Error("a fresh installation has the client tunnel off")
	}

	if err := SaveClientTunnelSettings(true, "/opt/tunnel-client"); err != nil {
		t.Fatalf("save: %v", err)
	}
	settings, err = LoadOperationalSettings()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !settings.ClientTunnelEnabled {
		t.Error("enabled did not persist")
	}
	if settings.ClientTunnelBin != "/opt/tunnel-client" {
		t.Errorf("bin = %q, want /opt/tunnel-client", settings.ClientTunnelBin)
	}

	// Clearing the path returns to PATH lookup instead of persisting an empty
	// string that the loader would then reject.
	if err := SaveClientTunnelSettings(true, ""); err != nil {
		t.Fatalf("save cleared bin: %v", err)
	}
	settings, err = LoadOperationalSettings()
	if err != nil {
		t.Fatalf("reload after clear: %v", err)
	}
	if settings.ClientTunnelBin != "" {
		t.Errorf("cleared bin = %q, want empty", settings.ClientTunnelBin)
	}
}

// TestLegacyEnabledKeySurvivesUnrelatedSave pins the migration contract: a
// tunnel enabled through the legacy openai_tunnel.enabled key stays enabled
// when an unrelated setting is saved, because LoadOperationalSettings falls
// back to the legacy key until its compatibility window closes.
func TestLegacyEnabledKeySurvivesUnrelatedSave(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set(KeyOpenAITunnelEnabled, true)

	if err := SaveDefaultOnDisconnect("close"); err != nil {
		t.Fatalf("unrelated save: %v", err)
	}
	settings, err := LoadOperationalSettings()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !settings.ClientTunnelEnabled {
		t.Error("saving an unrelated setting downgraded a legacy-enabled tunnel")
	}
}
