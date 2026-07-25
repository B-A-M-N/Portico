package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/paoloanzn/portico/internal/core"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// Store is the SQLite-backed persistence layer.
// Only the supervisor may open a Store.
type Store struct {
	db          *sql.DB
	mu          sync.RWMutex
	secretStore *SecretStore
	path        string
}

// migration defines a schema migration.
type migration struct {
	version int
	sql     string
	// onApply is an optional function to run custom migration logic.
	// If provided, it's called within the migration transaction instead of executing sql.
	onApply func(tx *sql.Tx) error
}

var migrations = []migration{
	{
		version: 1,
		sql: `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS connection_profiles (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    revision INTEGER NOT NULL,
    source_json BLOB NOT NULL,
    exposure_json BLOB NOT NULL,
    protection_json BLOB NOT NULL,
    provider_json BLOB NOT NULL,
    lifecycle_json BLOB NOT NULL,
    desired_state TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS connection_runtime (
    connection_id TEXT PRIMARY KEY,
    runtime_state TEXT NOT NULL,
    provider_id TEXT,
    public_address TEXT,
    private_address TEXT,
    connector_json BLOB,
    provider_runtime_json BLOB,
    endpoint_json BLOB,
    diagnostics_json BLOB,
    active_operation_id TEXT,
    error_json BLOB,
    last_observation TEXT,
    last_transition TEXT
);

CREATE TABLE IF NOT EXISTS provider_accounts (
    id TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL,
    label TEXT NOT NULL,
    credential_ref TEXT,
    metadata_json BLOB,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS provider_resources (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    connection_id TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    external_id TEXT NOT NULL,
    ownership TEXT NOT NULL,
    spec_hash TEXT,
    metadata_json BLOB,
    created_at TEXT NOT NULL,
    UNIQUE(provider_id, resource_type, external_id)
);

CREATE TABLE IF NOT EXISTS operation_plans (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL,
    profile_revision INTEGER NOT NULL,
    provider_id TEXT NOT NULL,
    intent TEXT NOT NULL,
    steps_json BLOB NOT NULL,
    fingerprint TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT,
    UNIQUE(connection_id, fingerprint)
);

CREATE TABLE IF NOT EXISTS operations (
    id TEXT PRIMARY KEY,
    plan_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    state TEXT NOT NULL,
    started_at TEXT NOT NULL,
    completed_at TEXT,
    error_json BLOB,
    FOREIGN KEY(plan_id) REFERENCES operation_plans(id)
);

CREATE TABLE IF NOT EXISTS operation_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id TEXT NOT NULL,
    step_id TEXT,
    event_type TEXT NOT NULL,
    stage TEXT NOT NULL,
    summary TEXT,
    error TEXT,
    sequence INTEGER NOT NULL,
    timestamp TEXT NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES operations(id)
);

CREATE TABLE IF NOT EXISTS findings (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL,
    segment TEXT NOT NULL,
    severity TEXT NOT NULL,
    summary TEXT NOT NULL,
    explanation TEXT,
    evidence_json BLOB,
    repair_options_json BLOB,
    resolved_at TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(connection_id) REFERENCES connection_profiles(id)
);

CREATE INDEX IF NOT EXISTS idx_operations_connection ON operations(connection_id);
CREATE TABLE IF NOT EXISTS operation_step_results (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id TEXT NOT NULL,
    step_id TEXT NOT NULL,
    step_kind TEXT NOT NULL,
    status TEXT NOT NULL, -- not_started, started, outcome_unknown, succeeded, failed, compensation_pending, compensated, compensation_failed
    provider_request_id TEXT,
    result_json BLOB,
    recovery_status TEXT NOT NULL DEFAULT 'normal', -- normal, recovery_required
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES operations(id)
);

CREATE INDEX IF NOT EXISTS idx_step_results_operation ON operation_step_results(operation_id);
CREATE INDEX IF NOT EXISTS idx_findings_connection ON findings(connection_id);

-- Traffic samples for telemetry
CREATE TABLE IF NOT EXISTS traffic_samples (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    connection_id TEXT NOT NULL,
    timestamp TEXT NOT NULL,
    origin_latency_ms REAL,
    public_latency_ms REAL,
    request_count INTEGER,
    error_count INTEGER,
    edge_location TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(connection_id) REFERENCES connection_profiles(id)
);

CREATE INDEX IF NOT EXISTS idx_traffic_samples_connection ON traffic_samples(connection_id);
CREATE INDEX IF NOT EXISTS idx_traffic_samples_timestamp ON traffic_samples(timestamp);

-- Event sequence for SSE replay
CREATE TABLE IF NOT EXISTS event_sequence (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    last_sequence INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);

INSERT OR IGNORE INTO event_sequence (id, last_sequence, updated_at) VALUES (1, 0, datetime('now'));

PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
`,
	},
	{
		version: 2,
		sql: `
-- Migration 2: Add missing indexes and tables, freeze migration 1.
CREATE INDEX IF NOT EXISTS idx_findings_unresolved ON findings(resolved_at) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_findings_connection_segment ON findings(connection_id, segment);
CREATE INDEX IF NOT EXISTS idx_traffic_samples_timestamp ON traffic_samples(timestamp);
CREATE INDEX IF NOT EXISTS idx_operations_started ON operations(started_at);
CREATE INDEX IF NOT EXISTS idx_operation_events_sequence ON operation_events(operation_id, sequence);

INSERT OR IGNORE INTO event_sequence (id, last_sequence, updated_at) VALUES (1, 0, datetime('now'));

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key TEXT PRIMARY KEY,
    operation_id TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES operations(id)
);

CREATE TABLE IF NOT EXISTS connector_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    connection_id TEXT NOT NULL,
    path TEXT NOT NULL,
    size INTEGER DEFAULT 0,
    rotated_at TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(connection_id) REFERENCES connection_profiles(id)
);

PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
`,
	},
	{
		version: 3,
		onApply: func(tx *sql.Tx) error {
			return addColumnIfNotExists(tx, "operation_plans", "warnings_json", "BLOB")
		},
	},
	{
		version: 4,
		onApply: func(tx *sql.Tx) error {
			return addColumnIfNotExists(tx, "operation_plans", "expected_json", "BLOB")
		},
	},
	{
		version: 5,
		onApply: func(tx *sql.Tx) error {
			return addColumnIfNotExists(tx, "operation_plans", "preconditions_json", "BLOB")
		},
	},
	{
		version: 6,
		onApply: func(tx *sql.Tx) error {
			return addColumnIfNotExists(tx, "operation_plans", "observed_fingerprint", "TEXT")
		},
	},
	{
		version: 7,
		sql: `
-- Migration 7: Separate resource lifecycle from ownership.
-- Add lifecycle column with default "present" for new rows.
-- Existing rows retain NULL lifecycle which maps to "present".
ALTER TABLE provider_resources ADD COLUMN lifecycle TEXT;
`,
	},
	{
		version: 8,
		onApply: func(tx *sql.Tx) error {
			// Create the new table with the correct uniqueness constraint.
			_, err := tx.Exec(`
				CREATE TABLE provider_resources_new (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					connection_id TEXT NOT NULL,
					provider_id TEXT NOT NULL,
					resource_type TEXT NOT NULL,
					external_id TEXT NOT NULL,
					ownership TEXT NOT NULL,
					spec_hash TEXT,
					metadata_json BLOB,
					lifecycle TEXT,
					created_at TEXT NOT NULL,
					UNIQUE(provider_id, resource_type, external_id)
				)`)
			if err != nil {
				return fmt.Errorf("create new table: %w", err)
			}

			// Detect duplicate resource keys before migration.
			rows, err := tx.Query(`
				SELECT provider_id, resource_type, external_id, COUNT(*) as cnt
				FROM provider_resources
				GROUP BY provider_id, resource_type, external_id
				HAVING cnt > 1`)
			if err != nil {
				return fmt.Errorf("detect duplicates: %w", err)
			}
			defer rows.Close()

			var hasDuplicates bool
			for rows.Next() {
				hasDuplicates = true
				break
			}
			rows.Close()

			if hasDuplicates {
				// Write conflicts to a migration_conflicts table for review.
				_, err := tx.Exec(`
					CREATE TABLE IF NOT EXISTS migration_conflicts (
						id INTEGER PRIMARY KEY AUTOINCREMENT,
						migration_version INTEGER NOT NULL,
						table_name TEXT NOT NULL,
						conflict_type TEXT NOT NULL,
						details_json BLOB NOT NULL,
						created_at TEXT NOT NULL
					)`)
				if err != nil {
					return fmt.Errorf("create migration_conflicts table: %w", err)
				}

				// Log duplicate resource keys.
				dupRows, err := tx.Query(`
					SELECT provider_id, resource_type, external_id, COUNT(*) as cnt,
					       GROUP_CONCAT(connection_id) as connections
					FROM provider_resources
					GROUP BY provider_id, resource_type, external_id
					HAVING cnt > 1`)
				if err != nil {
					return fmt.Errorf("query duplicates: %w", err)
				}
				defer dupRows.Close()

				for dupRows.Next() {
					var provID, resType, extID, connections string
					var cnt int
					if err := dupRows.Scan(&provID, &resType, &extID, &cnt, &connections); err != nil {
						return fmt.Errorf("scan duplicate: %w", err)
					}
					details := map[string]string{
						"provider_id":   provID,
						"resource_type": resType,
						"external_id":   extID,
						"connections":   connections,
						"count":         fmt.Sprintf("%d", cnt),
					}
					detailsJSON, _ := json.Marshal(details)
					_, err := tx.Exec(`
						INSERT INTO migration_conflicts (migration_version, table_name, conflict_type, details_json, created_at)
						VALUES (8, 'provider_resources', 'duplicate_resource_key', ?, ?)`,
						detailsJSON, time.Now().UTC().Format(time.RFC3339))
					if err != nil {
						return fmt.Errorf("log conflict: %w", err)
					}
				}
			}

			// Copy data, keeping only the first row per (provider_id, resource_type, external_id).
			// Duplicates are logged in migration_conflicts for manual review.
			_, err = tx.Exec(`
				INSERT OR IGNORE INTO provider_resources_new
				    (connection_id, provider_id, resource_type, external_id, ownership, spec_hash, metadata_json, lifecycle, created_at)
				SELECT connection_id, provider_id, resource_type, external_id, ownership, spec_hash, metadata_json, lifecycle, created_at
				FROM provider_resources
				ORDER BY id`)
			if err != nil {
				return fmt.Errorf("copy resources: %w", err)
			}

			// Duplicated keys had ambiguous ownership: demote the surviving
			// row to external so no connection gets automatic deletion
			// authority until the recorded conflict is resolved.
			if hasDuplicates {
				_, err = tx.Exec(`
					UPDATE provider_resources_new SET ownership = 'external'
					WHERE (provider_id, resource_type, external_id) IN (
						SELECT provider_id, resource_type, external_id
						FROM provider_resources
						GROUP BY provider_id, resource_type, external_id
						HAVING COUNT(*) > 1
					)`)
				if err != nil {
					return fmt.Errorf("demote duplicated ownership: %w", err)
				}
			}

			// Replace the table.
			if _, err := tx.Exec("DROP TABLE provider_resources"); err != nil {
				return fmt.Errorf("drop old table: %w", err)
			}
			if _, err := tx.Exec("ALTER TABLE provider_resources_new RENAME TO provider_resources"); err != nil {
				return fmt.Errorf("rename table: %w", err)
			}
			return nil
		},
	},
	{
		version: 9,
		sql: `
-- Migration 9: Add durable tunnel credential storage.
-- Stores encrypted tunnel tokens so permanent connectors can survive supervisor restart.
-- Tokens are encrypted with AES-GCM using a key derived from the machine ID.
CREATE TABLE IF NOT EXISTS tunnel_credentials (
    connection_id TEXT PRIMARY KEY,
    tunnel_id TEXT NOT NULL,
    token_encrypted BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
`,
	},
	{
		version: 10,
		sql: `
-- Migration 10: Add operation step results table for recovery.
-- Tracks step execution state so interrupted operations can be recovered.
CREATE TABLE IF NOT EXISTS operation_step_results (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id TEXT NOT NULL,
    step_id TEXT NOT NULL,
    step_kind TEXT NOT NULL,
    status TEXT NOT NULL,
    provider_request_id TEXT,
    result_json BLOB,
    recovery_status TEXT NOT NULL DEFAULT 'normal',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES operations(id)
);
CREATE INDEX IF NOT EXISTS idx_step_results_operation ON operation_step_results(operation_id);
`,
	},
	{
		version: 11,
		sql:     migrationEventsTable,
	},
}

