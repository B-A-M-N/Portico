package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/core"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// Store is the SQLite-backed persistence layer.
// Only the supervisor may open a Store.
type Store struct {
	db          *sql.DB
	mu          sync.RWMutex
	secretStore *SecretStore
	path        string
	now         func() time.Time
}

type clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

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
    connection_id TEXT NOT NULL,
    step_id TEXT NOT NULL,
    step_kind TEXT NOT NULL,
    status TEXT NOT NULL, -- not_started, started, outcome_unknown, succeeded, failed, compensation_pending, compensated, compensation_failed
    provider_request_id TEXT,
    result_json BLOB,
    recovery_status TEXT NOT NULL DEFAULT 'normal', -- normal, recovery_required
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES operations(id),
    UNIQUE(operation_id, step_id)
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
		onApply: migrateV1ToV2,
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
	{
		version: 12,
		onApply: func(tx *sql.Tx) error {
			// Bind credentials to their provider in the AAD context.
			return addColumnIfNotExists(tx, "tunnel_credentials", "provider_id", "TEXT NOT NULL DEFAULT ''")
		},
	},
	{
		version: 13,
		onApply: func(tx *sql.Tx) error {
			if err := addColumnIfNotExists(tx, "operation_step_results", "connection_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM operation_step_results
WHERE id NOT IN (SELECT MIN(id) FROM operation_step_results GROUP BY operation_id, step_id)`); err != nil {
				return err
			}
			_, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_step_results_operation_step
ON operation_step_results(operation_id, step_id)`)
			return err
		},
	},
	{
		version: 14,
		sql: `
CREATE TABLE IF NOT EXISTS resource_cleanup_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    external_id TEXT NOT NULL,
    cleanup_state TEXT NOT NULL,
    last_error TEXT,
    provider_request_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(provider_id, resource_type, external_id)
);
CREATE INDEX IF NOT EXISTS idx_cleanup_items_connection ON resource_cleanup_items(connection_id, cleanup_state);
		`,
	},
	{
		version: 15,
		onApply: func(tx *sql.Tx) error {
			// A connection can legitimately retain a credential for an old
			// tunnel while a replacement is being created or compensated. The
			// original one-row-per-connection schema silently overwrote that
			// evidence and made exact cleanup impossible.
			if _, err := tx.Exec(`
CREATE TABLE tunnel_credentials_new (
    connection_id TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    tunnel_id TEXT NOT NULL,
    token_encrypted BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (connection_id, provider_id, tunnel_id)
);
INSERT INTO tunnel_credentials_new
    (connection_id, provider_id, tunnel_id, token_encrypted, created_at, updated_at)
SELECT connection_id, COALESCE(provider_id, ''), tunnel_id, token_encrypted, created_at, updated_at
FROM tunnel_credentials;
DROP TABLE tunnel_credentials;
ALTER TABLE tunnel_credentials_new RENAME TO tunnel_credentials;
CREATE INDEX idx_tunnel_credentials_connection ON tunnel_credentials(connection_id, updated_at DESC);`); err != nil {
				return fmt.Errorf("rebuild tunnel credential identity: %w", err)
			}
			return nil
		},
	},
	{
		version: 16,
		sql: `
-- Provider account tokens are encrypted installation secrets addressed by an
-- opaque reference stored in provider_accounts. They are deliberately not
-- connection/tunnel credentials and must survive connection replacement.
CREATE TABLE IF NOT EXISTS provider_credentials (
    credential_ref TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL,
    secret_encrypted BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_provider ON provider_credentials(provider_id);
`,
	},
	{
		version: 17,
		onApply: func(tx *sql.Tx) error {
			return addColumnIfNotExists(tx, "connection_runtime", "runtime_revision", "INTEGER NOT NULL DEFAULT 0")
		},
	},
	{
		version: 18,
		onApply: migrateProfilesToVersionedSpec,
	},
	{
		version: 19,
		onApply: migrateProviderAccountsToProviderScopedKey,
	},
	{
		version: 20,
		onApply: migrateCleanupItemsRecordTheirAccount,
	},
	{
		version: 21,
		sql: `
-- Removing an account deleted its row and its credential and left no trace.
-- Every connection mutation leaves an operations row; this one left nothing, so
-- a support export could not answer "was this credential removed, and when".
--
-- There is no foreign key: the thing it refers to is what was deleted.
CREATE TABLE IF NOT EXISTS provider_account_removals (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id TEXT NOT NULL,
    account_id TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    credential_ref TEXT,
    credential_removed INTEGER NOT NULL DEFAULT 0,
    preview_fingerprint TEXT NOT NULL,
    removed_at TEXT NOT NULL
);
`,
	},
	{
		version: 22,
		onApply: func(tx *sql.Tx) error {
			if err := addColumnIfNotExists(tx, "operation_plans", "edit_payload_json", "BLOB"); err != nil {
				return err
			}
			return addColumnIfNotExists(tx, "operation_plans", "provider_account_id", "TEXT NOT NULL DEFAULT ''")
		},
	},
	{
		version: 23,
		onApply: func(tx *sql.Tx) error {
			if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS idempotency_keys (
    key TEXT PRIMARY KEY,
    operation_id TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES operations(id)
)`); err != nil {
				return err
			}
			return addColumnIfNotExists(tx, "idempotency_keys", "plan_id", "TEXT NOT NULL DEFAULT ''")
		},
	},
}

// migrateCleanupItemsRecordTheirAccount adds the account that can discharge a
// cleanup obligation.
//
// A cleanup item records which provider it belongs to but not which account.
// The account holds the credential, so removing an account could take away the
// only means of removing a resource Portico had created and not yet cleaned up,
// leaving it at the provider with nothing able to delete it.
//
// It tolerates the table being absent. A database upgraded from a schema old
// enough to predate it has nothing to alter, and refusing to start over a
// column on a table with no rows would be a worse outcome than adding it later.
func migrateCleanupItemsRecordTheirAccount(tx *sql.Tx) error {
	var present int
	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='resource_cleanup_items'",
	).Scan(&present); err != nil {
		return fmt.Errorf("check for resource_cleanup_items: %w", err)
	}
	if present == 0 {
		return nil
	}

	var hasColumn int
	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM pragma_table_info('resource_cleanup_items') WHERE name='provider_account_id'",
	).Scan(&hasColumn); err != nil {
		return fmt.Errorf("check for provider_account_id: %w", err)
	}
	if hasColumn > 0 {
		return nil
	}

	if _, err := tx.Exec(
		"ALTER TABLE resource_cleanup_items ADD COLUMN provider_account_id TEXT NOT NULL DEFAULT ''",
	); err != nil {
		return fmt.Errorf("add provider_account_id: %w", err)
	}
	return nil
}

// migrateProviderAccountsToProviderScopedKey rebuilds provider_accounts with a
// composite primary key of (provider_id, id).
//
// The table was keyed on id alone, and both upserts declared
// ON CONFLICT(id) DO UPDATE SET provider_id=excluded.provider_id, so any writer
// supplying a colliding account ID silently reassigned another provider's
// account — its owner, its credential reference and its status. Two providers
// also could not each hold an account named "default".
//
// Widening the key cannot produce a duplicate: the old schema declared
// id TEXT PRIMARY KEY, so every existing row already has a distinct id, and a
// set of tuples unique in one component is unique in any superset of them. The
// row-count check below proves that at runtime instead of relying on the
// argument, and turns any surprise into a refusal — the store has already
// written a pre-migration backup by the time this runs.
//
// Rows corrupted by the original defect are NOT repaired here and must not be.
// A takeover rewrote provider_id and credential_ref together, so the row is
// internally consistent and indistinguishable from a legitimate one; any
// heuristic would invent history. Recovery is from the .backup-* file.
func migrateProviderAccountsToProviderScopedKey(tx *sql.Tx) error {
	var before int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM provider_accounts`).Scan(&before); err != nil {
		return fmt.Errorf("count provider accounts before rebuild: %w", err)
	}

	// Column names, declared types and nullability are identical to the
	// original table. credential_ref stays nullable on purpose: making it NOT
	// NULL here would conflate this change with a separate read-path defect.
	if _, err := tx.Exec(`
		CREATE TABLE provider_accounts_new (
			id TEXT NOT NULL,
			provider_id TEXT NOT NULL,
			label TEXT NOT NULL,
			credential_ref TEXT,
			metadata_json BLOB,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (provider_id, id)
		)`); err != nil {
		return fmt.Errorf("create provider_accounts with provider-scoped key: %w", err)
	}

	// Explicit column list, no re-encoding: metadata_json moves as an opaque
	// blob and both timestamps move verbatim, so nothing is recomputed.
	if _, err := tx.Exec(`
		INSERT INTO provider_accounts_new
			(id, provider_id, label, credential_ref, metadata_json, status, created_at, updated_at)
		SELECT id, provider_id, label, credential_ref, metadata_json, status, created_at, updated_at
		FROM provider_accounts`); err != nil {
		return fmt.Errorf("copy provider accounts: %w", err)
	}

	var after int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM provider_accounts_new`).Scan(&after); err != nil {
		return fmt.Errorf("count provider accounts after rebuild: %w", err)
	}
	if before != after {
		// Refuse rather than lose. The transaction rolls back, the database
		// stays at the previous version, and the pre-migration backup stands.
		return fmt.Errorf(
			"provider account rebuild would lose rows: %d before, %d after; "+
				"the database is unchanged and a pre-migration backup was written", before, after)
	}

	if _, err := tx.Exec(`DROP TABLE provider_accounts`); err != nil {
		return fmt.Errorf("drop old provider_accounts: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE provider_accounts_new RENAME TO provider_accounts`); err != nil {
		return fmt.Errorf("rename provider_accounts: %w", err)
	}
	return nil
}

// migrateProfilesToVersionedSpec converts connection_profiles from the
// service-exposure-only column layout to a versioned tagged-spec layout.
//
// Before this migration the connection kind was not stored at all: it was
// inferred on load from the fact that the schema could only describe a service
// exposure. That made the kind a property of the code rather than of the data.
//
// The conversion is done in place with ADD COLUMN / backfill / DROP COLUMN
// rather than by rebuilding the table. connection_profiles is a parent table
// and operations, resources and findings hold immediate foreign keys to it;
// with foreign_keys=on a DROP TABLE performs an implicit DELETE FROM that
// violates those constraints, and PRAGMA foreign_keys cannot be suspended
// inside the migration transaction. In-place column changes never orphan the
// dependent rows.
func migrateProfilesToVersionedSpec(tx *sql.Tx) error {
	for _, col := range []struct{ name, decl string }{
		{"kind", "TEXT NOT NULL DEFAULT ''"},
		{"spec_version", "INTEGER NOT NULL DEFAULT 0"},
		{"spec_json", "BLOB NOT NULL DEFAULT ''"},
		{"driver_json", "BLOB NOT NULL DEFAULT ''"},
	} {
		if err := addColumnIfNotExists(tx, "connection_profiles", col.name, col.decl); err != nil {
			return fmt.Errorf("add %s: %w", col.name, err)
		}
	}

	rows, err := tx.Query(`SELECT id, source_json, exposure_json, protection_json, provider_json
		FROM connection_profiles`)
	if err != nil {
		return fmt.Errorf("read legacy profiles: %w", err)
	}

	type converted struct {
		id         string
		specJSON   []byte
		driverJSON []byte
	}
	var pending []converted

	for rows.Next() {
		var id string
		var sourceJSON, exposureJSON, protectionJSON, providerJSON []byte
		if err := rows.Scan(&id, &sourceJSON, &exposureJSON, &protectionJSON, &providerJSON); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy profile: %w", err)
		}

		// A row that cannot be decoded aborts the whole migration. Converting
		// the remainder would leave the database in a half-migrated state that
		// no later run could distinguish from a complete one.
		var spec core.ServiceExposureSpec
		if err := json.Unmarshal(sourceJSON, &spec.Source); err != nil {
			rows.Close()
			return fmt.Errorf("profile %s: decode legacy source: %w", id, err)
		}
		if err := json.Unmarshal(exposureJSON, &spec.Exposure); err != nil {
			rows.Close()
			return fmt.Errorf("profile %s: decode legacy exposure: %w", id, err)
		}
		if err := json.Unmarshal(protectionJSON, &spec.Protection); err != nil {
			rows.Close()
			return fmt.Errorf("profile %s: decode legacy protection: %w", id, err)
		}

		specJSON, err := json.Marshal(core.ConnectionSpec{ServiceExposure: &spec})
		if err != nil {
			rows.Close()
			return fmt.Errorf("profile %s: encode spec: %w", id, err)
		}

		// provider_json already holds a DriverSelection; it is carried across
		// verbatim rather than re-encoded, so no field is reinterpreted here.
		pending = append(pending, converted{id: id, specJSON: specJSON, driverJSON: providerJSON})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate legacy profiles: %w", err)
	}
	rows.Close()

	for _, c := range pending {
		if _, err := tx.Exec(`UPDATE connection_profiles
			SET kind = ?, spec_version = ?, spec_json = ?, driver_json = ?
			WHERE id = ?`,
			string(core.ConnectionServiceExposure), currentProfileSpecVersion,
			c.specJSON, c.driverJSON, c.id); err != nil {
			return fmt.Errorf("profile %s: write converted spec: %w", c.id, err)
		}
	}

	for _, col := range []string{"source_json", "exposure_json", "protection_json", "provider_json"} {
		if _, err := tx.Exec("ALTER TABLE connection_profiles DROP COLUMN " + col); err != nil {
			return fmt.Errorf("drop legacy column %s: %w", col, err)
		}
	}
	return nil
}

// Open opens the SQLite database at path, runs migrations, and returns a Store.
func Open(path string) (*Store, error) {
	return OpenWithClock(path, wallClock{})
}

