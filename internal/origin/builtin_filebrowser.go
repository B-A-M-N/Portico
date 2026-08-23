package origin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"golang.org/x/sys/unix"
)

// safePath resolves a relative path against root using fd-relative operations.
// It uses openat() with the root file descriptor to prevent symlink retarget attacks.
// The rootFd must be opened with O_PATH and kept open for the lifetime of the browser.
func (fb *BuiltinFileBrowser) safePath(rel string) (string, error) {
	// Strip leading slash to make it relative
	rel = strings.TrimPrefix(rel, "/")

	// Clean the relative path
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" {
		return fb.canonicalRoot, nil
	}

	// Reject absolute paths (after stripping leading slash, should not have another)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path not allowed")
	}

	// Reject path traversal attempts
	if strings.HasPrefix(rel, "..") || strings.Contains(rel, "/..") {
		return "", fmt.Errorf("path traversal rejected")
	}

	// Use openat to open the path relative to rootFd
	// O_PATH allows us to get a fd without actually opening the file for I/O
	fd, err := unix.Openat(fb.rootFd, rel, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("opening path: %w", err)
	}
	defer unix.Close(fd)

	// Get the actual path via /proc/self/fd/N
	procPath := fmt.Sprintf("/proc/self/fd/%d", fd)
	resolvedPath, err := os.Readlink(procPath)
	if err != nil {
		return "", fmt.Errorf("reading fd link: %w", err)
	}

	// Verify the resolved path is still within the canonical root
	if !strings.HasPrefix(resolvedPath, fb.canonicalRoot+string(filepath.Separator)) && resolvedPath != fb.canonicalRoot {
		return "", fmt.Errorf("path outside root")
	}

	return resolvedPath, nil
}

// safePathLegacy resolves a relative path against root using path-based resolution.
// This is used by BuiltinStatic which has simpler security requirements.
// For write operations, use the fd-relative safePath method on BuiltinFileBrowser.
func safePath(root, rel string) (string, error) {
	// Clean the root path
	cleanRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("root path: %w", err)
	}

	// Resolve symlinks in root
	realRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		// If root doesn't exist yet, use cleaned abs
		realRoot = cleanRoot
	}

	// Walk the relative path component by component, resolving symlinks
	// for each intermediate component. This prevents symlink escape through
	// children of root even when the final path is inside.
	rel = filepath.Clean(rel)
	current := realRoot
	if rel == "." || rel == "" {
		return current, nil
	}

	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == ".." {
			// Parent reference: check we don't escape root
			parent := filepath.Dir(current)
			if parent == current {
				return "", fmt.Errorf("path traversal rejected")
			}
			// If parent is within root, allow it
			if parent == realRoot || strings.HasPrefix(parent, realRoot+string(filepath.Separator)) {
				current = parent
			} else {
				return "", fmt.Errorf("path traversal rejected")
			}
		} else if part == "." || part == "" {
			continue
		} else {
			next := filepath.Join(current, part)
			// If next exists and is a symlink, resolve it
			realNext, err := filepath.EvalSymlinks(next)
			if err != nil {
				// Target may not exist; keep the cleaned path
				realNext = next
			}
			// Verify this component didn't escape root
			if realNext != realRoot && !strings.HasPrefix(realNext, realRoot+string(filepath.Separator)) {
				return "", fmt.Errorf("path outside root")
			}
			current = realNext
		}
	}

	return current, nil
}

// P0 #2: Use fd-relative operations for filesystem access.
// The rootedDir provides the secure primitives; renameNoReplace is now
// in the platform-specific files (builtin_filebrowser_linux.go / _other.go).

// BuiltinFileBrowser serves a web-based file browser.
type BuiltinFileBrowser struct {
	cfg           Config
	server        *http.Server
	listener      net.Listener
	canonicalRoot string // pinned canonical root at construction time
	rootFd        int    // file descriptor for root directory (O_PATH)
	serveDone     chan struct{}
	csrfKey       [32]byte // random key for HMAC-based CSRF tokens
	rooted        *rootedDir // P0 #2: fd-relative operations
}

