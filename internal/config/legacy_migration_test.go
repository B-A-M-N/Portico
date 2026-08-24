package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// Importing an older installation's configuration.
//
// This used to be an open-ended compatibility read: with no Portico config present,
// viper was pointed at the legacy config.yaml and left there. The settings were
// re-read from the old file on every start, so the installation never moved and a user
// editing the file Portico documents saw no effect. docs/REMAINING_WORK.md asks for an
// explicit migration instead.

// legacyFixture creates an old-style config.yaml in a fake home directory, and points
// both the legacy lookup and the Portico config directory at temporary paths.
func legacyFixture(t *testing.T, contents string) (configDir, legacyPath string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Dir() reads XDG_CONFIG_HOME, so the Portico directory is separate from the
	// legacy one and the test is not sensitive to the developer's own config.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	// The real legacy location, which legacyDir derives from the home directory.
	legacyDirPath := filepath.Join(home, ".config", "flare-cli")
	if err := os.MkdirAll(legacyDirPath, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath = filepath.Join(legacyDirPath, "config.yaml")
	if err := os.WriteFile(legacyPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	configDir = filepath.Join(xdg, "portico")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Each case gets a clean viper, since it is global state.
	viper.Reset()
	t.Cleanup(viper.Reset)
	return configDir, legacyPath
}

// TestLegacySettingsAreImportedOnce pins the whole point of the change.
func TestLegacySettingsAreImportedOnce(t *testing.T) {
	configDir, legacyPath := legacyFixture(t, "cloudflare:\n  account_id: acct-from-legacy\n")

	if err := migrateLegacyConfig(configDir); err != nil {
		t.Fatalf("migrateLegacyConfig: %v", err)
	}

	// The value is now Portico's.
	if got := viper.GetString("cloudflare.account_id"); got != "acct-from-legacy" {
		t.Fatalf("the legacy account ID was not imported: %q", got)
	}

	// In Portico's own file, in Portico's own format.
	target := filepath.Join(configDir, "config.toml")
	written, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the migrated config was not written: %v", err)
	}
	if !strings.Contains(string(written), "acct-from-legacy") {
		t.Fatalf("the written config does not carry the imported value:\n%s", written)
	}

	// And the original is moved aside, so the next start does not migrate again and
	// discard anything changed since.
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("the legacy file is still in place, so it would be re-read: %v", err)
	}
	// Renamed rather than deleted: this is the user's own configuration, and a
	// migration that misread something should leave the original recoverable.
	if _, err := os.Stat(legacyPath + ".migrated"); err != nil {
		t.Fatalf("the legacy file was deleted rather than kept: %v", err)
	}
}

// TestASecondStartDoesNotMigrateAgain pins that the import is not repeated.
//
// This is what "once" has to mean in practice: a user who changes a setting after
// migrating must not have it overwritten by the old file on the next start.
func TestASecondStartDoesNotMigrateAgain(t *testing.T) {
	configDir, _ := legacyFixture(t, "cloudflare:\n  account_id: acct-from-legacy\n")

	if err := migrateLegacyConfig(configDir); err != nil {
		t.Fatal(err)
	}
	// The user changes it afterwards.
	viper.Set("cloudflare.account_id", "acct-the-user-chose")

	// A second start finds nothing to migrate.
	if err := migrateLegacyConfig(configDir); err != nil {
		t.Fatalf("the second migration attempt failed: %v", err)
	}
	if got := viper.GetString("cloudflare.account_id"); got != "acct-the-user-chose" {
		t.Fatalf("the second start overwrote the user's own value with %q", got)
	}
}

// TestAFreshInstallationIsNotAFailure pins that no legacy file is the common case.
func TestAFreshInstallationIsNotAFailure(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", xdg)
	viper.Reset()
	t.Cleanup(viper.Reset)

	configDir := filepath.Join(xdg, "portico")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := migrateLegacyConfig(configDir); err != nil {
		t.Fatalf("a fresh installation reported a migration failure: %v", err)
	}
	// And it does not create a config file for settings nobody has set.
	if _, err := os.Stat(filepath.Join(configDir, "config.toml")); !os.IsNotExist(err) {
		t.Error("a fresh installation wrote a config file during migration")
	}
}

// TestAnUnreadableLegacyFileIsReportedInPlainLanguage pins that a corrupt old config
// says what could not be read.
//
// The alternative is a viper parse error at startup with no indication that it came
// from a file in a directory the user may have forgotten exists.
func TestAnUnreadableLegacyFileIsReportedInPlainLanguage(t *testing.T) {
	configDir, legacyPath := legacyFixture(t, "this: is: not: valid: yaml:\n\t- neither is this\n")

	err := migrateLegacyConfig(configDir)
	if err == nil {
		t.Fatal("an unparseable legacy config was accepted")
	}
	if !strings.Contains(err.Error(), "previous installation") {
		t.Errorf("the error does not say where the problem is: %v", err)
	}
	if !strings.Contains(err.Error(), legacyPath) {
		t.Errorf("the error does not name the file: %v", err)
	}
	// The original is left alone: nothing was successfully imported, so moving it
	// aside would lose the only copy.
	if _, statErr := os.Stat(legacyPath); statErr != nil {
		t.Errorf("a legacy file that could not be read was moved anyway: %v", statErr)
	}
}

// TestDefaultsSurviveAMigrationThatDoesNotMentionThem pins that importing a partial
// legacy file does not blank everything else.
func TestDefaultsSurviveAMigrationThatDoesNotMentionThem(t *testing.T) {
	configDir, _ := legacyFixture(t, "cloudflare:\n  account_id: acct-from-legacy\n")

	// A default the legacy file says nothing about.
	viper.SetDefault("launch.mode", "manual")

	if err := migrateLegacyConfig(configDir); err != nil {
		t.Fatal(err)
	}
	if got := viper.GetString("launch.mode"); got != "manual" {
		t.Fatalf("a default not mentioned by the legacy file became %q", got)
	}
	if got := viper.GetString("cloudflare.account_id"); got != "acct-from-legacy" {
		t.Fatalf("the imported value was lost: %q", got)
	}
}