// Open opens the SQLite database at path, runs migrations, and returns a Store.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store ping: %w", err)
	}

	s := &Store{db: db, path: path}

	// Initialize the secret store with a random installation key.
	// The key is stored alongside the database in a 0600-protected file.
	secretStore, err := NewSecretStore(filepath.Dir(path))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init secret store: %w", err)
	}
	s.secretStore = secretStore

	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, fmt.Errorf("store chmod: %w", err)
	}
	if err := s.runMigrations(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying database handle (for tests).
func (s *Store) DB() *sql.DB {
	return s.db
}

// addColumnIfNotExists adds a column to a table if it doesn't already exist.
// It uses PRAGMA table_info to check for column existence, making it idempotent.
func addColumnIfNotExists(tx *sql.Tx, table, column, colType string) error {
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()

	exists := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, column) {
			exists = true
			break
		}
	}
	if rows.Err() != nil {
		return rows.Err()
	}

	if exists {
		return nil // Column already exists, nothing to do
	}

	_, err = tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, colType))
	return err
}

// copyFile copies src to dst with 0600 permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// --------------- credential encryption ---------------

// deriveKey derives an AES-256 key from the machine ID.
// DEPRECATED: Use SecretStore for new credentials. This is kept for migration
// of existing credentials encrypted with the old scheme.
func deriveKey() []byte {
	// Use machine-id as the key source (Linux-specific).
	// Falls back to a default key if machine-id is unavailable.
	data, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		data, err = os.ReadFile("/var/lib/dbus/machine-id")
	}
	if err != nil || len(data) == 0 {
		// Fallback: use a derived key from the database path.
		// This is less secure but better than plaintext.
		data = []byte("portico-fallback-key-" + os.Getenv("HOME"))
	}
	hash := sha256.Sum256(data)
	return hash[:]
}

// encryptCredential encrypts a token using the SecretStore with context-bound AAD.
// The context binds the ciphertext to a specific connection/provider/tunnel.
// Encryption without an established secret store is refused: the deprecated
// machine-ID-derived key is predictable and must never protect new secrets.
func encryptCredential(store *SecretStore, plaintext, context string) ([]byte, error) {
	if store == nil {
		return nil, fmt.Errorf("secret store not initialized: refusing to encrypt credential with legacy key")
	}
	return store.Encrypt([]byte(plaintext), context)
}

// decryptCredential decrypts a token using the SecretStore.
// Legacy machine-ID-encrypted blobs (pre-SecretStore, raw AES-GCM bytes
// rather than EncryptedBlob JSON) are still readable for migration.
// A valid modern blob that fails to decrypt is an error — never a
// silent downgrade to the legacy key.
func decryptCredential(store *SecretStore, ciphertext []byte, context string) (string, error) {
	if isLegacyCredentialBlob(ciphertext) {
		return decryptCredentialLegacy(ciphertext)
	}
	if store == nil {
		return "", fmt.Errorf("secret store not initialized: cannot decrypt credential")
	}
	plaintext, err := store.Decrypt(ciphertext, context)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// isLegacyCredentialBlob reports whether the ciphertext predates the
// SecretStore format. Modern blobs are EncryptedBlob JSON documents.
func isLegacyCredentialBlob(ciphertext []byte) bool {
	var blob EncryptedBlob
	return json.Unmarshal(ciphertext, &blob) != nil || blob.Ciphertext == nil
}

// decryptCredentialLegacy decrypts using the deprecated machine-ID-derived key.
func decryptCredentialLegacy(ciphertext []byte) (string, error) {
	key := deriveKey()
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// --------------- migrations ---------------

func (s *Store) runMigrations() error {
	// Determine whether any migration is pending; if so, back up the
	// database file first so destructive rebuilds can be recovered from.
	pending := false
	for _, m := range migrations {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = ?", m.version).Scan(&count); err != nil {
			// schema_migrations may not exist yet (fresh database): no backup needed.
			pending = m.version > 1
			break
		}
		if count == 0 {
			pending = true
			break
		}
	}
	if pending && s.path != "" {
		if info, err := os.Stat(s.path); err == nil && info.Size() > 0 {
			backupPath := fmt.Sprintf("%s.backup-%s", s.path, time.Now().UTC().Format("20060102T150405Z"))
			if err := copyFile(s.path, backupPath); err != nil {
				return fmt.Errorf("pre-migration backup: %w", err)
			}
		}
	}

	for _, m := range migrations {
		// Check if already applied.
		var count int
		err := s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = ?", m.version).Scan(&count)
		if err != nil {
			// schema_migrations table might not exist yet (version 1); that's fine.
			if m.version > 1 {
				return fmt.Errorf("migration check version %d: %w", m.version, err)
			}
		}
		if count > 0 {
			continue
		}

		// Apply migration in a transaction.
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("migration begin %d: %w", m.version, err)
		}

		var applyErr error
		if m.onApply != nil {
			applyErr = m.onApply(tx)
		} else if m.sql != "" {
			_, applyErr = tx.Exec(m.sql)
		}

		if applyErr != nil {
			tx.Rollback()
			return fmt.Errorf("migration apply %d: %w", m.version, applyErr)
		}

		// Record migration.
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, ?)",
			m.version, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration record %d: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration commit %d: %w", m.version, err)
		}
	}
	return nil
}

// --------------- connection creation ---------------