// OpenWithClock opens a store with an injected clock. Production callers use
// Open; the seam keeps time-based retention deterministic in tests.
func OpenWithClock(path string, clk clock) (*Store, error) {
	if clk == nil {
		clk = wallClock{}
	}
	// SQLite creates the database file, but not its parent directory. Create
	// the Portico data directory before opening so a first-run XDG location is
	// usable without any external bootstrap step.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store ping: %w", err)
	}

	s := &Store{db: db, path: path, now: func() time.Time { return clk.Now().UTC() }}

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
		secretStore.Destroy()
		return nil, fmt.Errorf("store chmod: %w", err)
	}
	if err := s.runMigrations(); err != nil {
		db.Close()
		secretStore.Destroy()
		return nil, fmt.Errorf("store migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.secretStore != nil {
		s.secretStore.Destroy()
	}
	return err
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

// migrateV1ToV2 is deliberately a compatibility migration, not merely an
// index migration. The first released Portico schema used diagnostic_findings,
// plan_json, error_text, and seq-keyed operation events. Migration 1 was later
// expanded in source, but databases that already recorded version 1 must be
// upgraded from their original shape without losing their history.
func migrateV1ToV2(tx *sql.Tx) error {
	if err := migrateLegacyOperationTables(tx); err != nil {
		return err
	}
	if err := migrateLegacyIdempotencyKeys(tx); err != nil {
		return err
	}

	if _, err := tx.Exec(`
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
CREATE TABLE IF NOT EXISTS event_sequence (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    last_sequence INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS connector_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    connection_id TEXT NOT NULL,
    path TEXT NOT NULL,
    size INTEGER DEFAULT 0,
    rotated_at TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(connection_id) REFERENCES connection_profiles(id)
);`); err != nil {
		return fmt.Errorf("create v2 tables: %w", err)
	}

	legacyFindings, err := tableExists(tx, "diagnostic_findings")
	if err != nil {
		return err
	}
	if legacyFindings {
		if _, err := tx.Exec(`
INSERT OR IGNORE INTO findings
    (id, connection_id, segment, severity, summary, explanation, evidence_json, repair_options_json, resolved_at, created_at)
SELECT id, connection_id, segment, severity, summary, explanation, evidence_json, repair_options_json, resolved_at, observed_at
FROM diagnostic_findings`); err != nil {
			return fmt.Errorf("copy legacy findings: %w", err)
		}
	}

	// The original resource inventory had no creation timestamp. Unknown
	// provenance is represented by an empty timestamp rather than inventing a
	// current time, which would misstate ownership history.
	if err := addColumnIfNotExists(tx, "provider_resources", "created_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("add provider resource creation time: %w", err)
	}

	if _, err := tx.Exec(`
CREATE INDEX IF NOT EXISTS idx_findings_unresolved ON findings(resolved_at) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_findings_connection_segment ON findings(connection_id, segment);
CREATE INDEX IF NOT EXISTS idx_traffic_samples_timestamp ON traffic_samples(timestamp);
CREATE INDEX IF NOT EXISTS idx_operations_started ON operations(started_at);
CREATE INDEX IF NOT EXISTS idx_operation_events_sequence ON operation_events(operation_id, sequence);
INSERT OR IGNORE INTO event_sequence (id, last_sequence, updated_at) VALUES (1, 0, datetime('now'));`); err != nil {
		return fmt.Errorf("create v2 indexes: %w", err)
	}
	return nil
}

func migrateLegacyOperationTables(tx *sql.Tx) error {
	legacyPlans, err := tableHasColumn(tx, "operation_plans", "plan_json")
	if err != nil {
		return err
	}
	if !legacyPlans {
		return nil
	}

	// Build all replacement tables before replacing any source table. A
	// failure therefore rolls back cleanly with the legacy data untouched.
	if _, err := tx.Exec(`
CREATE TABLE operation_plans_v2 (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL,
    profile_revision INTEGER NOT NULL,
    provider_id TEXT NOT NULL,
    intent TEXT NOT NULL,
    steps_json BLOB NOT NULL,
    fingerprint TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT
);
CREATE TABLE operations_v2 (
    id TEXT PRIMARY KEY,
    plan_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    state TEXT NOT NULL,
    started_at TEXT NOT NULL,
    completed_at TEXT,
    error_json BLOB,
    FOREIGN KEY(plan_id) REFERENCES operation_plans_v2(id)
);
CREATE TABLE operation_events_v2 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id TEXT NOT NULL,
    step_id TEXT,
    event_type TEXT NOT NULL,
    stage TEXT NOT NULL,
    summary TEXT,
    error TEXT,
    sequence INTEGER NOT NULL,
    timestamp TEXT NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES operations_v2(id)
);
INSERT INTO operation_plans_v2
    (id, connection_id, profile_revision, provider_id, intent, steps_json, fingerprint, created_at, expires_at)
SELECT id, connection_id, profile_revision, provider_id, intent,
       CASE WHEN json_valid(plan_json) THEN COALESCE(json_extract(plan_json, '$.Steps'), '[]') ELSE '[]' END,
       fingerprint, created_at, expires_at
FROM operation_plans;
INSERT INTO operations_v2
    (id, plan_id, connection_id, state, started_at, completed_at, error_json)
SELECT id, plan_id, connection_id, state, started_at, completed_at,
       CASE WHEN error_text IS NULL OR error_text = '' THEN NULL
            ELSE json_object('Code', 'PTO-LEGACY-OPERATION', 'Message', error_text) END
FROM operations;
INSERT INTO operation_events_v2
    (operation_id, step_id, event_type, stage, summary, error, sequence, timestamp)
SELECT operation_id, NULL, event_type, COALESCE(stage, ''), NULL, NULL, seq, occurred_at
FROM operation_events WHERE operation_id IS NOT NULL;
DROP TABLE operation_events;
DROP TABLE operations;
DROP TABLE operation_plans;
ALTER TABLE operation_plans_v2 RENAME TO operation_plans;
ALTER TABLE operations_v2 RENAME TO operations;
ALTER TABLE operation_events_v2 RENAME TO operation_events;`); err != nil {
		return fmt.Errorf("rebuild legacy operation tables: %w", err)
	}
	return nil
}

func migrateLegacyIdempotencyKeys(tx *sql.Tx) error {
	legacy, err := tableHasColumn(tx, "idempotency_keys", "key_hash")
	if err != nil {
		return err
	}
	if !legacy {
		_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS idempotency_keys (
    key TEXT PRIMARY KEY,
    operation_id TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES operations(id)
)`)
		return err
	}
	_, err = tx.Exec(`
CREATE TABLE idempotency_keys_v2 (
    key TEXT PRIMARY KEY,
    operation_id TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES operations(id)
);
INSERT INTO idempotency_keys_v2 (key, operation_id, created_at)
SELECT key_hash, NULL, created_at FROM idempotency_keys;
DROP TABLE idempotency_keys;
ALTER TABLE idempotency_keys_v2 RENAME TO idempotency_keys;`)
	if err != nil {
		return fmt.Errorf("rebuild legacy idempotency keys: %w", err)
	}
	return nil
}

func tableExists(tx *sql.Tx, table string) (bool, error) {
	var name string
	err := tx.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func tableHasColumn(tx *sql.Tx, table, column string) (bool, error) {
	exists, err := tableExists(tx, table)
	if err != nil || !exists {
		return false, err
	}
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
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

// credentialAADSchemaVersion identifies the AAD context layout so it can
// evolve without silently accepting mismatched bindings.
const credentialAADSchemaVersion = 2

// credentialContext builds the v2 AAD context binding a credential to its
// connection, provider, tunnel, and AAD schema version.
func credentialContext(connID core.ConnectionID, providerID core.ProviderID, tunnelID string) string {
	return fmt.Sprintf("v2:%s:%s:%s:schema=%d", connID, providerID, tunnelID, credentialAADSchemaVersion)
}

func providerCredentialContext(providerID core.ProviderID, credentialRef string) string {
	return fmt.Sprintf("v2:provider:%s:%s:schema=%d", providerID, credentialRef, credentialAADSchemaVersion)
}

// legacyCredentialContext is the pre-v2 AAD context (connection:tunnel),
// kept readable for credentials encrypted before provider binding.
func legacyCredentialContext(connID core.ConnectionID, tunnelID string) string {
	return string(connID) + ":" + tunnelID
}

// encryptCredential encrypts a token using the SecretStore with context-bound AAD.
// The context binds the ciphertext to a specific connection/provider/tunnel.
// Encryption without an established secret store is refused: the deprecated
// machine-ID-derived key is predictable and must never protect new secrets.
func encryptCredential(store *SecretStore, plaintext []byte, context string) ([]byte, error) {
	if store == nil {
		return nil, fmt.Errorf("secret store not initialized: refusing to encrypt credential with legacy key")
	}
	return store.Encrypt(plaintext, context)
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
	// Refuse an on-disk schema written by a newer Portico. Proceeding could
	// silently discard columns or reinterpret state this binary does not know.
	var latestOnDisk sql.NullInt64
	if err := s.db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&latestOnDisk); err == nil && latestOnDisk.Valid {
		latestSupported := migrations[len(migrations)-1].version
		if latestOnDisk.Int64 > int64(latestSupported) {
			return fmt.Errorf("database schema version %d is newer than supported version %d", latestOnDisk.Int64, latestSupported)
		}
	}

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
			// WAL may contain committed pages not present in the main file. A
			// checkpoint is required before the file copy is a usable backup.
			var busy, logFrames, checkpointed int
			if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
				return fmt.Errorf("checkpoint before migration backup: %w", err)
			}
			if busy != 0 {
				return fmt.Errorf("checkpoint before migration backup is busy")
			}
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
	kind, specJSON, err := encodeProfileSpec(profile)
	if err != nil {
		return err
	}
	driverJSON, err := json.Marshal(profile.Driver)
	if err != nil {
		return fmt.Errorf("marshal driver: %w", err)
	}
	lifecycleJSON, err := json.Marshal(profile.Lifecycle)
	if err != nil {
		return fmt.Errorf("marshal lifecycle: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO connection_profiles
			(id, name, revision, kind, spec_version, spec_json,
			 driver_json, lifecycle_json, desired_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		profile.ID, profile.Name, profile.Revision,
		kind, currentProfileSpecVersion, specJSON,
		driverJSON, lifecycleJSON,
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
	if _, err := appendEventTx(ctx, tx, "", profile.ID, string(core.EventConnectionCreated), string(core.StageSucceeded), profile.UpdatedAt, map[string]string{
		"connection_id": string(profile.ID),
		"name":          profile.Name,
	}); err != nil {
		return fmt.Errorf("persist connection creation event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit connection creation: %w", err)
	}
	return nil
}

// --------------- profile CRUD ---------------

// currentProfileSpecVersion is the encoding version this binary writes into
// connection_profiles.spec_version. It versions the shape of spec_json
// independently of the table schema, so a spec encoding change does not require
// a table migration and can be refused precisely.
const currentProfileSpecVersion = 1

// ErrUnsupportedSpecVersion reports a stored connection spec written by a newer
// Portico. Decoding it under this binary's assumptions could silently
// reinterpret durable state, so it is refused instead.
var ErrUnsupportedSpecVersion = errors.New("unsupported connection spec version")

// encodeProfileSpec marshals a profile's spec union for storage and derives the
// kind to store alongside it.
func encodeProfileSpec(p *core.ConnectionProfile) (kind string, specJSON []byte, err error) {
	if p == nil {
		return "", nil, fmt.Errorf("profile is nil")
	}
	armKind, err := core.SpecArmKind(p.Spec)
	if err != nil {
		return "", nil, fmt.Errorf("connection %s: %w", p.ID, err)
	}
	// A declared kind that disagrees with the populated arm would make the
	// stored kind column a lie, which is exactly what this schema exists to
	// prevent.
	if p.Kind != "" && p.Kind != armKind {
		return "", nil, fmt.Errorf(
			"connection %s: declared kind %q does not match populated spec arm %q", p.ID, p.Kind, armKind)
	}
	specJSON, err = json.Marshal(p.Spec)
	if err != nil {
		return "", nil, fmt.Errorf("connection %s: marshal spec: %w", p.ID, err)
	}
	return string(armKind), specJSON, nil
}

// decodeProfileSpec restores a profile's spec union from storage, refusing
// unknown encodings and rows whose stored kind disagrees with the stored arm.
func decodeProfileSpec(id core.ConnectionID, kind string, specVersion int, specJSON []byte) (core.ConnectionSpec, core.ConnectionKind, error) {
	var spec core.ConnectionSpec
	if specVersion > currentProfileSpecVersion {
		return spec, "", fmt.Errorf("profile %s: %w: stored version %d, supported %d",
			id, ErrUnsupportedSpecVersion, specVersion, currentProfileSpecVersion)
	}
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return spec, "", fmt.Errorf("profile %s: unmarshal spec: %w", id, err)
	}
	armKind, err := core.SpecArmKind(spec)
	if err != nil {
		return spec, "", fmt.Errorf("profile %s: %w", id, err)
	}
	if core.ConnectionKind(kind) != armKind {
		return spec, "", fmt.Errorf("profile %s: stored kind %q does not match stored spec arm %q", id, kind, armKind)
	}
	return spec, armKind, nil
}

// SaveProfile persists a connection profile.
func (s *Store) SaveProfile(ctx context.Context, p *core.ConnectionProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	kind, specJSON, err := encodeProfileSpec(p)
	if err != nil {
		return err
	}
	driverJSON, err := json.Marshal(p.Driver)
	if err != nil {
		return fmt.Errorf("marshal driver: %w", err)
	}
	lifecycleJSON, err := json.Marshal(p.Lifecycle)
	if err != nil {
		return fmt.Errorf("marshal lifecycle: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO connection_profiles
			(id, name, revision, kind, spec_version, spec_json,
			 driver_json, lifecycle_json, desired_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, revision=excluded.revision,
			kind=excluded.kind, spec_version=excluded.spec_version,
			spec_json=excluded.spec_json, driver_json=excluded.driver_json,
			lifecycle_json=excluded.lifecycle_json, desired_state=excluded.desired_state,
			created_at=excluded.created_at, updated_at=excluded.updated_at`,
		p.ID, p.Name, p.Revision,
		kind, currentProfileSpecVersion, specJSON,
		driverJSON, lifecycleJSON,
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
	kind, specJSON, err := encodeProfileSpec(profile)
	if err != nil {
		return nil, err
	}
	driverJSON, err := json.Marshal(profile.Driver)
	if err != nil {
		return nil, fmt.Errorf("marshal driver: %w", err)
	}
	lifecycleJSON, err := json.Marshal(profile.Lifecycle)
	if err != nil {
		return nil, fmt.Errorf("marshal lifecycle: %w", err)
	}

	newRevision := expectedRevision + 1
	updatedAtTime := time.Now().UTC()
	updatedAt := updatedAtTime.Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin profile update: %w", err)
	}
	defer tx.Rollback()

	// Optimistic update: only update if revision matches
	result, err := tx.ExecContext(ctx, `
		UPDATE connection_profiles
		SET name = ?, revision = ?, kind = ?, spec_version = ?, spec_json = ?,
		    driver_json = ?, lifecycle_json = ?, desired_state = ?, updated_at = ?
		WHERE id = ? AND revision = ?`,
		profile.Name, newRevision, kind, currentProfileSpecVersion, specJSON,
		driverJSON, lifecycleJSON, string(profile.Desired), updatedAt,
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
		tx.Rollback()
		existing, err := s.loadProfileLocked(ctx, profile.ID)
		if err != nil {
			return nil, fmt.Errorf("profile not found or revision mismatch: %w", err)
		}
		return nil, fmt.Errorf("revision mismatch: expected %d, current %d", expectedRevision, existing.Revision)
	}
	if _, err := appendEventTx(ctx, tx, "", profile.ID, string(core.EventConnectionUpdated), string(core.StageSucceeded), updatedAtTime, map[string]any{
		"connection_id": string(profile.ID),
		"revision":      newRevision,
	}); err != nil {
		return nil, fmt.Errorf("persist profile update event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit profile update: %w", err)
	}

	committed := profile.DeepCopy()
	committed.Revision = newRevision
	committed.UpdatedAt = updatedAtTime
	return committed, nil
}

// loadProfileLocked loads a profile assuming the caller holds s.mu.
func (s *Store) loadProfileLocked(ctx context.Context, id core.ConnectionID) (*core.ConnectionProfile, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, revision, kind, spec_version, spec_json,
		       driver_json, lifecycle_json, desired_state, created_at, updated_at
		FROM connection_profiles WHERE id = ?`, id)

	var p core.ConnectionProfile
	var specJSON, driverJSON, lifecycleJSON []byte
	var kind, desiredState, createdAtStr, updatedAtStr string
	var specVersion int

	err := row.Scan(
		&p.ID, &p.Name, &p.Revision,
		&kind, &specVersion, &specJSON,
		&driverJSON, &lifecycleJSON,
		&desiredState, &createdAtStr, &updatedAtStr,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("profile not found: %s", id)
	}
	if err != nil {
		return nil, err
	}

	spec, armKind, err := decodeProfileSpec(id, kind, specVersion, specJSON)
	if err != nil {
		return nil, err
	}
	p.Spec = spec
	p.Kind = armKind
	if err := json.Unmarshal(driverJSON, &p.Driver); err != nil {
		return nil, fmt.Errorf("profile %s: unmarshal driver: %w", id, err)
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

// Snapshot is a consistent read of the state projections needed by IPC. The
// event high-water comes from the same SQLite read transaction as profiles and
// runtimes, so a reconnecting client can safely use LastEvent-ID=LastSeq.
type Snapshot struct {
	Profiles []*core.ConnectionProfile
	Runtimes []*core.ConnectionRuntime
	LastSeq  int64
}

// ReadSnapshot returns profiles, runtimes, and the durable event high-water
// from one read transaction. Do not replace this with separate list/query
// calls: a state commit between those calls can make a returned LastSeq lie
// about the state represented by the snapshot.
func (s *Store) ReadSnapshot(ctx context.Context) (*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin snapshot: %w", err)
	}
	defer tx.Rollback()

	profiles, err := readSnapshotProfiles(ctx, tx)
	if err != nil {
		return nil, err
	}
	runtimes, err := readSnapshotRuntimes(ctx, tx)
	if err != nil {
		return nil, err
	}
	var lastSeq int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq), 0) FROM events").Scan(&lastSeq); err != nil {
		return nil, fmt.Errorf("snapshot event high-water: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit snapshot: %w", err)
	}
	return &Snapshot{Profiles: profiles, Runtimes: runtimes, LastSeq: lastSeq}, nil
}

func readSnapshotProfiles(ctx context.Context, tx *sql.Tx) ([]*core.ConnectionProfile, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, revision, kind, spec_version, spec_json,
		       driver_json, lifecycle_json, desired_state, created_at, updated_at
		FROM connection_profiles ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("snapshot profiles: %w", err)
	}
	defer rows.Close()
	var profiles []*core.ConnectionProfile
	for rows.Next() {
		var p core.ConnectionProfile
		var specJSON, driverJSON, lifecycleJSON []byte
		var kind, desiredState, createdAt, updatedAt string
		var specVersion int
		if err := rows.Scan(&p.ID, &p.Name, &p.Revision, &kind, &specVersion, &specJSON,
			&driverJSON, &lifecycleJSON, &desiredState, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan snapshot profile: %w", err)
		}
		spec, armKind, err := decodeProfileSpec(p.ID, kind, specVersion, specJSON)
		if err != nil {
			return nil, fmt.Errorf("snapshot %w", err)
		}
		p.Spec = spec
		p.Kind = armKind
		if err := json.Unmarshal(driverJSON, &p.Driver); err != nil {
			return nil, fmt.Errorf("snapshot profile %s driver: %w", p.ID, err)
		}
		if err := json.Unmarshal(lifecycleJSON, &p.Lifecycle); err != nil {
			return nil, fmt.Errorf("snapshot profile %s lifecycle: %w", p.ID, err)
		}
		p.Desired = core.DesiredConnectionState(desiredState)
		if p.CreatedAt, err = time.Parse(time.RFC3339, createdAt); err != nil {
			return nil, fmt.Errorf("snapshot profile %s created_at: %w", p.ID, err)
		}
		if p.UpdatedAt, err = time.Parse(time.RFC3339, updatedAt); err != nil {
			return nil, fmt.Errorf("snapshot profile %s updated_at: %w", p.ID, err)
		}
		profiles = append(profiles, &p)
	}
	return profiles, rows.Err()
}

func readSnapshotRuntimes(ctx context.Context, tx *sql.Tx) ([]*core.ConnectionRuntime, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT connection_id, runtime_state, provider_id, public_address, private_address,
		       connector_json, provider_runtime_json, endpoint_json, diagnostics_json,
		       active_operation_id, error_json, last_observation, last_transition,
		       runtime_revision
		FROM connection_runtime`)
	if err != nil {
		return nil, fmt.Errorf("snapshot runtimes: %w", err)
	}
	defer rows.Close()
	var runtimes []*core.ConnectionRuntime
	for rows.Next() {
		var rt core.ConnectionRuntime
		var runtimeState, lastObs, lastTrans string
		var providerID, publicAddress, privateAddress sql.NullString
		var connectorJSON, providerJSON, endpointJSON, diagnosticsJSON, errorJSON []byte
		var activeOperation sql.NullString
		var runtimeRev int64
		if err := rows.Scan(&rt.ConnectionID, &runtimeState, &providerID, &publicAddress, &privateAddress,
			&connectorJSON, &providerJSON, &endpointJSON, &diagnosticsJSON, &activeOperation, &errorJSON, &lastObs, &lastTrans,
			&runtimeRev); err != nil {
			return nil, fmt.Errorf("scan snapshot runtime: %w", err)
		}
		rt.State = core.RuntimeState(runtimeState)
		rt.RuntimeRevision = uint64(runtimeRev)
		if rt.LastObservedAt, err = time.Parse(time.RFC3339, lastObs); err != nil {
			return nil, fmt.Errorf("snapshot runtime %s last_observation: %w", rt.ConnectionID, err)
		}
		if rt.LastTransition, err = time.Parse(time.RFC3339, lastTrans); err != nil {
			return nil, fmt.Errorf("snapshot runtime %s last_transition: %w", rt.ConnectionID, err)
		}
		if providerID.Valid {
			rt.Provider.ProviderID = core.ProviderID(providerID.String)
		}
		if publicAddress.Valid {
			rt.Endpoint.PublicAddress = publicAddress.String
		}
		if privateAddress.Valid {
			rt.Endpoint.PrivateAddress = privateAddress.String
		}
		if len(connectorJSON) > 0 {
			if err := json.Unmarshal(connectorJSON, &rt.Connector); err != nil {
				return nil, fmt.Errorf("snapshot runtime %s connector: %w", rt.ConnectionID, err)
			}
		}
		if len(providerJSON) > 0 {
			if err := json.Unmarshal(providerJSON, &rt.Provider); err != nil {
				return nil, fmt.Errorf("snapshot runtime %s provider: %w", rt.ConnectionID, err)
			}
		}
		if len(endpointJSON) > 0 {
			if err := json.Unmarshal(endpointJSON, &rt.Endpoint); err != nil {
				return nil, fmt.Errorf("snapshot runtime %s endpoint: %w", rt.ConnectionID, err)
			}
		}
		if len(diagnosticsJSON) > 0 {
			if err := json.Unmarshal(diagnosticsJSON, &rt.Diagnostics); err != nil {
				return nil, fmt.Errorf("snapshot runtime %s diagnostics: %w", rt.ConnectionID, err)
			}
		}
		if len(errorJSON) > 0 {
			if err := json.Unmarshal(errorJSON, &rt.Error); err != nil {
				return nil, fmt.Errorf("snapshot runtime %s error: %w", rt.ConnectionID, err)
			}
		}
		if activeOperation.Valid {
			opID := core.OperationID(activeOperation.String)
			rt.ActiveOperation = &opID
		}
		runtimes = append(runtimes, &rt)
	}
	return runtimes, rows.Err()
}

