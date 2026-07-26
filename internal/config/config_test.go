package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialRoundTripIsEncrypted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const token = "cf-secret-token-that-must-not-be-plaintext"

	if err := SaveCredential(token); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	path, err := CredentialPath()
	if err != nil {
		t.Fatalf("CredentialPath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read encrypted credential: %v", err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("credential file contains the plaintext token")
	}
	got, err := LoadCredential()
	if err != nil {
		t.Fatalf("LoadCredential: %v", err)
	}
	if got != token {
		t.Fatalf("loaded token = %q, want %q", got, token)
	}
}

func TestLoadCredentialMigratesAndRemovesLegacyPlaintext(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(home, "portico-config")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	legacyPath := filepath.Join(home, ".config", "flare-cli", "credentials")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0700); err != nil {
		t.Fatalf("create legacy directory: %v", err)
	}
	const token = "legacy-plaintext-token"
	if err := os.WriteFile(legacyPath, []byte(token+"\n"), 0600); err != nil {
		t.Fatalf("write legacy credential: %v", err)
	}

	got, err := LoadCredential()
	if err != nil {
		t.Fatalf("LoadCredential: %v", err)
	}
	if got != token {
		t.Fatalf("migrated token = %q, want %q", got, token)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy plaintext remains or could not be checked: %v", err)
	}
	credentialPath, err := CredentialPath()
	if err != nil {
		t.Fatalf("CredentialPath: %v", err)
	}
	ciphertext, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatalf("read migrated credential: %v", err)
	}
	if strings.Contains(string(ciphertext), token) {
		t.Fatal("migrated credential contains plaintext token")
	}
}