// CreateConnection atomically persists a connection profile and its
// initial runtime in a single SQLite transaction. It validates that
// the connection does not already exist, then writes both records
// together so that a partial failure cannot leave the store in an
// inconsistent state.
func (s *Store) CreateConnection(
	ctx context.Context,
	profile *core.ConnectionProfile,
	rt *core.ConnectionRuntime,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Verify the connection does not already exist.
	var count int
	err = tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM connection_profiles WHERE id = ?",
		profile.ID).Scan(&count)
	if err != nil {
		return fmt.Errorf("check existing connection: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("connection already exists: %s", profile.ID)
	}

	// Persist profile.
	sourceJSON, err := json.Marshal(profile.Source)
	if err != nil {
		return fmt.Errorf("marshal source: %w", err)
	}
	exposureJSON, err := json.Marshal(profile.Exposure)
	if err != nil {
		return fmt.Errorf("marshal exposure: %w", err)
	}
	protectionJSON, err := json.Marshal(profile.Protection)
	if err != nil {
		return fmt.Errorf("marshal protection: %w", err)
	}
	providerJSON, err := json.Marshal(profile.Provider)
	if err != nil {
		return fmt.Errorf("marshal provider: %w", err)
	}
	lifecycleJSON, err := json.Marshal(profile.Lifecycle)
	if err != nil {
		return fmt.Errorf("marshal lifecycle: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO connection_profiles
			(id, name, revision, source_json, exposure_json, protection_json,
			 provider_json, lifecycle_json, desired_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		profile.ID, profile.Name, profile.Revision,
		sourceJSON, exposureJSON, protectionJSON,
		providerJSON, lifecycleJSON,
		string(profile.Desired),
		profile.CreatedAt.Format(time.RFC3339),
		profile.UpdatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("insert profile: %w", err)
	}

	// Persist runtime.
	connectorJSON, err := json.Marshal(rt.Connector)
	if err != nil {
		return fmt.Errorf("marshal connector: %w", err)
	}
	providerRuntimeJSON, err := json.Marshal(rt.Provider)
	if err != nil {
		return fmt.Errorf("marshal provider runtime: %w", err)
	}
	endpointJSON, err := json.Marshal(rt.Endpoint)
	if err != nil {
		return fmt.Errorf("marshal endpoint: %w", err)
	}
	diagnosticsJSON, err := json.Marshal(rt.Diagnostics)
	if err != nil {
		return fmt.Errorf("marshal diagnostics: %w", err)
	}
	var errorJSON []byte
	if rt.Error != nil {
		errorJSON, err = json.Marshal(rt.Error)
		if err != nil {
			return fmt.Errorf("marshal error: %w", err)
		}
	}
	var activeOpID *core.OperationID
	if rt.ActiveOperation != nil {
		activeOpID = rt.ActiveOperation
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO connection_runtime
			(connection_id, runtime_state, provider_id, public_address, private_address,
			 connector_json, provider_runtime_json, endpoint_json, diagnostics_json,
			 active_operation_id, error_json, last_observation, last_transition)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rt.ConnectionID, string(rt.State), rt.Provider.ProviderID,
		rt.Endpoint.PublicAddress, rt.Endpoint.PrivateAddress,
		connectorJSON, providerRuntimeJSON, endpointJSON, diagnosticsJSON,
		activeOpID, errorJSON,
		rt.LastObservedAt.Format(time.RFC3339),
		rt.LastTransition.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("insert runtime: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit connection creation: %w", err)
	}
	return nil
}

// --------------- profile CRUD ---------------

// SaveProfile persists a connection profile.
func (s *Store) SaveProfile(ctx context.Context, p *core.ConnectionProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sourceJSON, err := json.Marshal(p.Source)
	if err != nil {
		return fmt.Errorf("marshal source: %w", err)
	}
	exposureJSON, err := json.Marshal(p.Exposure)
	if err != nil {
		return fmt.Errorf("marshal exposure: %w", err)
	}
	protectionJSON, err := json.Marshal(p.Protection)
	if err != nil {
		return fmt.Errorf("marshal protection: %w", err)
	}
	providerJSON, err := json.Marshal(p.Provider)
	if err != nil {
		return fmt.Errorf("marshal provider: %w", err)
	}
	lifecycleJSON, err := json.Marshal(p.Lifecycle)
	if err != nil {
		return fmt.Errorf("marshal lifecycle: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO connection_profiles
			(id, name, revision, source_json, exposure_json, protection_json,
			 provider_json, lifecycle_json, desired_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, revision=excluded.revision,
			source_json=excluded.source_json, exposure_json=excluded.exposure_json,
			protection_json=excluded.protection_json, provider_json=excluded.provider_json,
			lifecycle_json=excluded.lifecycle_json, desired_state=excluded.desired_state,
			created_at=excluded.created_at, updated_at=excluded.updated_at`,
		p.ID, p.Name, p.Revision,
		sourceJSON, exposureJSON, protectionJSON,
		providerJSON, lifecycleJSON,
		string(p.Desired),
		p.CreatedAt.Format(time.RFC3339),
		p.UpdatedAt.Format(time.RFC3339),
	)
	return err
}

// LoadProfile loads a single profile by ID.
func (s *Store) LoadProfile(ctx context.Context, id core.ConnectionID) (*core.ConnectionProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadProfileLocked(ctx, id)
}

// UpdateProfile updates a profile using optimistic concurrency control.
// It atomically updates the profile only if the revision matches expectedRevision.
// Returns the committed profile with the new revision.
func (s *Store) UpdateProfile(ctx context.Context, profile *core.ConnectionProfile, expectedRevision uint64) (*core.ConnectionProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Serialize profile fields
	sourceJSON, err := json.Marshal(profile.Source)
	if err != nil {
		return nil, fmt.Errorf("marshal source: %w", err)
	}
	exposureJSON, err := json.Marshal(profile.Exposure)
	if err != nil {
		return nil, fmt.Errorf("marshal exposure: %w", err)
	}
	protectionJSON, err := json.Marshal(profile.Protection)
	if err != nil {
		return nil, fmt.Errorf("marshal protection: %w", err)
	}
	providerJSON, err := json.Marshal(profile.Provider)
	if err != nil {
		return nil, fmt.Errorf("marshal provider: %w", err)
	}
	lifecycleJSON, err := json.Marshal(profile.Lifecycle)
	if err != nil {
		return nil, fmt.Errorf("marshal lifecycle: %w", err)
	}

	newRevision := expectedRevision + 1
	updatedAt := time.Now().UTC().Format(time.RFC3339)

	// Optimistic update: only update if revision matches
	result, err := s.db.ExecContext(ctx, `
		UPDATE connection_profiles
		SET name = ?, revision = ?, source_json = ?, exposure_json = ?, protection_json = ?,
		    provider_json = ?, lifecycle_json = ?, desired_state = ?, updated_at = ?
		WHERE id = ? AND revision = ?`,
		profile.Name, newRevision, sourceJSON, exposureJSON, protectionJSON,
		providerJSON, lifecycleJSON, string(profile.Desired), updatedAt,
		profile.ID, expectedRevision,
	)
	if err != nil {
		return nil, fmt.Errorf("update profile: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("rows affected: %w", err)
	}

	if rowsAffected == 0 {
		// Revision mismatch or profile not found
		existing, err := s.loadProfileLocked(ctx, profile.ID)
		if err != nil {
			return nil, fmt.Errorf("profile not found or revision mismatch: %w", err)
		}
		return nil, fmt.Errorf("revision mismatch: expected %d, current %d", expectedRevision, existing.Revision)
	}

	// Load and return the committed profile
	return s.loadProfileLocked(ctx, profile.ID)
}

// loadProfileLocked loads a profile assuming the caller holds s.mu.
func (s *Store) loadProfileLocked(ctx context.Context, id core.ConnectionID) (*core.ConnectionProfile, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, revision, source_json, exposure_json, protection_json,
		       provider_json, lifecycle_json, desired_state, created_at, updated_at
		FROM connection_profiles WHERE id = ?`, id)

	var p core.ConnectionProfile
	var sourceJSON, exposureJSON, protectionJSON, providerJSON, lifecycleJSON []byte
	var desiredState, createdAtStr, updatedAtStr string

	err := row.Scan(
		&p.ID, &p.Name, &p.Revision,
		&sourceJSON, &exposureJSON, &protectionJSON,
		&providerJSON, &lifecycleJSON,
		&desiredState, &createdAtStr, &updatedAtStr,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("profile not found: %s", id)
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(sourceJSON, &p.Source); err != nil {
		return nil, fmt.Errorf("profile %s: unmarshal source: %w", id, err)
	}
	if err := json.Unmarshal(exposureJSON, &p.Exposure); err != nil {
		return nil, fmt.Errorf("profile %s: unmarshal exposure: %w", id, err)
	}
	if err := json.Unmarshal(protectionJSON, &p.Protection); err != nil {
		return nil, fmt.Errorf("profile %s: unmarshal protection: %w", id, err)
	}
	if err := json.Unmarshal(providerJSON, &p.Provider); err != nil {
		return nil, fmt.Errorf("profile %s: unmarshal provider: %w", id, err)
	}
	if err := json.Unmarshal(lifecycleJSON, &p.Lifecycle); err != nil {
		return nil, fmt.Errorf("profile %s: unmarshal lifecycle: %w", id, err)
	}
	p.Desired = core.DesiredConnectionState(desiredState)
	if createdAtStr != "" {
		p.CreatedAt, err = time.Parse(time.RFC3339, createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("profile %s: parse created_at: %w", id, err)
		}
	}
	if updatedAtStr != "" {
		p.UpdatedAt, err = time.Parse(time.RFC3339, updatedAtStr)
		if err != nil {
			return nil, fmt.Errorf("profile %s: parse updated_at: %w", id, err)
		}
	}

	return &p, nil
}

// ListProfiles returns all profiles.
func (s *Store) ListProfiles(ctx context.Context) ([]*core.ConnectionProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM connection_profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []core.ConnectionID
	for rows.Next() {
		var id core.ConnectionID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]*core.ConnectionProfile, 0, len(ids))
	for _, id := range ids {
		p, err := s.loadProfileLocked(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, nil
}

// DeleteProfile removes a profile by ID.
func (s *Store) DeleteProfile(ctx context.Context, id core.ConnectionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, "DELETE FROM connection_profiles WHERE id = ?", id)
	return err
}

// --------------- runtime CRUD ---------------

// SaveRuntime persists a connection runtime.
func (s *Store) SaveRuntime(ctx context.Context, rt *core.ConnectionRuntime) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	connectorJSON, err := json.Marshal(rt.Connector)
	if err != nil {
		return fmt.Errorf("marshal connector: %w", err)
	}
	providerJSON, err := json.Marshal(rt.Provider)
	if err != nil {
		return fmt.Errorf("marshal provider: %w", err)
	}
	endpointJSON, err := json.Marshal(rt.Endpoint)
	if err != nil {
		return fmt.Errorf("marshal endpoint: %w", err)
	}
	diagnosticsJSON, err := json.Marshal(rt.Diagnostics)
	if err != nil {
		return fmt.Errorf("marshal diagnostics: %w", err)
	}
	var errorJSON []byte
	if rt.Error != nil {
		errorJSON, err = json.Marshal(rt.Error)
		if err != nil {
			return fmt.Errorf("marshal error: %w", err)
		}
	}
	var activeOpID *core.OperationID
	if rt.ActiveOperation != nil {
		activeOpID = rt.ActiveOperation
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO connection_runtime
			(connection_id, runtime_state, provider_id, public_address, private_address,
			 connector_json, provider_runtime_json, endpoint_json, diagnostics_json,
			 active_operation_id, error_json, last_observation, last_transition)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(connection_id) DO UPDATE SET
			runtime_state=excluded.runtime_state, provider_id=excluded.provider_id,
			public_address=excluded.public_address, private_address=excluded.private_address,
			connector_json=excluded.connector_json, provider_runtime_json=excluded.provider_runtime_json,
			endpoint_json=excluded.endpoint_json, diagnostics_json=excluded.diagnostics_json,
			active_operation_id=excluded.active_operation_id, error_json=excluded.error_json,
			last_observation=excluded.last_observation, last_transition=excluded.last_transition`,
		rt.ConnectionID, string(rt.State), rt.Provider.ProviderID,
		rt.Endpoint.PublicAddress, rt.Endpoint.PrivateAddress,
		connectorJSON, providerJSON, endpointJSON, diagnosticsJSON,
		activeOpID, errorJSON,
		rt.LastObservedAt.Format(time.RFC3339),
		rt.LastTransition.Format(time.RFC3339),
	)
	return err
}

// LoadRuntime loads runtime by connection ID.
func (s *Store) LoadRuntime(ctx context.Context, id core.ConnectionID) (*core.ConnectionRuntime, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx, `
		SELECT connection_id, runtime_state, provider_id, public_address, private_address,
		       connector_json, provider_runtime_json, endpoint_json, diagnostics_json,
		       active_operation_id, error_json, last_observation, last_transition
		FROM connection_runtime WHERE connection_id = ?`, id)

	var rt core.ConnectionRuntime
	var runtimeState, lastObs, lastTrans string
	var provID, pubAddr, privAddr sql.NullString
	var connectorJSON, provRuntimeJSON, endpointJSON, diagJSON []byte
	var activeOpID sql.NullString
	var errorJSON []byte

	err := row.Scan(
		&rt.ConnectionID, &runtimeState, &provID, &pubAddr, &privAddr,
		&connectorJSON, &provRuntimeJSON, &endpointJSON, &diagJSON,
		&activeOpID, &errorJSON, &lastObs, &lastTrans,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("runtime not found: %s", id)
	}
	if err != nil {
		return nil, err
	}

	rt.State = core.RuntimeState(runtimeState)
	rt.LastObservedAt, err = time.Parse(time.RFC3339, lastObs)
	if err != nil {
		return nil, fmt.Errorf("parse last_observed_at: %w", err)
	}
	rt.LastTransition, err = time.Parse(time.RFC3339, lastTrans)
	if err != nil {
		return nil, fmt.Errorf("parse last_transition: %w", err)
	}

	if provID.Valid {
		rt.Provider.ProviderID = core.ProviderID(provID.String)
	}
	if pubAddr.Valid {
		rt.Endpoint.PublicAddress = pubAddr.String
	}
	if privAddr.Valid {
		rt.Endpoint.PrivateAddress = privAddr.String
	}
	if len(connectorJSON) > 0 {
		if err := json.Unmarshal(connectorJSON, &rt.Connector); err != nil {
			return nil, fmt.Errorf("unmarshal connector: %w", err)
		}
	}
	if len(provRuntimeJSON) > 0 {
		if err := json.Unmarshal(provRuntimeJSON, &rt.Provider); err != nil {
			return nil, fmt.Errorf("unmarshal provider: %w", err)
		}
	}
	if len(endpointJSON) > 0 {
		if err := json.Unmarshal(endpointJSON, &rt.Endpoint); err != nil {
			return nil, fmt.Errorf("unmarshal endpoint: %w", err)
		}
	}
	if len(diagJSON) > 0 {
		if err := json.Unmarshal(diagJSON, &rt.Diagnostics); err != nil {
			return nil, fmt.Errorf("unmarshal diagnostics: %w", err)
		}
	}
	if len(errorJSON) > 0 {
		if err := json.Unmarshal(errorJSON, &rt.Error); err != nil {
			return nil, fmt.Errorf("unmarshal error: %w", err)
		}
	}
	if activeOpID.Valid {
		opID := core.OperationID(activeOpID.String)
		rt.ActiveOperation = &opID
	}

	return &rt, nil
}

// DeleteRuntime removes a runtime record.
func (s *Store) DeleteRuntime(ctx context.Context, id core.ConnectionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, "DELETE FROM connection_runtime WHERE connection_id = ?", id)
	return err
}

// --------------- provider resources ---------------

// ResourceAssociationConflict is returned when a resource is already
// associated with a different connection.
type ResourceAssociationConflict struct {
	ProviderID   core.ProviderID
	ResourceType core.ResourceType
	ExternalID   string
	OwnerConnID  core.ConnectionID
}

func (e *ResourceAssociationConflict) Error() string {
	return fmt.Sprintf("resource %s/%s/%s already belongs to connection %s",
		e.ProviderID, e.ResourceType, e.ExternalID, e.OwnerConnID)
}

// SaveResource persists a provider resource.
// It rejects cross-connection conflicts: if a resource with the same
// provider_id, resource_type, and external_id exists under a different
// connection, it returns *ResourceAssociationConflict.
// For same-connection resources, it updates observation metadata only
// (not ownership - use AdoptResource for ownership changes).
func (s *Store) SaveResource(ctx context.Context, res *core.ProviderResource) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	metaJSON, err := json.Marshal(res.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)

	// Check if resource exists under a different connection.
	var existingConnectionID string
	err = s.db.QueryRowContext(ctx, `
		SELECT connection_id FROM provider_resources
		WHERE provider_id = ? AND resource_type = ? AND external_id = ?`,
		res.ProviderID, string(res.Type), res.ExternalID,
	).Scan(&existingConnectionID)

	if err == nil {
		// Resource exists - check if it's under the same connection.
		if existingConnectionID != string(res.ConnectionID) {
			return &ResourceAssociationConflict{
				ProviderID:   res.ProviderID,
				ResourceType: res.Type,
				ExternalID:   res.ExternalID,
				OwnerConnID:  core.ConnectionID(existingConnectionID),
			}
		}
		// Same connection - update observation metadata ONLY (not ownership).
		_, err = s.db.ExecContext(ctx, `
			UPDATE provider_resources
			SET spec_hash = ?, metadata_json = ?
			WHERE provider_id = ? AND resource_type = ? AND external_id = ? AND connection_id = ?`,
			res.SpecHash, metaJSON,
			res.ProviderID, string(res.Type), res.ExternalID, res.ConnectionID,
		)
		return err
	}

	if err != sql.ErrNoRows {
		// Unexpected error.
		return fmt.Errorf("query existing resource: %w", err)
	}

	// Resource does not exist - insert it.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO provider_resources
			(connection_id, provider_id, resource_type, external_id, ownership, spec_hash, metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		res.ConnectionID, res.ProviderID, string(res.Type), res.ExternalID,
		string(res.Ownership), res.SpecHash, metaJSON, now,
	)
	return err
}

// AdoptResource transfers ownership of a resource from one connection to another.
// This is an explicit, audited operation that requires the expected current owner.
func (s *Store) AdoptResource(ctx context.Context, providerID core.ProviderID, resourceType core.ResourceType,
	externalID string, expectedCurrentOwner core.ConnectionID, newOwner core.ConnectionID) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.ExecContext(ctx, `
		UPDATE provider_resources
		SET connection_id = ?, ownership = ?
		WHERE provider_id = ? AND resource_type = ? AND external_id = ? AND connection_id = ?`,
		newOwner, string(core.OwnershipAdopted),
		providerID, string(resourceType), externalID, expectedCurrentOwner,
	)
	if err != nil {
		return fmt.Errorf("adopt resource: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("resource %s/%s/%s not found under expected owner %s",
			providerID, resourceType, externalID, expectedCurrentOwner)
	}
	return nil
}

// ListResourcesByConnection returns resources for a connection.
func (s *Store) ListResourcesByConnection(ctx context.Context, connID core.ConnectionID) ([]core.ProviderResource, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, connection_id, provider_id, resource_type, external_id, ownership, lifecycle, spec_hash, metadata_json
		FROM provider_resources WHERE connection_id = ?`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var resources []core.ProviderResource
	for rows.Next() {
		var r core.ProviderResource
		var rid, connIDStr, provID, resType, extID, ownership string
		var lifecycle sql.NullString
		var specHash sql.NullString
		var metaJSON []byte

		if err := rows.Scan(&rid, &connIDStr, &provID, &resType, &extID, &ownership, &lifecycle, &specHash, &metaJSON); err != nil {
			return nil, err
		}
		r.ID = rid
		r.ConnectionID = core.ConnectionID(connIDStr)
		r.ProviderID = core.ProviderID(provID)
		r.Type = core.ResourceType(resType)
		r.ExternalID = extID
		r.Ownership = core.ResourceOwnership(ownership)
		if lifecycle.Valid {
			r.Lifecycle = core.ResourceLifecycle(lifecycle.String)
		}
		if specHash.Valid {
			r.SpecHash = specHash.String
		}
		if len(metaJSON) > 0 {
			if err := json.Unmarshal(metaJSON, &r.Metadata); err != nil {
				return nil, fmt.Errorf("unmarshal metadata: %w", err)
			}
		}
		resources = append(resources, r)
	}
	return resources, nil
}

// LoadResource looks up a resource by provider_id, resource_type, and external_id.
// It returns the resource if found, or sql.ErrNoRows if not found.
func (s *Store) LoadResource(ctx context.Context, providerID core.ProviderID, resourceType core.ResourceType, externalID string) (*core.ProviderResource, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var r core.ProviderResource
	var rid, connIDStr, provID, resType, extID, ownership string
	var lifecycle sql.NullString
	var specHash sql.NullString
	var metaJSON []byte

	err := s.db.QueryRowContext(ctx, `
		SELECT id, connection_id, provider_id, resource_type, external_id, ownership, lifecycle, spec_hash, metadata_json
		FROM provider_resources
		WHERE provider_id = ? AND resource_type = ? AND external_id = ?`,
		providerID, string(resourceType), externalID,
	).Scan(&rid, &connIDStr, &provID, &resType, &extID, &ownership, &lifecycle, &specHash, &metaJSON)

	if err != nil {
		return nil, err
	}

	r.ID = rid
	r.ConnectionID = core.ConnectionID(connIDStr)
	r.ProviderID = core.ProviderID(provID)
	r.Type = core.ResourceType(resType)
	r.ExternalID = extID
	r.Ownership = core.ResourceOwnership(ownership)
	if lifecycle.Valid {
		r.Lifecycle = core.ResourceLifecycle(lifecycle.String)
	}
	if specHash.Valid {
		r.SpecHash = specHash.String
	}
	if len(metaJSON) > 0 {
		if err := json.Unmarshal(metaJSON, &r.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}

	return &r, nil
}

// MarkResourceRemovalPending marks a provider resource as pending removal.
// This is called before attempting to delete a remote resource,
// so that interrupted deletions can be distinguished from not-yet-started ones.
func (s *Store) MarkResourceRemovalPending(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, resourceType core.ResourceType, externalID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		"UPDATE provider_resources SET lifecycle = ? WHERE connection_id = ? AND provider_id = ? AND resource_type = ? AND external_id = ?",
		string(core.LifecycleRemovalPending), connID, string(providerID), string(resourceType), externalID,
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("no managed resource found for %s %s %s (connection %s)", providerID, resourceType, externalID, connID)
	}
	return nil
}

// MarkResourceRemoved marks a provider resource as confirmed removed by the provider.
// This is called after a provider successfully deletes a remote resource,
// so that CommitConnectionDeletion can distinguish resolved resources
// from ones that still need cleanup.
func (s *Store) MarkResourceRemoved(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, resourceType core.ResourceType, externalID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		"UPDATE provider_resources SET lifecycle = ? WHERE connection_id = ? AND provider_id = ? AND resource_type = ? AND external_id = ?",
		string(core.LifecycleRemoved), connID, string(providerID), string(resourceType), externalID,
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("no managed resource found for %s %s %s (connection %s)", providerID, resourceType, externalID, connID)
	}
	return nil
}

// MarkResourceRemovalFailed marks a provider resource as failed removal,
// preserving it for retry or manual cleanup.
func (s *Store) MarkResourceRemovalFailed(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, resourceType core.ResourceType, externalID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		"UPDATE provider_resources SET lifecycle = ? WHERE connection_id = ? AND provider_id = ? AND resource_type = ? AND external_id = ?",
		string(core.LifecycleRemovalFailed), connID, string(providerID), string(resourceType), externalID,
	)
	return err
}

// MarkAssociatedPoliciesRemoved marks all Access policy resources associated
// with an Access application as removed. This is called after the parent
// application is successfully deleted, since policy deletion cascades from
// application deletion.
func (s *Store) MarkAssociatedPoliciesRemoved(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, appExternalID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		"UPDATE provider_resources SET lifecycle = ? WHERE connection_id = ? AND provider_id = ? AND resource_type = ? AND json_extract(metadata_json, '$.app_id') = ?",
		string(core.LifecycleRemoved), connID, string(providerID), string(core.ResourceAccessPolicy), appExternalID,
	)
	return err
}

// --------------- operation plans ---------------

// SaveOrGetPlan persists an operation plan and returns the canonical plan.
// If an equivalent plan already exists (by fingerprint), it returns the existing plan.
// This ensures plan ID canonicalization through persistence.
func (s *Store) SaveOrGetPlan(ctx context.Context, plan *core.OperationPlan) (*core.OperationPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Try to insert the new plan first.
	err := s.savePlanLocked(ctx, plan)
	if err == nil {
		// Insert succeeded - return the plan as-is.
		return plan, nil
	}

	// Check if it's a constraint violation (plan already exists with same fingerprint).
	if isConstraintError(err) {
		// Load the existing plan by fingerprint.
		existing, err := s.loadPlanByFingerprintLocked(ctx, plan.ConnectionID, plan.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf("load existing plan by fingerprint: %w", err)
		}
		return existing, nil
	}

	// Some other error occurred.
	return nil, fmt.Errorf("save plan: %w", err)
}

// isConstraintError checks if an error is a SQLite constraint violation.
func isConstraintError(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrConstraint
	}
	return false
}

// loadPlanByFingerprintLocked loads a plan by its uniqueness key
// (connection_id, fingerprint) assuming the caller holds s.mu.
func (s *Store) loadPlanByFingerprintLocked(ctx context.Context, connID core.ConnectionID, fingerprint string) (*core.OperationPlan, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, connection_id, profile_revision, provider_id, intent, steps_json, fingerprint,
		       created_at, expires_at, warnings_json, expected_json, preconditions_json, observed_fingerprint
		FROM operation_plans WHERE connection_id = ? AND fingerprint = ?`, connID, fingerprint)

	var p core.OperationPlan
	var stepsJSON, warningsJSON, expectedJSON, precondsJSON []byte
	var createdAtStr string
	var expiresAt *string
	var observedFP *string

	err := row.Scan(
		&p.ID, &p.ConnectionID, &p.ProfileRevision, &p.Provider,
		&p.Intent, &stepsJSON, &p.Fingerprint,
		&createdAtStr, &expiresAt, &warningsJSON, &expectedJSON, &precondsJSON, &observedFP,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("plan not found with fingerprint: %s", fingerprint)
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(stepsJSON, &p.Steps); err != nil {
		return nil, fmt.Errorf("unmarshal steps: %w", err)
	}
	if len(warningsJSON) > 0 {
		if err := json.Unmarshal(warningsJSON, &p.Warnings); err != nil {
			return nil, fmt.Errorf("unmarshal warnings: %w", err)
		}
	}
	if err := json.Unmarshal(expectedJSON, &p.Expected); err != nil {
		return nil, fmt.Errorf("unmarshal expected: %w", err)
	}
	if len(precondsJSON) > 0 {
		if err := json.Unmarshal(precondsJSON, &p.Preconditions); err != nil {
			return nil, fmt.Errorf("unmarshal preconditions: %w", err)
		}
	}

	p.CreatedAt, err = time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}

	if expiresAt != nil {
		p.ExpiresAt, err = time.Parse(time.RFC3339, *expiresAt)
		if err != nil {
			return nil, fmt.Errorf("parse expires_at: %w", err)
		}
	}

	if observedFP != nil {
		p.ObservedFingerprint = *observedFP
	}

	return &p, nil
}

// SavePlan persists an operation plan.
func (s *Store) SavePlan(ctx context.Context, plan *core.OperationPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savePlanLocked(ctx, plan)
}

// savePlanLocked persists an operation plan assuming the caller holds s.mu.
func (s *Store) savePlanLocked(ctx context.Context, plan *core.OperationPlan) error {
	stepsJSON, err := json.Marshal(plan.Steps)
	if err != nil {
		return fmt.Errorf("marshal steps: %w", err)
	}
	createdAt := plan.CreatedAt.Format(time.RFC3339)
	var expiresAt *string
	if !plan.ExpiresAt.IsZero() {
		exp := plan.ExpiresAt.Format(time.RFC3339)
		expiresAt = &exp
	}
	var warningsJSON, expectedJSON, precondsJSON []byte
	if len(plan.Warnings) > 0 {
		warningsJSON, err = json.Marshal(plan.Warnings)
		if err != nil {
			return fmt.Errorf("marshal warnings: %w", err)
		}
	}
	expectedJSON, err = json.Marshal(plan.Expected)
	if err != nil {
		return fmt.Errorf("marshal expected: %w", err)
	}
	if len(plan.Preconditions) > 0 {
		precondsJSON, err = json.Marshal(plan.Preconditions)
		if err != nil {
			return fmt.Errorf("marshal preconditions: %w", err)
		}
	}
	var observedFP *string
	if plan.ObservedFingerprint != "" {
		observedFP = &plan.ObservedFingerprint
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO operation_plans
			(id, connection_id, profile_revision, provider_id, intent, steps_json, fingerprint,
			 created_at, expires_at, warnings_json, expected_json, preconditions_json, observed_fingerprint)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		plan.ID, plan.ConnectionID, plan.ProfileRevision, plan.Provider,
		string(plan.Intent), stepsJSON, plan.Fingerprint,
		createdAt, expiresAt, warningsJSON, expectedJSON, precondsJSON, observedFP,
	)
	return err
}

// LoadPlan loads a plan by ID.
func (s *Store) LoadPlan(ctx context.Context, id core.PlanID) (*core.OperationPlan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadPlanLocked(ctx, id)
}

// loadPlanLocked loads a plan assuming the caller holds s.mu.
func (s *Store) loadPlanLocked(ctx context.Context, id core.PlanID) (*core.OperationPlan, error) {

	row := s.db.QueryRowContext(ctx, `
		SELECT id, connection_id, profile_revision, provider_id, intent, steps_json, fingerprint,
		       created_at, expires_at, warnings_json, expected_json, preconditions_json, observed_fingerprint
		FROM operation_plans WHERE id = ?`, id)

	var plan core.OperationPlan
	var stepsJSON, createdAt, expiresAt, warningsJSON, expectedJSON, precondsJSON []byte
	var intent string
	var observedFP sql.NullString
	if err := row.Scan(
		&plan.ID, &plan.ConnectionID, &plan.ProfileRevision, &plan.Provider,
		&intent, &stepsJSON, &plan.Fingerprint, &createdAt, &expiresAt,
		&warningsJSON, &expectedJSON, &precondsJSON, &observedFP,
	); err != nil {
		return nil, err
	}
	plan.Intent = core.OperationIntent(intent)
	if err := json.Unmarshal(stepsJSON, &plan.Steps); err != nil {
		return nil, err
	}
	if len(createdAt) > 0 {
		var err error
		plan.CreatedAt, err = time.Parse(time.RFC3339, string(createdAt))
		if err != nil {
			return nil, fmt.Errorf("parse created_at: %w", err)
		}
	}
	if len(expiresAt) > 0 {
		var err error
		plan.ExpiresAt, err = time.Parse(time.RFC3339, string(expiresAt))
		if err != nil {
			return nil, fmt.Errorf("parse expires_at: %w", err)
		}
	}
	if len(warningsJSON) > 0 {
		if err := json.Unmarshal(warningsJSON, &plan.Warnings); err != nil {
			return nil, fmt.Errorf("unmarshal warnings: %w", err)
		}
	}
	if len(expectedJSON) > 0 {
		if err := json.Unmarshal(expectedJSON, &plan.Expected); err != nil {
			return nil, fmt.Errorf("unmarshal expected: %w", err)
		}
	}
	if len(precondsJSON) > 0 {
		if err := json.Unmarshal(precondsJSON, &plan.Preconditions); err != nil {
			return nil, fmt.Errorf("unmarshal preconditions: %w", err)
		}
	}
	if observedFP.Valid {
		plan.ObservedFingerprint = observedFP.String
	}
	return &plan, nil
}

// ListIncompletePlans returns plans that haven't been applied yet.
// A plan is considered incomplete only if it has no associated operation
// with a completed or failed state (i.e., it hasn't been executed yet).
func (s *Store) ListIncompletePlans(ctx context.Context) ([]*core.OperationPlan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id FROM operation_plans p
		WHERE p.expires_at > ?
		AND NOT EXISTS (
			SELECT 1 FROM operations o
			WHERE o.plan_id = p.id
			AND o.state IN ('completed', 'failed')
		)
		ORDER BY p.created_at`, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []core.PlanID
	for rows.Next() {
		var id core.PlanID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]*core.OperationPlan, 0, len(ids))
	for _, id := range ids {
		p, err := s.loadPlanLocked(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, nil
}

// --------------- operations ---------------

// SaveOperation persists an operation record.
func (s *Store) SaveOperation(ctx context.Context, opID core.OperationID, planID core.PlanID, connID core.ConnectionID, state string, startedAt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO operations (id, plan_id, connection_id, state, started_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			state=excluded.state,
			plan_id=excluded.plan_id, connection_id=excluded.connection_id`,
		opID, planID, connID, state, startedAt,
	)
	return err
}

// CompleteOperation marks an operation as completed and clears the active operation reference.
func (s *Store) CompleteOperation(ctx context.Context, opID core.OperationID, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE operations SET state = ?, completed_at = ? WHERE id = ?`,
		state, time.Now().UTC().Format(time.RFC3339), opID,
	)
	if err != nil {
		return err
	}

	// Clear active operation reference in runtime
	_, err = s.db.ExecContext(ctx, `
		UPDATE connection_runtime SET active_operation_id = NULL
		WHERE active_operation_id = ?`,
		opID,
	)
	return err
}

// ClearActiveOperation clears the active operation reference for a connection.
func (s *Store) ClearActiveOperation(ctx context.Context, connID core.ConnectionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE connection_runtime SET active_operation_id = NULL
		WHERE connection_id = ?`,
		connID,
	)
	return err
}

// UpdateOperationState updates operation state.
func (s *Store) UpdateOperationState(ctx context.Context, opID core.OperationID, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE operations SET state = ?, completed_at = ? WHERE id = ?`,
		state, time.Now().UTC().Format(time.RFC3339), opID,
	)
	return err
}

// --------------- runtime transition commits ---------------

// CommitOpenSuccess persists the runtime state transition to "open"
// after a successful open operation. It clears the active operation,
// sets the connector status to running, and records the transition
// timestamp immediately so the state survives supervisor crashes.
// (SPEC P0 #13)
func (s *Store) CommitOpenSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID, startedAt time.Time) (*core.RuntimeCommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	committedAt := time.Now().UTC().Truncate(time.Second)
	now := committedAt.Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Mark the operation as completed.
	_, err = tx.ExecContext(ctx,
		"UPDATE operations SET state = 'completed', completed_at = ? WHERE id = ?",
		now, opID)
	if err != nil {
		return nil, fmt.Errorf("complete operation: %w", err)
	}

	// 2. Commit desired=open state atomically with the operation.
	_, err = tx.ExecContext(ctx,
		"UPDATE connection_profiles SET desired_state = ?, revision = revision + 1, updated_at = ? WHERE id = ?",
		string(core.DesiredOpen), now, connID)
	if err != nil {
		return nil, fmt.Errorf("update desired state: %w", err)
	}

	// 3. Update runtime to open state.
	_, err = tx.ExecContext(ctx, `
		UPDATE connection_runtime SET
			runtime_state = ?,
			active_operation_id = NULL,
			last_transition = ?,
			error_json = NULL
		WHERE connection_id = ?`,
		string(core.RuntimeOpen), now, connID)
	if err != nil {
		return nil, fmt.Errorf("update runtime: %w", err)
	}

	// 4. Update connector status to running.
	_, err = tx.ExecContext(ctx, `
		UPDATE connection_runtime SET
			connector_json = (
				SELECT json_insert(connector_json, '$.status', 'running')
				FROM connection_runtime WHERE connection_id = ?
			)
		WHERE connection_id = ? AND connector_json IS NOT NULL`,
		connID, connID)
	if err != nil {
		return nil, fmt.Errorf("update connector status: %w", err)
	}

	// 5. Read back the exact committed values.
	var revision uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT revision FROM connection_profiles WHERE id = ?", connID).Scan(&revision); err != nil {
		return nil, fmt.Errorf("read committed revision: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &core.RuntimeCommitResult{
		ProfileRevision: revision,
		DesiredState:    core.DesiredOpen,
		RuntimeState:    core.RuntimeOpen,
		LastTransition:  committedAt,
	}, nil
}

