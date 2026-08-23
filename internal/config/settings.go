package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// Operational settings.
//
// These are the choices that outlive a session and belong to the installation
// rather than to any one connection. Only the supervisor calls into here: the
// TUI reaches them over IPC, because the supervisor owns durable state.
//
// Launch mode used to be runtime-only, so the interface had to tell the user
// their explicit choice would last "until the supervisor restarts". A setting
// that does not survive a restart is not a setting.

// OperationalSettings is the persisted operational configuration.
type OperationalSettings struct {
	LaunchMode          string
	DefaultAutoStart    bool
	DefaultOnDisconnect string
}

// LoadOperationalSettings reads the stored settings, falling back to the
// defaults for anything absent.
func LoadOperationalSettings() OperationalSettings {
	return OperationalSettings{
		LaunchMode:          normalizeLaunchMode(viper.GetString(KeyLaunchMode)),
		DefaultAutoStart:    viper.GetBool(KeyDefaultAutoStart),
		DefaultOnDisconnect: normalizeOnDisconnect(viper.GetString(KeyDefaultOnDisconnect)),
	}
}

// SaveLaunchMode persists the startup gate.
//
// The value is validated rather than coerced: a typo that silently armed every
// connection is the opposite of what this gate is for.
func SaveLaunchMode(mode string) error {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	if normalized != "manual" && normalized != "auto" {
		return fmt.Errorf("launch mode must be \"manual\" or \"auto\", got %q", mode)
	}
	viper.Set(KeyLaunchMode, normalized)
	return SaveConfig()
}

// SaveDefaultAutoStart persists whether a new connection is created armed.
func SaveDefaultAutoStart(enabled bool) error {
	viper.Set(KeyDefaultAutoStart, enabled)
	return SaveConfig()
}

// SaveDefaultOnDisconnect persists what a new connection does when the client
// that created it goes away.
func SaveDefaultOnDisconnect(policy string) error {
	normalized := strings.ToLower(strings.TrimSpace(policy))
	if normalized != "keep_alive" && normalized != "close" {
		return fmt.Errorf("disconnect policy must be \"keep_alive\" or \"close\", got %q", policy)
	}
	viper.Set(KeyDefaultOnDisconnect, normalized)
	return SaveConfig()
}

// normalizeLaunchMode maps anything unrecognised to auto, which is the
// behaviour an installation had before the setting existed.
func normalizeLaunchMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "manual") {
		return "manual"
	}
	return "auto"
}

// normalizeOnDisconnect maps anything unrecognised to keep_alive, which is what
// the wizard hardcoded before the setting existed.
func normalizeOnDisconnect(policy string) string {
	if strings.EqualFold(strings.TrimSpace(policy), "close") {
		return "close"
	}
	return "keep_alive"
}