// DeleteProfile removes a profile by ID.
func (s *Store) DeleteProfile(ctx context.Context, id core.ConnectionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, "DELETE FROM connection_profiles WHERE id = ?", id)
	return err
}

// --------------- runtime CRUD ---------------

type runtimeExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// upsertRuntime writes a complete runtime projection through either the
// database or an existing transaction. Keeping serialization here prevents
// state/event transactions from drifting from ordinary runtime persistence.
func upsertRuntime(ctx context.Context, execer runtimeExecer, rt *core.ConnectionRuntime) error {
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

	_, err = execer.ExecContext(ctx, `
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

// SaveRuntime persists a connection runtime.
func (s *Store) SaveRuntime(ctx context.Context, rt *core.ConnectionRuntime) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return upsertRuntime(ctx, s.db, rt)
}

// ErrRuntimeRevisionMismatch is returned by SaveRuntimeCAS when another writer
// persisted between the caller's load and this CAS write.
var ErrRuntimeRevisionMismatch = errors.New("runtime: revision mismatch")

// SaveRuntimeCAS persists a connection runtime with optimistic concurrency
// control. The stored runtime_revision must match the persisted value; on
// success the persisted revision is bumped by 1 and rt.RuntimeRevision is
// updated in place to match.
//
// Returns ErrRuntimeRevisionMismatch when another writer persisted between
// the caller's load and this call. Callers must reload and re-apply.
func (s *Store) SaveRuntimeCAS(ctx context.Context, rt *core.ConnectionRuntime) error {
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

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO connection_runtime
			(connection_id, runtime_state, provider_id, public_address, private_address,
			 connector_json, provider_runtime_json, endpoint_json, diagnostics_json,
			 active_operation_id, error_json, last_observation, last_transition)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(connection_id) DO UPDATE SET
			runtime_revision = runtime_revision + 1,
			runtime_state = excluded.runtime_state,
			provider_id = excluded.provider_id,
			public_address = excluded.public_address,
			private_address = excluded.private_address,
			connector_json = excluded.connector_json,
			provider_runtime_json = excluded.provider_runtime_json,
			endpoint_json = excluded.endpoint_json,
			diagnostics_json = excluded.diagnostics_json,
			active_operation_id = excluded.active_operation_id,
			error_json = excluded.error_json,
			last_observation = excluded.last_observation,
			last_transition = excluded.last_transition
		WHERE runtime_revision = ?`,
		rt.ConnectionID, string(rt.State), rt.Provider.ProviderID,
		rt.Endpoint.PublicAddress, rt.Endpoint.PrivateAddress,
		connectorJSON, providerJSON, endpointJSON, diagnosticsJSON,
		activeOpID, errorJSON,
		rt.LastObservedAt.Format(time.RFC3339),
		rt.LastTransition.Format(time.RFC3339),
		rt.RuntimeRevision,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRuntimeRevisionMismatch
	}
	rt.RuntimeRevision++
	return nil
}

// CommitConnectorRuntimeEvent updates connector-derived runtime state and
// appends its normalized durable event in one transaction. SSE callers must
// dispatch only after this returns successfully.
func (s *Store) CommitConnectorRuntimeEvent(ctx context.Context, rt *core.ConnectionRuntime, eventType, stage string, occurredAt time.Time, payload any) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := upsertRuntime(ctx, tx, rt); err != nil {
		return 0, err
	}
	seq, err := appendEventTx(ctx, tx, "", rt.ConnectionID, eventType, stage, occurredAt, payload)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// LoadRuntime loads runtime by connection ID.
func (s *Store) LoadRuntime(ctx context.Context, id core.ConnectionID) (*core.ConnectionRuntime, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx, `
		SELECT connection_id, runtime_state, provider_id, public_address, private_address,
		       connector_json, provider_runtime_json, endpoint_json, diagnostics_json,
		       active_operation_id, error_json, last_observation, last_transition,
		       runtime_revision
		FROM connection_runtime WHERE connection_id = ?`, id)

	var rt core.ConnectionRuntime
	var runtimeState, lastObs, lastTrans string
	var provID, pubAddr, privAddr sql.NullString
	var connectorJSON, provRuntimeJSON, endpointJSON, diagJSON []byte
	var activeOpID sql.NullString
	var errorJSON []byte

	var runtimeRev int64
	err := row.Scan(
		&rt.ConnectionID, &runtimeState, &provID, &pubAddr, &privAddr,
		&connectorJSON, &provRuntimeJSON, &endpointJSON, &diagJSON,
		&activeOpID, &errorJSON, &lastObs, &lastTrans,
		&runtimeRev,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("runtime not found: %s", id)
	}
	if err != nil {
		return nil, err
	}

	rt.State = core.RuntimeState(runtimeState)
	rt.RuntimeRevision = uint64(runtimeRev)
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
			(connection_id, provider_id, resource_type, external_id, ownership, lifecycle, spec_hash, metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		res.ConnectionID, res.ProviderID, string(res.Type), res.ExternalID,
		string(res.Ownership), string(core.LifecyclePresent), res.SpecHash, metaJSON, now,
	)
	return err
}