// CommitCloseSuccess persists the runtime state transition to "closed"
// after a successful close operation. It clears the active operation
// and records the transition timestamp immediately.
func (s *Store) CommitCloseSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) (*core.RuntimeCommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	committedAt := time.Now().UTC().Truncate(time.Second)
	now := committedAt.Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Mark the operation as completed.
	_, err = tx.ExecContext(ctx,
		"UPDATE operations SET state = 'completed', completed_at = ? WHERE id = ?",
		now, opID)
	if err != nil {
		return nil, fmt.Errorf("complete operation: %w", err)
	}

	// 2. Commit desired=closed state atomically with the operation.
	_, err = tx.ExecContext(ctx,
		"UPDATE connection_profiles SET desired_state = ?, revision = revision + 1, updated_at = ? WHERE id = ?",
		string(core.DesiredClosed), now, connID)
	if err != nil {
		return nil, fmt.Errorf("update desired state: %w", err)
	}

	// 3. Update runtime to closed state.
	_, err = tx.ExecContext(ctx, `
		UPDATE connection_runtime SET
			runtime_state = ?,
			active_operation_id = NULL,
			last_transition = ?,
			error_json = NULL
		WHERE connection_id = ?`,
		string(core.RuntimeClosed), now, connID)
	if err != nil {
		return nil, fmt.Errorf("update runtime: %w", err)
	}

	// 4. Update connector status to stopped.
	_, err = tx.ExecContext(ctx, `
		UPDATE connection_runtime SET
			connector_json = (
				SELECT json_insert(connector_json, '$.status', 'stopped')
				FROM connection_runtime WHERE connection_id = ?
			)
		WHERE connection_id = ? AND connector_json IS NOT NULL`,
		connID, connID)
	if err != nil {
		return nil, fmt.Errorf("update connector status: %w", err)
	}

	// 5. Read back the exact committed values.
	var revision uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT revision FROM connection_profiles WHERE id = ?", connID).Scan(&revision); err != nil {
		return nil, fmt.Errorf("read committed revision: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &core.RuntimeCommitResult{
		ProfileRevision: revision,
		DesiredState:    core.DesiredClosed,
		RuntimeState:    core.RuntimeClosed,
		LastTransition:  committedAt,
	}, nil
}

