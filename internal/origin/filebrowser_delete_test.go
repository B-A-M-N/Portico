//go:build linux

package origin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deleting a file the served directory does not contain.
//
// The file browser validated a pathname and then acted on that pathname. Between the
// two, the path can be replaced — classically by a symlink pointing outside the
// served directory — so the check and the operation apply to different files. The
// rootedDir primitives exist to close that window by working relative to a pinned
// directory descriptor and refusing to follow symlinks, and nothing used them: the
// browser constructed one at startup and deleted by pathname anyway.
//
// A comment describing the risk does not fix it, and the code carried one.

// browserFixture serves a directory with a file in it, and a secret outside it.
func browserFixture(t *testing.T) (*BuiltinFileBrowser, string, string) {
	t.Helper()
	root := t.TempDir()
	outside := t.TempDir()

	served := filepath.Join(root, "served.txt")
	if err := os.WriteFile(served, []byte("served"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}

	fb, err := NewBuiltinFileBrowser(Config{
		Type: TypeBuiltinFileBrowser, Path: root, AllowDelete: true,
	})
	if err != nil {
		t.Fatalf("NewBuiltinFileBrowser: %v", err)
	}
	t.Cleanup(func() { _ = fb.Stop(context.Background()) })
	return fb, root, secret
}

// postDelete drives the delete handler the way the browser's own form does.
func postDelete(t *testing.T, fb *BuiltinFileBrowser, path string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("path", path)
	// The token is bound to the path, as the browser's own form binds it.
	form.Set("csrf", fb.CSRFToken(path))

	req := httptest.NewRequest(http.MethodPost, "/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	fb.handleDelete(rec, req)
	return rec
}

// TestDeletingThroughASymlinkDoesNotReachOutside pins the property the fd-relative
// primitives exist for.
//
// A symlink inside the served directory pointing at a file outside it must not give
// the delete handler a way to remove that file. Unlinkat with the root descriptor
// removes the link itself, never its target.
func TestDeletingThroughASymlinkDoesNotReachOutside(t *testing.T) {
	fb, root, secret := browserFixture(t)

	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	postDelete(t, fb, "escape.txt")

	// Whatever the handler decided, the file outside the served directory survives.
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("a file outside the served directory was removed through a symlink: %v", err)
	}
	// And if the link was removed, it was the link and not the target.
	if _, err := os.Lstat(link); err == nil {
		return // the link is still there, which is also acceptable
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatal("the symlink's target was removed rather than the symlink")
	}
}

// TestDeletingATraversalPathIsRefused pins the simpler escape.
func TestDeletingATraversalPathIsRefused(t *testing.T) {
	fb, _, secret := browserFixture(t)

	for _, attempt := range []string{
		"../secret.txt",
		"../../secret.txt",
		"served/../../secret.txt",
		secret, // an absolute path outside the root
	} {
		rec := postDelete(t, fb, attempt)
		if rec.Code == http.StatusSeeOther {
			t.Errorf("the traversal %q was accepted", attempt)
		}
		if _, err := os.Stat(secret); err != nil {
			t.Fatalf("the traversal %q removed a file outside the served directory", attempt)
		}
	}
}

// TestDeletingAServedFileStillWorks pins that the tightening did not break the
// feature.
//
// A security check that refuses everything is not a fix. The handler still has to
// remove a file the served directory genuinely contains.
func TestDeletingAServedFileStillWorks(t *testing.T) {
	fb, root, _ := browserFixture(t)
	target := filepath.Join(root, "served.txt")

	rec := postDelete(t, fb, "served.txt")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deleting a served file returned %d, want a redirect", rec.Code)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the served file was not removed: %v", err)
	}
}

// TestDeletingADirectoryIsRefused pins the existing guard, through the new path.
func TestDeletingADirectoryIsRefused(t *testing.T) {
	fb, root, _ := browserFixture(t)
	dir := filepath.Join(root, "subdir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	rec := postDelete(t, fb, "subdir")
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a directory was deleted")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the directory was removed: %v", err)
	}
}
