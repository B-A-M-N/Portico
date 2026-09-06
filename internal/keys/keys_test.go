package keys

import "testing"

func TestInstallationKeyFilenameAuthority(t *testing.T) {
	tests := map[string]bool{
		"portico-key.bin":          true,
		"portico-key-v2.bin":       true,
		"portico-key-v999.bin":     true,
		"portico-foo.bin":          false,
		"portico-key-v.bin":        false,
		"portico-key-v0.bin":       false,
		"portico-key-v1.bin":       false,
		"portico-key-v2.txt":       false,
		"portico-key-v2.bin.bak":   false,
		"portico-key-v2.bin.extra": false,
	}
	for name, want := range tests {
		if got := IsInstallationKeyFile(name); got != want {
			t.Errorf("IsInstallationKeyFile(%q) = %t, want %t", name, got, want)
		}
	}
}
