package config

import (
	"testing"

	"github.com/spf13/viper"
)

func TestClientTunnelCanonicalEnvironmentWinsOverLegacy(t *testing.T) {
	t.Setenv("PORTICO_ENABLE_CLIENT_TUNNEL", "0")
	t.Setenv("PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL", "1")
	if got := ClientTunnelEnabled(); got {
		t.Fatal("legacy OpenAI opt-in overrode the canonical disabled value")
	}

	t.Setenv("PORTICO_ENABLE_CLIENT_TUNNEL", "1")
	if got := ClientTunnelEnabled(); !got {
		t.Fatal("canonical client-tunnel opt-in was ignored")
	}
}

func TestClientTunnelLegacyEnvironmentRemainsAccepted(t *testing.T) {
	t.Setenv("PORTICO_ENABLE_CLIENT_TUNNEL", "")
	t.Setenv("PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL", "1")
	if got := ClientTunnelEnabled(); !got {
		t.Fatal("legacy OpenAI opt-in was not accepted during migration")
	}
}

func TestClientTunnelBinaryCanonicalEnvironmentWinsOverLegacy(t *testing.T) {
	viper.Set(KeyClientTunnelBin, "configured-client")
	t.Cleanup(func() { viper.Set(KeyClientTunnelBin, "") })
	t.Setenv("PORTICO_CLIENT_TUNNEL_BIN", "/canonical/tunnel-client")
	t.Setenv("PORTICO_OPENAI_TUNNEL_BIN", "/legacy/tunnel-client")
	if got := ClientTunnelBin(); got != "/canonical/tunnel-client" {
		t.Fatalf("binary = %q, want canonical environment value", got)
	}
}

func TestClientTunnelBinaryCanonicalConfigWinsOverLegacyConfig(t *testing.T) {
	t.Setenv("PORTICO_CLIENT_TUNNEL_BIN", "")
	t.Setenv("PORTICO_OPENAI_TUNNEL_BIN", "")
	viper.Set(KeyClientTunnelBin, "configured-client")
	viper.Set(KeyOpenAITunnelBin, "legacy-client")
	t.Cleanup(func() {
		viper.Set(KeyClientTunnelBin, "")
		viper.Set(KeyOpenAITunnelBin, "")
	})
	if got := ClientTunnelBin(); got != "configured-client" {
		t.Fatalf("binary = %q, want canonical config value", got)
	}
}

func TestClientTunnelBinaryLegacyConfigRemainsAccepted(t *testing.T) {
	t.Setenv("PORTICO_CLIENT_TUNNEL_BIN", "")
	t.Setenv("PORTICO_OPENAI_TUNNEL_BIN", "")
	viper.Set(KeyClientTunnelBin, "")
	viper.Set(KeyOpenAITunnelBin, "legacy-client")
	t.Cleanup(func() { viper.Set(KeyOpenAITunnelBin, "") })
	if got := ClientTunnelBin(); got != "legacy-client" {
		t.Fatalf("binary = %q, want legacy config fallback", got)
	}
}
