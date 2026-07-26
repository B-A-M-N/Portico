package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildV7Fixture creates a database migrated only through version 7 and
// seeds duplicate provider resource rows (same provider/type/external ID
// owned by two different connections).
func buildV7Fixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()

	for _, m := range migrations {
		if m.version > 7 {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin migration %d: %v", m.version, err)
		}
		if m.onApply != nil {
			err = m.onApply(tx)
		} else if m.sql != "" {
			_, err = tx.Exec(m.sql)
		}
		if err != nil {
			tx.Rollback()
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, ?)",
			m.version, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			tx.Rollback()
			t.Fatalf("record migration %d: %v", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit migration %d: %v", m.version, err)
		}
	}

	// Recreate provider_resources in its true pre-v8 shape (no unique
	// key), which is what allowed duplicate ownership rows to exist.
	if _, err := db.Exec(`DROP TABLE provider_resources`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE provider_resources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			connection_id TEXT NOT NULL,
			provider_id TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			external_id TEXT NOT NULL,
			ownership TEXT NOT NULL,
			spec_hash TEXT,
			metadata_json BLOB,
			lifecycle TEXT,
			created_at TEXT NOT NULL
		)`); err != nil {
		t.Fatalf("recreate legacy table: %v", err)
	}

	// Seed duplicate ownership rows (allowed pre-v8: no unique constraint).
	now := time.Now().UTC().Format(time.RFC3339)
	for _, connID := range []string{"conn-a", "conn-b"} {
		if _, err := db.Exec(`
			INSERT INTO provider_resources
				(connection_id, provider_id, resource_type, external_id, ownership, created_at)
			VALUES (?, 'cloudflare', 'tunnel', 'tun-dup', 'managed', ?)`,
			connID, now); err != nil {
			t.Fatalf("seed duplicate for %s: %v", connID, err)
		}
	}
}

// Migration 8 must record duplicate-ownership conflicts, keep a single
// surviving row, demote its ownership to external (no automatic deletion
// authority), and back up the database before the destructive rebuild.
func TestMigration8DuplicateOwnershipFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v7.db")
	buildV7Fixture(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate v8+): %v", err)
	}
	defer s.Close()

	// Exactly one surviving row for the duplicated key.
	var count int
	if err := s.DB().QueryRow(
		"SELECT COUNT(*) FROM provider_resources WHERE provider_id='cloudflare' AND resource_type='tunnel' AND external_id='tun-dup'").
		Scan(&count); err != nil {
		t.Fatalf("count survivors: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 surviving row, got %d", count)
	}

	// Survivor must be demoted to external ownership.
	var ownership string
	if err := s.DB().QueryRow(
		"SELECT ownership FROM provider_resources WHERE external_id='tun-dup'").Scan(&ownership); err != nil {
		t.Fatalf("read ownership: %v", err)
	}
	if ownership != "external" {
		t.Fatalf("expected demoted ownership 'external', got %q", ownership)
	}

	// Conflict must be recorded for manual review.
	var conflicts int
	if err := s.DB().QueryRow(
		"SELECT COUNT(*) FROM migration_conflicts WHERE migration_version=8 AND conflict_type='duplicate_resource_key'").
		Scan(&conflicts); err != nil {
		t.Fatalf("count conflicts: %v", err)
	}
	if conflicts == 0 {
		t.Fatal("expected a recorded migration conflict")
	}

	// A pre-migration backup file must exist.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	foundBackup := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".backup-") {
			foundBackup = true
			break
		}
	}
	if !foundBackup {
		t.Fatal("expected pre-migration backup file")
	}
}

// A clean v7 database without duplicates migrates without recording conflicts.
func TestMigration8NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clean-v7.db")

	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	for _, m := range migrations {
		if m.version > 7 {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin migration %d: %v", m.version, err)
		}
		if m.onApply != nil {
			err = m.onApply(tx)
		} else if m.sql != "" {
			_, err = tx.Exec(m.sql)
		}
		if err != nil {
			tx.Rollback()
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, ?)",
			m.version, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			tx.Rollback()
			t.Fatalf("record migration %d: %v", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit migration %d: %v", m.version, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var conflicts int
	err = s.DB().QueryRow("SELECT COUNT(*) FROM migration_conflicts").Scan(&conflicts)
	if err == nil && conflicts != 0 {
		t.Fatalf("expected no conflicts on clean migration, got %d", conflicts)
	}
	// err != nil means the conflicts table was never created — also fine.
}

func TestMigration15PreservesCredentialAndAllowsReplacementTunnel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v14-credentials.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	for _, m := range migrations {
		if m.version > 14 {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin migration %d: %v", m.version, err)
		}
		if m.onApply != nil {
			err = m.onApply(tx)
		} else if m.sql != "" {
			_, err = tx.Exec(m.sql)
		}
		if err != nil {
			tx.Rollback()
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
		if _, err := tx.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", m.version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			t.Fatalf("record migration %d: %v", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit migration %d: %v", m.version, err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO tunnel_credentials
		(connection_id, provider_id, tunnel_id, token_encrypted, created_at, updated_at)
		VALUES ('conn-1', 'cloudflare', 'tunnel-old', X'0102', ?, ?)`, now, now); err != nil {
		db.Close()
		t.Fatalf("seed v14 credential: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(`INSERT INTO tunnel_credentials
		(connection_id, provider_id, tunnel_id, token_encrypted, created_at, updated_at)
		VALUES ('conn-1', 'cloudflare', 'tunnel-new', X'0304', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert replacement credential after migration: %v", err)
	}
	var count int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM tunnel_credentials WHERE connection_id = 'conn-1'").Scan(&count); err != nil {
		t.Fatalf("count migrated credentials: %v", err)
	}
	if count != 2 {
		t.Fatalf("credential count = %d, want 2", count)
	}
}

func TestOpenRefusesNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
		INSERT INTO schema_migrations(version, applied_at) VALUES (999, 'future');`); err != nil {
		db.Close()
		t.Fatalf("seed future schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("expected future schema refusal, got %v", err)
	}
}

// TestMigration2UpgradesReleasedV1Schema protects the real upgrade path from
// Portico's first shipped database. That schema differs from the later source
// definition of migration 1, so migration 2 must normalize it before later
// migrations assume modern columns exist.
func TestMigration2UpgradesReleasedV1Schema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "released-v1.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	_, err = db.Exec(`
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
INSERT INTO schema_migrations VALUES (1, '2026-01-01T00:00:00Z');
CREATE TABLE connection_profiles (
    id TEXT PRIMARY KEY, name TEXT NOT NULL, revision INTEGER NOT NULL,
    source_json BLOB NOT NULL, exposure_json BLOB NOT NULL, protection_json BLOB NOT NULL,
    provider_json BLOB NOT NULL, lifecycle_json BLOB NOT NULL, desired_state TEXT NOT NULL,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
INSERT INTO connection_profiles VALUES ('conn-1', 'legacy', 1, '{}', '{}', '{}', '{}', '{}', 'closed', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
CREATE TABLE provider_resources (
    id INTEGER PRIMARY KEY AUTOINCREMENT, connection_id TEXT NOT NULL, provider_id TEXT NOT NULL,
    resource_type TEXT NOT NULL, external_id TEXT NOT NULL, ownership TEXT NOT NULL,
    spec_hash TEXT, metadata_json BLOB, UNIQUE(provider_id, resource_type, external_id)
);
INSERT INTO provider_resources (connection_id, provider_id, resource_type, external_id, ownership)
VALUES ('conn-1', 'cloudflare', 'tunnel', 'tun-1', 'managed');
CREATE TABLE diagnostic_findings (
    id TEXT PRIMARY KEY, connection_id TEXT NOT NULL, segment TEXT NOT NULL, severity TEXT NOT NULL,
    summary TEXT NOT NULL, explanation TEXT, evidence_json BLOB, repair_options_json BLOB,
    observed_at TEXT NOT NULL, resolved_at TEXT
);
INSERT INTO diagnostic_findings VALUES ('finding-1', 'conn-1', 'connector', 'error', 'stopped', '', '[]', '[]', '2026-01-02T00:00:00Z', NULL);
CREATE TABLE operation_plans (
    id TEXT PRIMARY KEY, connection_id TEXT NOT NULL, profile_revision INTEGER NOT NULL,
    provider_id TEXT NOT NULL, intent TEXT NOT NULL, plan_json BLOB NOT NULL,
    fingerprint TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL
);
INSERT INTO operation_plans VALUES ('plan-1', 'conn-1', 1, 'cloudflare', 'open',
    '{"Steps":[{"ID":"step-1","Kind":"validate_account"}]}', 'fingerprint',
    '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z');
CREATE TABLE operations (
    id TEXT PRIMARY KEY, plan_id TEXT NOT NULL, connection_id TEXT NOT NULL, state TEXT NOT NULL,
    started_at TEXT NOT NULL, completed_at TEXT, error_text TEXT
);
INSERT INTO operations VALUES ('op-1', 'plan-1', 'conn-1', 'failed', '2026-01-01T00:00:00Z', '2026-01-01T00:01:00Z', 'legacy error');
CREATE TABLE operation_events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT, operation_id TEXT, connection_id TEXT,
    occurred_at TEXT NOT NULL, event_type TEXT NOT NULL, stage TEXT, payload_json BLOB NOT NULL
);
INSERT INTO operation_events (operation_id, connection_id, occurred_at, event_type, stage, payload_json)
VALUES ('op-1', 'conn-1', '2026-01-01T00:00:30Z', 'operation.failed', 'failed', '{}');
CREATE TABLE idempotency_keys (key_hash TEXT PRIMARY KEY, plan_id TEXT, created_at TEXT NOT NULL);
INSERT INTO idempotency_keys VALUES ('legacy-key', 'plan-1', '2026-01-01T00:00:00Z');
CREATE TABLE connection_runtime (
    connection_id TEXT PRIMARY KEY, runtime_state TEXT NOT NULL, provider_id TEXT,
    public_address TEXT, private_address TEXT, connector_json BLOB, provider_runtime_json BLOB,
    endpoint_json BLOB, diagnostics_json BLOB, active_operation_id TEXT, error_json BLOB,
    last_observation TEXT, last_transition TEXT
);
CREATE TABLE provider_accounts (
    id TEXT PRIMARY KEY, provider_id TEXT NOT NULL, label TEXT NOT NULL, credential_ref TEXT,
    metadata_json BLOB, status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);`)
	if err != nil {
		db.Close()
		t.Fatalf("seed released v1 schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy v1: %v", err)
	}
	defer s.Close()

	var findingCreatedAt, resourceCreatedAt, steps, failure, eventType, key string
	if err := s.DB().QueryRow("SELECT created_at FROM findings WHERE id = 'finding-1'").Scan(&findingCreatedAt); err != nil {
		t.Fatalf("read migrated finding: %v", err)
	}
	if findingCreatedAt != "2026-01-02T00:00:00Z" {
		t.Fatalf("finding creation time = %q", findingCreatedAt)
	}
	if err := s.DB().QueryRow("SELECT created_at FROM provider_resources WHERE external_id = 'tun-1'").Scan(&resourceCreatedAt); err != nil {
		t.Fatalf("read migrated resource: %v", err)
	}
	if resourceCreatedAt != "" {
		t.Fatalf("legacy resource creation time = %q, want empty/unknown", resourceCreatedAt)
	}
	if err := s.DB().QueryRow("SELECT steps_json FROM operation_plans WHERE id = 'plan-1'").Scan(&steps); err != nil {
		t.Fatalf("read migrated plan: %v", err)
	}
	if !strings.Contains(steps, "step-1") {
		t.Fatalf("migrated steps missing legacy step: %s", steps)
	}
	if err := s.DB().QueryRow("SELECT error_json FROM operations WHERE id = 'op-1'").Scan(&failure); err != nil {
		t.Fatalf("read migrated operation error: %v", err)
	}
	if !strings.Contains(failure, "legacy error") {
		t.Fatalf("migrated failure missing message: %s", failure)
	}
	if err := s.DB().QueryRow("SELECT event_type FROM operation_events WHERE operation_id = 'op-1'").Scan(&eventType); err != nil {
		t.Fatalf("read migrated event: %v", err)
	}
	if eventType != "operation.failed" {
		t.Fatalf("event type = %q", eventType)
	}
	if err := s.DB().QueryRow("SELECT key FROM idempotency_keys WHERE key = 'legacy-key'").Scan(&key); err != nil {
		t.Fatalf("read migrated idempotency key: %v", err)
	}
	if key != "legacy-key" {
		t.Fatalf("idempotency key = %q", key)
	}
}
