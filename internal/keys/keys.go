// Package keys owns the durable installation-key filename contract.
package keys

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

const legacyFilename = "portico-key.bin"

// IsInstallationKeyFile reports whether name belongs to Portico's keyring.
// Unrelated .bin files must not be counted as installation keys by health or
// recovery tooling.
func IsInstallationKeyFile(name string) bool {
	if name == legacyFilename {
		return true
	}
	_, ok := VersionedKeyFileVersion(name)
	return ok
}

// InstallationKeyPath returns the durable filename for a key version.
func InstallationKeyPath(dataDir string, version int) string {
	if version <= 1 {
		return filepath.Join(dataDir, legacyFilename)
	}
	return filepath.Join(dataDir, fmt.Sprintf("portico-key-v%d.bin", version))
}

// VersionedKeyFileVersion parses a rotated key filename. Version one keeps
// the legacy unversioned filename and is therefore not accepted here.
func VersionedKeyFileVersion(name string) (int, bool) {
	const prefix = "portico-key-v"
	const suffix = ".bin"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	version, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix))
	return version, err == nil && version > 1
}
