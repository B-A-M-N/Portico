package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/B-A-M-N/portico/internal/store"
	"github.com/spf13/viper"
)

// Config key constants.
const (
	KeyAccountID           = "cloudflare.account_id"
	KeyZoneID              = "cloudflare.zone_id"
	KeyDomain              = "cloudflare.domain"
	KeyAPITokenEnv         = "cloudflare.api_token_env"
	KeyTeamDomain          = "cloudflare.team_domain"
	KeyCloudflaredBin      = "paths.cloudflared_bin"
	KeyStateDir            = "paths.state_dir"
	KeyDefaultAuth         = "defaults.auth"
	KeyDefaultSessionDur   = "defaults.session_duration"
	KeyDefaultHostTemplate = "defaults.hostname_template"
	KeyDefaultReuseTunnel  = "defaults.reuse_tunnel"
	KeyLogLevel            = "log_level"
	KeyConfigVersion       = "config_version"

	// Operational settings the supervisor owns. These are what the Settings
	// screen writes through IPC; the TUI never touches this file itself.
	//
	// KeyLaunchMode is the startup gate. It was previously runtime-only, so a
	// user's explicit choice was reported as lasting "until the supervisor
	// restarts" — which is not a setting, it is a session preference.
	KeyLaunchMode = "operations.launch_mode"
	// KeyDefaultAutoStart and KeyDefaultOnDisconnect are what a new connection
	// is created with. The wizard hardcoded true and keep_alive respectively.
	KeyDefaultAutoStart    = "operations.default_auto_start"
	KeyDefaultOnDisconnect = "operations.default_on_disconnect"

	// Ngrok config keys
	KeyNgrokAPITokenEnv = "ngrok.api_token_env"
	KeyNgrokAccountID   = "ngrok.account_id"
	KeyNgrokBin         = "paths.ngrok_bin"
)

// CurrentConfigVersion is the latest config schema version.
// Increment this when adding new required fields or changing semantics.
//
// v2 adds the operations section: launch mode, and the defaults a new
// connection is created with. Those choices existed before it — launch mode as
// runtime-only state, the connection defaults as literals in the wizard — so
// the migration writes the behaviour that was already in force rather than
// changing anything.
const CurrentConfigVersion = 2

// Init initializes viper with defaults, config file, and env bindings.
func Init() error {
	dir, err := Dir()
	if err != nil {
		return err
	}

	setDefaults(dir)

	viper.SetConfigFile(filepath.Join(dir, "config.toml"))
	viper.SetConfigType("toml")

	viper.SetEnvPrefix("PORTICO")
	viper.AutomaticEnv()

	// CLOUDFLARE_API_TOKEN is the standard provider environment variable;
	// PORTICO_CLOUDFLARE_API_TOKEN is supported for a Portico-only shell.
	_ = viper.BindEnv("cloudflare.api_token", "CLOUDFLARE_API_TOKEN", "PORTICO_CLOUDFLARE_API_TOKEN")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok || errors.Is(err, os.ErrNotExist) {
			// No Portico config yet. An older Flare installation may have one, in
			// which case it is migrated once rather than read from indefinitely.
			return migrateLegacyConfig(dir)
		}
		return fmt.Errorf("reading config: %w", err)
	}

	// Config version migration
	if err := migrateConfig(); err != nil {
		return fmt.Errorf("config migration: %w", err)
	}

	return nil
}