// NewBuiltinFileBrowser creates a file browser origin. The configured root is
// canonicalised (symlinks followed) once at construction and pinned via a file
// descriptor for path validation. Note: actual filesystem operations currently
// use pathname-based access, so there is a small validation-to-action window.
// For TOCTOU-safe operations, the rootedDir primitives would need to be used
// end-to-end (see P1 backlog).
func NewBuiltinFileBrowser(cfg Config) (*BuiltinFileBrowser, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("--path is required for builtin:file-browser origin")
	}

	absPath, err := filepath.Abs(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("resolving path: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		absPath = realRoot
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("checking path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", absPath)
	}

	return newBuiltinFileBrowserFromValidated(cfg, absPath)
}

// ValidateBuiltinFileBrowserConfig validates the configuration without opening
// file descriptors. Use this in Plan() to avoid FD leaks for previews that
// never become connections.
func ValidateBuiltinFileBrowserConfig(cfg Config) error {
	if cfg.Path == "" {
		return fmt.Errorf("--path is required for builtin:file-browser origin")
	}

	absPath, err := filepath.Abs(cfg.Path)
	if err != nil {
		return fmt.Errorf("resolving path: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		absPath = realRoot
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("checking path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", absPath)
	}

	return nil
}

func newBuiltinFileBrowserFromValidated(cfg Config, absPath string) (*BuiltinFileBrowser, error) {
	// Open root directory with O_PATH to pin it. All subsequent operations
	// will be relative to this fd, preventing symlink retarget attacks.
	rootFd, err := unix.Open(absPath, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening root fd: %w", err)
	}

	cfg.Path = absPath
	var csrfKey [32]byte
	if _, err := rand.Read(csrfKey[:]); err != nil {
		unix.Close(rootFd)
		return nil, fmt.Errorf("generating CSRF token key: %w", err)
	}

	return &BuiltinFileBrowser{cfg: cfg, canonicalRoot: absPath, rootFd: rootFd, csrfKey: csrfKey, rooted: newRootedDir(rootFd, absPath)}, nil
}

// Root returns the pinned canonical root path.
func (fb *BuiltinFileBrowser) Root() string {
	return fb.canonicalRoot
}

func (fb *BuiltinFileBrowser) Type() Type {
	return TypeBuiltinFileBrowser
}

func (fb *BuiltinFileBrowser) Start(_ context.Context) (string, error) {
	listenAddr := "127.0.0.1:0"
	if fb.cfg.ListenPort != 0 {
		listenAddr = fmt.Sprintf("127.0.0.1:%d", fb.cfg.ListenPort)
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", fmt.Errorf("binding to loopback: %w", err)
	}
	fb.listener = listener

	mux := http.NewServeMux()
	mux.HandleFunc("/", fb.handleBrowse)
	mux.HandleFunc("/api/files", fb.handleAPI)
	if fb.cfg.Download {
		mux.HandleFunc("/download/", fb.handleDownload)
	}
	if fb.cfg.AllowUpload && !fb.cfg.ReadOnly {
		// P0 #1: Apply body limit BEFORE CSRF check. The CSRF middleware
		// falls back to reading _csrf from the form body, so the body must
		// already be limited when that happens.
		mux.HandleFunc("/upload", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const maxUploadSize = 64 << 20
			const multipartOverhead = 2 << 10
			r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+multipartOverhead)
			fb.requireCSRF(fb.handleUpload).ServeHTTP(w, r)
		}))
	}
	if fb.cfg.AllowDelete && !fb.cfg.ReadOnly {
		// P0 #1: Apply body limit BEFORE CSRF check for the same reason.
		mux.HandleFunc("/delete", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const maxDeleteBody = 64 << 10
			r.Body = http.MaxBytesReader(w, r.Body, maxDeleteBody)
			fb.requireCSRF(fb.handleDelete).ServeHTTP(w, r)
		}))
	}

	// Apply security headers globally. Mutation endpoints enforce their
	// own per-route body limits (handleUpload uses MaxBytesReader), so
	// no global request-size wrapper is applied here.
	handler := securityHeadersMiddleware(mux)
	fb.server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	fb.serveDone = make(chan struct{})
	go func() {
		_ = fb.server.Serve(listener)
		close(fb.serveDone)
	}()

	addr := listener.Addr().String()
	return fmt.Sprintf("http://%s", addr), nil
}

