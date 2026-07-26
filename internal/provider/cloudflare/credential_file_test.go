package cloudflare

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCredentialFilesAreScopedAndCleaned(t *testing.T) {
	dir := t.TempDir()
	path, cleanup, err := createCredentialFile(dir, []byte("token"))
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credential mode = %o, want 0600", info.Mode().Perm())
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credential file remains after cleanup: %v", err)
	}
}

func TestSweepStaleCredentialFilesLeavesActiveAndUnknownFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	stale := filepath.Join(dir, "cred-0123456789abcdef0123456789abcdef.tmp")
	active := filepath.Join(dir, "cred-fedcba9876543210fedcba9876543210.tmp")
	unknown := filepath.Join(dir, "notes.txt")
	for _, path := range []string{stale, active, unknown} {
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	if err := os.Chtimes(stale, now.Add(-staleCredentialAge-time.Second), now.Add(-staleCredentialAge-time.Second)); err != nil {
		t.Fatalf("age stale credential: %v", err)
	}
	if err := sweepStaleCredentialFiles(dir, now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale known credential remains: %v", err)
	}
	for _, path := range []string{active, unknown} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("file %q should remain: %v", path, err)
		}
	}
}
