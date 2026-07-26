package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewSecretStoreDoesNotReplaceUnreadableKey(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSecretStore(dir)
	if err != nil {
		t.Fatalf("create secret store: %v", err)
	}
	if store == nil {
		t.Fatal("nil secret store")
	}
	path := filepath.Join(dir, keyFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatalf("chmod key: %v", err)
	}
	if _, err := NewSecretStore(dir); err == nil {
		t.Fatal("expected insecure key permissions to fail")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key after failed load: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("failed key load replaced the existing installation key")
	}
}

func TestNewSecretStoreRejectsSymlinkedKey(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, make([]byte, 32), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, keyFileName)); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := NewSecretStore(dir); err == nil {
		t.Fatal("expected symlinked installation key to be rejected")
	}
	if info, err := os.Lstat(filepath.Join(dir, keyFileName)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("key path should remain an untouched symlink, info=%v err=%v", info, err)
	}
}

func TestNewSecretStoreRejectsCorruptKeyWithoutReplacingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, keyFileName)
	corrupt := []byte("not-a-32-byte-installation-key")
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatalf("write corrupt key: %v", err)
	}
	if _, err := NewSecretStore(dir); err == nil {
		t.Fatal("expected corrupt installation key to fail")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corrupt key: %v", err)
	}
	if string(after) != string(corrupt) {
		t.Fatal("corrupt installation key was replaced")
	}
}

func TestSecretStoreRotationKeepsOldBlobsReadableUntilRetired(t *testing.T) {
	dir := t.TempDir()
	secrets, err := NewSecretStore(dir)
	if err != nil {
		t.Fatalf("NewSecretStore: %v", err)
	}
	defer secrets.Destroy()
	before, err := secrets.Encrypt([]byte("before-rotation"), "test-context")
	if err != nil {
		t.Fatalf("encrypt before rotation: %v", err)
	}
	if version, err := secrets.Rotate(); err != nil || version != 2 {
		t.Fatalf("Rotate = version %d, err %v; want 2, nil", version, err)
	}
	after, err := secrets.Encrypt([]byte("after-rotation"), "test-context")
	if err != nil {
		t.Fatalf("encrypt after rotation: %v", err)
	}
	for name, blob := range map[string][]byte{"before": before, "after": after} {
		plain, err := secrets.Decrypt(blob, "test-context")
		if err != nil {
			t.Fatalf("decrypt %s blob: %v", name, err)
		}
		if string(plain) != name+"-rotation" {
			t.Fatalf("%s plaintext = %q", name, plain)
		}
	}

	// A fresh process must recover both retained key versions.
	reloaded, err := NewSecretStore(dir)
	if err != nil {
		t.Fatalf("reload keyring: %v", err)
	}
	plain, err := reloaded.Decrypt(before, "test-context")
	if err != nil || string(plain) != "before-rotation" {
		t.Fatalf("reloaded old blob = %q, %v", plain, err)
	}
	if err := reloaded.RetireVersionsBefore(2); err != nil {
		t.Fatalf("retire old key: %v", err)
	}
	reloaded.Destroy()

	postRetire, err := NewSecretStore(dir)
	if err != nil {
		t.Fatalf("reload retired keyring: %v", err)
	}
	defer postRetire.Destroy()
	plain, err = postRetire.Decrypt(after, "test-context")
	if err != nil || string(plain) != "after-rotation" {
		t.Fatalf("post-retirement current blob = %q, %v", plain, err)
	}
	if _, err := postRetire.Decrypt(before, "test-context"); err == nil {
		t.Fatal("retired key unexpectedly decrypted an old blob")
	}
}
