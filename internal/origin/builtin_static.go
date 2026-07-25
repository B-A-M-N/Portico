package origin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// BuiltinStatic serves a static directory over HTTP.
type BuiltinStatic struct {
	cfg           Config
	server        *http.Server
	listener      net.Listener
	canonicalRoot string // pinned canonical root at construction time
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
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
		handler = fs
	}

	if s.cfg.CacheControl != "" {
		handler = cacheControlMiddleware(s.cfg.CacheControl, handler)
	}

	// Add security headers
	handler = securityHeadersMiddleware(handler)

	s.server = &http.Server{Handler: handler, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}

	go s.server.Serve(listener)

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
	if s.listener == nil {
		return fmt.Errorf("server not started")
	}
	return nil
}

// spaHandler serves the index file for any path that doesn't match a real file.
// The root path is pinned at construction time to prevent symlink escape.
func spaHandler(root, index string, fileServer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use safePath to prevent traversal
		target, err := safePath(root, r.URL.Path)
		if err != nil {
			// Fall back to index for SPA
			indexPath, err := safePath(root, index)
			if err == nil {
				http.ServeFile(w, r, indexPath)
			} else {
				http.Error(w, "not found", http.StatusNotFound)
			}
			return
		}
		if _, err := os.Stat(target); os.IsNotExist(err) {
			// SPA fallback: serve index
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
