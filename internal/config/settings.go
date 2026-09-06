package config

import (
	"errors"
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
func LoadOperationalSettings() (OperationalSettings, error) {
	launchMode, err := readSettingString(KeyLaunchMode, "auto")
	if err != nil {
		return OperationalSettings{}, err
	}
	launchMode = strings.ToLower(strings.TrimSpace(launchMode))
	if launchMode != "manual" && launchMode != "auto" {
		return OperationalSettings{}, fmt.Errorf("invalid launch mode %q", launchMode)
	}

	autoStart, err := readSettingBool(KeyDefaultAutoStart, true)
	if err != nil {
		return OperationalSettings{}, err
	}
	disconnect, err := readSettingString(KeyDefaultOnDisconnect, "keep_alive")
	if err != nil {
		return OperationalSettings{}, err
	}
	disconnect = strings.ToLower(strings.TrimSpace(disconnect))
	if disconnect != "keep_alive" && disconnect != "close" {
		return OperationalSettings{}, fmt.Errorf("invalid disconnect policy %q", disconnect)
	}
	return OperationalSettings{
		LaunchMode:          launchMode,
		DefaultAutoStart:    autoStart,
		DefaultOnDisconnect: disconnect,
	}, nil
}

// SettingsPatch updates only the fields supplied by the caller.
type SettingsPatch struct {
	LaunchMode          *string
	DefaultAutoStart    *bool
	DefaultOnDisconnect *string
}

// UpdateOperationalSettings validates and persists a complete merged settings
// value in one write, so a partial update cannot leave the installation split
// across multiple SaveConfig calls.
func UpdateOperationalSettings(patch SettingsPatch) (OperationalSettings, error) {
	current, err := LoadOperationalSettings()
	if err != nil {
		return OperationalSettings{}, err
	}
	if patch.LaunchMode != nil {
		current.LaunchMode = *patch.LaunchMode
	}
	if patch.DefaultAutoStart != nil {
		current.DefaultAutoStart = *patch.DefaultAutoStart
	}
	if patch.DefaultOnDisconnect != nil {
		current.DefaultOnDisconnect = *patch.DefaultOnDisconnect
	}
	if err := SaveOperationalSettings(current); err != nil {
		return OperationalSettings{}, err
	}
	return current, nil
}

// SaveOperationalSettings persists all operational settings atomically from
// the caller's perspective. A failed disk write restores the previous in
// memory values before returning the error.
func SaveOperationalSettings(settings OperationalSettings) error {
	previous, err := LoadOperationalSettings()
	if err != nil {
		return err
	}
	if err := validateOperationalSettings(settings); err != nil {
		return err
	}
	applyOperationalSettings(settings)
	if err := SaveConfig(); err != nil {
		applyOperationalSettings(previous)
		return err
	}
	return nil
}

func validateOperationalSettings(settings OperationalSettings) error {
	mode := strings.ToLower(strings.TrimSpace(settings.LaunchMode))
	if mode != "manual" && mode != "auto" {
		return fmt.Errorf("launch mode must be \"manual\" or \"auto\", got %q", settings.LaunchMode)
	}
	policy := strings.ToLower(strings.TrimSpace(settings.DefaultOnDisconnect))
	if policy != "keep_alive" && policy != "close" {
		return fmt.Errorf("disconnect policy must be \"keep_alive\" or \"close\", got %q", settings.DefaultOnDisconnect)
	}
	return nil
}

func applyOperationalSettings(settings OperationalSettings) {
	viper.Set(KeyLaunchMode, strings.ToLower(strings.TrimSpace(settings.LaunchMode)))
	viper.Set(KeyDefaultAutoStart, settings.DefaultAutoStart)
	viper.Set(KeyDefaultOnDisconnect, strings.ToLower(strings.TrimSpace(settings.DefaultOnDisconnect)))
}

func readSettingString(key, fallback string) (string, error) {
	if !viper.IsSet(key) {
		return fallback, nil
	}
	value := viper.Get(key)
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("setting %s must be a string, got %T", key, value)
	}
	if strings.TrimSpace(s) == "" {
		return "", errors.New("setting " + key + " cannot be empty")
	}
	return s, nil
}

func readSettingBool(key string, fallback bool) (bool, error) {
	if !viper.IsSet(key) {
		return fallback, nil
	}
	value := viper.Get(key)
	switch typed := value.(type) {
	case bool:
		return typed, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("setting %s must be a boolean, got %T", key, value)
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
	current, err := LoadOperationalSettings()
	if err != nil {
		return err
	}
	current.LaunchMode = normalized
	return SaveOperationalSettings(current)
}

// SaveDefaultAutoStart persists whether a new connection is created armed.
func SaveDefaultAutoStart(enabled bool) error {
	current, err := LoadOperationalSettings()
	if err != nil {
		return err
	}
	current.DefaultAutoStart = enabled
	return SaveOperationalSettings(current)
}

// SaveDefaultOnDisconnect persists what a new connection does when the client
// that created it goes away.
func SaveDefaultOnDisconnect(policy string) error {
	normalized := strings.ToLower(strings.TrimSpace(policy))
	if normalized != "keep_alive" && normalized != "close" {
		return fmt.Errorf("disconnect policy must be \"keep_alive\" or \"close\", got %q", policy)
	}
	current, err := LoadOperationalSettings()
	if err != nil {
		return err
	}
	current.DefaultOnDisconnect = normalized
	return SaveOperationalSettings(current)
}
