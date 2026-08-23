package origin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestBuiltinStatic_SensitivePaths verifies that the sensitive-file policy
// (e.g. id_rsa, credentials.json) is enforced in non-SPA static mode — not
// just hidden files. P0 #3.
func TestBuiltinStatic_SensitivePaths(t *testing.T) {
	root := t.TempDir()

	// Create sensitive files that should be denied even in non-SPA mode.
	sensitive := []string{"id_rsa", "credentials.json", "token"}
	for _, name := range sensitive {
		if err := os.WriteFile(filepath.Join(root, name), []byte("sensitive"), 0600); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}
	// And a safe file.
	if err := os.WriteFile(filepath.Join(root, "public.txt"), []byte("public"), 0644); err != nil {
		t.Fatalf("creating public.txt: %v", err)
	}

	bs, err := NewBuiltinStatic(Config{
		Type: TypeBuiltinStatic,
		Path: root,
	})
	if err != nil {
		t.Fatalf("NewBuiltinStatic: %v", err)
	}

	addr, err := bs.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer bs.Stop(context.Background())

	// Sensitive files must be 403 even in non-SPA mode.
	for _, name := range sensitive {
		req := httptest.NewRequest(http.MethodGet, addr+"/"+name, nil)
		rec := httptest.NewRecorder()
		bs.server.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("non-SPA GET %q: got status %d, want 403", name, rec.Code)
		}
	}

	// Safe file must be served.
	req := httptest.NewRequest(http.MethodGet, addr+"/public.txt", nil)
	rec := httptest.NewRecorder()
	bs.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("non-SPA GET public.txt: got status %d, want 200", rec.Code)
	}
}