func (fb *BuiltinFileBrowser) Stop(ctx context.Context) error {
	// Server shutdown and root-FD close are independent and idempotent so a
	// constructed-but-never-started browser (no server) is still closable.
	var shutdownErr error
	if fb.server != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		shutdownErr = fb.server.Shutdown(shutdownCtx)
	}
	fb.closeRootFd()
	return shutdownErr
}

// closeRootFd closes the pinned root descriptor if still open. Safe to call
// multiple times; after the first call it is a no-op.
func (fb *BuiltinFileBrowser) closeRootFd() {
	if fb.rootFd >= 0 {
		unix.Close(fb.rootFd)
		fb.rootFd = -1
	}
}

func (fb *BuiltinFileBrowser) Logs() io.ReadCloser {
	return nil
}

func (fb *BuiltinFileBrowser) Healthy(_ context.Context) error {
	if fb.server == nil || fb.listener == nil {
		return fmt.Errorf("server not started")
	}
	select {
	case <-fb.serveDone:
		return fmt.Errorf("server stopped")
	default:
		return nil
	}
}

// CSRFToken generates an HMAC-based CSRF token for the given path.
func (fb *BuiltinFileBrowser) CSRFToken(path string) string {
	h := hmac.New(sha256.New, fb.csrfKey[:])
	io.WriteString(h, path)
	return base64.URLEncoding.EncodeToString(h.Sum(nil))
}

// requireCSRF checks that the request includes a valid CSRF token.
// It accepts the token from either:
//   - the X-CSRF-Token header (for API/programmatic callers)
//   - the _csrf form value (for ordinary HTML form submissions)
//
// A per-route request-body limiter MUST be installed before calling
// this handler for multipart uploads (MaxBytesReader in handleUpload);
// ParseMultipartForm consumes the body, so the limiter must wrap it.
func (fb *BuiltinFileBrowser) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			// Fall back to form value for ordinary HTML form submissions.
			// This requires the body to have already been parsed or limited.
			token = r.FormValue("_csrf")
		}
		if token == "" {
			http.Error(w, "CSRF token required", http.StatusForbidden)
			return
		}
		expected := fb.CSRFToken(r.URL.Path)
		if !hmac.Equal([]byte(token), []byte(expected)) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (fb *BuiltinFileBrowser) Identity() (core.ProcessIdentity, bool) {
	return core.ProcessIdentity{}, false
}

func (fb *BuiltinFileBrowser) ProcessGroupID() int {
	return 0
}

type fileEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
	ModTime string `json:"mod_time"`
}

func (fb *BuiltinFileBrowser) listDir(relPath string) ([]fileEntry, error) {
	absPath, err := fb.safePath(relPath)
	if err != nil {
		return nil, fmt.Errorf("path check: %w", err)
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, err
	}

	var files []fileEntry
	for _, e := range entries {
		// Skip hidden and sensitive entries unless the user explicitly opted in.
		if err := fb.checkPublishPath(e.Name()); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileEntry{
			Name:    e.Name(),
			Size:    info.Size(),
			IsDir:   e.IsDir(),
			ModTime: info.ModTime().Format(time.RFC3339),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		return files[i].Name < files[j].Name
	})

	return files, nil
}

func (fb *BuiltinFileBrowser) handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		relPath = "/"
	}

	files, err := fb.listDir(relPath)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(files); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// isHiddenPath reports whether any segment of the relative path is hidden (starts with .).
func isHiddenPath(rel string) bool {
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// sensitivePathSegments lists directory/file segments whose exposure is
// categorically denied unless the user has made an explicit dangerous opt-in.
// These are version-control metadata directories and credential stores that
// must never be served to a remote caller.
var sensitivePathSegments = map[string]bool{
	".git":       true,
	".ssh":       true,
	".gnupg":     true,
	".env":       true,
	"id_rsa":     true,
	"id_ed25519": true,
	"id_ecdsa":   true,
	"id_dsa":     true,
}

// isSensitivePath reports whether the relative path contains any segment that
// must be categorically denied regardless of the ShowHidden setting. This is
// separate from hidden-file policy: a user may choose to expose dotfiles, but
// never version-control metadata or SSH/GPG credentials.
func isSensitivePath(rel string) bool {
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if sensitivePathSegments[part] {
			return true
		}
	}
	// Also catch any filename that looks like an environment/credential file.
	// This runs in addition to the segment map above to catch files like
	// ".env.local", ".npmrc", ".pypirc", ".aws/credentials", etc.
	base := filepath.Base(rel)
	switch base {
	case ".env", ".env.local", ".env.production", ".env.development",
		".npmrc", ".pypirc", ".pip", ".aws", ".azure", ".gcloud",
		"credentials", "credentials.json", "token", "token.json",
		"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
		"known_hosts", "authorized_keys", "config":
		return true
	}
	return false
}