func setDefaults(stateDir string) {
	viper.SetDefault(KeyAPITokenEnv, "CLOUDFLARE_API_TOKEN")
	viper.SetDefault(KeyCloudflaredBin, "cloudflared")
	viper.SetDefault(KeyStateDir, stateDir)
	viper.SetDefault(KeyDefaultAuth, "otp")
	viper.SetDefault(KeyDefaultSessionDur, "30m")
	viper.SetDefault(KeyDefaultHostTemplate, "{app}-{id}.{domain}")
	viper.SetDefault(KeyDefaultReuseTunnel, true)
	viper.SetDefault(KeyLogLevel, "info")
	viper.SetDefault(KeyConfigVersion, CurrentConfigVersion)

	// Operational settings. The defaults are what Portico did before they were
	// configurable, so an existing installation behaves exactly as it did:
	// auto honours each connection's own AutoStart flag, and a new connection
	// is created armed and surviving client disconnection.
	viper.SetDefault(KeyLaunchMode, "auto")
	viper.SetDefault(KeyDefaultAutoStart, true)
	viper.SetDefault(KeyDefaultOnDisconnect, "keep_alive")

	// Ngrok defaults
	viper.SetDefault(KeyNgrokAPITokenEnv, "NGROK_AUTHTOKEN")
	viper.SetDefault(KeyNgrokBin, "ngrok")
}

// migrateConfig handles config schema migrations.
// It reads the config_version field and applies migrations to bring
// the config up to CurrentConfigVersion.
func migrateConfig() error {
	version := viper.GetInt(KeyConfigVersion)
	if version == 0 {
		// No version set - treat as version 1 (first version with explicit versioning)
		version = 1
		viper.Set(KeyConfigVersion, CurrentConfigVersion)
	}

	// Apply migrations sequentially
	for v := version; v < CurrentConfigVersion; v++ {
		if err := applyMigration(v, v+1); err != nil {
			return fmt.Errorf("migration v%d->v%d: %w", v, v+1, err)
		}
		viper.Set(KeyConfigVersion, v+1)
	}

	// Persist updated config version
	configFile := viper.ConfigFileUsed()
	if configFile != "" {
		if err := viper.WriteConfig(); err != nil {
			return fmt.Errorf("writing migrated config: %w", err)
		}
	}

	return nil
}

// applyMigration applies a single version migration.
// Add new migration cases here when incrementing CurrentConfigVersion.
func applyMigration(from, to int) error {
	switch from {
	case 1:
		return migrateV1toV2()
	}
	return nil
}

// migrateV1toV2 writes the operations section explicitly.
//
// A v1 config has no operations keys, so reading them falls through to the
// defaults — which are exactly what Portico did before they were configurable.
// Writing them makes the behaviour visible in the file rather than implicit in
// the binary, so a user can see what their installation will do and change it.
//
// It is idempotent and never overwrites a value that is already set, so running
// it twice — or on a config that has since been edited by hand — changes
// nothing.
func migrateV1toV2() error {
	if !viper.IsSet(KeyLaunchMode) {
		viper.Set(KeyLaunchMode, "auto")
	}
	if !viper.IsSet(KeyDefaultAutoStart) {
		viper.Set(KeyDefaultAutoStart, true)
	}
	if !viper.IsSet(KeyDefaultOnDisconnect) {
		viper.Set(KeyDefaultOnDisconnect, "keep_alive")
	}
	return nil
}

// CloudflaredBin returns the configured cloudflared binary path.
func CloudflaredBin() string {
	return viper.GetString(KeyCloudflaredBin)
}

// AccountID returns the configured Cloudflare account ID.
func AccountID() string { return viper.GetString(KeyAccountID) }

// ZoneID returns the configured Cloudflare zone ID.
func ZoneID() string { return viper.GetString(KeyZoneID) }

// Dir returns the config directory path, creating it if needed.
func Dir() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	dir := filepath.Join(configHome, "portico")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("creating config directory: %w", err)
	}
	return dir, nil
}

