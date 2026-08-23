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

// TestBuiltinFileBrowser_CSRFFromForm verifies that ordinary HTML form
// submissions using a hidden _csrf field (without X-CSRF-Token header)
// succeed — the actual rendered upload/delete forms rely on this.
func TestBuiltinFileBrowser_CSRFFromForm(t *testing.T) {
	root := t.TempDir()

	fb, err := NewBuiltinFileBrowser(Config{
		Type:        TypeBuiltinFileBrowser,
		Path:        root,
		AllowUpload: true,
		AllowDelete: true,
		Download:    true,
	})
	if err != nil {
		t.Fatalf("NewBuiltinFileBrowser: %v", err)
	}

	addr, err := fb.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fb.Stop(context.Background())

	// Get CSRF tokens for the upload and delete paths.
	uploadToken := fb.CSRFToken("/upload")
	deleteToken := fb.CSRFToken("/delete")

	// Helper: wrap handler the same way Start() does — body limit THEN csrf.
	uploadRoute := func(w http.ResponseWriter, r *http.Request) {
		const maxUploadSize = 64 << 20
		const multipartOverhead = 2 << 10
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+multipartOverhead)
		fb.requireCSRF(fb.handleUpload).ServeHTTP(w, r)
	}
	deleteRoute := func(w http.ResponseWriter, r *http.Request) {
		const maxDeleteBody = 64 << 10
		r.Body = http.MaxBytesReader(w, r.Body, maxDeleteBody)
		fb.requireCSRF(fb.handleDelete).ServeHTTP(w, r)
	}

	// --- UPLOAD via ordinary form submission (no X-CSRF-Token header) --- {
	var uploadBody bytes.Buffer
	mpw := multipart.NewWriter(&uploadBody)
	// Include _csrf as a form field (like the HTML form does).
	if err := mpw.WriteField("_csrf", uploadToken); err != nil {
		t.Fatalf("WriteField _csrf: %v", err)
	}
	filePart, err := mpw.CreateFormFile("file", "form-upload.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := io.Copy(filePart, strings.NewReader("form upload contents")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	mpw.Close()

	req := httptest.NewRequest(http.MethodPost, addr+"/upload", &uploadBody)
	req.Header.Set("Content-Type", mpw.FormDataContentType())
	// Deliberately NO X-CSRF-Token header — ordinary forms can't set it.

	rec := httptest.NewRecorder()
	uploadRoute(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: got status %d, want 201; body=%q", rec.Code, rec.Body.String())
	}

	// Verify the file was written.
	dest := filepath.Join(root, "form-upload.txt")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "form upload contents" {
		t.Errorf("file contents = %q, want %q", string(data), "form upload contents")
	}
	// }

	// --- DELETE via ordinary form submission (no X-CSRF-Token header) --- {
	var deleteBody bytes.Buffer
	dpw := multipart.NewWriter(&deleteBody)
	if err := dpw.WriteField("_csrf", deleteToken); err != nil {
		t.Fatalf("WriteField _csrf: %v", err)
	}
	if err := dpw.WriteField("path", "form-upload.txt"); err != nil {
		t.Fatalf("WriteField path: %v", err)
	}
	dpw.Close()

	req = httptest.NewRequest(http.MethodPost, addr+"/delete", &deleteBody)
	req.Header.Set("Content-Type", dpw.FormDataContentType())
	// Deliberately NO X-CSRF-Token header.

	rec = httptest.NewRecorder()
	deleteRoute(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: got status %d, want 303; body=%q", rec.Code, rec.Body.String())
	}

	// Verify the file was removed.
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("file still exists after delete: %v", err)
	}
	// }
}

// TestBuiltinFileBrowser_CSRFInvalidFormToken verifies that an invalid
// _csrf form value is rejected.
func TestBuiltinFileBrowser_CSRFInvalidFormToken(t *testing.T) {
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

	var uploadBody bytes.Buffer
	mpw := multipart.NewWriter(&uploadBody)
	if err := mpw.WriteField("_csrf", "invalid-token-value"); err != nil {
		t.Fatalf("WriteField _csrf: %v", err)
	}
	filePart, err := mpw.CreateFormFile("file", "should-not-exist.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	io.Copy(filePart, strings.NewReader("should not be written"))
	mpw.Close()

	req := httptest.NewRequest(http.MethodPost, addr+"/upload", &uploadBody)
	req.Header.Set("Content-Type", mpw.FormDataContentType())

	rec := httptest.NewRecorder()
	// Body limit MUST be applied BEFORE csrf check (same as Start() wiring).
	req.Body = io.NopCloser(io.LimitReader(req.Body, 64<<20+2<<10))
	fb.requireCSRF(fb.handleUpload).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("upload with bad _csrf: got status %d, want 403; body=%q", rec.Code, rec.Body.String())
	}

	// Verify no file was written.
	if _, err := os.Stat(filepath.Join(root, "should-not-exist.txt")); !os.IsNotExist(err) {
		t.Errorf("file was written despite invalid CSRF token")
	}
}
