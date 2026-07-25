package cloudflare

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// credentialDir is the directory for temporary credential files.
var credentialDir string

// SetCredentialDir sets the directory used for temporary credential files.
func SetCredentialDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create credential dir: %w", err)
	}
	credentialDir = dir
	return nil
}

// createCredentialFile creates a temporary credential file with a random name
// in a 0700 directory. The file is created with 0600 permissions.
// Returns the file path and a cleanup function.
func createCredentialFile(token []byte) (string, func(), error) {
	if credentialDir == "" {
		return "", nil, fmt.Errorf("credential dir not set")
	}

	// Generate a random filename (16 bytes = 32 hex chars).
	randomBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, randomBytes); err != nil {
		return "", nil, fmt.Errorf("generate random filename: %w", err)
	}
	filename := "cred-" + hex.EncodeToString(randomBytes) + ".tmp"
	path := filepath.Join(credentialDir, filename)

	// Create the file with 0600 permissions (owner read/write only).
	// Use O_CREATE|O_EXCL to prevent overwriting existing files.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("create credential file: %w", err)
	}

	// Write the token.
	if _, err := f.Write(token); err != nil {
		f.Close()
		os.Remove(path)
		return "", nil, fmt.Errorf("write credential: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", nil, fmt.Errorf("close credential file: %w", err)
	}

	// Return path and cleanup function.
	cleanup := func() {
		os.Remove(path)
	}
	return path, cleanup, nil
}

// sweepStaleCredentialFiles removes credential files left by a previous
// supervisor crash. Called on startup.
func sweepStaleCredentialFiles() error {
	if credentialDir == "" {
		return nil
	}
	entries, err := os.ReadDir(credentialDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read credential dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Remove all files in the credential directory.
		// They are temporary and should not survive restarts.
		path := filepath.Join(credentialDir, entry.Name())
		if err := os.Remove(path); err != nil {
			// Log but don't fail - best effort cleanup.
			continue
		}
	}
	return nil
}

// withCredentialFileUntilReady creates a scoped credential file for a
// process start where the file must outlive the start call until the
// started process has consumed the token. It invokes start with the
// file path, then waits for ready(handle) before returning. The file
// is removed on every exit path — start failure, readiness failure,
// panic, cancellation, and success — via defer.
func withCredentialFileUntilReady[T any](dir string, token []byte, start func(path string) (T, error), ready func(handle T) error) (T, error) {
	var zero T
	if err := SetCredentialDir(dir); err != nil {
		return zero, err
	}
	path, cleanup, err := createCredentialFile(token)
	if err != nil {
		return zero, err
	}
	// Removal must happen on ALL paths, including panics; only defer
	// guarantees that. By the time this function returns, the token
	// consumer has either read the file or failed — either way the
	// file must not linger on disk.
	defer cleanup()

	handle, err := start(path)
	if err != nil {
		return zero, err
	}
	if ready != nil {
		if err := ready(handle); err != nil {
			return handle, err
		}
	}
	return handle, nil
}