// migrateLegacyConfig imports an older Flare installation's configuration once.
//
// This used to be an open-ended compatibility read: when no Portico config existed,
// viper was pointed at the legacy config.yaml and left there. Two problems followed
// from that. The settings were re-read from the old file on every start, so the
// installation never actually moved and a user editing the file Portico documents saw
// no effect. And viper's config file pointer stayed on the legacy path, so anything
// writing config without resetting it first would have written YAML back to the old
// location — the writers here do reset it, which is the only reason that never became
// a data-loss bug.
//
// So the values are read once, written to the Portico path in Portico's format, and
// the legacy file is renamed rather than deleted. Renaming is deliberate: this is a
// user's own configuration, and a migration that turns out to have misread something
// should leave the original recoverable.
func migrateLegacyConfig(dir string) error {
	legacy := filepath.Join(legacyDir(), "config.yaml")
	if _, err := os.Stat(legacy); err != nil {
		// Nothing to migrate. A fresh installation is the common case and is not a
		// failure.
		return nil
	}

	// Read the legacy file through a separate viper so the values can be inspected
	// without leaving the global instance pointed at it.
	legacyValues := viper.New()
	legacyValues.SetConfigFile(legacy)
	legacyValues.SetConfigType("yaml")
	if err := legacyValues.ReadInConfig(); err != nil {
		return fmt.Errorf("reading the configuration from your previous installation "+
			"(%s): %w", legacy, err)
	}

	for _, key := range legacyValues.AllKeys() {
		// Defaults are already set, so only a value the legacy file actually carries
		// should override one.
		if legacyValues.IsSet(key) {
			viper.Set(key, legacyValues.Get(key))
		}
	}

	// Write it where Portico reads from, in the format Portico writes.
	target := filepath.Join(dir, "config.toml")
	viper.SetConfigFile(target)
	viper.SetConfigType("toml")
	if err := viper.WriteConfigAs(target); err != nil {
		return fmt.Errorf("saving your previous settings to %s: %w", target, err)
	}

	// Move the original aside. Leaving it in place would make the next start migrate
	// again and discard anything changed since.
	if err := os.Rename(legacy, legacy+".migrated"); err != nil {
		// The settings are already saved, so this is not worth failing the start
		// over — but it does mean the next start would migrate again, so it is worth
		// saying.
		return fmt.Errorf("your settings were imported to %s, but the old file could "+
			"not be renamed (%s): %w", target, legacy, err)
	}
	return nil
}

func legacyDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "flare-cli")
}

// SessionsDir returns the sessions directory, creating it if needed.
func SessionsDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	sessDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		return "", fmt.Errorf("creating sessions directory: %w", err)
	}
	return sessDir, nil
}

// WriteConfig writes the current viper config to the config file.
func WriteConfig() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	viper.SetConfigFile(filepath.Join(dir, "config.toml"))
	return viper.WriteConfig()
}

// SaveConfig writes viper config, creating the file if it doesn't exist.
func SaveConfig() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "config.toml")
	viper.SetConfigFile(path)
	viper.SetConfigType("toml")
	if err := viper.WriteConfigAs(path); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// SaveCloudflareSetup stores non-secret Cloudflare routing identifiers in the
// XDG config file and persists the token through the encrypted credential
// store. Callers must have obtained the token without placing it in argv.
func SaveCloudflareSetup(accountID, zoneID, token string) error {
	if accountID == "" || zoneID == "" || token == "" {
		return fmt.Errorf("cloudflare account ID, zone ID, and token are required")
	}
	if err := SaveCredential(token); err != nil {
		return err
	}
	viper.Set(KeyAccountID, accountID)
	viper.Set(KeyZoneID, zoneID)
	if err := SaveConfig(); err != nil {
		return err
	}
	return nil
}

// APIToken retrieves the Cloudflare API token from the standard environment
// variable, the Portico-scoped environment variable, or encrypted local
// credentials created by the legacy login flow.
func APIToken() string {
	// Env var takes highest precedence.
	envName := viper.GetString(KeyAPITokenEnv)
	if envName == "" {
		envName = "CLOUDFLARE_API_TOKEN"
	}
	if token := os.Getenv(envName); token != "" {
		return token
	}
	// Stored credentials are encrypted at rest. A legacy plaintext credential
	// is read only for backwards compatibility; new writes never create one.
	if token, err := LoadCredential(); err == nil && token != "" {
		return token
	}
	return ""
}

