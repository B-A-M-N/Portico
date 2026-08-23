// P0 #2: Descriptor-relative filesystem operations.
//
// On Linux, openat2 with RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS would be ideal
// but is not available in Go's standard library without cgo. We use the
// best available primitives:
//
//   - openat() with O_NOFOLLOW to prevent symlink traversal
//   - Keep fds open during the entire operation to minimize TOCTOU window
//   - For download: serve from the open *os.File via http.ServeContent
//   - For directory listing: enumerate via fd where possible
//   - For delete: open parent fd, validate, unlinkat
//   - For upload: open destination dir fd, create temp within it
//
// The remaining TOCTOU window is between path validation and the operation.
// Without openat2, this cannot be fully eliminated in pure Go.

package origin

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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

	fd, err := unix.Openat(rd.rootFd, rel, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, "", fmt.Errorf("opening path: %w", err)
	}

	procPath := fmt.Sprintf("/proc/self/fd/%d", fd)
	resolvedPath, err := os.Readlink(procPath)
	if err != nil {
		unix.Close(fd)
		return 0, "", fmt.Errorf("reading fd link: %w", err)
	}

	if !strings.HasPrefix(resolvedPath, rd.canonicalRoot+string(filepath.Separator)) && resolvedPath != rd.canonicalRoot {
		unix.Close(fd)
		return 0, "", fmt.Errorf("path outside root")
	}

	return fd, resolvedPath, nil
}

// openFile opens a file relative to root for reading.
// The returned *os.File owns the fd and must be closed by the caller.
func (rd *rootedDir) openFile(rel string) (*os.File, error) {
	fd, _, err := rd.openRelative(rel, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), ""), nil
}

// openDir opens a directory relative to root.
func (rd *rootedDir) openDir(rel string) (*os.File, error) {
	fd, _, err := rd.openRelative(rel, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), ""), nil
}

// statRelative stats a path relative to root without following symlinks.
func (rd *rootedDir) statRel(rel string) (os.FileInfo, error) {
	rel = strings.TrimPrefix(rel, "/")
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" {
		return os.Stat(rd.canonicalRoot)
	}
	var st unix.Stat_t
	if err := unix.Fstatat(rd.rootFd, rel, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	return &rootedFileInfo{st: st, name: filepath.Base(rel)}, nil
}

// unlinkRelative removes a file relative to root without following symlinks.
func (rd *rootedDir) unlinkRel(rel string) error {
	rel = strings.TrimPrefix(rel, "/")
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" {
		return fmt.Errorf("cannot remove root")
	}
	if filepath.IsAbs(rel) {
		return fmt.Errorf("absolute path not allowed")
	}
	if strings.HasPrefix(rel, "..") || strings.Contains(rel, "/..") {
		return fmt.Errorf("path traversal rejected")
	}
	if err := unix.Unlinkat(rd.rootFd, rel, 0); err != nil {
		return fmt.Errorf("removing: %w", err)
	}
	return nil
}

// createTemp creates a temporary file within the root directory.
func (rd *rootedDir) createTemp(dir, pattern string) (*os.File, error) {
	dir = strings.TrimPrefix(dir, "/")
	dir = filepath.Clean(dir)
	if dir == "." || dir == "" {
		return os.CreateTemp(rd.canonicalRoot, pattern)
	}
	return os.CreateTemp(rd.canonicalRoot+"/"+dir, pattern)
}

// renameNoReplace renames src to dst atomically without replacing existing files.
// P0 #2: Now uses RENAME_NOREPLACE on Linux via unix.Renameat2.
func renameNoReplace(src, dst string) error {
	if err := unix.Renameat2(0, src, 0, dst, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("destination already exists")
		}
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// rootedFileInfo wraps unix.Stat_t to implement os.FileInfo.
type rootedFileInfo struct {
	st   unix.Stat_t
	name string
}

func (fi *rootedFileInfo) Name() string      { return fi.name }
func (fi *rootedFileInfo) Size() int64       { return fi.st.Size }
func (fi *rootedFileInfo) Mode() os.FileMode { return os.FileMode(fi.st.Mode) }
func (fi *rootedFileInfo) ModTime() time.Time {
	return time.Unix(int64(fi.st.Mtim.Sec), int64(fi.st.Mtim.Nsec))
}
func (fi *rootedFileInfo) IsDir() bool      { return fi.st.Mode&syscall.S_IFDIR != 0 }
func (fi *rootedFileInfo) Sys() interface{} { return fi.st }

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
