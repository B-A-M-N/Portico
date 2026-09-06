package store

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func randomBytes(b []byte) error {
	_, err := rand.Read(b)
	return err
}

// Audit P1-12 acceptance: credential-health detection across wrong key,
// corrupt ciphertext, healthy post-rotation state, and fresh install.

func TestCredentialHealthFreshInstallHasNoRecords(t *testing.T) {
	s := newTestStore(t)
	report, err := s.CheckCredentialHealth(context.Background())
	if err != nil {
		t.Fatalf("CheckCredentialHealth: %v", err)
	}
	if report.Total != 0 || report.Healthy != 0 || report.Unhealthy != 0 {
		t.Fatalf("a fresh install reported records: %#v", report)
	}
}

func TestCredentialHealthHealthyAndPostRotation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveProviderCredential(ctx, "cloudflare", "ref-a", []byte("token-a")); err != nil {
		t.Fatalf("SaveProviderCredential: %v", err)
	}

	if _, err := s.RotateSecretKey(ctx); err != nil {
		t.Fatalf("RotateSecretKey: %v", err)
	}
	if err := s.SaveProviderCredential(ctx, "gateway", "gateway/conn-1", []byte("gw-token")); err != nil {
		t.Fatalf("SaveProviderCredential (post-rotation): %v", err)
	}

	report, err := s.CheckCredentialHealth(ctx)
	if err != nil {
		t.Fatalf("CheckCredentialHealth: %v", err)
	}
	if report.Total < 2 || report.Unhealthy != 0 {
		t.Fatalf("post-rotation health = %#v; every record should decrypt", report)
	}
	for _, rec := range report.Records {
		if rec.Problem != "" {
			t.Fatalf("record %q unexpectedly unhealthy: %s", rec.Reference, rec.Problem)
		}
	}
}

// A record encrypted under a key that is then DESTROYED (simulating the
// historical key version being lost or the whole data dir being swapped)
// must be reported as unhealthy — not silently "present".
func TestCredentialHealthDetectsWrongKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := s1.SaveProviderCredential(ctx, "cloudflare", "ref-a", []byte("token-a")); err != nil {
		t.Fatalf("SaveProviderCredential: %v", err)
	}
	s1.Close()

	// Reopen with a FRESH secret store over the same database: a new
	// installation key replaces the one the record was encrypted under.
	if err := overwriteKeyFiles(dir); err != nil {
		t.Fatalf("overwriteKeyFiles: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	report, err := s2.CheckCredentialHealth(ctx)
	if err != nil {
		t.Fatalf("CheckCredentialHealth: %v", err)
	}
	if report.Unhealthy == 0 {
		t.Fatalf("a record under a destroyed key was reported healthy: %#v", report)
	}
	found := false
	for _, rec := range report.Records {
		if rec.Reference == "ref-a" && rec.Problem != "" {
			found = true
			if !strings.Contains(rec.Problem, "key") && !strings.Contains(rec.Problem, "corrupt") &&
				!strings.Contains(rec.Problem, "decryption") {
				t.Fatalf("problem classification unclear: %q", rec.Problem)
			}
		}
	}
	if !found {
		t.Fatal("the affected record was not identified")
	}
}

func TestCredentialHealthDetectsCorruptCiphertext(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveProviderCredential(ctx, "cloudflare", "ref-corrupt", []byte("token")); err != nil {
		t.Fatalf("SaveProviderCredential: %v", err)
	}
	// Corrupt the stored blob directly (still read-only with respect to the
	// API under test — this is fixture setup).
	s.mu.Lock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE provider_credentials SET secret_encrypted = ? WHERE credential_ref = ?`,
		[]byte("{not valid json}"), "ref-corrupt")
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}

	report, err := s.CheckCredentialHealth(ctx)
	if err != nil {
		t.Fatalf("CheckCredentialHealth: %v", err)
	}
	found := false
	for _, rec := range report.Records {
		if rec.Reference == "ref-corrupt" {
			found = true
			if rec.Problem == "" {
				t.Fatal("corrupt ciphertext was not detected")
			}
			if strings.Contains(rec.Problem, "token") {
				t.Fatalf("problem text leaked secret material? %q", rec.Problem)
			}
		}
	}
	if !found {
		t.Fatal("the corrupt record was not enumerated")
	}
}

// overwriteKeyFiles replaces the installation key files in dir with a brand
// new random key, simulating a lost/replaced key without deleting them.
func overwriteKeyFiles(dir string) error {
	fresh, err := NewSecretStore(filepath.Join(dir, "fresh-keys"))
	if err != nil {
		return err
	}
	defer fresh.Destroy()
	entries, err := listKeyFiles(dir)
	if err != nil {
		return err
	}
	newKey := make([]byte, 32)
	if err := randomBytes(newKey); err != nil {
		return err
	}
	for _, name := range entries {
		// Direct write (not writeKeyFile, which is NOREPLACE by design):
		// the fixture simulates a replaced key file on disk.
		if err := os.WriteFile(filepath.Join(dir, name), newKey, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func listKeyFiles(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "portico-key*.bin"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}
	return names, nil
}