// CommitRepairSuccess persists the runtime state transition after a
// successful repair operation. The runtime transitions to "open" if
// the repair was for an open connection, or remains in its current
// open state. The active operation is cleared.
func (s *Store) CommitRepairSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) (*core.RuntimeCommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	committedAt := time.Now().UTC().Truncate(time.Second)
	now := committedAt.Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Mark the operation as completed.
	_, err = tx.ExecContext(ctx,
		"UPDATE operations SET state = 'completed', completed_at = ? WHERE id = ?",
		now, opID)
	if err != nil {
		return nil, fmt.Errorf("complete operation: %w", err)
	}

	// 2. Update runtime to open state (repair restores to open).
	// Repair does not change the desired profile configuration, so the
	// profile revision is NOT incremented.
	_, err = tx.ExecContext(ctx, `
		UPDATE connection_runtime SET
			runtime_state = ?,
			active_operation_id = NULL,
			last_transition = ?,
			error_json = NULL
		WHERE connection_id = ?`,
		string(core.RuntimeOpen), now, connID)
	if err != nil {
		return nil, fmt.Errorf("update runtime: %w", err)
	}

	// 3. Read back the committed profile values (unchanged by repair).
	var revision uint64
	var desired string
	if err := tx.QueryRowContext(ctx,
		"SELECT revision, desired_state FROM connection_profiles WHERE id = ?", connID).Scan(&revision, &desired); err != nil {
		return nil, fmt.Errorf("read committed profile: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &core.RuntimeCommitResult{
		ProfileRevision: revision,
		DesiredState:    core.DesiredConnectionState(desired),
		RuntimeState:    core.RuntimeOpen,
		LastTransition:  committedAt,
	}, nil
}

// CommitOperationFailure persists the runtime state transition to "error"
// after a failed operation. It records the error details and clears
// the active operation so the connection can be recovered.
func (s *Store) CommitOperationFailure(ctx context.Context, connID core.ConnectionID, opID core.OperationID, errMsg string, provider core.ProviderID, retryable bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Mark the operation as failed.
	_, err = tx.ExecContext(ctx,
		"UPDATE operations SET state = 'failed', completed_at = ? WHERE id = ?",
		now, opID)
	if err != nil {
		return fmt.Errorf("complete operation: %w", err)
	}

	// 2. Update runtime to error state with error details.
	errorJSON, err := json.Marshal(&core.PorticoError{
		Code:      "PTO-OP-PROVIDER-FAILED",
		Message:   errMsg,
		Retryable: retryable,
		Provider:  provider,
	})
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE connection_runtime SET
			runtime_state = ?,
			active_operation_id = NULL,
			last_transition = ?,
			error_json = ?
		WHERE connection_id = ?`,
		string(core.RuntimeError), now, errorJSON, connID)
	if err != nil {
		return fmt.Errorf("update runtime: %w", err)
	}

	return tx.Commit()
}

// CommitDeleteSuccess persists the complete deletion of a connection
// in a single transaction. It marks the operation as completed and
// removes all runtime, profile, resource, finding, and log records.
// Only managed resources block deletion; external/adopted resources are
// detached locally. Resources with removal_failed or orphaned lifecycle
// are preserved as cleanup evidence until explicitly resolved.
func (s *Store) CommitDeleteSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Mark the operation as completed.
	if opID != "" {
		_, err = tx.ExecContext(ctx,
			"UPDATE operations SET state = 'completed', completed_at = ? WHERE id = ?",
			now, opID)
		if err != nil {
			return fmt.Errorf("complete operation: %w", err)
		}
	}

	// 2. Verify no managed resources remain except confirmed-removed.
	// Managed resources in any other state (present, removal_pending, removal_failed, orphaned)
	// block deletion. External/adopted resources are detached without remote deletion.
	var unresolvedManaged int
	err = tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM provider_resources WHERE connection_id = ? AND ownership = ? AND (lifecycle IS NULL OR lifecycle != ?)",
		connID, string(core.OwnershipManaged), string(core.LifecycleRemoved)).Scan(&unresolvedManaged)
	if err != nil {
		return fmt.Errorf("check managed resources: %w", err)
	}
	if unresolvedManaged > 0 {
		return fmt.Errorf("cannot delete connection %s: %d managed resource(s) not confirmed removed", connID, unresolvedManaged)
	}

	// 2b. Detach external/adopted resources locally (they are not deleted remotely).
	_, err = tx.ExecContext(ctx,
		"DELETE FROM provider_resources WHERE connection_id = ? AND ownership != ?",
		connID, string(core.OwnershipManaged))
	if err != nil {
		return fmt.Errorf("detach external resources: %w", err)
	}

	// 3. Clear active operation reference in runtime.
	_, err = tx.ExecContext(ctx,
		"UPDATE connection_runtime SET active_operation_id = NULL WHERE connection_id = ?",
		connID)
	if err != nil {
		return fmt.Errorf("clear active operation: %w", err)
	}

	// 4. Delete findings.
	_, err = tx.ExecContext(ctx, "DELETE FROM findings WHERE connection_id = ?", connID)
	if err != nil {
		return fmt.Errorf("delete findings: %w", err)
	}

	// 4b. Delete dependent rows BEFORE the profile to avoid FK violations.
	_, err = tx.ExecContext(ctx, "DELETE FROM traffic_samples WHERE connection_id = ?", connID)
	if err != nil {
		return fmt.Errorf("delete traffic samples: %w", err)
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM connector_logs WHERE connection_id = ?", connID)
	if err != nil {
		return fmt.Errorf("delete connector logs: %w", err)
	}

	// 5. Delete only successfully removed or pending resources.
	// Preserve removal_failed and orphaned rows as cleanup evidence.
	_, err = tx.ExecContext(ctx,
		"DELETE FROM provider_resources WHERE connection_id = ? AND lifecycle IN (?, ?)",
		connID, string(core.LifecycleRemoved), string(core.LifecycleRemovalPending))
	if err != nil {
		return fmt.Errorf("delete provider resources: %w", err)
	}

	// 6. Delete tunnel credentials for this connection.
	_, err = tx.ExecContext(ctx, "DELETE FROM tunnel_credentials WHERE connection_id = ?", connID)
	if err != nil {
		return fmt.Errorf("delete tunnel credentials: %w", err)
	}

	// 7. Delete runtime.
	_, err = tx.ExecContext(ctx, "DELETE FROM connection_runtime WHERE connection_id = ?", connID)
	if err != nil {
		return fmt.Errorf("delete runtime: %w", err)
	}

	// 8. Delete profile.
	_, err = tx.ExecContext(ctx, "DELETE FROM connection_profiles WHERE id = ?", connID)
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}

	return tx.Commit()
}

// --------------- diagnostics ---------------

// SaveFinding persists a diagnostic finding.
func (s *Store) SaveFinding(ctx context.Context, f *core.DiagnosticFinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	evidenceJSON, err := json.Marshal(f.Evidence)
	if err != nil {
		return fmt.Errorf("marshal evidence: %w", err)
	}
	repairJSON, err := json.Marshal(f.RepairOptions)
	if err != nil {
		return fmt.Errorf("marshal repair options: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO findings
			(id, connection_id, segment, severity, summary, explanation, evidence_json, repair_options_json, resolved_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			connection_id=excluded.connection_id, segment=excluded.segment,
			severity=excluded.severity, summary=excluded.summary,
			explanation=excluded.explanation, evidence_json=excluded.evidence_json,
			repair_options_json=excluded.repair_options_json,
			resolved_at=excluded.resolved_at`,
		f.ID, f.ConnectionID, string(f.Segment), string(f.Severity),
		f.Summary, f.Explanation, evidenceJSON, repairJSON,
		nil, // resolved_at
		f.ObservedAt.Format(time.RFC3339),
	)
	return err
}

