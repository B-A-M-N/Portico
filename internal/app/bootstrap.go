package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// Bootstrap handles supervisor startup: locking, re-exec, and XDG path resolution.
type Bootstrap struct {
	Paths Paths
}

// Paths holds all filesystem paths used by Portico.
type Paths struct {
	SocketPath   string
	DatabasePath string
	ConfigPath   string
	LogDir       string
	ConnectorDir string
}

// ConnectorLogDir returns the connector logs subdirectory.
func (p Paths) ConnectorLogDir() string {
	return p.ConnectorDir
}

// DefaultPaths returns XDG-compliant default paths.
func DefaultPaths() Paths {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = filepath.Join("/tmp", fmt.Sprintf("portico-%d", os.Getuid()))
	}

	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(os.Getenv("HOME"), ".local", "share")
	}

	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(os.Getenv("HOME"), ".config")
	}

	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}

	return Paths{
		SocketPath:   filepath.Join(runtimeDir, "portico", "portico.sock"),
		DatabasePath: filepath.Join(dataHome, "portico", "portico.db"),
		ConfigPath:   filepath.Join(configHome, "portico", "config.toml"),
		LogDir:       filepath.Join(stateHome, "portico", "logs"),
		ConnectorDir: filepath.Join(stateHome, "portico", "connectors"),
	}
}

// Executable returns the path to the current executable for re-exec.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	return exe, nil
}