// CreateManagedResource persists a resource created by Portico. Ownership
// defaults to managed when unset. Cross-connection conflicts return
// *ResourceAssociationConflict. An existing managed row is an idempotent
// re-create and is explicitly returned to present lifecycle; plain
// observation refreshes deliberately do not make that transition.
func (s *Store) CreateManagedResource(ctx context.Context, res *core.ProviderResource) error {
	cp := *res
	if cp.Ownership == "" {
		cp.Ownership = core.OwnershipManaged
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	metaJSON, err := json.Marshal(cp.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)

	var existingConnectionID string
	err = s.db.QueryRowContext(ctx, `
		SELECT connection_id FROM provider_resources
		WHERE provider_id = ? AND resource_type = ? AND external_id = ?`,
		cp.ProviderID, string(cp.Type), cp.ExternalID,
	).Scan(&existingConnectionID)
	if err == nil {
		if existingConnectionID != string(cp.ConnectionID) {
			return &ResourceAssociationConflict{
				ProviderID: cp.ProviderID, ResourceType: cp.Type, ExternalID: cp.ExternalID,
				OwnerConnID: core.ConnectionID(existingConnectionID),
			}
		}
		_, err = s.db.ExecContext(ctx, `
			UPDATE provider_resources
			SET spec_hash = ?, metadata_json = ?,
				lifecycle = CASE WHEN ownership = ? THEN ? ELSE lifecycle END
			WHERE provider_id = ? AND resource_type = ? AND external_id = ? AND connection_id = ?`,
			cp.SpecHash, metaJSON, string(core.OwnershipManaged), string(core.LifecyclePresent),
			cp.ProviderID, string(cp.Type), cp.ExternalID, cp.ConnectionID,
		)
		return err
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("query existing resource: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO provider_resources
			(connection_id, provider_id, resource_type, external_id, ownership, lifecycle, spec_hash, metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cp.ConnectionID, cp.ProviderID, string(cp.Type), cp.ExternalID,
		string(cp.Ownership), string(core.LifecyclePresent), cp.SpecHash, metaJSON, now,
	)
	return err
}

// UpdateResourceObservation refreshes observation metadata (spec hash and
// metadata only) for a resource already tracked under the given connection.
// It can never insert a row, change ownership, or change association.
func (s *Store) UpdateResourceObservation(ctx context.Context, res *core.ProviderResource) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	metaJSON, err := json.Marshal(res.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	var existingConnectionID string
	err = s.db.QueryRowContext(ctx, `
		SELECT connection_id FROM provider_resources
		WHERE provider_id = ? AND resource_type = ? AND external_id = ?`,
		res.ProviderID, string(res.Type), res.ExternalID,
	).Scan(&existingConnectionID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("resource not tracked: %s/%s/%s", res.ProviderID, res.Type, res.ExternalID)
	}
	if err != nil {
		return fmt.Errorf("query existing resource: %w", err)
	}
	if existingConnectionID != string(res.ConnectionID) {
		return &ResourceAssociationConflict{
			ProviderID:   res.ProviderID,
			ResourceType: res.Type,
			ExternalID:   res.ExternalID,
			OwnerConnID:  core.ConnectionID(existingConnectionID),
		}
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE provider_resources
		SET spec_hash = ?, metadata_json = ?
		WHERE provider_id = ? AND resource_type = ? AND external_id = ? AND connection_id = ?`,
		res.SpecHash, metaJSON,
		res.ProviderID, string(res.Type), res.ExternalID, res.ConnectionID,
	)
	return err
}

// TransferResourceAssociation moves a resource to a new connection with an
// expected-current-owner check, preserving the ownership class. Use
// AdoptResource when the ownership class should become adopted.
func (s *Store) TransferResourceAssociation(ctx context.Context, providerID core.ProviderID, resourceType core.ResourceType,
	externalID string, expectedCurrentOwner core.ConnectionID, newOwner core.ConnectionID) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.ExecContext(ctx, `
		UPDATE provider_resources
		SET connection_id = ?
		WHERE provider_id = ? AND resource_type = ? AND external_id = ? AND connection_id = ?`,
		newOwner, providerID, string(resourceType), externalID, expectedCurrentOwner,
	)
	if err != nil {
		return fmt.Errorf("transfer resource association: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("transfer failed: resource %s/%s/%s not owned by %s",
			providerID, resourceType, externalID, expectedCurrentOwner)
	}
	return nil
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

// MarkResourceExternallyRemoved records that an exact provider lookup
// authoritatively returned not found. This is distinct from a Portico-issued
// deletion, whose lifecycle is LifecycleRemoved.
func (s *Store) MarkResourceExternallyRemoved(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, resourceType core.ResourceType, externalID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		"UPDATE provider_resources SET lifecycle = ? WHERE connection_id = ? AND provider_id = ? AND resource_type = ? AND external_id = ?",
		string(core.LifecycleExternallyRemoved), connID, string(providerID), string(resourceType), externalID,
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("no exact resource found for external removal: %s %s %s (connection %s)", providerID, resourceType, externalID, connID)
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
		       created_at, expires_at, warnings_json, expected_json, preconditions_json, observed_fingerprint,
		       edit_payload_json, provider_account_id
		FROM operation_plans WHERE connection_id = ? AND fingerprint = ?`, connID, fingerprint)

	var p core.OperationPlan
	var stepsJSON, warningsJSON, expectedJSON, precondsJSON []byte
	var createdAtStr string
	var expiresAt *string
	var observedFP *string
	var editPayloadJSON []byte

	err := row.Scan(
		&p.ID, &p.ConnectionID, &p.ProfileRevision, &p.Provider,
		&p.Intent, &stepsJSON, &p.Fingerprint,
		&createdAtStr, &expiresAt, &warningsJSON, &expectedJSON, &precondsJSON, &observedFP,
		&editPayloadJSON, &p.Account,
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
	if len(editPayloadJSON) > 0 {
		p.EditPayload = &core.EditPayload{}
		if err := json.Unmarshal(editPayloadJSON, p.EditPayload); err != nil {
			return nil, fmt.Errorf("unmarshal edit payload: %w", err)
		}
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
	var editPayloadJSON []byte
	if plan.EditPayload != nil {
		editPayloadJSON, err = json.Marshal(plan.EditPayload)
		if err != nil {
			return fmt.Errorf("marshal edit payload: %w", err)
		}
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO operation_plans
			(id, connection_id, profile_revision, provider_id, intent, steps_json, fingerprint,
			 created_at, expires_at, warnings_json, expected_json, preconditions_json, observed_fingerprint,
			 edit_payload_json, provider_account_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		plan.ID, plan.ConnectionID, plan.ProfileRevision, plan.Provider,
		string(plan.Intent), stepsJSON, plan.Fingerprint,
		createdAt, expiresAt, warningsJSON, expectedJSON, precondsJSON, observedFP,
		editPayloadJSON, plan.Account,
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
		       created_at, expires_at, warnings_json, expected_json, preconditions_json, observed_fingerprint,
		       edit_payload_json, provider_account_id
		FROM operation_plans WHERE id = ?`, id)

	var plan core.OperationPlan
	var stepsJSON, createdAt, expiresAt, warningsJSON, expectedJSON, precondsJSON []byte
	var intent string
	var observedFP sql.NullString
	var editPayloadJSON []byte
	if err := row.Scan(
		&plan.ID, &plan.ConnectionID, &plan.ProfileRevision, &plan.Provider,
		&intent, &stepsJSON, &plan.Fingerprint, &createdAt, &expiresAt,
		&warningsJSON, &expectedJSON, &precondsJSON, &observedFP,
		&editPayloadJSON, &plan.Account,
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
	if len(editPayloadJSON) > 0 {
		plan.EditPayload = &core.EditPayload{}
		if err := json.Unmarshal(editPayloadJSON, plan.EditPayload); err != nil {
			return nil, fmt.Errorf("unmarshal edit payload: %w", err)
		}
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operation creation: %w", err)
	}
	defer tx.Rollback()
	var existing int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM operations WHERE id = ?", opID).Scan(&existing); err != nil {
		return fmt.Errorf("check existing operation: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO operations (id, plan_id, connection_id, state, started_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			state=excluded.state,
			plan_id=excluded.plan_id, connection_id=excluded.connection_id`,
		opID, planID, connID, state, startedAt,
	)
	if err != nil {
		return err
	}
	if existing == 0 {
		occurredAt, parseErr := time.Parse(time.RFC3339, startedAt)
		if parseErr != nil {
			occurredAt = time.Now().UTC()
		}
		if _, err := appendEventTx(ctx, tx, opID, connID, "operation.created", string(core.StageStarted), occurredAt, map[string]string{
			"operation_id":  string(opID),
			"connection_id": string(connID),
			"plan_id":       string(planID),
		}); err != nil {
			return fmt.Errorf("persist operation creation event: %w", err)
		}
	}
	return tx.Commit()
}

// CompleteOperation marks an operation as completed and clears the active operation reference.
func (s *Store) CompleteOperation(ctx context.Context, opID core.OperationID, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operation completion: %w", err)
	}
	defer tx.Rollback()
	var connID core.ConnectionID
	if err := tx.QueryRowContext(ctx, "SELECT connection_id FROM operations WHERE id = ?", opID).Scan(&connID); err != nil {
		return fmt.Errorf("lookup operation for completion: %w", err)
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		UPDATE operations SET state = ?, completed_at = ? WHERE id = ?`,
		state, now.Format(time.RFC3339), opID,
	)
	if err != nil {
		return err
	}

	// Clear active operation reference in runtime
	_, err = tx.ExecContext(ctx, `
		UPDATE connection_runtime SET active_operation_id = NULL
		WHERE active_operation_id = ?`,
		opID,
	)
	if err != nil {
		return err
	}
	eventType, stage := core.EventOperationCompleted, core.StageSucceeded
	if state == "failed" {
		eventType, stage = core.EventOperationFailed, core.StageFailed
	}
	if _, err := appendEventTx(ctx, tx, opID, connID, string(eventType), string(stage), now, map[string]string{
		"operation_id":  string(opID),
		"connection_id": string(connID),
		"state":         state,
	}); err != nil {
		return fmt.Errorf("persist generic operation terminal event: %w", err)
	}
	return tx.Commit()
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
			runtime_revision = runtime_revision + 1,
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
	if _, err := appendEventTx(ctx, tx, opID, connID, string(core.EventOperationCompleted), string(core.StageSucceeded), committedAt, core.OperationEvent{
		OperationID: opID, ConnectionID: connID, Stage: core.StageSucceeded, Message: "Open operation completed", Timestamp: committedAt,
	}); err != nil {
		return nil, fmt.Errorf("persist open completion event: %w", err)
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
			runtime_revision = runtime_revision + 1,
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
	if _, err := appendEventTx(ctx, tx, opID, connID, string(core.EventOperationCompleted), string(core.StageSucceeded), committedAt, core.OperationEvent{
		OperationID: opID, ConnectionID: connID, Stage: core.StageSucceeded, Message: "Close operation completed", Timestamp: committedAt,
	}); err != nil {
		return nil, fmt.Errorf("persist close completion event: %w", err)
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
			runtime_revision = runtime_revision + 1,
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
	if _, err := appendEventTx(ctx, tx, opID, connID, string(core.EventOperationCompleted), string(core.StageSucceeded), committedAt, core.OperationEvent{
		OperationID: opID, ConnectionID: connID, Stage: core.StageSucceeded, Message: "Repair operation completed", Timestamp: committedAt,
	}); err != nil {
		return nil, fmt.Errorf("persist repair completion event: %w", err)
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

// CommitEditSuccess persists the result of a successful edit operation.
// Unlike open/close, an edit:
//   - preserves the current desired state (the user did not request open/close)
//   - preserves the current runtime state (a running connector stays running
//     unless the edit explicitly requires restart)
//   - does NOT increment the profile revision (the profile was already updated
//     atomically in the edit's apply-profile step)
func (s *Store) CommitEditSuccess(ctx context.Context, connID core.ConnectionID, opID core.OperationID) (*core.RuntimeCommitResult, error) {
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

	// 2. Clear the active operation on the runtime row, preserving the current
	//    desired state and runtime state (the edit did not request open/close).
	//    Bump runtime_revision as required by CAS design.
	_, err = tx.ExecContext(ctx, `
		UPDATE connection_runtime SET
			active_operation_id = NULL,
			runtime_revision = runtime_revision + 1,
			last_transition = ?,
			error_json = NULL
		WHERE connection_id = ?`,
		now, connID)
	if err != nil {
		return nil, fmt.Errorf("clear active operation: %w", err)
	}

	// 3. Read back the committed profile values and current runtime state.
	var revision uint64
	var desired string
	var runtimeState core.RuntimeState
	if err := tx.QueryRowContext(ctx,
		"SELECT revision, desired_state FROM connection_profiles WHERE id = ?", connID).Scan(&revision, &desired); err != nil {
		return nil, fmt.Errorf("read committed profile: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		"SELECT runtime_state FROM connection_runtime WHERE connection_id = ?", connID).Scan(&runtimeState); err != nil {
		return nil, fmt.Errorf("read committed runtime: %w", err)
	}
	if _, err := appendEventTx(ctx, tx, opID, connID, string(core.EventOperationCompleted), string(core.StageSucceeded), committedAt, core.OperationEvent{
		OperationID: opID, ConnectionID: connID, Stage: core.StageSucceeded, Message: "Edit operation completed", Timestamp: committedAt,
	}); err != nil {
		return nil, fmt.Errorf("persist edit completion event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &core.RuntimeCommitResult{
		ProfileRevision: revision,
		DesiredState:    core.DesiredConnectionState(desired),
		RuntimeState:    runtimeState,
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
			runtime_revision = runtime_revision + 1,
			runtime_state = ?,
			active_operation_id = NULL,
			last_transition = ?,
			error_json = ?
		WHERE connection_id = ?`,
		string(core.RuntimeError), now, errorJSON, connID)
	if err != nil {
		return fmt.Errorf("update runtime: %w", err)
	}
	if _, err = appendEventTx(ctx, tx, opID, connID, string(core.EventOperationFailed), string(core.StageFailed), time.Now().UTC(), core.OperationEvent{
		OperationID: opID, ConnectionID: connID, Stage: core.StageFailed, Error: errMsg, Timestamp: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("persist operation failure event: %w", err)
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
		if _, err = appendEventTx(ctx, tx, opID, connID, string(core.EventOperationCompleted), string(core.StageSucceeded), time.Now().UTC(), core.OperationEvent{
			OperationID: opID, ConnectionID: connID, Stage: core.StageSucceeded, Message: "Delete operation completed", Timestamp: time.Now().UTC(),
		}); err != nil {
			return fmt.Errorf("persist delete completion event: %w", err)
		}
	}

	// 2. Verify no managed resources remain except confirmed-removed or
	// externally removed. The latter is an authoritative remote 404, not an
	// unresolved cleanup obligation.
	// Managed resources in any other state (present, removal_pending, removal_failed, orphaned)
	// block deletion. External/adopted resources are detached without remote deletion.
	var unresolvedManaged int
	err = tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM provider_resources WHERE connection_id = ? AND ownership = ? AND (lifecycle IS NULL OR lifecycle NOT IN (?, ?))",
		connID, string(core.OwnershipManaged), string(core.LifecycleRemoved), string(core.LifecycleExternallyRemoved)).Scan(&unresolvedManaged)
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

	// 5. Delete only successfully removed, externally removed, or pending resources.
	// Preserve removal_failed and orphaned rows as cleanup evidence.
	_, err = tx.ExecContext(ctx,
		"DELETE FROM provider_resources WHERE connection_id = ? AND lifecycle IN (?, ?, ?)",
		connID, string(core.LifecycleRemoved), string(core.LifecycleExternallyRemoved), string(core.LifecycleRemovalPending))
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

// SyncFindings atomically upserts the findings produced by one diagnostic run
// and resolves every previously active finding for that connection which is
// absent from the current stable-ID set.
func (s *Store) SyncFindings(ctx context.Context, connID core.ConnectionID, findings []core.DiagnosticFinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin finding sync: %w", err)
	}
	defer tx.Rollback()
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339)
	active := make(map[core.FindingID]struct{}, len(findings))
	for _, finding := range findings {
		if finding.ConnectionID != connID {
			return fmt.Errorf("finding %s belongs to %s, expected %s", finding.ID, finding.ConnectionID, connID)
		}
		evidenceJSON, marshalErr := json.Marshal(finding.Evidence)
		if marshalErr != nil {
			return fmt.Errorf("marshal finding evidence: %w", marshalErr)
		}
		repairJSON, marshalErr := json.Marshal(finding.RepairOptions)
		if marshalErr != nil {
			return fmt.Errorf("marshal finding repair options: %w", marshalErr)
		}
		var alreadyActive bool
		lookupErr := tx.QueryRowContext(ctx, `
			SELECT 1 FROM findings WHERE id = ? AND resolved_at IS NULL`, finding.ID).Scan(new(int))
		switch lookupErr {
		case nil:
			alreadyActive = true
		case sql.ErrNoRows:
			// A new or previously resolved finding needs a new durable event.
		default:
			return fmt.Errorf("check finding %s: %w", finding.ID, lookupErr)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO findings (id, connection_id, segment, severity, summary, explanation, evidence_json, repair_options_json, resolved_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
			ON CONFLICT(id) DO UPDATE SET
				segment=excluded.segment, severity=excluded.severity, summary=excluded.summary,
				explanation=excluded.explanation, evidence_json=excluded.evidence_json,
				repair_options_json=excluded.repair_options_json, resolved_at=NULL`,
			finding.ID, connID, string(finding.Segment), string(finding.Severity), finding.Summary,
			finding.Explanation, evidenceJSON, repairJSON, now)
		if err != nil {
			return fmt.Errorf("upsert finding %s: %w", finding.ID, err)
		}
		if !alreadyActive {
			if _, err := appendEventTx(ctx, tx, "", connID, string(core.EventDiagnosticFinding), string(core.StageSucceeded), nowTime, map[string]string{
				"connection_id": string(connID), "finding_id": string(finding.ID), "segment": string(finding.Segment),
			}); err != nil {
				return fmt.Errorf("append finding event %s: %w", finding.ID, err)
			}
		}
		active[finding.ID] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM findings WHERE connection_id = ? AND resolved_at IS NULL", connID)
	if err != nil {
		return err
	}
	var resolve []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if _, ok := active[core.FindingID(id)]; !ok {
			resolve = append(resolve, id)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range resolve {
		if _, err := tx.ExecContext(ctx, "UPDATE findings SET resolved_at = ? WHERE id = ?", now, id); err != nil {
			return err
		}
		if _, err := appendEventTx(ctx, tx, "", connID, string(core.EventDiagnosticResolved), string(core.StageSucceeded), nowTime, map[string]string{
			"connection_id": string(connID), "finding_id": id,
		}); err != nil {
			return fmt.Errorf("append finding resolution event %s: %w", id, err)
		}
	}
	// Keep the runtime projection in the same transaction as findings and
	// their event rows. A missing runtime is valid for an interrupted/new
	// connection, so zero affected rows are intentionally not an error.
	diagnosticsJSON, err := json.Marshal(findings)
	if err != nil {
		return fmt.Errorf("marshal runtime diagnostics: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE connection_runtime SET diagnostics_json = ? WHERE connection_id = ?`, diagnosticsJSON, connID); err != nil {
		return fmt.Errorf("update runtime diagnostics: %w", err)
	}
	return tx.Commit()
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

// SaveProviderCredential stores an API credential by an opaque account
// reference. Provider account rows retain only this reference, never the
// secret itself.
func (s *Store) SaveProviderCredential(ctx context.Context, providerID core.ProviderID, credentialRef string, secret []byte) error {
	if providerID == "" || strings.TrimSpace(credentialRef) == "" || len(secret) == 0 {
		return fmt.Errorf("provider ID, credential reference, and secret are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	encrypted, err := encryptCredential(s.secretStore, secret, providerCredentialContext(providerID, credentialRef))
	if err != nil {
		return fmt.Errorf("encrypt provider credential: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO provider_credentials (credential_ref, provider_id, secret_encrypted, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(credential_ref) DO UPDATE SET
			provider_id=excluded.provider_id,
			secret_encrypted=excluded.secret_encrypted,
			updated_at=excluded.updated_at`,
		credentialRef, string(providerID), encrypted, now, now)
	return err
}

// LoadProviderCredential resolves one credential reference for the expected
// provider. A reference belonging to another provider is rejected rather than
// decrypted under a different AAD context.
func (s *Store) LoadProviderCredential(ctx context.Context, providerID core.ProviderID, credentialRef string) (string, error) {
	if providerID == "" || strings.TrimSpace(credentialRef) == "" {
		return "", fmt.Errorf("provider ID and credential reference are required")
	}
	s.mu.RLock()
	var storedProvider string
	var encrypted []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT provider_id, secret_encrypted FROM provider_credentials WHERE credential_ref = ?`, credentialRef).Scan(&storedProvider, &encrypted)
	if err == sql.ErrNoRows {
		s.mu.RUnlock()
		return "", nil
	}
	if err != nil {
		s.mu.RUnlock()
		return "", err
	}
	if storedProvider != string(providerID) {
		s.mu.RUnlock()
		return "", fmt.Errorf("credential reference %q belongs to provider %q", credentialRef, storedProvider)
	}
	secret, err := decryptCredential(s.secretStore, encrypted, providerCredentialContext(providerID, credentialRef))
	s.mu.RUnlock()
	if err != nil {
		return "", fmt.Errorf("decrypt provider credential: %w", err)
	}
	return secret, nil
}

// SaveTunnelCredential stores an encrypted tunnel token for a connection.
// The token is encrypted with AES-GCM using the SecretStore installation key,
// with AAD binding the connection, provider, tunnel, and AAD schema version.
func (s *Store) SaveTunnelCredential(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string, token []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	context := credentialContext(connID, providerID, tunnelID)
	encrypted, err := encryptCredential(s.secretStore, token, context)
	if err != nil {
		return fmt.Errorf("encrypt credential: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tunnel_credentials (connection_id, provider_id, tunnel_id, token_encrypted, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(connection_id, provider_id, tunnel_id) DO UPDATE SET
			token_encrypted=excluded.token_encrypted,
			updated_at=excluded.updated_at`,
		connID, string(providerID), tunnelID, encrypted, now, now,
	)
	return err
}

// LoadTunnelCredential loads the sole credential for a connection. It exists
// for compatibility with one-tunnel connections; callers that know a tunnel
// ID must use LoadTunnelCredentialExact. Multiple rows are deliberately an
// error rather than an arbitrary newest-token selection.
func (s *Store) LoadTunnelCredential(ctx context.Context, connID core.ConnectionID) (tunnelID, token string, err error) {
	s.mu.RLock()
	rows, queryErr := s.db.QueryContext(ctx, `
		SELECT tunnel_id, provider_id, token_encrypted
		FROM tunnel_credentials WHERE connection_id = ?
		ORDER BY updated_at DESC LIMIT 2`, connID)
	if queryErr != nil {
		s.mu.RUnlock()
		return "", "", queryErr
	}
	var rowsFound int
	var providerID string
	var encrypted []byte
	for rows.Next() {
		rowsFound++
		if rowsFound == 1 {
			if err := rows.Scan(&tunnelID, &providerID, &encrypted); err != nil {
				rows.Close()
				s.mu.RUnlock()
				return "", "", err
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.mu.RUnlock()
		return "", "", err
	}
	if err := rows.Close(); err != nil {
		s.mu.RUnlock()
		return "", "", err
	}
	if rowsFound == 0 {
		s.mu.RUnlock()
		return "", "", nil
	}
	if rowsFound > 1 {
		s.mu.RUnlock()
		return "", "", fmt.Errorf("ambiguous tunnel credentials for connection %s; select an exact provider and tunnel", connID)
	}
	token, legacy, decryptErr := s.decryptTunnelCredentialLocked(connID, core.ProviderID(providerID), tunnelID, encrypted)
	s.mu.RUnlock()
	if decryptErr != nil {
		return "", "", decryptErr
	}
	if legacy && providerID != "" {
		if err := s.SaveTunnelCredential(ctx, connID, core.ProviderID(providerID), tunnelID, []byte(token)); err != nil {
			return "", "", fmt.Errorf("migrate legacy credential: %w", err)
		}
	}
	return tunnelID, token, nil
}

// LoadTunnelCredentialExact loads the token bound to one provider tunnel.
// Legacy provider-unbound rows are accepted once and immediately rewritten
// with the supplied provider binding after successful decryption.
func (s *Store) LoadTunnelCredentialExact(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string) (string, error) {
	s.mu.RLock()
	var storedProvider string
	var encrypted []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT provider_id, token_encrypted FROM tunnel_credentials
		WHERE connection_id = ? AND tunnel_id = ? AND provider_id IN (?, '')
		ORDER BY CASE WHEN provider_id = ? THEN 0 ELSE 1 END LIMIT 1`,
		connID, tunnelID, string(providerID), string(providerID)).Scan(&storedProvider, &encrypted)
	if err == sql.ErrNoRows {
		s.mu.RUnlock()
		return "", nil
	}
	if err != nil {
		s.mu.RUnlock()
		return "", err
	}
	token, legacy, decryptErr := s.decryptTunnelCredentialLocked(connID, core.ProviderID(storedProvider), tunnelID, encrypted)
	s.mu.RUnlock()
	if decryptErr != nil {
		return "", decryptErr
	}
	if legacy || storedProvider != string(providerID) {
		if err := s.SaveTunnelCredential(ctx, connID, providerID, tunnelID, []byte(token)); err != nil {
			return "", fmt.Errorf("migrate legacy credential: %w", err)
		}
		if storedProvider == "" {
			if err := s.DeleteTunnelCredentialExact(ctx, connID, "", tunnelID); err != nil {
				return "", fmt.Errorf("remove migrated legacy credential: %w", err)
			}
		}
	}
	return token, nil
}

func (s *Store) decryptTunnelCredentialLocked(connID core.ConnectionID, providerID core.ProviderID, tunnelID string, encrypted []byte) (token string, legacy bool, err error) {
	credCtx := legacyCredentialContext(connID, tunnelID)
	legacy = providerID == "" || isLegacyCredentialBlob(encrypted)
	if providerID != "" {
		credCtx = credentialContext(connID, providerID, tunnelID)
	}
	token, err = decryptCredential(s.secretStore, encrypted, credCtx)
	if err != nil {
		return "", legacy, fmt.Errorf("decrypt credential: %w", err)
	}
	return token, legacy, nil
}

// DeleteTunnelCredentialExact removes only the credential bound to a
// particular tunnel. Compensation must never erase a replacement tunnel's
// token merely because it shares the same connection.
func (s *Store) DeleteTunnelCredentialExact(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, "DELETE FROM tunnel_credentials WHERE connection_id = ? AND provider_id = ? AND tunnel_id = ?", connID, string(providerID), tunnelID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows > 1 {
		return fmt.Errorf("credential identity conflict for connection %s tunnel %s", connID, tunnelID)
	}
	return nil
}

// RotateSecretKey rotates the installation key and transactionally rewrites
// every durable tunnel credential under the new version. Older keys remain
// available until every row commits successfully; only then are they retired.
// If a rewrite fails, the new key is retained alongside the old keys so the
// database remains decryptable and a later retry is safe.
func (s *Store) RotateSecretKey(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secretStore == nil {
		return 0, fmt.Errorf("secret store not initialized")
	}
	version, err := s.secretStore.Rotate()
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return version, fmt.Errorf("begin credential rotation: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT connection_id, provider_id, tunnel_id, token_encrypted
		FROM tunnel_credentials`)
	if err != nil {
		return version, fmt.Errorf("list credentials for rotation: %w", err)
	}
	type credentialRow struct {
		connectionID core.ConnectionID
		providerID   core.ProviderID
		tunnelID     string
		token        string
	}
	type providerCredentialRow struct {
		providerID core.ProviderID
		ref        string
		secret     string
	}
	var credentials []credentialRow
	for rows.Next() {
		var row credentialRow
		var encrypted []byte
		if err := rows.Scan(&row.connectionID, &row.providerID, &row.tunnelID, &encrypted); err != nil {
			rows.Close()
			return version, fmt.Errorf("scan credential for rotation: %w", err)
		}
		context := legacyCredentialContext(row.connectionID, row.tunnelID)
		if row.providerID != "" {
			context = credentialContext(row.connectionID, row.providerID, row.tunnelID)
		}
		row.token, err = decryptCredential(s.secretStore, encrypted, context)
		if err != nil {
			rows.Close()
			return version, fmt.Errorf("decrypt credential %s/%s: %w", row.connectionID, row.tunnelID, err)
		}
		credentials = append(credentials, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return version, fmt.Errorf("iterate credentials for rotation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return version, fmt.Errorf("close credential iterator: %w", err)
	}
	providerRows, err := tx.QueryContext(ctx, `
		SELECT provider_id, credential_ref, secret_encrypted FROM provider_credentials`)
	if err != nil {
		return version, fmt.Errorf("list provider credentials for rotation: %w", err)
	}
	var providerCredentials []providerCredentialRow
	for providerRows.Next() {
		var row providerCredentialRow
		var encrypted []byte
		if err := providerRows.Scan(&row.providerID, &row.ref, &encrypted); err != nil {
			providerRows.Close()
			return version, fmt.Errorf("scan provider credential for rotation: %w", err)
		}
		row.secret, err = decryptCredential(s.secretStore, encrypted, providerCredentialContext(row.providerID, row.ref))
		if err != nil {
			providerRows.Close()
			return version, fmt.Errorf("decrypt provider credential %s/%s: %w", row.providerID, row.ref, err)
		}
		providerCredentials = append(providerCredentials, row)
	}
	if err := providerRows.Err(); err != nil {
		providerRows.Close()
		return version, fmt.Errorf("iterate provider credentials for rotation: %w", err)
	}
	if err := providerRows.Close(); err != nil {
		return version, fmt.Errorf("close provider credential iterator: %w", err)
	}
	for _, row := range credentials {
		context := legacyCredentialContext(row.connectionID, row.tunnelID)
		if row.providerID != "" {
			context = credentialContext(row.connectionID, row.providerID, row.tunnelID)
		}
		encrypted, err := encryptCredential(s.secretStore, []byte(row.token), context)
		if err != nil {
			return version, fmt.Errorf("encrypt credential %s/%s: %w", row.connectionID, row.tunnelID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tunnel_credentials SET token_encrypted = ?, updated_at = ?
			WHERE connection_id = ? AND provider_id = ? AND tunnel_id = ?`,
			encrypted, time.Now().UTC().Format(time.RFC3339), row.connectionID, string(row.providerID), row.tunnelID); err != nil {
			return version, fmt.Errorf("rewrite credential %s/%s: %w", row.connectionID, row.tunnelID, err)
		}
	}
	for _, row := range providerCredentials {
		encrypted, err := encryptCredential(s.secretStore, []byte(row.secret), providerCredentialContext(row.providerID, row.ref))
		if err != nil {
			return version, fmt.Errorf("encrypt provider credential %s/%s: %w", row.providerID, row.ref, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE provider_credentials SET secret_encrypted = ?, updated_at = ?
			WHERE provider_id = ? AND credential_ref = ?`,
			encrypted, time.Now().UTC().Format(time.RFC3339), string(row.providerID), row.ref); err != nil {
			return version, fmt.Errorf("rewrite provider credential %s/%s: %w", row.providerID, row.ref, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return version, fmt.Errorf("commit credential rotation: %w", err)
	}
	if err := s.secretStore.RetireVersionsBefore(version); err != nil {
		return version, fmt.Errorf("retire old credential keys: %w", err)
	}
	return version, nil
}

// CommitStepResult is retained for compatibility; normal execution uses
// CommitStepOutcome, which additionally maintains the recovery ledger.
func (s *Store) CommitStepResult(ctx context.Context, req core.StepCommitRequest) error {
	return s.CommitStepOutcome(ctx, req)
}

// CommitStepOutcome atomically persists the full result of executing a step:
// terminal step event, provider resources, credential mutations, and lifecycle
// changes all in a single transaction, together with the durable recovery
// ledger row. (SPEC P0 atomic step commit)
func (s *Store) CommitStepOutcome(ctx context.Context, req core.StepCommitRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	stepID := req.Result.StepID
	if stepID == "" {
		stepID = req.Step.ID
	}
	resultJSON, err := marshalStepResult(req.Result)
	if err != nil {
		return fmt.Errorf("marshal step result: %w", err)
	}
	status := string(StepSucceeded)
	if !req.Result.Succeeded {
		status = string(StepFailed)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO operation_step_results
			(operation_id, connection_id, step_id, step_kind, status, result_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(operation_id, step_id) DO UPDATE SET
			connection_id=excluded.connection_id,
			step_kind=excluded.step_kind,
			status=excluded.status,
			result_json=excluded.result_json,
			updated_at=excluded.updated_at`,
		req.OperationID, req.ConnectionID, stepID, string(req.Step.Kind), status, resultJSON, now, now)
	if err != nil {
		return fmt.Errorf("persist step recovery outcome: %w", err)
	}

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
		req.OperationID, stepID, eventType, stage, req.Step.Summary, errorMsg, 0, now)
	if err != nil {
		return fmt.Errorf("persist step event: %w", err)
	}
	if _, err = appendEventTx(ctx, tx, req.OperationID, req.ConnectionID, eventType, stage, time.Now().UTC(), core.OperationEvent{
		OperationID: req.OperationID, ConnectionID: req.ConnectionID, Stage: core.EventStage(stage),
		StepID: stepID, StepKind: req.Step.Kind, Message: req.Step.Summary, Error: errorMsg,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("persist durable step event: %w", err)
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
				SET spec_hash = ?, metadata_json = ?,
					lifecycle = CASE WHEN ownership = ? THEN ? ELSE lifecycle END
				WHERE provider_id = ? AND resource_type = ? AND external_id = ? AND connection_id = ?`,
				res.SpecHash, metaJSON, string(core.OwnershipManaged), string(core.LifecyclePresent),
				res.ProviderID, string(res.Type), res.ExternalID, res.ConnectionID,
			)
			if err != nil {
				return fmt.Errorf("update resource %s: %w", res.ExternalID, err)
			}
		case qErr == sql.ErrNoRows:
			_, err = tx.ExecContext(ctx, `
				INSERT INTO provider_resources
					(connection_id, provider_id, resource_type, external_id, ownership, lifecycle, spec_hash, metadata_json, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				res.ConnectionID, res.ProviderID, string(res.Type), res.ExternalID,
				string(res.Ownership), string(core.LifecyclePresent), res.SpecHash, metaJSON, now,
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
		credCtx := credentialContext(req.ConnectionID, req.Provider, cred.TunnelID)
		encrypted, encErr := encryptCredential(s.secretStore, cred.Secret, credCtx)
		if encErr != nil {
			return fmt.Errorf("encrypt credential: %w", encErr)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO tunnel_credentials (connection_id, provider_id, tunnel_id, token_encrypted, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(connection_id, provider_id, tunnel_id) DO UPDATE SET
				token_encrypted=excluded.token_encrypted,
				updated_at=excluded.updated_at`,
			req.ConnectionID, string(req.Provider), cred.TunnelID, encrypted, now, now)
		if err != nil {
			return fmt.Errorf("persist credential: %w", err)
		}
	}

	// 4. Apply lifecycle changes.
	for _, mark := range req.Lifecycle {
		result, updateErr := tx.ExecContext(ctx,
			"UPDATE provider_resources SET lifecycle = ? WHERE connection_id = ? AND provider_id = ? AND resource_type = ? AND external_id = ?",
			string(mark.NewLifecycle), req.ConnectionID, string(mark.ProviderID), string(mark.ResourceType), mark.ExternalID)
		if updateErr != nil {
			return fmt.Errorf("update lifecycle %s: %w", mark.ExternalID, updateErr)
		}
		rows, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("check lifecycle %s: %w", mark.ExternalID, rowsErr)
		}
		if rows != 1 {
			return fmt.Errorf("lifecycle conflict for %s: expected one resource, updated %d", mark.ExternalID, rows)
		}
	}

	// 5. Cascade policy removal for deleted Access applications. The durable
	// inventory defines the exact expected policy count. A lifecycle transition
	// that touches fewer rows is an integrity conflict, not a successful delete.
	for _, appID := range req.RemovedAccessApps {
		var expected int
		if err = tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM provider_resources
			WHERE connection_id = ? AND resource_type = ?
				AND json_extract(metadata_json, '$.app_id') = ?`,
			req.ConnectionID, string(core.ResourceAccessPolicy), appID,
		).Scan(&expected); err != nil {
			return fmt.Errorf("count policies for app %s: %w", appID, err)
		}
		result, updateErr := tx.ExecContext(ctx,
			"UPDATE provider_resources SET lifecycle = ? WHERE connection_id = ? AND resource_type = ? AND json_extract(metadata_json, '$.app_id') = ?",
			string(core.LifecycleRemoved), req.ConnectionID, string(core.ResourceAccessPolicy), appID)
		if updateErr != nil {
			return fmt.Errorf("cascade policy removal for app %s: %w", appID, updateErr)
		}
		rows, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("check cascaded policies for app %s: %w", appID, rowsErr)
		}
		if int(rows) != expected {
			return fmt.Errorf("policy lifecycle conflict for app %s: expected %d policies, updated %d", appID, expected, rows)
		}
	}

	return tx.Commit()
}

// BeginStep atomically records a recovery-ledger "started" row and its
// durable operation event before a provider or local-origin mutation begins.
func (s *Store) BeginStep(ctx context.Context, operationID core.OperationID, connectionID core.ConnectionID, step core.PlanStep) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO operation_step_results
			(operation_id, connection_id, step_id, step_kind, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(operation_id, step_id) DO UPDATE SET
			connection_id=excluded.connection_id,
			step_kind=excluded.step_kind,
			status=excluded.status,
			updated_at=excluded.updated_at`,
		operationID, connectionID, step.ID, string(step.Kind), string(StepStarted), now, now)
	if err != nil {
		return fmt.Errorf("record step start: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO operation_events
			(operation_id, step_id, event_type, stage, summary, error, sequence, timestamp)
		VALUES (?, ?, ?, ?, ?, '', 0, ?)`,
		operationID, step.ID, string(core.EventOperationStepStarted), string(core.StageStarted), step.Summary, now)
	if err != nil {
		return fmt.Errorf("persist step-start event: %w", err)
	}
	if _, err = appendEventTx(ctx, tx, operationID, connectionID, string(core.EventOperationStepStarted), string(core.StageStarted), time.Now().UTC(), core.OperationEvent{
		OperationID: operationID, ConnectionID: connectionID, Stage: core.StageStarted,
		StepID: step.ID, StepKind: step.Kind, Message: step.Summary, Timestamp: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("persist durable step-start event: %w", err)
	}
	return tx.Commit()
}

// BeginCompensation durably records a compensation before it mutates the
// provider. A restart can therefore distinguish a not-started rollback from
// an outcome that must be recovered.
func (s *Store) BeginCompensation(ctx context.Context, operationID core.OperationID, connectionID core.ConnectionID, step core.PlanStep) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO operation_step_results (operation_id, connection_id, step_id, step_kind, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(operation_id, step_id) DO UPDATE SET
			connection_id=excluded.connection_id, step_kind=excluded.step_kind,
			status=excluded.status, updated_at=excluded.updated_at`,
		operationID, connectionID, step.ID, string(step.Kind), string(StepCompensationPending), now, now); err != nil {
		return fmt.Errorf("record compensation start: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO operation_events (operation_id, step_id, event_type, stage, summary, error, sequence, timestamp)
		VALUES (?, ?, ?, ?, ?, '', 0, ?)`,
		operationID, step.ID, string(core.EventOperationStepStarted), string(core.StageStarted), step.Summary, now); err != nil {
		return fmt.Errorf("persist compensation start event: %w", err)
	}
	return tx.Commit()
}

// CommitCompensationOutcome atomically stores the terminal compensation
// state and its operation event.
func (s *Store) CommitCompensationOutcome(ctx context.Context, operationID core.OperationID, connectionID core.ConnectionID, step core.PlanStep, result core.StepResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	encoded, err := marshalStepResult(result)
	if err != nil {
		return fmt.Errorf("marshal compensation result: %w", err)
	}
	status, eventType, stage := string(StepCompensated), string(core.EventOperationStepSucceeded), string(core.StageCompensated)
	errorText := ""
	if !result.Succeeded {
		status, eventType, stage = string(StepCompensationFailed), string(core.EventOperationStepFailed), string(core.StageFailed)
		if result.Error != nil {
			errorText = result.Error.Error()
		}
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO operation_step_results (operation_id, connection_id, step_id, step_kind, status, result_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(operation_id, step_id) DO UPDATE SET
			connection_id=excluded.connection_id, step_kind=excluded.step_kind,
			status=excluded.status, result_json=excluded.result_json, updated_at=excluded.updated_at`,
		operationID, connectionID, step.ID, string(step.Kind), status, encoded, now, now); err != nil {
		return fmt.Errorf("record compensation outcome: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO operation_events (operation_id, step_id, event_type, stage, summary, error, sequence, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?)`,
		operationID, step.ID, eventType, stage, step.Summary, errorText, now); err != nil {
		return fmt.Errorf("persist compensation outcome event: %w", err)
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

// persistedStepResult deliberately omits credential secret bytes. The
// recovery ledger needs outcome evidence, not a second secret store.
type persistedStepResult struct {
	StepID    string                  `json:"step_id"`
	Succeeded bool                    `json:"succeeded"`
	Error     string                  `json:"error,omitempty"`
	Resources []core.ProviderResource `json:"resources,omitempty"`
}

func marshalStepResult(result core.StepResult) ([]byte, error) {
	document := persistedStepResult{
		StepID:    result.StepID,
		Succeeded: result.Succeeded,
		Resources: result.Resources,
	}
	if result.Error != nil {
		document.Error = result.Error.Error()
	}
	return json.Marshal(document)
}

func unmarshalStepResult(data []byte) (core.StepResult, error) {
	var document persistedStepResult
	if err := json.Unmarshal(data, &document); err != nil {
		return core.StepResult{}, err
	}
	result := core.StepResult{StepID: document.StepID, Succeeded: document.Succeeded, Resources: document.Resources}
	if document.Error != "" {
		result.Error = errors.New(document.Error)
	}
	return result, nil
}

// RecordStepStart records that a step has started execution.
func (s *Store) RecordStepStart(ctx context.Context, opID core.OperationID, stepID string, stepKind core.StepKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO operation_step_results (operation_id, connection_id, step_id, step_kind, status, created_at, updated_at)
		VALUES (?, '', ?, ?, ?, ?, ?)
		ON CONFLICT(operation_id, step_id) DO UPDATE SET status=excluded.status, updated_at=excluded.updated_at`,
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
			result, decodeErr := unmarshalStepResult(resultJSON)
			if decodeErr != nil {
				return nil, fmt.Errorf("decode step result for operation %s step %s: %w", opID, r.StepID, decodeErr)
			}
			r.Result = result
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
			INSERT INTO operation_step_results (operation_id, connection_id, step_id, step_kind, status, created_at, updated_at)
			VALUES (?, '', ?, ?, ?, ?, ?)`,
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
	Sequence  int64
	StepID    string
	EventType string
	Stage     string
	Summary   string
	Error     string
	Timestamp string
}

// StoredOperation is the durable representation used by IPC operation
// queries. Unlike controller.Operation it remains available after a
// supervisor restart.
type StoredOperation struct {
	ID           core.OperationID
	PlanID       core.PlanID
	ConnectionID core.ConnectionID
	State        string
	StartedAt    string
	CompletedAt  string
	Error        string
}

// CleanupItem persists an externally-created resource that needs explicit
// follow-up even when no provider_resources row was committed.
type CleanupItem struct {
	OperationID  core.OperationID
	ConnectionID core.ConnectionID
	ProviderID   core.ProviderID
	// AccountID is the account whose credential created the resource, and is
	// therefore the one that can remove it. Without it, removing an account can
	// take away the only means of discharging this obligation.
	AccountID    core.ProviderAccountID
	ResourceType core.ResourceType
	ExternalID   string
	State        string
	LastError    string
}

// RecordCleanupItem inserts or refreshes a durable cleanup obligation. It is
// deliberately independent from provider_resources: this is the path for
// resources created successfully before the normal persistence transaction
// failed.
// CountUnresolvedCleanupItems reports how many provider resources Portico has
// recorded and not yet finished removing.
//
// An unresolved item is a resource that exists at the provider with a Portico
// record saying it should not. It is the state that quietly costs money and blocks
// account removal, and nothing surfaced it: a health check that omits it reports a
// clean machine while resources accumulate at the provider.
func (s *Store) CountUnresolvedCleanupItems(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Every state this table records is an unfinished obligation: a row is written
	// when compensation could not run, could not be journaled, failed, or finished
	// with an unknown outcome — "compensation_pending", "compensation_failed" and
	// "outcome_unknown" are the states in use, and there is no "resolved".
	//
	// Nothing removes a row either, so this counts every obligation ever recorded
	// rather than only the outstanding ones. That is worth stating plainly: the
	// number is a lower bound on trouble and not a live queue depth, and a health
	// check must describe it as history rather than implying the resources are
	// still there. Discharging these records is unbuilt work, tracked in
	// docs/REMAINING_WORK.md; counting them is how it stops being invisible.
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM resource_cleanup_items`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count unresolved cleanup items: %w", err)
	}
	return count, nil
}

func (s *Store) RecordCleanupItem(ctx context.Context, item CleanupItem) error {
	if item.OperationID == "" || item.ConnectionID == "" || item.ProviderID == "" || item.ResourceType == "" || item.ExternalID == "" || item.State == "" {
		return fmt.Errorf("invalid cleanup item")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO resource_cleanup_items
			(operation_id, connection_id, provider_id, provider_account_id, resource_type, external_id, cleanup_state, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, resource_type, external_id) DO UPDATE SET
			operation_id=excluded.operation_id, connection_id=excluded.connection_id,
			provider_account_id=excluded.provider_account_id,
			cleanup_state=excluded.cleanup_state, last_error=excluded.last_error,
			updated_at=excluded.updated_at`,
		item.OperationID, item.ConnectionID, item.ProviderID, string(item.AccountID),
		string(item.ResourceType), item.ExternalID,
		item.State, item.LastError, now, now)
	return err
}

// OperationSummary is one entry of operation history: the durable operation row
// joined to the identity of the plan it executed.
//
// Intent, provider and fingerprint live on the plan rather than the operation,
// so history must join them. Without the join a caller can only see an opaque
// plan ID and has to guess what the operation was actually doing.
type OperationSummary struct {
	ID              core.OperationID
	PlanID          core.PlanID
	ConnectionID    core.ConnectionID
	ProviderID      string
	Intent          string
	Fingerprint     string
	ProfileRevision uint64
	State           string
	StartedAt       string
	CompletedAt     string
	Error           string
}

// defaultOperationHistoryLimit bounds history reads when a caller does not
// specify one. Operation history is unbounded on disk; a UI listing must not
// load all of it.
const defaultOperationHistoryLimit = 100

// ListRecentOperations returns operation history newest first, bounded by limit.
func (s *Store) ListRecentOperations(ctx context.Context, limit int) ([]OperationSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = defaultOperationHistoryLimit
	}

	// LEFT JOIN: an operation whose plan row has been pruned is still real
	// history and must not vanish from the list.
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id, o.plan_id, o.connection_id, o.state, o.started_at, o.completed_at, o.error_json,
		       COALESCE(p.provider_id, ''), COALESCE(p.intent, ''), COALESCE(p.fingerprint, ''),
		       COALESCE(p.profile_revision, 0)
		FROM operations o
		LEFT JOIN operation_plans p ON p.id = o.plan_id
		ORDER BY o.started_at DESC, o.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent operations: %w", err)
	}
	defer rows.Close()

	var out []OperationSummary
	for rows.Next() {
		var op OperationSummary
		var completedAt sql.NullString
		var errorJSON []byte
		if err := rows.Scan(
			&op.ID, &op.PlanID, &op.ConnectionID, &op.State, &op.StartedAt, &completedAt, &errorJSON,
			&op.ProviderID, &op.Intent, &op.Fingerprint, &op.ProfileRevision,
		); err != nil {
			return nil, fmt.Errorf("scan operation history row: %w", err)
		}
		op.CompletedAt = completedAt.String
		if len(errorJSON) > 0 {
			// A malformed error blob must not hide the operation itself: the
			// row is still evidence that the operation ran and failed.
			var failure core.PorticoError
			if err := json.Unmarshal(errorJSON, &failure); err == nil {
				op.Error = failure.Message
			} else {
				op.Error = "failure detail could not be decoded"
			}
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// GetOperation returns one operation from the durable journal.
func (s *Store) GetOperation(ctx context.Context, opID core.OperationID) (*StoredOperation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var operation StoredOperation
	var completedAt sql.NullString
	var errorJSON []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, plan_id, connection_id, state, started_at, completed_at, error_json
		FROM operations WHERE id = ?`, opID).Scan(
		&operation.ID, &operation.PlanID, &operation.ConnectionID, &operation.State,
		&operation.StartedAt, &completedAt, &errorJSON)
	if err != nil {
		return nil, err
	}
	operation.CompletedAt = completedAt.String
	if len(errorJSON) > 0 {
		var failure core.PorticoError
		if err := json.Unmarshal(errorJSON, &failure); err != nil {
			return nil, fmt.Errorf("decode operation %s error: %w", opID, err)
		}
		operation.Error = failure.Message
	}
	return &operation, nil
}

// GetOperationEvents returns the journal events for an operation in
// insertion order.
func (s *Store) GetOperationEvents(ctx context.Context, opID core.OperationID) ([]OperationJournalEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, step_id, event_type, stage, summary, error, timestamp
		FROM operation_events WHERE operation_id = ? ORDER BY id`, opID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []OperationJournalEvent
	for rows.Next() {
		var e OperationJournalEvent
		var stepID, summary, errMsg sql.NullString
		if err := rows.Scan(&e.Sequence, &stepID, &e.EventType, &e.Stage, &summary, &errMsg, &e.Timestamp); err != nil {
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

	connectionID := core.ConnectionID("")
	if opEvent, ok := event.Data.(core.OperationEvent); ok {
		connectionID = opEvent.ConnectionID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operation event: %w", err)
	}
	defer tx.Rollback()
	if connectionID == "" && opID != "" {
		var conn string
		if err := tx.QueryRowContext(ctx, "SELECT connection_id FROM operations WHERE id = ?", opID).Scan(&conn); err != nil {
			return fmt.Errorf("lookup operation connection: %w", err)
		}
		connectionID = core.ConnectionID(conn)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO operation_events
			(operation_id, step_id, event_type, stage, summary, error, sequence, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		opID, stepID, eventType, stage, summary, errorMsg, event.Sequence, timestamp,
	)
	if err != nil {
		return err
	}
	if _, err := appendEventTx(ctx, tx, opID, connectionID, string(event.Type), stage, event.Timestamp, event.Data); err != nil {
		return fmt.Errorf("append unified operation event: %w", err)
	}
	return tx.Commit()
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

// CreateProviderAccountCredentialIfAbsent stores an account and its secret
// together, and only when the provider has no account under that ID.
//
// The account row and the credential must be guarded as one thing. Guarding the
// account alone is not enough: provider_credentials is keyed on the credential
// reference, every writer derives the same reference for a given account, and
// SaveProviderCredential is an upsert — so a bootstrap that wrote the secret
// first replaced a validated token while the account row kept its label, zone
// and status, and the adapter was then built from the wrong secret.
//
// Writing the account first and inspecting how many rows it affected is what
// makes this safe. A query beforehand would be a race; reversing the two calls
// would leave an account pointing at a credential that failed to persist.
//
// Reports whether a row was created.
func (s *Store) CreateProviderAccountCredentialIfAbsent(ctx context.Context, account core.ProviderAccount, secret []byte) (bool, error) {
	if account.ID == "" || account.Provider == "" || strings.TrimSpace(account.Label) == "" || strings.TrimSpace(account.CredentialRef) == "" {
		return false, fmt.Errorf("provider account ID, provider, label, and credential reference are required")
	}
	if len(secret) == 0 {
		return false, fmt.Errorf("a provider credential is required")
	}
	if account.Status == "" {
		account.Status = core.AccountPending
	}
	metadata, err := json.Marshal(account.Metadata)
	if err != nil {
		return false, fmt.Errorf("marshal provider account metadata: %w", err)
	}
	// Encrypting before the transaction keeps key work off the write lock and
	// means a cipher failure cannot leave a half-open transaction.
	encrypted, err := encryptCredential(s.secretStore, secret,
		providerCredentialContext(account.Provider, account.CredentialRef))
	if err != nil {
		return false, fmt.Errorf("encrypt provider credential: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin provider account transaction: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO provider_accounts
			(id, provider_id, label, credential_ref, metadata_json, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, id) DO NOTHING`,
		string(account.ID), string(account.Provider), account.Label, account.CredentialRef,
		metadata, string(account.Status), now, now)
	if err != nil {
		return false, fmt.Errorf("save provider account: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		// The provider already has this account. Its credential is not ours to
		// replace: this writer found a token, it did not verify one.
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_credentials (credential_ref, provider_id, secret_encrypted, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(credential_ref) DO UPDATE SET
			provider_id=excluded.provider_id,
			secret_encrypted=excluded.secret_encrypted,
			updated_at=excluded.updated_at`,
		account.CredentialRef, string(account.Provider), encrypted, now, now); err != nil {
		// The deferred rollback undoes the account insert, so the account and
		// its secret are created together or not at all.
		return false, fmt.Errorf("save provider credential: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit provider account: %w", err)
	}
	return true, nil
}

// UpsertProviderAccount records account metadata and an opaque credential
// reference. The secret belongs in provider_credentials and is never encoded
// in this row or its metadata.
//
// This overwrites label, metadata and status, so it is for writers that have
// established those facts. A writer that merely found a credential lying about
// must use CreateProviderAccountIfAbsent.
func (s *Store) UpsertProviderAccount(ctx context.Context, account core.ProviderAccount) error {
	if account.ID == "" || account.Provider == "" || strings.TrimSpace(account.Label) == "" || strings.TrimSpace(account.CredentialRef) == "" {
		return fmt.Errorf("provider account ID, provider, label, and credential reference are required")
	}
	// An unstated status means pending, never authenticated. Defaulting the
	// other way makes "I forgot to set this" indistinguishable from "this
	// credential was checked", which is the mistake that let Portico advertise
	// providers that could not perform a single operation.
	if account.Status == "" {
		account.Status = core.AccountPending
	}
	metadata, err := json.Marshal(account.Metadata)
	if err != nil {
		return fmt.Errorf("marshal provider account metadata: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO provider_accounts
			(id, provider_id, label, credential_ref, metadata_json, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		-- The conflict target is the composite key, and provider_id is absent
		-- from the SET list, so an upsert can never change which provider owns
		-- an existing account.
		ON CONFLICT(provider_id, id) DO UPDATE SET
			label=excluded.label,
			credential_ref=excluded.credential_ref,
			metadata_json=excluded.metadata_json,
			status=excluded.status,
			updated_at=excluded.updated_at`,
		string(account.ID), string(account.Provider), account.Label, account.CredentialRef,
		metadata, string(account.Status), now, now)
	return err
}

// UpsertProviderAccountCredential atomically updates an account's opaque
// credential reference and the encrypted secret it resolves to. It is used by
// supervisor account setup so a visible account can never reference a missing
// credential, and a failed account write cannot leave a usable orphan secret.
func (s *Store) UpsertProviderAccountCredential(ctx context.Context, account core.ProviderAccount, secret []byte) error {
	if account.ID == "" || account.Provider == "" || strings.TrimSpace(account.Label) == "" || strings.TrimSpace(account.CredentialRef) == "" || len(secret) == 0 {
		return fmt.Errorf("provider account ID, provider, label, credential reference, and secret are required")
	}
	// An unstated status means pending, never authenticated. Defaulting the
	// other way makes "I forgot to set this" indistinguishable from "this
	// credential was checked", which is the mistake that let Portico advertise
	// providers that could not perform a single operation.
	if account.Status == "" {
		account.Status = core.AccountPending
	}
	metadata, err := json.Marshal(account.Metadata)
	if err != nil {
		return fmt.Errorf("marshal provider account metadata: %w", err)
	}
	encrypted, err := encryptCredential(s.secretStore, secret, providerCredentialContext(account.Provider, account.CredentialRef))
	if err != nil {
		return fmt.Errorf("encrypt provider credential: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin provider account transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_credentials (credential_ref, provider_id, secret_encrypted, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(credential_ref) DO UPDATE SET
			provider_id=excluded.provider_id,
			secret_encrypted=excluded.secret_encrypted,
			updated_at=excluded.updated_at`,
		account.CredentialRef, string(account.Provider), encrypted, now, now); err != nil {
		return fmt.Errorf("save provider credential: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_accounts
			(id, provider_id, label, credential_ref, metadata_json, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		-- The conflict target is the composite key, and provider_id is absent
		-- from the SET list, so an upsert can never change which provider owns
		-- an existing account.
		ON CONFLICT(provider_id, id) DO UPDATE SET
			label=excluded.label,
			credential_ref=excluded.credential_ref,
			metadata_json=excluded.metadata_json,
			status=excluded.status,
			updated_at=excluded.updated_at`,
		string(account.ID), string(account.Provider), account.Label, account.CredentialRef,
		metadata, string(account.Status), now, now); err != nil {
		return fmt.Errorf("save provider account: %w", err)
	}
	return tx.Commit()
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

// LookupIdempotentKey returns the operation ID associated with the given
// idempotency key, or empty string if no mapping exists.
func (s *Store) LookupIdempotentKey(ctx context.Context, key string) (core.OperationID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var opID string
	err := s.db.QueryRowContext(ctx,
		"SELECT operation_id FROM idempotency_keys WHERE key = ?", key,
	).Scan(&opID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup idempotency key: %w", err)
	}
	return core.OperationID(opID), nil
}

// RecordIdempotentKey associates an idempotency key with an operation ID.
// If the key already exists, the existing mapping is preserved (first-write-wins).
func (s *Store) RecordIdempotentKey(ctx context.Context, key string, opID core.OperationID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx,
		"UPDATE idempotency_keys SET operation_id = ? WHERE key = ? AND (operation_id IS NULL OR operation_id = '')",
		string(opID), key,
	)
	if err == nil {
		var updated int64
		updated, err = result.RowsAffected()
		if err == nil && updated == 0 {
			_, err = s.db.ExecContext(ctx,
				"INSERT OR IGNORE INTO idempotency_keys (key, operation_id, created_at) VALUES (?, ?, ?)",
				key, string(opID), time.Now().UTC().Format(time.RFC3339),
			)
		}
	}
	if err != nil {
		return fmt.Errorf("record idempotency key: %w", err)
	}
	return nil
}

// DeleteProviderAccount removes an account row and its stored credential.
//
// Callers must confirm no connection depends on the account first;
// ConnectionsUsingAccount exists for exactly that check. Removing an
// account still referenced by a profile would strand that connection with an
// opaque "provider account unavailable" error and no way to see why.
func (s *Store) DeleteProviderAccount(ctx context.Context, providerID core.ProviderID, accountID core.ProviderAccountID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete account: %w", err)
	}
	defer tx.Rollback()

	var credentialRef sql.NullString
	err = tx.QueryRowContext(ctx,
		"SELECT credential_ref FROM provider_accounts WHERE id = ? AND provider_id = ?",
		string(accountID), string(providerID)).Scan(&credentialRef)
	if err == sql.ErrNoRows {
		return fmt.Errorf("provider account not found: %s/%s", providerID, accountID)
	}
	if err != nil {
		return fmt.Errorf("read account: %w", err)
	}

	if credentialRef.Valid && credentialRef.String != "" {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM provider_credentials WHERE credential_ref = ?", credentialRef.String); err != nil {
			return fmt.Errorf("delete account credential: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM provider_accounts WHERE id = ? AND provider_id = ?",
		string(accountID), string(providerID)); err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	return tx.Commit()
}

// AccountDependency is one thing that stands in the way of removing an account.
//
// Reporting only connections was not enough: a resource Portico created and has
// not yet cleaned up needs the credential that created it, and removing the
// account takes away the only means of cleaning it up.
type AccountDependency struct {
	// Kind is what sort of thing depends on the account: "connection" or
	// "cleanup_item".
	Kind string
	// ID identifies it, and Name is what to call it when explaining.
	ID   string
	Name string
	// Explanation says why this blocks removal, in terms of consequence.
	Explanation string
}

// AccountDependencies is everything that blocks removing one account.
type AccountDependencies []AccountDependency

// ConnectionIDs returns just the connections, for callers that only report those.
func (d AccountDependencies) ConnectionIDs() []string {
	var ids []string
	for _, dep := range d {
		if dep.Kind == "connection" {
			ids = append(ids, dep.ID)
		}
	}
	return ids
}

// AccountRemovalPreview is what removing an account would do, computed from the
// same evidence the removal itself decides on.
//
// The alternative — each client composing its own description — is how the
// confirmation screen came to promise that Portico would "forget the credential
// it stored" for accounts whose credential is not stored at all.
type AccountRemovalPreview struct {
	ProviderID   core.ProviderID
	AccountID    core.ProviderAccountID
	Label        string
	Status       string
	Dependencies AccountDependencies
	// Removable is true when nothing blocks the removal.
	Removable bool
	// CredentialStored reports that a stored credential row exists for this
	// account, rather than the account merely naming one.
	CredentialStored bool
	// Fingerprint binds this preview to the removal it describes. Applying with
	// a fingerprint that no longer matches is refused rather than performed
	// against a subject that has changed underneath it.
	Fingerprint string
	ObservedAt  time.Time
}

// StalePreviewError reports that the account, or what depends on it, changed
// between previewing a removal and applying it.
//
// Current is computed inside the refusing transaction, so what the caller is
// shown next is coherent with the refusal rather than a second read that could
// disagree with it.
type StalePreviewError struct {
	Expected string
	Current  *AccountRemovalPreview
}

func (e *StalePreviewError) Error() string {
	return "this account changed since the removal was previewed; review it again"
}

// PreviewProviderAccountRemoval reports what removing an account would do.
//
// It calls the same dependency query the removal decides on. Two queries would
// eventually disagree, and the disagreement would be a preview describing a
// removal that does something else.
func (s *Store) PreviewProviderAccountRemoval(
	ctx context.Context,
	providerID core.ProviderID,
	accountID core.ProviderAccountID,
) (*AccountRemovalPreview, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin removal preview: %w", err)
	}
	defer tx.Rollback()

	preview, err := previewAccountRemovalTx(ctx, tx, providerID, accountID)
	if err != nil {
		return nil, err
	}
	return preview, tx.Rollback()
}

// previewAccountRemovalTx builds the preview inside a caller's transaction, so
// the same evidence can serve a preview and a removal decision.
func previewAccountRemovalTx(
	ctx context.Context,
	tx *sql.Tx,
	providerID core.ProviderID,
	accountID core.ProviderAccountID,
) (*AccountRemovalPreview, error) {
	var credentialRef sql.NullString
	var label, status, updatedAt sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT credential_ref, label, status, updated_at FROM provider_accounts
		 WHERE id = ? AND provider_id = ?`,
		string(accountID), string(providerID)).Scan(&credentialRef, &label, &status, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("provider account not found: %s/%s", providerID, accountID)
	}
	if err != nil {
		return nil, fmt.Errorf("read account: %w", err)
	}

	// Whether a credential is actually held, not merely referenced. An account
	// row always names a reference; the row it names may not exist.
	credentialStored := false
	if credentialRef.Valid && credentialRef.String != "" {
		var count int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM provider_credentials WHERE credential_ref = ?",
			credentialRef.String).Scan(&count); err != nil {
			return nil, fmt.Errorf("check stored credential: %w", err)
		}
		credentialStored = count > 0
	}

	deps, err := accountDependenciesTx(ctx, tx, providerID, accountID)
	if err != nil {
		return nil, err
	}

	return &AccountRemovalPreview{
		ProviderID:       providerID,
		AccountID:        accountID,
		Label:            label.String,
		Status:           status.String,
		Dependencies:     deps,
		Removable:        len(deps) == 0,
		CredentialStored: credentialStored,
		Fingerprint: fingerprintAccountRemoval(
			string(providerID), string(accountID), credentialRef.String, updatedAt.String, deps),
		ObservedAt: time.Now().UTC(),
	}, nil
}

// fingerprintAccountRemoval identifies the subject a preview described.
//
// It hashes what makes the removal a different act: the account's identity, the
// credential it references, when the account was last written — so an account
// removed and re-created under the same key is a different subject — and the
// set of things that depend on it, so "nothing depends on this" is part of what
// was shown. Display text is excluded: a relabelled account is the same
// removal, and a preview must not expire because a sentence changed.
func fingerprintAccountRemoval(
	providerID, accountID, credentialRef, accountUpdatedAt string,
	deps AccountDependencies,
) string {
	canonical := make([]string, 0, len(deps))
	for _, dep := range deps {
		canonical = append(canonical, dep.Kind+"|"+dep.ID)
	}
	sort.Strings(canonical)

	h := sha256.New()
	for _, part := range append([]string{
		providerID, accountID, credentialRef, accountUpdatedAt,
	}, canonical...) {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DeleteProviderAccountIfUnused removes an account only if nothing depends on
// it, deciding and deleting inside one write transaction.
//
// The check and the delete used to be separate operations under separate locks.
// A connection created or edited between them would bind an account that was
// about to be deleted, and because the account reference lives inside the
// profile's driver JSON rather than behind a foreign key, nothing at the
// database level would refuse it. The connection would be stranded with a
// credential that no longer exists.
//
// It returns the dependencies when it refuses, so the caller can say what has
// to change rather than only that something does.
func (s *Store) DeleteProviderAccountIfUnused(
	ctx context.Context,
	providerID core.ProviderID,
	accountID core.ProviderAccountID,
	previewFingerprint string,
) (AccountDependencies, error) {
	// Required, not optional. Treating an empty fingerprint as "skip the check"
	// would make every caller that forgets it silently unchecked, which is the
	// opposite of what the check is for.
	if previewFingerprint == "" {
		return nil, fmt.Errorf("removing an account requires the fingerprint of the preview it was confirmed from")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin remove account: %w", err)
	}
	defer tx.Rollback()

	// The preview is recomputed here, inside the transaction that will delete,
	// so the decision and the deletion still cannot be separated.
	preview, err := previewAccountRemovalTx(ctx, tx, providerID, accountID)
	if err != nil {
		return nil, err
	}

	// Dependencies first. Someone with a blocking connection should be told
	// what blocks it, not that their preview expired.
	if len(preview.Dependencies) > 0 {
		return preview.Dependencies, nil
	}
	if preview.Fingerprint != previewFingerprint {
		return nil, &StalePreviewError{Expected: previewFingerprint, Current: preview}
	}

	var credentialRef sql.NullString
	if err := tx.QueryRowContext(ctx,
		"SELECT credential_ref FROM provider_accounts WHERE id = ? AND provider_id = ?",
		string(accountID), string(providerID)).Scan(&credentialRef); err != nil {
		return nil, fmt.Errorf("read account: %w", err)
	}

	if credentialRef.Valid && credentialRef.String != "" {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM provider_credentials WHERE credential_ref = ?", credentialRef.String); err != nil {
			return nil, fmt.Errorf("delete account credential: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM provider_accounts WHERE id = ? AND provider_id = ?",
		string(accountID), string(providerID)); err != nil {
		return nil, fmt.Errorf("delete account: %w", err)
	}

	// The record is written in the same transaction as the deletion. One that
	// could be committed separately would be able to lie in either direction:
	// a removal with no record, or a record of a removal that did not happen.
	credentialRemoved := 0
	if preview.CredentialStored {
		credentialRemoved = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_account_removals
			(provider_id, account_id, label, credential_ref, credential_removed,
			 preview_fingerprint, removed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(providerID), string(accountID), preview.Label,
		credentialRef.String, credentialRemoved,
		previewFingerprint, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return nil, fmt.Errorf("record account removal: %w", err)
	}

	return nil, tx.Commit()
}

// accountDependenciesTx finds everything that depends on an account, inside the
// caller's transaction so the answer cannot go stale before it is acted on.
func accountDependenciesTx(
	ctx context.Context,
	tx *sql.Tx,
	providerID core.ProviderID,
	accountID core.ProviderAccountID,
) (AccountDependencies, error) {
	var deps AccountDependencies

	rows, err := tx.QueryContext(ctx, "SELECT id, name, driver_json FROM connection_profiles")
	if err != nil {
		return nil, fmt.Errorf("list profiles for account dependency check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var driverJSON []byte
		if err := rows.Scan(&id, &name, &driverJSON); err != nil {
			return nil, fmt.Errorf("scan profile driver: %w", err)
		}
		var driver core.DriverSelection
		if err := json.Unmarshal(driverJSON, &driver); err != nil {
			// A profile whose driver cannot be decoded must not be silently
			// treated as independent of the account.
			return nil, fmt.Errorf("profile %s: decode driver: %w", id, err)
		}
		if driver.ProviderID == providerID && driver.AccountID == accountID {
			deps = append(deps, AccountDependency{
				Kind: "connection", ID: id, Name: name,
				Explanation: "this connection uses the account and could not open without it",
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// A resource Portico created and has not finished cleaning up needs the
	// credential that created it. Removing the account would leave the resource
	// at the provider with nothing able to remove it.
	cleanupRows, err := tx.QueryContext(ctx, `
		SELECT resource_type, external_id FROM resource_cleanup_items
		WHERE provider_id = ? AND provider_account_id = ?`,
		string(providerID), string(accountID))
	if err != nil {
		return nil, fmt.Errorf("list cleanup items for account dependency check: %w", err)
	}
	defer cleanupRows.Close()
	for cleanupRows.Next() {
		var resourceType, externalID string
		if err := cleanupRows.Scan(&resourceType, &externalID); err != nil {
			return nil, fmt.Errorf("scan cleanup item: %w", err)
		}
		deps = append(deps, AccountDependency{
			Kind: "cleanup_item", ID: externalID,
			Name: resourceType + " " + externalID,
			Explanation: "Portico still has to remove this from the provider, " +
				"and this account's credential is what can do it",
		})
	}
	return deps, cleanupRows.Err()
}

// ConnectionsUsingAccount returns the connections whose driver selects the
// given provider account, so account removal can report what it would strand
// instead of failing later with an unexplained error.
func (s *Store) ConnectionsUsingAccount(ctx context.Context, providerID core.ProviderID, accountID core.ProviderAccountID) ([]core.ConnectionID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, "SELECT id, driver_json FROM connection_profiles")
	if err != nil {
		return nil, fmt.Errorf("list profiles for account dependency check: %w", err)
	}
	defer rows.Close()

	var dependents []core.ConnectionID
	for rows.Next() {
		var id core.ConnectionID
		var driverJSON []byte
		if err := rows.Scan(&id, &driverJSON); err != nil {
			return nil, fmt.Errorf("scan profile driver: %w", err)
		}
		var driver core.DriverSelection
		if err := json.Unmarshal(driverJSON, &driver); err != nil {
			// A profile whose driver cannot be decoded must not be silently
			// treated as independent of the account.
			return nil, fmt.Errorf("profile %s: decode driver: %w", id, err)
		}
		if driver.ProviderID == providerID && driver.AccountID == accountID {
			dependents = append(dependents, id)
		}
	}
	return dependents, rows.Err()
}

// SchemaVersion returns the highest applied migration version. A support export
// needs it to tell whether a reported problem belongs to a database that has
// been upgraded.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var version sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}