// ListUnresolvedFindings returns unresolved findings for a connection.
func (s *Store) ListUnresolvedFindings(ctx context.Context, connID core.ConnectionID) ([]core.DiagnosticFinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, connection_id, segment, severity, summary, explanation, evidence_json, repair_options_json, resolved_at, created_at
		FROM findings WHERE connection_id = ? AND resolved_at IS NULL`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var findings []core.DiagnosticFinding
	for rows.Next() {
		var f core.DiagnosticFinding
		var evidenceJSON, repairJSON []byte
		var resolvedAt sql.NullString
		var createdAt string
		var seg, sev string
		var connIDStr string

		if err := rows.Scan(&f.ID, &connIDStr, &seg, &sev, &f.Summary, &f.Explanation, &evidenceJSON, &repairJSON, &resolvedAt, &createdAt); err != nil {
			return nil, err
		}
		f.ConnectionID = core.ConnectionID(connIDStr)
		f.Segment = core.RouteSegmentID(seg)
		f.Severity = core.Severity(sev)
		if len(evidenceJSON) > 0 {
			if err := json.Unmarshal(evidenceJSON, &f.Evidence); err != nil {
				return nil, fmt.Errorf("unmarshal evidence: %w", err)
			}
		}
		if len(repairJSON) > 0 {
			if err := json.Unmarshal(repairJSON, &f.RepairOptions); err != nil {
				return nil, fmt.Errorf("unmarshal repair options: %w", err)
			}
		}
		if resolvedAt.Valid {
			t, err := time.Parse(time.RFC3339, resolvedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse resolved_at: %w", err)
			}
			f.ResolvedAt = &t
		}
		f.ObservedAt, err = time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse observed_at: %w", err)
		}
		findings = append(findings, f)
	}
	return findings, rows.Err()
}

// ListFindingsByConnection returns all findings for a connection.
func (s *Store) ListFindingsByConnection(ctx context.Context, connID core.ConnectionID) ([]core.DiagnosticFinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, connection_id, segment, severity, summary, explanation, evidence_json, repair_options_json, resolved_at, created_at
		FROM findings WHERE connection_id = ?`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var findings []core.DiagnosticFinding
	for rows.Next() {
		var f core.DiagnosticFinding
		var evidenceJSON, repairJSON []byte
		var resolvedAt sql.NullString
		var createdAt string
		var seg, sev string
		var connIDStr string

		if err := rows.Scan(&f.ID, &connIDStr, &seg, &sev, &f.Summary, &f.Explanation, &evidenceJSON, &repairJSON, &resolvedAt, &createdAt); err != nil {
			return nil, err
		}
		f.ConnectionID = core.ConnectionID(connIDStr)
		f.Segment = core.RouteSegmentID(seg)
		f.Severity = core.Severity(sev)
		if len(evidenceJSON) > 0 {
			if err := json.Unmarshal(evidenceJSON, &f.Evidence); err != nil {
				return nil, fmt.Errorf("unmarshal evidence: %w", err)
			}
		}
		if len(repairJSON) > 0 {
			if err := json.Unmarshal(repairJSON, &f.RepairOptions); err != nil {
				return nil, fmt.Errorf("unmarshal repair options: %w", err)
			}
		}
		if resolvedAt.Valid {
			t, err := time.Parse(time.RFC3339, resolvedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse resolved_at: %w", err)
			}
			f.ResolvedAt = &t
		}
		f.ObservedAt, err = time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse observed_at: %w", err)
		}
		findings = append(findings, f)
	}
	return findings, rows.Err()
}

