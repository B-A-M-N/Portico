package origin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// BuiltinStatic serves a static directory over HTTP.
type BuiltinStatic struct {
	cfg           Config
	server        *http.Server
	listener      net.Listener
	canonicalRoot string // pinned canonical root at construction time
	serveDone     chan struct{}
}

// NewBuiltinStatic creates a static file server origin.
func NewBuiltinStatic(cfg Config) (*BuiltinStatic, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("--path is required for builtin:static origin")
	}

	absPath, err := filepath.Abs(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("resolving path: %w", err)
	}

	// Resolve symlinks to pin the canonical root
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

	cfg.Path = absPath
	return &BuiltinStatic{cfg: cfg, canonicalRoot: absPath}, nil
}

func (s *BuiltinStatic) Type() Type {
	return TypeBuiltinStatic
}

func (s *BuiltinStatic) Start(_ context.Context) (string, error) {
	listenAddr := "127.0.0.1:0"
	if s.cfg.ListenPort != 0 {
		listenAddr = fmt.Sprintf("127.0.0.1:%d", s.cfg.ListenPort)
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", fmt.Errorf("binding to loopback: %w", err)
	}
	s.listener = listener

	// Use pinned canonical root for file serving
	fs := http.FileServer(http.Dir(s.canonicalRoot))

	var handler http.Handler
	if s.cfg.SPA {
		index := s.cfg.Index
		if index == "" {
			index = "index.html"
		}
		handler = spaHandler(s.canonicalRoot, index, fs)
	} else {
		handler = noSymlink(s.canonicalRoot, fs)
	}

	if s.cfg.CacheControl != "" {
		handler = cacheControlMiddleware(s.cfg.CacheControl, handler)
	}

	// Add security headers
	handler = securityHeadersMiddleware(handler)

	s.server = &http.Server{Handler: handler, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}

	s.serveDone = make(chan struct{})
	go func() {
		_ = s.server.Serve(listener)
		close(s.serveDone)
	}()

	addr := listener.Addr().String()
	return fmt.Sprintf("http://%s", addr), nil
}

func (s *BuiltinStatic) Stop(ctx context.Context) error {
	if s.server != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return s.server.Shutdown(shutdownCtx)
	}
	return nil
}

func (s *BuiltinStatic) Logs() io.ReadCloser {
	return nil
}

func (s *BuiltinStatic) Healthy(_ context.Context) error {
	if s.server == nil || s.listener == nil {
		return fmt.Errorf("server not started")
	}
	// Check if Serve() has exited — a closed serveDone channel means the
	// server is down. Use select to avoid race with a concurrent shutdown.
	select {
	case <-s.serveDone:
		return fmt.Errorf("server stopped")
	default:
		return nil
	}
}

// noSymlink wraps a handler to reject paths that resolve through symlinks
// beneath the root directory. http.Dir(root) follows descendant symlinks
// so an extra check is needed to prevent root/public-link -> /outside from
// being served.
func noSymlink(root string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject hidden files by default (e.g. .env, .git/config).
		if isHiddenPath(r.URL.Path) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		target, err := safePath(root, r.URL.Path)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *BuiltinStatic) Identity() (core.ProcessIdentity, bool) {
	return core.ProcessIdentity{}, false
}

func (s *BuiltinStatic) ProcessGroupID() int {
	return 0
}

// spaHandler serves the index file for any path that doesn't match a real file.
// The root path is pinned at construction time to prevent symlink escape.
//
// SECURITY: A path rejected by safePath (symlink escape, invalid path, etc.)
// must NOT become a successful SPA response. Only "legitimate in-root path
// does not exist" gets SPA fallback; path validation failures get 403/400.
func spaHandler(root, index string, fileServer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject hidden files and sensitive paths by default.
		if err := checkPublishPathStatic(r.URL.Path); err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Use safePath to prevent traversal.
		target, pathErr := safePath(root, r.URL.Path)
		if pathErr != nil {
			// Path validation failed (symlink escape, invalid path, etc.).
			// Do NOT fall back to SPA index — this would let an attacker
			// probe paths via the SPA response status.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if _, err := os.Stat(target); os.IsNotExist(err) {
			// Legitimate in-root path does not exist — SPA fallback.
			indexPath, err := safePath(root, index)
			if err == nil {
				http.ServeFile(w, r, indexPath)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// checkPublishPathStatic applies the hidden-file policy for BuiltinStatic,
// which doesn't have access to a BuiltinFileBrowser receiver. It rejects
// hidden paths and sensitive segments.
func checkPublishPathStatic(rel string) error {
	if isSensitivePath(rel) {
		return fmt.Errorf("sensitive path denied")
	}
	if isHiddenPath(rel) {
		return fmt.Errorf("hidden path denied")
	}
	return nil
}

// cacheControlMiddleware adds a Cache-Control header to all responses.
func cacheControlMiddleware(value string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", value)
		next.ServeHTTP(w, r)
	})
}

// securityHeadersMiddleware adds security headers to all responses.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
