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
)

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
			// One-way compatibility read for an existing Flare installation.
			// New writes always use the XDG Portico path and TOML.
			legacy := filepath.Join(legacyDir(), "config.yaml")
			if _, statErr := os.Stat(legacy); statErr == nil {
				viper.SetConfigFile(legacy)
				if legacyErr := viper.ReadInConfig(); legacyErr != nil {
					return fmt.Errorf("reading legacy config: %w", legacyErr)
				}
			}
			return nil
		}
		return fmt.Errorf("reading config: %w", err)
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
		return fmt.Errorf("cloudflare.account_id is not set (run 'portico legacy init' or configure Portico Cloudflare setup)")
	}
	if viper.GetString(KeyZoneID) == "" {
		return fmt.Errorf("cloudflare.zone_id is not set (run 'portico legacy init' or configure Portico Cloudflare setup)")
	}
	if viper.GetString(KeyDomain) == "" {
		return fmt.Errorf("cloudflare.domain is not set (run 'portico legacy init' or configure Portico Cloudflare setup)")
	}
	if APIToken() == "" {
		return fmt.Errorf("no API token found (run 'portico legacy auth login' or set CLOUDFLARE_API_TOKEN)")
	}
	return nil
}