// CredentialPath returns the encrypted credential path.
func CredentialPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cloudflare-token.enc"), nil
}

const credentialContext = "portico:cloudflare-api-token:v1"

// SaveCredential encrypts an API token using the installation key before
// persisting it. The encrypted blob and key are both protected by 0600 files.
func SaveCredential(token string) error {
	path, err := CredentialPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	secrets, err := store.NewSecretStore(dir)
	if err != nil {
		return fmt.Errorf("credential secret store: %w", err)
	}
	defer secrets.Destroy()
	blob, err := secrets.Encrypt([]byte(token), credentialContext)
	if err != nil {
		return fmt.Errorf("encrypt credential: %w", err)
	}
	return os.WriteFile(path, blob, 0600)
}

// LoadCredential reads the encrypted Portico credential. If an old Flare
// plaintext credential exists, it is migrated once into the encrypted store
// and removed; production never continues to use the plaintext file as a
// standing fallback.
func LoadCredential() (string, error) {
	path, err := CredentialPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		secrets, secretErr := store.NewSecretStore(filepath.Dir(path))
		if secretErr != nil {
			return "", fmt.Errorf("credential secret store: %w", secretErr)
		}
		defer secrets.Destroy()
		plain, decryptErr := secrets.Decrypt(data, credentialContext)
		if decryptErr != nil {
			return "", fmt.Errorf("decrypt credential: %w", decryptErr)
		}
		return string(plain), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}

	legacyPath := filepath.Join(legacyDir(), "credentials")
	legacy, legacyErr := os.ReadFile(legacyPath)
	if legacyErr != nil {
		return "", legacyErr
	}
	token := strings.TrimSpace(string(legacy))
	if token == "" {
		return "", fmt.Errorf("legacy credential is empty")
	}
	if err := SaveCredential(token); err != nil {
		return "", fmt.Errorf("migrate legacy credential: %w", err)
	}
	if err := os.Remove(legacyPath); err != nil {
		return "", fmt.Errorf("remove migrated legacy credential: %w", err)
	}
	return token, nil
}

// DeleteCredential removes the credentials file.
func DeleteCredential() error {
	path, err := CredentialPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Validate checks that required config values are present.
func Validate() error {
	if viper.GetString(KeyAccountID) == "" {
		return fmt.Errorf("cloudflare.account_id is not set (run 'portico provider login cloudflare' or set CLOUDFLARE_ACCOUNT_ID)")
	}
	if viper.GetString(KeyZoneID) == "" {
		return fmt.Errorf("cloudflare.zone_id is not set (run 'portico provider login cloudflare' or set CLOUDFLARE_ZONE_ID)")
	}
	if viper.GetString(KeyDomain) == "" {
		return fmt.Errorf("cloudflare.domain is not set (run 'portico provider login cloudflare' or set CLOUDFLARE_DOMAIN)")
	}
	if APIToken() == "" {
		return fmt.Errorf("no API token found (run 'portico provider login cloudflare' or set CLOUDFLARE_API_TOKEN)")
	}
	return nil
}

// NgrokAPIToken retrieves the Ngrok API token from environment or credentials.
func NgrokAPIToken() string {
	envName := viper.GetString(KeyNgrokAPITokenEnv)
	if envName == "" {
		envName = "NGROK_AUTHTOKEN"
	}
	if token := os.Getenv(envName); token != "" {
		return token
	}
	// Could add credential loading here similar to Cloudflare
	return ""
}

// NgrokBin returns the configured ngrok binary path.
func NgrokBin() string {
	bin := viper.GetString(KeyNgrokBin)
	if bin == "" {
		return "ngrok"
	}
	return bin
}

// NgrokAccountID returns the configured Ngrok account ID.
func NgrokAccountID() string {
	return viper.GetString(KeyNgrokAccountID)
}
