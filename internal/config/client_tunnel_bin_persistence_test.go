package config

import (
	"testing"

	"github.com/spf13/viper"
)

// A cleared ClientTunnelBin must survive a fresh read of the config file.
//
// The clear was written with viper.Set(key, nil), which does not clear a key
// viper has already read from the file: the override is dropped, AllSettings
// re-merges the old value, and the "cleared" path was rewritten verbatim on
// the next save. Every in-process test passed; the first reload from disk
// restored the value the user had deleted. Writing the empty string and
// reading it as the legitimate "resolve on PATH" answer is the one form whose
// round trip is honest.
func TestClearingClientTunnelBinPersistsAcrossAProcessRestart(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PORTICO_CLIENT_TUNNEL_BIN", "")
	t.Setenv("PORTICO_OPENAI_TUNNEL_BIN", "")
	viper.Reset()
	t.Cleanup(viper.Reset)
	if err := Init(); err != nil {
		t.Fatal(err)
	}

	if err := SaveClientTunnelSettings(false, "/opt/x"); err != nil {
		t.Fatalf("saving a path: %v", err)
	}
	// viper.Reset + Init is a fresh process's read of the written file.
	viper.Reset()
	if err := Init(); err != nil {
		t.Fatal(err)
	}
	if got := ClientTunnelBin(); got != "/opt/x" {
		t.Fatalf("after set, a fresh read = %q, want the saved path", got)
	}

	if err := SaveClientTunnelSettings(false, ""); err != nil {
		t.Fatalf("clearing the path: %v", err)
	}
	viper.Reset()
	if err := Init(); err != nil {
		t.Fatal(err)
	}
	if got := ClientTunnelBin(); got != "" {
		t.Fatalf("after clear, a fresh read = %q, want empty (find on PATH)", got)
	}

	// And the clear is durable: saving an unrelated setting must not resurrect
	// the deleted value by re-merging it from the file.
	if err := SaveLaunchMode("manual"); err != nil {
		t.Fatalf("saving an unrelated setting: %v", err)
	}
	viper.Reset()
	if err := Init(); err != nil {
		t.Fatal(err)
	}
	if got := ClientTunnelBin(); got != "" {
		t.Fatalf("a later save resurrected the cleared path: %q", got)
	}
}
