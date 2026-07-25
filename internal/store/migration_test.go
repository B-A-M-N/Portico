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
