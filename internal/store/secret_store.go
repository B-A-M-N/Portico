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
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// SecretStore provides encryption for sensitive data at rest.
// It uses a random 256-bit installation key stored in a protected file,
// with AES-GCM authenticated encryption bound to the encryption context.
type SecretStore struct {
	mu             sync.Mutex
	keys           map[int][]byte
	currentVersion int
	dataDir        string
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
	initialKeyVersion = 1
	keyFileName       = "portico-key.bin" // Version 1 compatibility name.
)

// NewSecretStore creates or loads the installation key.
// The key is stored in a file with restrictive permissions (0600).
func NewSecretStore(dataDir string) (*SecretStore, error) {
	keyPath := filepath.Join(dataDir, keyFileName)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}

	// Try to load an existing key. Only a genuinely missing key may be
	// created. Replacing an unreadable, corrupt, or insecure key would make
	// every previously encrypted credential unrecoverable.
	var initialKey []byte
	if key, err := loadKeyFile(keyPath); err == nil {
		initialKey = key
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("load installation key: %w", err)
	}

	keys := make(map[int][]byte)
	if initialKey != nil {
		keys[initialKeyVersion] = initialKey
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("read key directory: %w", err)
	}
	currentVersion := 0
	if initialKey != nil {
		currentVersion = initialKeyVersion
	}
	for _, entry := range entries {
		version, ok := versionedKeyFileVersion(entry.Name())
		if !ok {
			continue
		}
		key, err := loadKeyFile(filepath.Join(dataDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("load installation key version %d: %w", version, err)
		}
		keys[version] = key
		if version > currentVersion {
			currentVersion = version
		}
	}
	if len(keys) == 0 {
		// Generate a new random 256-bit installation key only for a genuinely
		// empty keyring. Existing invalid keys always fail closed above.
		initialKey = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, initialKey); err != nil {
			return nil, fmt.Errorf("generate installation key: %w", err)
		}
		if err := writeKeyFile(keyPath, initialKey); err != nil {
			return nil, fmt.Errorf("write key file: %w", err)
		}
		keys[initialKeyVersion] = initialKey
		currentVersion = initialKeyVersion
	}

	return &SecretStore{keys: keys, currentVersion: currentVersion, dataDir: dataDir}, nil
}

func versionedKeyFileVersion(name string) (int, bool) {
	const prefix = "portico-key-v"
	const suffix = ".bin"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	version, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix))
	return version, err == nil && version > initialKeyVersion
}

func versionedKeyFilePath(dataDir string, version int) string {
	return filepath.Join(dataDir, fmt.Sprintf("portico-key-v%d.bin", version))
}

func loadKeyFile(path string) ([]byte, error) {
	// Do not follow a symlink: an installation key must be a regular file
	// owned by the current user. Replacing a bad key would strand all
	// existing encrypted credentials, so every unexpected condition fails
	// closed.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("key file is not a regular file")
	}
	mode := info.Mode().Perm()
	if mode != 0600 {
		return nil, fmt.Errorf("key file has insecure permissions: %o", mode)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("key file is not owned by the current user")
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("key file changed while opening")
	}
	openedStat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || openedStat.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("key file owner changed while opening")
	}
	key, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key file has invalid length %d", len(key))
	}
	return key, nil
}

func writeKeyFile(path string, key []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("invalid installation key length %d", len(key))
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".portico-key-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(key); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// NewSecretStore only calls this after observing that the destination is
	// absent. RENAME_NOREPLACE preserves that guarantee if another process
	// creates the key between the check and this commit.
	if err := unix.Renameat2(unix.AT_FDCWD, tmpPath, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	if err := unix.Fsync(dirFD); err != nil {
		return err
	}
	return nil
}

// Encrypt encrypts plaintext using AES-GCM with context-bound AAD.
// The context parameter is used as Additional Authenticated Data (AAD)
// to bind the ciphertext to a specific connection/provider/tunnel.
func (s *SecretStore) Encrypt(plaintext []byte, context string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.keys[s.currentVersion]
	if len(key) != 32 {
		return nil, fmt.Errorf("current installation key is unavailable")
	}
	block, err := aes.NewCipher(key)
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
		KeyVersion: s.currentVersion,
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

	key, ok := s.keys[blob.KeyVersion]
	if !ok || len(key) != 32 {
		return nil, fmt.Errorf("unsupported key version: %d", blob.KeyVersion)
	}

	block, err := aes.NewCipher(key)
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

// Rotate creates and durably installs a new key version. Existing keys stay
// in the keyring so callers can re-encrypt stored blobs transactionally before
// they are retired with RetireVersionsBefore.
func (s *SecretStore) Rotate() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dataDir == "" {
		return 0, fmt.Errorf("secret store has no key directory")
	}
	nextVersion := s.currentVersion + 1
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return 0, fmt.Errorf("generate rotated installation key: %w", err)
	}
	if err := writeKeyFile(versionedKeyFilePath(s.dataDir, nextVersion), key); err != nil {
		return 0, fmt.Errorf("write rotated installation key: %w", err)
	}
	s.keys[nextVersion] = key
	s.currentVersion = nextVersion
	return nextVersion, nil
}

// CurrentKeyVersion reports the version used for new encryption.
func (s *SecretStore) CurrentKeyVersion() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentVersion
}

// RetireVersionsBefore securely removes obsolete versioned key files after
// all blobs have been verified under the current key. Version 1's compatibility
// filename is removed only when it is explicitly retired.
func (s *SecretStore) RetireVersionsBefore(version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version <= initialKeyVersion {
		return nil
	}
	for keyVersion, key := range s.keys {
		if keyVersion >= version {
			continue
		}
		path := versionedKeyFilePath(s.dataDir, keyVersion)
		if keyVersion == initialKeyVersion {
			path = filepath.Join(s.dataDir, keyFileName)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove retired installation key version %d: %w", keyVersion, err)
		}
		for i := range key {
			key[i] = 0
		}
		delete(s.keys, keyVersion)
	}
	return nil
}

// Destroy securely wipes the key from memory.
func (s *SecretStore) Destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.keys {
		for i := range key {
			key[i] = 0
		}
	}
	s.keys = nil
	s.currentVersion = 0
}
