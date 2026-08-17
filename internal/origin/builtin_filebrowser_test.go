package origin

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestBuiltinFileBrowser_SensitivePathsAreDenied verifies that .git, .ssh,
// .gnupg, and credential files are categorically denied regardless of the
// ShowHidden setting.
func TestBuiltinFileBrowser_SensitivePathsAreDenied(t *testing.T) {
	root := t.TempDir()

	// Create sensitive files/directories.
	sensitive := []string{".git", ".ssh", ".gnupg", ".env", "id_rsa", "credentials.json"}
	for _, name := range sensitive {
		if err := os.WriteFile(filepath.Join(root, name), []byte("sensitive"), 0600); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}
	// Also create a normal file to verify the browser still works.
	if err := os.WriteFile(filepath.Join(root, "public.txt"), []byte("public"), 0644); err != nil {
		t.Fatal(err)
	}

	fb, err := NewBuiltinFileBrowser(Config{
		Type:        TypeBuiltinFileBrowser,
		Path:        root,
		AllowUpload: true,
		Download:    true,
		ShowHidden:  true, // Even with ShowHidden=true, sensitive paths must be denied
	})
	if err != nil {
		t.Fatalf("NewBuiltinFileBrowser: %v", err)
	}

	addr, err := fb.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fb.Stop(context.Background())

	apiURL := addr + "/api/files?path=%2f"

	// List directory — sensitive entries must be excluded.
	req := httptest.NewRequest(http.MethodGet, apiURL, nil)
	rec := httptest.NewRecorder()
	fb.handleAPI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range sensitive {
		if strings.Contains(body, name) {
			t.Errorf("directory listing contains sensitive entry %q:\n%s", name, body)
		}
	}
	if !strings.Contains(body, "public.txt") {
		t.Errorf("directory listing missing normal file:\n%s", body)
	}

	// Attempt to download each sensitive file — must be 403.
	for _, name := range sensitive {
		req := httptest.NewRequest(http.MethodGet, addr+"/download/"+name, nil)
		rec := httptest.NewRecorder()
		fb.handleDownload(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("download %q: got status %d, want 403", name, rec.Code)
		}
	}

	// Attempt to upload a sensitive file — must be 403.
	var uploadBody bytes.Buffer
	mpw := multipart.NewWriter(&uploadBody)
	filePart, err := mpw.CreateFormFile("file", ".env")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	io.Copy(filePart, strings.NewReader("SECRET=1"))
	mpw.Close()

	req = httptest.NewRequest(http.MethodPost, addr+"/upload", &uploadBody)
	req.Header.Set("Content-Type", mpw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", fb.CSRFToken("/upload"))
	rec = httptest.NewRecorder()
	fb.requireCSRF(http.HandlerFunc(fb.handleUpload)).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("upload .env: got status %d, want 403", rec.Code)
	}
}

func TestBuiltinFileBrowser_UploadNewFile(t *testing.T) {
	root := t.TempDir()

	fb, err := NewBuiltinFileBrowser(Config{
		Type:        TypeBuiltinFileBrowser,
		Path:        root,
		AllowUpload: true,
		Download:    true,
	})
	if err != nil {
		t.Fatalf("NewBuiltinFileBrowser: %v", err)
	}

	// Start the browser to get a listening address.
	addr, err := fb.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fb.Stop(context.Background())

	// Build a multipart upload body.
	var body bytes.Buffer
	mpw := multipart.NewWriter(&body)
	filePart, err := mpw.CreateFormFile("file", "hello.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := io.Copy(filePart, strings.NewReader("hello world")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	mpw.Close()

	// Get a CSRF token for the upload path.
	token := fb.CSRFToken("/upload")

	req := httptest.NewRequest(http.MethodPost, addr+"/upload", &body)
	req.Header.Set("Content-Type", mpw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", token)

	rec := httptest.NewRecorder()
	// Use the same handler chain the server uses.
	fb.requireCSRF(http.HandlerFunc(fb.handleUpload)).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201; body=%q", rec.Code, rec.Body.String())
	}

	// Verify the file was written with exact contents.
	dest := filepath.Join(root, "hello.txt")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("file contents = %q, want %q", string(data), "hello world")
	}
}

// TestBuiltinFileBrowser_UploadHiddenFile verifies that uploading a file with
// a hidden name (e.g. .env) is rejected by default.
func TestBuiltinFileBrowser_UploadHiddenFile(t *testing.T) {
	root := t.TempDir()

	fb, err := NewBuiltinFileBrowser(Config{
		Type:        TypeBuiltinFileBrowser,
		Path:        root,
		AllowUpload: true,
	})
	if err != nil {
		t.Fatalf("NewBuiltinFileBrowser: %v", err)
	}

	addr, err := fb.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fb.Stop(context.Background())

	var body bytes.Buffer
	mpw := multipart.NewWriter(&body)
	filePart, err := mpw.CreateFormFile("file", ".env")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	io.Copy(filePart, strings.NewReader("SECRET=1"))
	mpw.Close()

	token := fb.CSRFToken("/upload")

	req := httptest.NewRequest(http.MethodPost, addr+"/upload", &body)
	req.Header.Set("Content-Type", mpw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", token)

	rec := httptest.NewRecorder()
	fb.requireCSRF(http.HandlerFunc(fb.handleUpload)).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got status %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
}

// TestBuiltinFileBrowser_StopClosesRootFd verifies that Stop closes the root
// file descriptor even when Start never succeeded (server is nil).
func TestBuiltinFileBrowser_StopClosesRootFd(t *testing.T) {
	root := t.TempDir()

	fb, err := NewBuiltinFileBrowser(Config{
		Type: TypeBuiltinFileBrowser,
		Path: root,
	})
	if err != nil {
		t.Fatalf("NewBuiltinFileBrowser: %v", err)
	}

	// Record the root FD before Stop.
	initialFd := fb.rootFd
	if initialFd < 0 {
		t.Fatalf("rootFd should be >= 0, got %d", initialFd)
	}

	// Stop without Start: server is nil, but rootFd must still be closed.
	if err := fb.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if fb.rootFd != -1 {
		t.Errorf("rootFd = %d, want -1 after Stop", fb.rootFd)
	}

	// Verify the FD is actually closed by trying to use it.
	// fstatat on a closed FD should return EBADF.
	var stat unix.Stat_t
	err = unix.Fstatat(initialFd, "", &stat, 0)
	if err == nil {
		t.Error("expected error using closed FD, got nil")
	}
}

// TestBuiltinFileBrowser_FailedStartStop verifies that a failed Start still
// allows Stop to clean up the root FD.
func TestBuiltinFileBrowser_FailedStartStop(t *testing.T) {
	root := t.TempDir()

	fb, err := NewBuiltinFileBrowser(Config{
		Type:       TypeBuiltinFileBrowser,
		Path:       root,
		ListenPort: 1, // privileged port — Start will fail to bind
	})
	if err != nil {
		t.Fatalf("NewBuiltinFileBrowser: %v", err)
	}

	// Start should fail because we can't bind to port 1.
	_, startErr := fb.Start(context.Background())
	if startErr == nil {
		t.Log("Start unexpectedly succeeded; skipping failure path")
	}

	// Stop must still close the root FD.
	if err := fb.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if fb.rootFd != -1 {
		t.Errorf("rootFd = %d, want -1 after Stop", fb.rootFd)
	}
}
