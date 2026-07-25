package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// SecretStore provides encryption for sensitive data at rest.
// It uses a random 256-bit installation key stored in a protected file,
// with AES-GCM authenticated encryption bound to the encryption context.
type SecretStore struct {
	mu  sync.Mutex
	key []byte
}

// KeyHeader is stored alongside encrypted blobs to support key rotation.
type KeyHeader struct {
	KeyVersion int    `json:"key_version"`
	Algorithm  string `json:"algorithm"`
}

// EncryptedBlob represents the full encrypted payload with metadata.
type EncryptedBlob struct {
	KeyVersion int    `json:"key_version"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
	ContextAAD []byte `json:"context_aad"` // Additional authenticated data
}

const (
	currentKeyVersion = 1
	keyFileName       = "portico-key.bin"
)

// NewSecretStore creates or loads the installation key.
// The key is stored in a file with restrictive permissions (0600).
func NewSecretStore(dataDir string) (*SecretStore, error) {
	keyPath := filepath.Join(dataDir, keyFileName)

	// Try to load existing key.
	if key, err := loadKeyFile(keyPath); err == nil {
		return &SecretStore{key: key}, nil
	}

	// Generate a new random 256-bit installation key.
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate installation key: %w", err)
	}

	// Ensure directory exists with restrictive permissions.
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}

	// Write key file with restrictive permissions.
	if err := writeKeyFile(keyPath, key); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}

	return &SecretStore{key: key}, nil
}

func loadKeyFile(path string) ([]byte, error) {
	// Check file permissions before reading.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	mode := info.Mode().Perm()
	if mode != 0600 {
		return nil, fmt.Errorf("key file has insecure permissions: %o", mode)
	}
	return os.ReadFile(path)
}

func writeKeyFile(path string, key []byte) error {
	// Write with 0600 permissions (owner read/write only).
	if err := os.WriteFile(path, key, 0600); err != nil {
		return err
	}
	// Verify the permissions were set correctly.
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0600 {
		return fmt.Errorf("key file permissions not set correctly: %o", info.Mode().Perm())
	}
	return nil
}

// Encrypt encrypts plaintext using AES-GCM with context-bound AAD.
// The context parameter is used as Additional Authenticated Data (AAD)
// to bind the ciphertext to a specific connection/provider/tunnel.
func (s *SecretStore) Encrypt(plaintext []byte, context string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	// Use context as AAD to bind the ciphertext to its purpose.
	aad := sha256.Sum256([]byte(context))
	ciphertext := gcm.Seal(nonce, nonce, plaintext, aad[:])

	// Build the encrypted blob with metadata.
	blob := EncryptedBlob{
		KeyVersion: currentKeyVersion,
		Nonce:      nonce, // Included for clarity, also prepended to ciphertext
		Ciphertext: ciphertext,
		ContextAAD: aad[:],
	}

	return json.Marshal(blob)
}

// Decrypt decrypts a blob encrypted with Encrypt.
// The context must match the context used during encryption.
func (s *SecretStore) Decrypt(blobBytes []byte, context string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var blob EncryptedBlob
	if err := json.Unmarshal(blobBytes, &blob); err != nil {
		return nil, fmt.Errorf("unmarshal blob: %w", err)
	}

	// Verify key version for future rotation support.
	if blob.KeyVersion != currentKeyVersion {
		return nil, fmt.Errorf("unsupported key version: %d", blob.KeyVersion)
	}

	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(blob.Ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ct := blob.Ciphertext[:nonceSize], blob.Ciphertext[nonceSize:]

	// Verify context matches (AAD binding).
	aad := sha256.Sum256([]byte(context))
	if string(aad[:]) != string(blob.ContextAAD) {
		return nil, fmt.Errorf("context mismatch: ciphertext bound to different context")
	}

	plaintext, err := gcm.Open(nil, nonce, ct, aad[:])
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}

// Destroy securely wipes the key from memory.
func (s *SecretStore) Destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.key {
		s.key[i] = 0
	}
	s.key = nil
}
