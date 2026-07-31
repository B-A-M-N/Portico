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

// TestDetectionSearchesEveryPlaceTheAdapterDoes is the anti-drift property:
// detection must agree with the code that consumes the credential. The ngrok
// adapter searches both ~/.config/ngrok/ngrok.yml and ~/.ngrok2/ngrok.yml, so a
// token in the second was reported as missing while the adapter would have used
// it — Portico asking for something it already had.
func TestDetectionSearchesEveryPlaceTheAdapterDoes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NGROK_AUTHTOKEN", "")

	legacyDir := filepath.Join(home, ".ngrok2")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "ngrok.yml"),
		[]byte("authtoken: a-real-looking-token\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var source *Detected
	for _, d := range DetectForProvider("ngrok") {
		if d.Kind == SourceClientConfig {
			source = &d
			break
		}
	}
	if source == nil {
		t.Fatal("no client-config source for ngrok")
	}
	if !source.Present {
		t.Fatalf("token in %s was not detected; searched %v", legacyDir, source.Searched)
	}
	// The reported location must be the file that actually carries the token,
	// not the first place searched, or the screen would point at the wrong file.
	if want := filepath.Join(legacyDir, "ngrok.yml"); source.Location != want {
		t.Fatalf("location = %q, want the file that matched %q", source.Location, want)
	}
}

// TestCloudflareTokenIsFoundUnderEitherName pins that the Portico-specific
// variable is detected. internal/cli/handler.go prefers it over the standard
// name, so a user who followed Portico's own error message was told no
// credential was present.
func TestCloudflareTokenIsFoundUnderEitherName(t *testing.T) {
	for _, name := range []string{"CLOUDFLARE_API_TOKEN", "PORTICO_CLOUDFLARE_API_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLOUDFLARE_API_TOKEN", "")
			t.Setenv("PORTICO_CLOUDFLARE_API_TOKEN", "")
			t.Setenv(name, "a-real-looking-token")

			if !HasAny("cloudflare") {
				t.Fatalf("a token in %s was not detected", name)
			}
			for _, d := range DetectForProvider("cloudflare") {
				if d.Kind == SourceEnvironment && d.Present && d.Location != name {
					t.Fatalf("location = %q, want the variable that was set, %q", d.Location, name)
				}
			}
		})
	}
}

// TestAbsentCredentialSaysWhereItLooked ensures a negative result does not
// overclaim. Portico can only report the places it knows to search; a token
// kept elsewhere is not proof that none exists.
func TestAbsentCredentialSaysWhereItLooked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"NGROK_AUTHTOKEN", "CLOUDFLARE_API_TOKEN", "PORTICO_CLOUDFLARE_API_TOKEN"} {
		t.Setenv(key, "")
	}

	for _, d := range Detect() {
		if len(d.Searched) == 0 {
			t.Fatalf("source %s/%s does not report where it looked", d.Provider, d.Location)
		}
		if d.Present {
			continue
		}
		for _, location := range d.Searched {
			if location == "" {
				t.Fatalf("source %s reports an empty search location", d.Provider)
			}
		}
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
