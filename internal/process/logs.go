package process

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const maxUnterminatedLogLine = 1 << 20

var (
	authorizationHeaderPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*)[^\r\n]*`)
	bearerTokenPattern         = regexp.MustCompile(`(?i)\bbearer\s+[-._~+/=a-z0-9]+`)
	tunnelTokenPattern         = regexp.MustCompile(`(?i)(?:tunnel[ _-]?token|token-file|credentials?-file)\s*(?:=|:)\s*\S+`)
)

// RedactingWriter buffers an unterminated log line and redacts it before it
// reaches its destination. Holding partial lines is intentional: process
// pipes may split a secret across writes, and per-write replacement would
// expose the first half of that secret permanently.
//
// Close flushes its buffered data but does not close the destination, making
// it safe for stdout and stderr redactors to share one RotatingWriter.
type RedactingWriter struct {
	mu         sync.Mutex
	dst        io.Writer
	redactions [][]byte
	pending    []byte
	closed     bool
}

// NewRedactingWriter wraps dst with streaming redaction for explicit secret
// values and common credential-bearing log formats.
func NewRedactingWriter(dst io.Writer, redactions []string) *RedactingWriter {
	values := make([][]byte, 0, len(redactions))
	for _, value := range redactions {
		if value != "" {
			values = append(values, []byte(value))
		}
	}
	return &RedactingWriter{dst: dst, redactions: values}
}

func (w *RedactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	w.pending = append(w.pending, p...)
	if len(w.pending) > maxUnterminatedLogLine && bytes.IndexByte(w.pending, '\n') < 0 {
		// Do not risk writing an arbitrarily long partial line whose secret may
		// not have arrived yet. Preserve the safety invariant over log fidelity.
		if _, err := io.WriteString(w.dst, "[portico: unterminated log line redacted]\n"); err != nil {
			return 0, err
		}
		w.pending = w.pending[:0]
		return len(p), nil
	}
	if err := w.flushCompleteLinesLocked(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *RedactingWriter) flushCompleteLinesLocked() error {
	last := bytes.LastIndexByte(w.pending, '\n')
	if last < 0 {
		return nil
	}
	if _, err := w.dst.Write(redactLogBytes(w.pending[:last+1], w.redactions)); err != nil {
		return err
	}
	copy(w.pending, w.pending[last+1:])
	w.pending = w.pending[:len(w.pending)-(last+1)]
	return nil
}

func (w *RedactingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if len(w.pending) == 0 {
		return nil
	}
	_, err := w.dst.Write(redactLogBytes(w.pending, w.redactions))
	w.pending = nil
	return err
}

func redactLogBytes(data []byte, values [][]byte) []byte {
	result := append([]byte(nil), data...)
	for _, value := range values {
		result = bytes.ReplaceAll(result, value, []byte("[REDACTED]"))
	}
	result = authorizationHeaderPattern.ReplaceAll(result, []byte("${1}[REDACTED]"))
	result = bearerTokenPattern.ReplaceAll(result, []byte("Bearer [REDACTED]"))
	result = tunnelTokenPattern.ReplaceAll(result, []byte("[REDACTED]"))
	return result
}

const (
	maxLogSize  = 10 * 1024 * 1024 // 10 MiB
	maxLogFiles = 5
)

// RotatingWriter is an io.WriteCloser that appends to a log file and
// performs rename-based rotation once the file would exceed the size
// threshold (SPEC §11.4). It never truncates existing logs on open.
// It is safe for concurrent use.
type RotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxSize  int64
	maxFiles int
	f        *os.File
	size     int64
}

// NewRotatingWriter opens (or creates) the log file at path in append mode.
// maxSize/maxFiles fall back to package defaults when <= 0.
func NewRotatingWriter(path string, maxSize int64, maxFiles int) (*RotatingWriter, error) {
	if maxSize <= 0 {
		maxSize = maxLogSize
	}
	if maxFiles <= 0 {
		maxFiles = maxLogFiles
	}
	w := &RotatingWriter{path: path, maxSize: maxSize, maxFiles: maxFiles}
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingWriter) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f = f
	w.size = info.Size()
	return nil
}

// Write appends p, rotating first if the file would exceed the threshold.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingWriter) rotateLocked() error {
	_ = w.f.Close()
	w.f = nil
	if err := shiftRotatedFiles(w.path, w.maxFiles); err != nil {
		// Rotation failure must not lose output: reopen and keep appending.
		if openErr := w.openLocked(); openErr != nil {
			return openErr
		}
		return nil
	}
	return w.openLocked()
}

// Close closes the underlying file descriptor. Idempotent.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// RotateLogs checks log file size and rotates if needed (SPEC §11.4).
func RotateLogs(logPath string) error {
	info, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if info.Size() < maxLogSize {
		return nil
	}

	return shiftRotatedFiles(logPath, maxLogFiles)
}

// shiftRotatedFiles performs rename-based rotation: existing backups shift
// up one slot (log.1 -> log.2, ...), the current log becomes log.1, and
// excess backups are pruned.
func shiftRotatedFiles(logPath string, maxFiles int) error {
	dir := filepath.Dir(logPath)
	base := filepath.Base(logPath)

	for i := maxFiles - 1; i >= 1; i-- {
		older := filepath.Join(dir, fmt.Sprintf("%s.%d", base, i))
		newer := filepath.Join(dir, fmt.Sprintf("%s.%d", base, i+1))
		if _, err := os.Stat(older); err == nil {
			_ = os.Rename(older, newer)
		}
	}

	// Rename current log to .1.
	first := filepath.Join(dir, fmt.Sprintf("%s.1", base))
	if err := os.Rename(logPath, first); err != nil {
		return err
	}

	// Prune excess files.
	CleanupLogs(dir, base)

	return nil
}

// CleanupLogs removes excess rotated log files.
func CleanupLogs(dir, base string) {
	pattern := fmt.Sprintf("%s.*", base)
	entries, _ := filepath.Glob(filepath.Join(dir, pattern))

	sort.Slice(entries, func(i, j int) bool {
		// Sort by extension number, descending
		ei := extractNumber(entries[i])
		ej := extractNumber(entries[j])
		return ei > ej
	})

	for len(entries) > maxLogFiles {
		os.Remove(entries[0])
		entries = entries[1:]
	}
}

func extractNumber(name string) int {
	parts := strings.Split(name, ".")
	if len(parts) < 2 {
		return 0
	}
	n := 0
	for _, c := range parts[len(parts)-1] {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}