// checkPublishPath is the centralized publish-path policy. It applies to
// every operation that exposes a local path to a remote caller: list, static
// GET, browser GET, download, upload, and delete. Hidden files are denied
// unless ShowHidden is enabled. Sensitive paths (.git, .ssh, .gnupg, and
// credential files) are always denied regardless of ShowHidden.
//
// Returns a nil error if the path may be served, or a non-nil error otherwise.
func (fb *BuiltinFileBrowser) checkPublishPath(rel string) error {
	if isSensitivePath(rel) {
		return fmt.Errorf("sensitive path denied")
	}
	if isHiddenPath(rel) && !fb.cfg.ShowHidden {
		return fmt.Errorf("hidden path denied")
	}
	return nil
}

// handleDownload serves a file for download. Hidden files and sensitive paths
// are rejected by default.
func (fb *BuiltinFileBrowser) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath := strings.TrimPrefix(r.URL.Path, "/download")

	// Apply the centralized publish-path policy (hidden + sensitive).
	if err := fb.checkPublishPath(relPath); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	absPath, err := fb.safePath(relPath)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	http.ServeFile(w, r, absPath)
}

// handleUpload processes a file upload. Hidden file targets are rejected by default.
// NOTE: r.Body must already be wrapped with http.MaxBytesReader by the caller
// (the /upload route handler in Start) — do NOT re-wrap here.

