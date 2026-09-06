//go:build linux

package origin

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func postUpload(t *testing.T, fb *BuiltinFileBrowser, dir, filename, contents string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mpw := multipart.NewWriter(&body)
	if dir != "" {
		if err := mpw.WriteField("path", dir); err != nil {
			t.Fatal(err)
		}
	}
	part, err := mpw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, contents); err != nil {
		t.Fatal(err)
	}
	if err := mpw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", mpw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", fb.CSRFToken("/upload"))
	rec := httptest.NewRecorder()
	fb.requireCSRF(http.HandlerFunc(fb.handleUpload)).ServeHTTP(rec, req)
	return rec
}

func TestFileBrowserOperationsRejectIntermediateSymlinkDirectories(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "incoming")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	fb, err := NewBuiltinFileBrowser(Config{
		Type: TypeBuiltinFileBrowser, Path: root,
		AllowUpload: true, AllowDelete: true, Download: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fb.Stop(context.Background()) })

	listReq := httptest.NewRequest(http.MethodGet, "/api/files?path="+url.QueryEscape("/incoming"), nil)
	listRec := httptest.NewRecorder()
	fb.handleAPI(listRec, listReq)
	if listRec.Code == http.StatusOK || strings.Contains(listRec.Body.String(), "must survive") {
		t.Fatalf("intermediate symlink was listed: status=%d body=%q", listRec.Code, listRec.Body.String())
	}

	if rec := getDownload(t, fb, "incoming/secret.txt"); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "must survive") {
		t.Fatalf("intermediate symlink was downloaded: status=%d body=%q", rec.Code, rec.Body.String())
	}

	if rec := postUpload(t, fb, "incoming", "created.txt", "should stay inside"); rec.Code == http.StatusCreated {
		t.Fatal("upload through an intermediate symlink succeeded")
	}
	if _, err := os.Lstat(filepath.Join(outside, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("upload through symlink created an outside file: %v", err)
	}

	if rec := postDelete(t, fb, "incoming/secret.txt"); rec.Code == http.StatusSeeOther {
		t.Fatal("delete through an intermediate symlink succeeded")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("delete through symlink removed outside file: %v", err)
	}
}

func TestFileBrowserRootDescriptorSurvivesConfiguredPathReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pinned.txt"), []byte("pinned"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "pinned.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}

	fb, err := NewBuiltinFileBrowser(Config{
		Type: TypeBuiltinFileBrowser, Path: root, Download: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fb.Stop(context.Background()) })

	moved := filepath.Join(parent, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	rec := getDownload(t, fb, "pinned.txt")
	if rec.Code != http.StatusOK || rec.Body.String() != "pinned" {
		t.Fatalf("pinned root was not used after replacement: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestFileBrowserUploadDoesNotReplaceDestinationSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "destination.txt")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	fb, err := NewBuiltinFileBrowser(Config{
		Type: TypeBuiltinFileBrowser, Path: root, AllowUpload: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fb.Stop(context.Background()) })

	if rec := postUpload(t, fb, "", "destination.txt", "replacement"); rec.Code == http.StatusCreated {
		t.Fatal("upload replaced a destination symlink")
	}
	data, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "must survive" {
		t.Fatalf("destination symlink target changed to %q", data)
	}
}
