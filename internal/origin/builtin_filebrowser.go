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

// renameNoReplace renames src to dst. On Linux it uses RENAME_NOREPLACE
// to prevent silent overwrites; on other platforms it falls back to the
// standard syscall.
func renameNoReplace(src, dst string) error {
	return renameNoReplaceImpl(src, dst)
}

// BuiltinFileBrowser serves a web-based file browser.
type BuiltinFileBrowser struct {
	cfg           Config
	server        *http.Server
	listener      net.Listener
	canonicalRoot string // pinned canonical root at construction time
	rootFd        int    // file descriptor for root directory (O_PATH)
	serveDone     chan struct{}
	csrfKey       [32]byte // random key for HMAC-based CSRF tokens
}

// NewBuiltinFileBrowser creates a file browser origin. The configured root is
// canonicalised (symlinks followed) once at construction and pinned via a file
// descriptor so the served root cannot be redirected by a later symlink retarget.
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

	return &BuiltinFileBrowser{cfg: cfg, canonicalRoot: absPath, rootFd: rootFd, csrfKey: csrfKey}, nil
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
		mux.HandleFunc("/upload", fb.requireCSRF(fb.handleUpload))
	}
	if fb.cfg.AllowDelete && !fb.cfg.ReadOnly {
		mux.HandleFunc("/delete", fb.requireCSRF(fb.handleDelete))
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
	if fb.server != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err := fb.server.Shutdown(shutdownCtx)
		// Close the root file descriptor to release the pinned directory
		if fb.rootFd >= 0 {
			unix.Close(fb.rootFd)
			fb.rootFd = -1
		}
		return err
	}
	return nil
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

// requireCSRF checks that the request includes a valid CSRF token
// in the X-CSRF-Token header or in the form value _csrf.
// The token is validated against the server-side HMAC key using the
// request path as the message, ensuring tokens are scope-bound.
func (fb *BuiltinFileBrowser) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
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
		if !fb.cfg.ShowHidden && strings.HasPrefix(e.Name(), ".") {
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(files); err != nil {
		http.Error(w, "encoding response: "+err.Error(), http.StatusInternalServerError)
	}
}

func (fb *BuiltinFileBrowser) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath := strings.TrimPrefix(r.URL.Path, "/download")
	absPath, err := fb.safePath(relPath)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	http.ServeFile(w, r, absPath)
}

func (fb *BuiltinFileBrowser) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if fb.cfg.ReadOnly {
		http.Error(w, "read-only mode", http.StatusForbidden)
		return
	}

	// Enforce a generous upload size limit (64 MiB).
	// The multipart form parser adds overhead for headers, boundaries,
	// and field data; 2 KiB covers that to avoid the complete body
	// being capped at exactly the nominal file-size limit.
	const maxUploadSize = 64 << 20
	const multipartOverhead = 2 << 10
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+multipartOverhead)
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

	destDir, err := fb.safePath(dir)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Sanitize filename
	cleanName := filepath.Base(header.Filename)
	destPath := filepath.Join(destDir, cleanName)

	// Verify dest is still within root
	_, err = fb.safePath(filepath.Join(dir, cleanName))
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Create a temporary file in the same directory, then atomically rename.
	// This prevents a copy failure from truncating or overwriting the destination.
	// We use RENAME_NOREPLACE to prevent silent overwrites of existing files.
	tmpFile, err := os.CreateTemp(destDir, ".upload-*")
	if err != nil {
		http.Error(w, "creating temp file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()

	// Ensure temp file cleanup even if rename fails.
	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}()

	if _, err := io.Copy(tmpFile, file); err != nil {
		http.Error(w, "writing file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpFile.Close(); err != nil {
		http.Error(w, "closing temp file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// fsync the temp file before rename for durability.
	if err := tmpFile.Sync(); err != nil {
		http.Error(w, "syncing file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Rename atomically with RENAME_NOREPLACE to prevent silent overwrites.
	if err := renameNoReplace(tmpPath, destPath); err != nil {
		http.Error(w, "renaming file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// fsync the containing directory after rename.
	if fd, dirErr := os.Open(destDir); dirErr == nil {
		_ = fd.Sync()
		fd.Close()
	}

	if err := os.Chmod(destPath, 0644); err != nil {
		http.Error(w, "chmod file: "+err.Error(), http.StatusInternalServerError)
		return
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
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
		http.Error(w, "checking path", http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "refusing to delete directories", http.StatusBadRequest)
		return
	}
	if err := os.Remove(target); err != nil {
		http.Error(w, "deleting file", http.StatusInternalServerError)
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

	files, err := fb.listDir(relPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
