package credentials

import (
	"fmt"
	"os"
)

// Resolver resolves credential references to actual values at operation time.
// Tokens are never serialized into plans, events, logs, runtime records, or UI.
type Resolver interface {
	Resolve(ref CredentialRef) (string, error)
}

// CredentialRef is a reference to a stored credential.
type CredentialRef struct {
	Source CredentialSource
	Key    string
}

// CredentialSource describes where the credential is stored.
type CredentialSource string

const (
	SourceEnv     CredentialSource = "env"
	SourceKeyring CredentialSource = "keyring"
	SourceSession CredentialSource = "session"
	SourceFile    CredentialSource = "file"
)

// EnvResolver resolves credentials from environment variables.
type EnvResolver struct{}

// Resolve reads an environment variable.
func (e *EnvResolver) Resolve(ref CredentialRef) (string, error) {
	val := os.Getenv(ref.Key)
	if val == "" {
		return "", fmt.Errorf("env var %s not set", ref.Key)
	}
	return val, nil
}

// MemoryResolver resolves credentials from an in-memory map (for testing).
type MemoryResolver struct {
	store map[string]string
}

// NewMemoryResolver creates an in-memory credential resolver.
func NewMemoryResolver() *MemoryResolver {
	return &MemoryResolver{store: make(map[string]string)}
}

// Resolve returns a stored credential.
func (m *MemoryResolver) Resolve(ref CredentialRef) (string, error) {
	val, ok := m.store[ref.Key]
	if !ok {
		return "", fmt.Errorf("credential %s not found", ref.Key)
	}
	return val, nil
}

// Set stores a credential.
func (m *MemoryResolver) Set(key, value string) {
	m.store[key] = value
}