// --------------- health ---------------

// Health returns the database version for health checks.
func (s *Store) Health(ctx context.Context) error {
	var version int
	return s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&version)
}

// DeleteResourcesByConnection deletes all provider resources for a connection.
func (s *Store) DeleteResourcesByConnection(ctx context.Context, connID core.ConnectionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, "DELETE FROM provider_resources WHERE connection_id = ?", connID)
	return err
}

// --------------- tunnel credential storage ---------------

// SaveTunnelCredential stores an encrypted tunnel token for a connection.
// The token is encrypted with AES-GCM using a key derived from the machine ID.
func (s *Store) SaveTunnelCredential(ctx context.Context, connID core.ConnectionID, tunnelID, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	context := string(connID) + ":" + tunnelID
	encrypted, err := encryptCredential(s.secretStore, token, context)
	if err != nil {
		return fmt.Errorf("encrypt credential: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tunnel_credentials (connection_id, tunnel_id, token_encrypted, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(connection_id) DO UPDATE SET
			tunnel_id=excluded.tunnel_id,
			token_encrypted=excluded.token_encrypted,
			updated_at=excluded.updated_at`,
		connID, tunnelID, encrypted, now, now,
	)
	return err
}

// LoadTunnelCredential loads and decrypts the tunnel token for a connection.
// Returns empty string and nil error if no credential is stored.
func (s *Store) LoadTunnelCredential(ctx context.Context, connID core.ConnectionID) (tunnelID, token string, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var encrypted []byte
	err = s.db.QueryRowContext(ctx,
		"SELECT tunnel_id, token_encrypted FROM tunnel_credentials WHERE connection_id = ?", connID,
	).Scan(&tunnelID, &encrypted)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}

	token, err = decryptCredential(s.secretStore, encrypted, string(connID)+":"+tunnelID)
	if err != nil {
		return "", "", fmt.Errorf("decrypt credential: %w", err)
	}
	return tunnelID, token, nil
}

// DeleteTunnelCredential removes the stored tunnel credential for a connection.
func (s *Store) DeleteTunnelCredential(ctx context.Context, connID core.ConnectionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, "DELETE FROM tunnel_credentials WHERE connection_id = ?", connID)
	return err
}

// CommitStepResult atomically persists the full result of executing a step:
// terminal step event, provider resources, credential mutations, and lifecycle
// changes all in a single transaction. (SPEC P0 atomic step commit)
func (s *Store) CommitStepResult(ctx context.Context, req core.StepCommitRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)

	// 1. Persist terminal step event.
	stage := string(core.StageSucceeded)
	eventType := string(core.EventOperationStepSucceeded)
	errorMsg := ""
	if !req.Result.Succeeded {
		stage = string(core.StageFailed)
		eventType = string(core.EventOperationStepFailed)
		if req.Result.Error != nil {
			errorMsg = req.Result.Error.Error()
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO operation_events
			(operation_id, step_id, event_type, stage, summary, error, sequence, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		req.OperationID, req.Result.StepID, eventType, stage, req.Step.Summary, errorMsg, 0, now)
	if err != nil {
		return fmt.Errorf("persist step event: %w", err)
	}

	// 2. Persist provider resources with the same association semantics as
	// SaveResource: cross-connection conflicts fail the transaction, and
	// same-connection updates never mutate ownership.
	for i := range req.Result.Resources {
		res := &req.Result.Resources[i]
		metaJSON, mErr := json.Marshal(res.Metadata)
		if mErr != nil {
			return fmt.Errorf("marshal metadata for %s: %w", res.ExternalID, mErr)
		}

		var existingConnectionID string
		qErr := tx.QueryRowContext(ctx, `
			SELECT connection_id FROM provider_resources
			WHERE provider_id = ? AND resource_type = ? AND external_id = ?`,
			res.ProviderID, string(res.Type), res.ExternalID,
		).Scan(&existingConnectionID)

		switch {
		case qErr == nil:
			if existingConnectionID != string(res.ConnectionID) {
				return &ResourceAssociationConflict{
					ProviderID:   res.ProviderID,
					ResourceType: res.Type,
					ExternalID:   res.ExternalID,
					OwnerConnID:  core.ConnectionID(existingConnectionID),
				}
			}
			_, err = tx.ExecContext(ctx, `
				UPDATE provider_resources
				SET spec_hash = ?, metadata_json = ?
				WHERE provider_id = ? AND resource_type = ? AND external_id = ? AND connection_id = ?`,
				res.SpecHash, metaJSON,
				res.ProviderID, string(res.Type), res.ExternalID, res.ConnectionID,
			)
			if err != nil {
				return fmt.Errorf("update resource %s: %w", res.ExternalID, err)
			}
		case qErr == sql.ErrNoRows:
			_, err = tx.ExecContext(ctx, `
				INSERT INTO provider_resources
					(connection_id, provider_id, resource_type, external_id, ownership, spec_hash, metadata_json, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				res.ConnectionID, res.ProviderID, string(res.Type), res.ExternalID,
				string(res.Ownership), res.SpecHash, metaJSON, now,
			)
			if err != nil {
				return fmt.Errorf("persist resource %s: %w", res.ExternalID, err)
			}
		default:
			return fmt.Errorf("query existing resource %s: %w", res.ExternalID, qErr)
		}
	}

	// 3. Persist credential mutations.
	for _, cred := range req.Result.CredentialMutations {
		credCtx := string(req.ConnectionID) + ":" + cred.TunnelID
		encrypted, encErr := encryptCredential(s.secretStore, cred.Token, credCtx)
		if encErr != nil {
			return fmt.Errorf("encrypt credential: %w", encErr)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO tunnel_credentials (connection_id, tunnel_id, token_encrypted, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(connection_id) DO UPDATE SET
				tunnel_id=excluded.tunnel_id,
				token_encrypted=excluded.token_encrypted,
				updated_at=excluded.updated_at`,
			req.ConnectionID, cred.TunnelID, encrypted, now, now)
		if err != nil {
			return fmt.Errorf("persist credential: %w", err)
		}
	}

	// 4. Apply lifecycle changes.
	for _, mark := range req.Lifecycle {
		_, err = tx.ExecContext(ctx,
			"UPDATE provider_resources SET lifecycle = ? WHERE connection_id = ? AND provider_id = ? AND resource_type = ? AND external_id = ?",
			string(mark.NewLifecycle), req.ConnectionID, string(mark.ProviderID), string(mark.ResourceType), mark.ExternalID)
		if err != nil {
			return fmt.Errorf("update lifecycle %s: %w", mark.ExternalID, err)
		}
	}

	// 5. Cascade policy removal for deleted Access applications.
	for _, appID := range req.RemovedAccessApps {
		_, err = tx.ExecContext(ctx,
			"UPDATE provider_resources SET lifecycle = ? WHERE connection_id = ? AND resource_type = ? AND json_extract(metadata_json, '$.app_id') = ?",
			string(core.LifecycleRemoved), req.ConnectionID, string(core.ResourceAccessPolicy), appID)
		if err != nil {
			return fmt.Errorf("cascade policy removal for app %s: %w", appID, err)
		}
	}

	return tx.Commit()
}

// ListRemainingManagedResources returns managed resources for a connection

// --------------- operation step results (for recovery) ---------------

// StepResultStatus describes the state of a step execution.
type StepResultStatus string

const (
	StepNotStarted          StepResultStatus = "not_started"
	StepStarted             StepResultStatus = "started"
	StepOutcomeUnknown      StepResultStatus = "outcome_unknown"
	StepSucceeded           StepResultStatus = "succeeded"
	StepFailed              StepResultStatus = "failed"
	StepCompensationPending StepResultStatus = "compensation_pending"
	StepCompensated         StepResultStatus = "compensated"
	StepCompensationFailed  StepResultStatus = "compensation_failed"
)

// RecordStepStart records that a step has started execution.
func (s *Store) RecordStepStart(ctx context.Context, opID core.OperationID, stepID string, stepKind core.StepKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO operation_step_results (operation_id, step_id, step_kind, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		opID, stepID, string(stepKind), string(StepStarted), now, now)
	return err
}

// RecordStepResult records the outcome of a step execution.
func (s *Store) RecordStepResult(ctx context.Context, opID core.OperationID, stepID string, status StepResultStatus, resultJSON []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE operation_step_results SET status = ?, result_json = ?, updated_at = ?
		WHERE operation_id = ? AND step_id = ?`,
		string(status), resultJSON, now, opID, stepID)
	return err
}

// MarkStepRecoveryRequired marks a step as needing recovery.
func (s *Store) MarkStepRecoveryRequired(ctx context.Context, opID core.OperationID, stepID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE operation_step_results SET recovery_status = 'recovery_required', updated_at = ?
		WHERE operation_id = ? AND step_id = ?`,
		now, opID, stepID)
	return err
}

// GetStepResults returns all step results for an operation.
func (s *Store) GetStepResults(ctx context.Context, opID core.OperationID) ([]core.StepExecutionResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `
		SELECT step_id, step_kind, status, provider_request_id, result_json, recovery_status
		FROM operation_step_results WHERE operation_id = ?`, opID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []core.StepExecutionResult
	for rows.Next() {
		var r core.StepExecutionResult
		var stepKind, status string
		var providerRequestID, recoveryStatus sql.NullString
		var resultJSON []byte
		if err := rows.Scan(&r.StepID, &stepKind, &status, &providerRequestID, &resultJSON, &recoveryStatus); err != nil {
			return nil, err
		}
		r.StepKind = core.StepKind(stepKind)
		r.Status = status
		r.ProviderRequestID = providerRequestID.String
		r.RecoveryRequired = recoveryStatus.String == "recovery_required"
		if len(resultJSON) > 0 {
			_ = json.Unmarshal(resultJSON, &r.Result)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ListIncompleteOperations returns operations that were interrupted.
func (s *Store) ListIncompleteOperations(ctx context.Context) ([]core.OperationID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT operation_id FROM operation_step_results
		WHERE status IN (?, ?, ?)`,
		string(StepStarted), string(StepOutcomeUnknown), string(StepCompensationPending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ops []core.OperationID
	for rows.Next() {
		var opID core.OperationID
		if err := rows.Scan(&opID); err != nil {
			return nil, err
		}
		ops = append(ops, opID)
	}
	return ops, rows.Err()
}

// IncompleteOperation describes an operation left in a non-terminal state,
// together with any recorded step results. Used by startup recovery.
type IncompleteOperation struct {
	ID           core.OperationID
	PlanID       core.PlanID
	ConnectionID core.ConnectionID
	State        string
	StartedAt    string
	StepResults  []core.StepExecutionResult
}

// ListNonTerminalOperations returns operations whose state is neither
// completed nor failed (e.g. "running", "pending"), together with their
// recorded step results from operation_step_results.
func (s *Store) ListNonTerminalOperations(ctx context.Context) ([]IncompleteOperation, error) {
	ops, err := s.listNonTerminalOperationRows(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ops {
		results, err := s.GetStepResults(ctx, ops[i].ID)
		if err != nil {
			return nil, err
		}
		ops[i].StepResults = results
	}
	return ops, nil
}

// listNonTerminalOperationRows reads the operations table under the read
// lock. Step results are attached by the caller outside the lock.
func (s *Store) listNonTerminalOperationRows(ctx context.Context) ([]IncompleteOperation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, plan_id, connection_id, state, started_at
		FROM operations
		WHERE state NOT IN ('completed', 'failed')
		ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ops []IncompleteOperation
	for rows.Next() {
		var op IncompleteOperation
		if err := rows.Scan(&op.ID, &op.PlanID, &op.ConnectionID, &op.State, &op.StartedAt); err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

// UpsertStepResultStatus records the status of a step result, inserting the
// row when no start was ever recorded for it (e.g. when startup recovery
// classifies an interrupted operation from the journal alone).
func (s *Store) UpsertStepResultStatus(ctx context.Context, opID core.OperationID, stepID string, stepKind core.StepKind, status StepResultStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE operation_step_results SET status = ?, updated_at = ?
		WHERE operation_id = ? AND step_id = ?`,
		string(status), now, opID, stepID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO operation_step_results (operation_id, step_id, step_kind, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			opID, stepID, string(stepKind), string(status), now, now)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OperationJournalEvent is one persisted operation journal entry from
// operation_events, used to reconstruct interrupted operations.
type OperationJournalEvent struct {
	StepID    string
	EventType string
	Stage     string
	Summary   string
	Error     string
	Timestamp string
}

// GetOperationEvents returns the journal events for an operation in
// insertion order.
func (s *Store) GetOperationEvents(ctx context.Context, opID core.OperationID) ([]OperationJournalEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `
		SELECT step_id, event_type, stage, summary, error, timestamp
		FROM operation_events WHERE operation_id = ? ORDER BY id`, opID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []OperationJournalEvent
	for rows.Next() {
		var e OperationJournalEvent
		var stepID, summary, errMsg sql.NullString
		if err := rows.Scan(&stepID, &e.EventType, &e.Stage, &summary, &errMsg, &e.Timestamp); err != nil {
			return nil, err
		}
		e.StepID = stepID.String
		e.Summary = summary.String
		e.Error = errMsg.String
		events = append(events, e)
	}
	return events, rows.Err()
}

// CommitOperationRecoveryRequired marks an interrupted operation as failed
// with recovery-required semantics: the operation transitions to 'failed'
// and the runtime records a PTO-OP-RECOVERY-REQUIRED error, so repair and
// reconcile workflows must resolve the uncertain provider state before
// further mutations. Not retryable: blindly re-running is unsafe.
func (s *Store) CommitOperationRecoveryRequired(ctx context.Context, connID core.ConnectionID, opID core.OperationID, errMsg string, provider core.ProviderID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		"UPDATE operations SET state = 'failed', completed_at = ? WHERE id = ?",
		now, opID)
	if err != nil {
		return fmt.Errorf("complete operation: %w", err)
	}

	errorJSON, err := json.Marshal(&core.PorticoError{
		Code:      "PTO-OP-RECOVERY-REQUIRED",
		Message:   errMsg,
		Retryable: false,
		Provider:  provider,
	})
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE connection_runtime SET
			runtime_state = ?,
			active_operation_id = NULL,
			last_transition = ?,
			error_json = ?
		WHERE connection_id = ?`,
		string(core.RuntimeError), now, errorJSON, connID)
	if err != nil {
		return fmt.Errorf("update runtime: %w", err)
	}

	return tx.Commit()
}

// AppendOperationEvent appends an operation event to the journal.
// Implements the controller.Journal interface.
func (s *Store) AppendOperationEvent(ctx context.Context, event core.Event, opID core.OperationID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var stepID, eventType, stage, summary, errorMsg string
	var timestamp string

	if opEvent, ok := event.Data.(core.OperationEvent); ok {
		stepID = opEvent.StepID
		eventType = string(opEvent.StepKind)
		stage = string(opEvent.Stage)
		summary = opEvent.Message
		errorMsg = opEvent.Error
		timestamp = opEvent.Timestamp.Format(time.RFC3339)
	} else {
		eventType = string(event.Type)
		timestamp = event.Timestamp.Format(time.RFC3339)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO operation_events
			(operation_id, step_id, event_type, stage, summary, error, sequence, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		opID, stepID, eventType, stage, summary, errorMsg, event.Sequence, timestamp,
	)
	return err
}

// AppendFinding appends a diagnostic finding to the journal.
// Implements the controller.Journal interface.
func (s *Store) AppendFinding(ctx context.Context, finding core.DiagnosticFinding) error {
	return s.SaveFinding(ctx, &finding)
}

// CommitConnectionDeletion transactionally commits a verified connection deletion.
// It verifies that all managed provider resources have been removed by the provider
// before deleting any records. Implements the controller.DeleteConnectionFinalizer interface.
func (s *Store) CommitConnectionDeletion(ctx context.Context, connID core.ConnectionID, operationID core.OperationID) error {
	return s.CommitDeleteSuccess(ctx, connID, operationID)
}

// ListProviderAccounts returns all provider accounts.
func (s *Store) ListProviderAccounts(ctx context.Context) ([]core.ProviderAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, provider_id, label, credential_ref, metadata_json, status, created_at, updated_at
		FROM provider_accounts ORDER BY provider_id, label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []core.ProviderAccount
	for rows.Next() {
		var a core.ProviderAccount
		var id, providerID, label, credRef, status string
		var metaJSON []byte
		var createdAt, updatedAt string

		if err := rows.Scan(&id, &providerID, &label, &credRef, &metaJSON, &status, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		a.ID = core.ProviderAccountID(id)
		a.Provider = core.ProviderID(providerID)
		a.Label = label
		a.CredentialRef = credRef
		if len(metaJSON) > 0 {
			if err := json.Unmarshal(metaJSON, &a.Metadata); err != nil {
				return nil, fmt.Errorf("unmarshal metadata: %w", err)
			}
		}
		a.Status = core.ProviderAccountStatus(status)
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// GetSequence returns the current event sequence number.
func (s *Store) GetSequence(ctx context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var seq int64
	err := s.db.QueryRowContext(ctx, "SELECT last_sequence FROM event_sequence WHERE id = 1").Scan(&seq)
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// SetSequence sets the event sequence number.
func (s *Store) SetSequence(ctx context.Context, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, "UPDATE event_sequence SET last_sequence = ?, updated_at = ? WHERE id = 1", seq, time.Now().UTC().Format(time.RFC3339))
	return err
}
