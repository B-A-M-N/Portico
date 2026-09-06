// Descriptor-relative filesystem operations for the built-in file browser.
//
// On Linux, openat2 with RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS would be ideal but is
// not available in Go's standard library without cgo. What is used instead:
//
//   - openat() with O_NOFOLLOW so a symlink is refused rather than followed
//   - the resulting descriptor is resolved through /proc/self/fd and compared to the
//     canonical root, so a path that leaves the root is refused even when each
//     component looked acceptable
//   - the descriptor is kept open and served from, so the bytes sent come from the
//     file that was checked rather than from a second resolution by name
//
// What is here is what the browser calls. This file previously also carried openDir,
// statRel, unlinkRel, createTemp and a FileInfo wrapper for statRel's result, none of
// which had ever had a caller. They are removed rather than kept: two of them offered
// no protection the callers did not already have (createTemp was os.CreateTemp with
// path joining, and the delete path already uses unlinkat through os.Remove), and
// openDir would have handed out the browser's own root descriptor for ".", so closing
// it would have closed the root. Unused code that looks like a security primitive is
// worse than absent code, because the next reader assumes the guarantee exists.

package origin

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// rootedDir provides fd-relative operations against a verified root directory.
type rootedDir struct {
	rootFd        int
	canonicalRoot string
}

// newRootedDir creates a rootedDir from an already-verified root fd.
func newRootedDir(rootFd int, canonicalRoot string) *rootedDir {
	return &rootedDir{rootFd: rootFd, canonicalRoot: canonicalRoot}
}

// openRelative opens a path relative to root with O_NOFOLLOW.
// Returns the fd and the resolved path (via /proc/self/fd/N).
func (rd *rootedDir) openRelative(rel string, flags int) (int, string, error) {
	rel = strings.TrimPrefix(rel, "/")
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" {
		return rd.rootFd, rd.canonicalRoot, nil
	}
	if filepath.IsAbs(rel) {
		return 0, "", fmt.Errorf("absolute path not allowed")
	}
	if strings.HasPrefix(rel, "..") || strings.Contains(rel, "/..") {
		return 0, "", fmt.Errorf("path traversal rejected")
	}

	base := rd.rootFd
	ownedBase := false
	parts := strings.Split(rel, string(filepath.Separator))
	var fd int
	var err error
	for i, part := range parts {
		componentFlags := flags | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			// Every intermediate component must be a real directory. This
			// prevents openat from following a symlink into another tree.
			componentFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		}
		fd, err = unix.Openat(base, part, componentFlags, 0)
		if err != nil {
			if ownedBase {
				unix.Close(base)
			}
			return 0, "", fmt.Errorf("opening path: %w", err)
		}
		if ownedBase {
			unix.Close(base)
		}
		base, ownedBase = fd, true
	}

	procPath := fmt.Sprintf("/proc/self/fd/%d", fd)
	resolvedPath, err := os.Readlink(procPath)
	if err != nil {
		unix.Close(fd)
		return 0, "", fmt.Errorf("reading fd link: %w", err)
	}

	rootPath := rd.currentRootPath()
	if !strings.HasPrefix(resolvedPath, rootPath+string(filepath.Separator)) && resolvedPath != rootPath {
		unix.Close(fd)
		return 0, "", fmt.Errorf("path outside root")
	}

	return fd, resolvedPath, nil
}

func (rd *rootedDir) currentRootPath() string {
	path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", rd.rootFd))
	if err != nil {
		return rd.canonicalRoot
	}
	return path
}

// openFile opens a file relative to root for reading.
// The returned *os.File owns the fd and must be closed by the caller.
func (rd *rootedDir) openFile(rel string) (*os.File, error) {
	fd, _, err := rd.openRelative(rel, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	if strings.Trim(rel, "/") == "" || filepath.Clean(rel) == "." {
		// openRelative returns the borrowed root descriptor for ".". Never
		// hand that descriptor to a caller that owns and closes its result.
		fd, err = unix.Dup(rd.rootFd)
		if err != nil {
			return nil, fmt.Errorf("duplicate root descriptor: %w", err)
		}
		unix.CloseOnExec(fd)
	}
	return os.NewFile(uintptr(fd), ""), nil
}

// openDir opens a directory relative to the root and returns an owned
// descriptor. The root itself is duplicated so closing the returned file never
// invalidates rootedDir's borrowed descriptor.
func (rd *rootedDir) openDir(rel string) (*os.File, error) {
	fd, _, err := rd.openRelative(rel, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	if strings.Trim(rel, "/") == "" || filepath.Clean(rel) == "." {
		fd, err = unix.Dup(rd.rootFd)
		if err != nil {
			return nil, fmt.Errorf("duplicate root descriptor: %w", err)
		}
		unix.CloseOnExec(fd)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("stat rooted directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd)
		return nil, fmt.Errorf("rooted path is not a directory")
	}
	return os.NewFile(uintptr(fd), ""), nil
}

// renameNoReplace renames src to dst atomically without replacing existing files.
// P0 #2: Now uses RENAME_NOREPLACE on Linux via unix.Renameat2.
// renameNoReplace renames src to dst and refuses to replace an existing file.
//
// An upload writes a temporary file and renames it into place, so a rename that
// silently replaced an existing file would let one upload destroy another's result
// without either being told.
//
// There used to be a second implementation of this, split across
// builtin_filebrowser_linux.go and builtin_filebrowser_other.go as renameNoReplaceImpl,
// with no caller. It was removed rather than adopted: the !linux arm fell back to
// os.Rename, which does replace silently, and the package does not build on !linux
// anyway — O_PATH, allowlistedEnv and verifyOriginIdentity are all Linux-only and
// untagged. A cross-platform fallback that cannot be reached, and would weaken the
// guarantee if it were, is worse than none.
func renameNoReplace(src, dst string) error {
	if err := unix.Renameat2(0, src, 0, dst, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("destination already exists")
		}
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// serveFile serves a file from an open fd using http.ServeContent.
// This avoids the TOCTOU window of path-based serving.
func serveFile(w http.ResponseWriter, r *http.Request, f *os.File, name string) {
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "is a directory", http.StatusForbidden)
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}
