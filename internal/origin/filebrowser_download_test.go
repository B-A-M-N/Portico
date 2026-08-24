//go:build linux

package origin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Serving a file the served directory does not contain.
//
// These hold the refusals in place. Verified by removing the guards one at a time:
// the symlink escape is refused by safePath, which resolves the descriptor through
// /proc/self/fd and compares it to the canonical root — not by the O_NOFOLLOW flag and
// not by the descriptor-based serving added alongside these tests. So the escape was
// already closed, and saying otherwise would be a claim these tests do not support.
//
// What the descriptor-based serving changes is narrower: http.ServeFile reopened the
// path by name, so the bytes sent came from a second resolution performed after the
// check. Serving from the descriptor safePath validated removes that second
// resolution. These tests cover the behaviour either way, which is why they passed
// before the change as well — that is worth recording rather than presenting them as
// proof of a fix they do not isolate.

// getDownload drives the download handler.
func getDownload(t *testing.T, fb *BuiltinFileBrowser, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/download/"+path, nil)
	rec := httptest.NewRecorder()
	fb.handleDownload(rec, req)
	return rec
}

// TestDownloadingThroughASymlinkIsRefused pins the property the fd-relative
// primitives exist for.
//
// A symlink inside the served directory pointing at a file outside it must not be a
// way to read that file. O_NOFOLLOW refuses the link rather than resolving it.
func TestDownloadingThroughASymlinkIsRefused(t *testing.T) {
	fb, root, secret := browserFixture(t)

	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	rec := getDownload(t, fb, "escape.txt")
	if rec.Code == http.StatusOK {
		t.Fatalf("a file outside the served directory was served through a symlink:\n%s",
			rec.Body.String())
	}
	if body := rec.Body.String(); contains(body, "must survive") {
		t.Fatalf("the contents of a file outside the served directory were disclosed:\n%s", body)
	}
}

// TestDownloadingASymlinkToAServedFileIsAlsoRefused pins that the refusal is about
// the link, not about where it points.
//
// A link is refused whatever its target, because deciding otherwise means resolving
// it — which is the operation that cannot be done safely between the check and the
// read.
func TestDownloadingASymlinkToAServedFileIsAlsoRefused(t *testing.T) {
	fb, root, _ := browserFixture(t)

	link := filepath.Join(root, "alias.txt")
	if err := os.Symlink(filepath.Join(root, "served.txt"), link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	if rec := getDownload(t, fb, "alias.txt"); rec.Code == http.StatusOK {
		t.Error("a symlink was resolved and served")
	}
}

// TestDownloadingAServedFileStillWorks pins that the tightening did not break the
// feature.
//
// A security check that refuses everything is not a fix. The handler still has to
// serve a file the directory genuinely contains, with its contents intact.
func TestDownloadingAServedFileStillWorks(t *testing.T) {
	fb, _, _ := browserFixture(t)

	rec := getDownload(t, fb, "served.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("serving a real file returned %d:\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "served" {
		t.Fatalf("the served contents are %q, want the file's own", body)
	}
}

// TestDownloadingSomethingAbsentIsNotFound pins that a missing file is a 404.
//
// Reporting it as forbidden tells the user they lack permission when the file is
// simply not there.
func TestDownloadingSomethingAbsentIsNotFound(t *testing.T) {
	fb, _, _ := browserFixture(t)

	if rec := getDownload(t, fb, "never-existed.txt"); rec.Code != http.StatusNotFound {
		t.Fatalf("an absent file returned %d, want 404", rec.Code)
	}
}

// TestDownloadingATraversalPathIsRefused pins the simpler escape.
func TestDownloadingATraversalPathIsRefused(t *testing.T) {
	fb, _, _ := browserFixture(t)

	for _, attempt := range []string{
		"../secret.txt",
		"../../secret.txt",
		"served/../../secret.txt",
	} {
		rec := getDownload(t, fb, attempt)
		if rec.Code == http.StatusOK {
			t.Errorf("the traversal %q was served:\n%s", attempt, rec.Body.String())
		}
		if contains(rec.Body.String(), "must survive") {
			t.Fatalf("the traversal %q disclosed a file outside the served directory", attempt)
		}
	}
}

// contains is strings.Contains, named for what the assertions above are asking.
func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