func (fb *BuiltinFileBrowser) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if fb.cfg.ReadOnly {
		http.Error(w, "read-only mode", http.StatusForbidden)
		return
	}

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "file too large", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer file.Close()

	dir := r.FormValue("path")
	if dir == "" {
		dir = "/"
	}

	// Apply the centralized publish-path policy to the directory.
	if err := fb.checkPublishPath(dir); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Validate the upload directory (parent) separately from the final filename.
	// The destination file does not exist yet, so it must not be validated as
	// an existing path.
	destDir, err := fb.safePath(dir)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Sanitize the filename from the upload header. Reject empty, hidden or
	// path-traversal filenames using the centralized publish-path policy.
	cleanName := filepath.Base(header.Filename)
	if cleanName == "" || cleanName == "." || cleanName == ".." {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := fb.checkPublishPath(cleanName); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Ensure the sanitized name does not escape via a trick (e.g. "foo/../../../etc/passwd").
	if cleanName != filepath.Clean(cleanName) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	destPath := filepath.Join(destDir, cleanName)

	// Create a temporary file in the same directory, then atomically rename.
	// This prevents a copy failure from truncating or overwriting the destination.
	// We use RENAME_NOREPLACE to prevent silent overwrites of existing files.
	tmpFile, err := os.CreateTemp(destDir, ".upload-*")
	if err != nil {
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()

	// Ensure temp file cleanup even if rename fails.
	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}()

	if _, err := io.Copy(tmpFile, file); err != nil {
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	// Sync BEFORE close — you cannot sync a closed file.
	if err := tmpFile.Sync(); err != nil {
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	if err := tmpFile.Chmod(0644); err != nil {
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	if err := tmpFile.Close(); err != nil {
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}

	// Rename atomically with RENAME_NOREPLACE to prevent silent overwrites.
	if err := renameNoReplace(tmpPath, destPath); err != nil {
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}

	// fsync the containing directory after rename.
	if fd, dirErr := os.Open(destDir); dirErr == nil {
		_ = fd.Sync()
		fd.Close()
	}

	w.WriteHeader(http.StatusCreated)
}

func (fb *BuiltinFileBrowser) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if fb.cfg.ReadOnly || !fb.cfg.AllowDelete {
		http.Error(w, "read-only mode", http.StatusForbidden)
		return
	}

	// Apply a small body limit BEFORE parsing form (prevents unbounded read).
	const maxDeleteBody = 64 << 10 // 64 KiB is generous for a single path field
	r.Body = http.MaxBytesReader(w, r.Body, maxDeleteBody)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Apply the centralized publish-path policy to the delete target.
	if err := fb.checkPublishPath(r.Form.Get("path")); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	target, err := fb.safePath(r.Form.Get("path"))
	if err != nil || target == fb.cfg.Path {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "refusing to delete directories", http.StatusBadRequest)
		return
	}
	if err := os.Remove(target); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (fb *BuiltinFileBrowser) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	relPath := r.URL.Path
	if relPath == "" {
		relPath = "/"
	}

	// Apply the centralized publish-path policy to the directory being browsed.
	if err := fb.checkPublishPath(relPath); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	files, err := fb.listDir(relPath)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := struct {
		Path       string
		Files      []fileEntry
		Download   bool
		Upload     bool
		Delete     bool
		ReadOnly   bool
		UploadCSRF string
		DeleteCSRF string
	}{
		Path:       relPath,
		Files:      files,
		Download:   fb.cfg.Download,
		Upload:     fb.cfg.AllowUpload && !fb.cfg.ReadOnly,
		Delete:     fb.cfg.AllowDelete && !fb.cfg.ReadOnly,
		ReadOnly:   fb.cfg.ReadOnly,
		UploadCSRF: fb.CSRFToken("/upload"),
		DeleteCSRF: fb.CSRFToken("/delete"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	fileBrowserTmpl.Execute(w, data)
}

var fileBrowserTmpl = template.Must(template.New("browser").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>File Browser - {{.Path}}</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
         max-width: 900px; margin: 0 auto; padding: 20px; color: #1a1a1a; background: #fafafa; }
  h1 { font-size: 1.2em; padding: 12px 0; border-bottom: 1px solid #e0e0e0; margin-bottom: 12px;
       word-break: break-all; }
  table { width: 100%; border-collapse: collapse; }
  th { text-align: left; padding: 8px 12px; border-bottom: 2px solid #e0e0e0; font-size: 0.85em;
       color: #666; text-transform: uppercase; }
  td { padding: 8px 12px; border-bottom: 1px solid #f0f0f0; }
  tr:hover { background: #f5f5f5; }
  a { color: #0066cc; text-decoration: none; }
  a:hover { text-decoration: underline; }
  .dir { font-weight: 600; }
  .size { color: #666; font-size: 0.9em; }
  .time { color: #999; font-size: 0.85em; }
  .actions { font-size: 0.85em; }
</style>
</head>
<body>
<h1>{{.Path}}</h1>
<table>
<thead><tr><th>Name</th><th>Size</th><th>Modified</th>{{if or .Download .Delete}}<th>Actions</th>{{end}}</tr></thead>
<tbody>
{{if ne .Path "/"}}
<tr><td colspan="4"><a href="../">..</a></td></tr>
{{end}}
{{range .Files}}
<tr>
  <td>{{if .IsDir}}<a class="dir" href="{{.Name}}/">{{.Name}}/</a>
      {{else}}<a href="{{.Name}}">{{.Name}}</a>{{end}}</td>
  <td class="size">{{if .IsDir}}-{{else}}{{.Size}}{{end}}</td>
  <td class="time">{{.ModTime}}</td>
  {{if or $.Download $.Delete}}<td class="actions">{{if not .IsDir}}
    {{if $.Download}}<a href="/download{{$.Path}}{{.Name}}">download</a>{{end}}
    {{if $.Delete}}<form action="/delete" method="post" style="display:inline"><input type="hidden" name="_csrf" value="{{$.DeleteCSRF}}"><input type="hidden" name="path" value="{{$.Path}}{{.Name}}"><button type="submit">delete</button></form>{{end}}
  {{end}}</td>{{end}}
</tr>
{{end}}
</tbody>
</table>
{{if .Upload}}
<form action="/upload" method="post" enctype="multipart/form-data">
  <input type="hidden" name="_csrf" value="{{.UploadCSRF}}">
  <input type="hidden" name="path" value="{{.Path}}">
  <label>Upload <input type="file" name="file" required></label>
  <button type="submit">Upload</button>
</form>
{{end}}
</body>
</html>`))
