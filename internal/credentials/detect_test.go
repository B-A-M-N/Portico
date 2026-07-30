package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetectionReportsPresenceWithoutReadingValues pins the core safety
// property: detection tells you where a credential is, never what it is, so the
// result is safe to render, log and export.
func TestDetectionReportsPresenceWithoutReadingValues(t *testing.T) {
	const secret = "ngrok-secret-value-DO-NOT-LEAK"
	t.Setenv("NGROK_AUTHTOKEN", secret)

	for _, d := range Detect() {
		if d.Location == secret || d.Description == secret || d.Action == secret {
			t.Fatalf("detection leaked a credential value: %+v", d)
		}
	}

	if !HasAny("ngrok") {
		t.Fatal("a credential in the environment was not detected")
	}
}

// TestDetectionFindsAnExistingClientConfig is the case that matters most for
// setup friction: the machine already has a working token and Portico should
// say so rather than asking for it again.
func TestDetectionFindsAnExistingClientConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NGROK_AUTHTOKEN", "")

	configDir := filepath.Join(home, ".config", "ngrok")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "ngrok.yml"),
		[]byte("version: \"3\"\nauthtoken: a-real-looking-token\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, d := range DetectForProvider("ngrok") {
		if d.Kind == SourceClientConfig && d.Present {
			found = true
		}
	}
	if !found {
		t.Fatal("an existing ngrok client configuration was not detected")
	}
}

// TestMissingCredentialsCarryAnAction ensures every gap is actionable rather
// than merely reported.
func TestMissingCredentialsCarryAnAction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"NGROK_AUTHTOKEN", "CLOUDFLARE_API_TOKEN", "CONTROL_PLANE_API_KEY"} {
		t.Setenv(key, "")
	}

	for _, d := range Detect() {
		if d.Present {
			continue
		}
		if d.Action == "" {
			t.Fatalf("missing credential %s/%s offers no action", d.Provider, d.Location)
		}
		if d.Description == "" {
			t.Fatalf("missing credential %s/%s does not say what it is for", d.Provider, d.Location)
		}
	}
}

// TestEmptyConfigValueIsNotPresence guards against treating a declared but
// empty key as a configured credential.
func TestEmptyConfigValueIsNotPresence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ngrok.yml")
	if err := os.WriteFile(path, []byte("authtoken:\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if configFileHasKey(path, "authtoken") {
		t.Fatal("an empty authtoken was treated as present")
	}
}
