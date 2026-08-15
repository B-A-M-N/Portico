package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
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
);
-- Accounts predating the provider-scoped key. credential_ref is written as ''
-- rather than NULL on purpose: the column is nullable and ListProviderAccounts
-- scans it into a plain string, which is a separate defect that must not be
-- conflated with this migration.
INSERT INTO provider_accounts VALUES
    ('acct-1', 'cloudflare', 'Personal', 'cloudflare:acct-1:api-token',
     '{"zone_id":"zone-a"}', 'authenticated', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z'),
    ('acct-2', 'ngrok', 'Work', 'ngrok:acct-2:api-token',
     '', 'pending', '2026-01-03T00:00:00Z', '2026-01-04T00:00:00Z');`)
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

	// Migration 19 rebuilds provider_accounts around a provider-scoped key.
	// Every column must survive the rebuild byte for byte: the copy moves
	// metadata as an opaque blob and timestamps verbatim, so nothing is
	// recomputed and no credential reference is rewritten.
	type accountRow struct{ label, ref, meta, status, created, updated string }
	accounts := map[string]accountRow{}
	rows, err := s.DB().Query(
		`SELECT id, provider_id, label, credential_ref, COALESCE(metadata_json, ''), status, created_at, updated_at
		 FROM provider_accounts`)
	if err != nil {
		t.Fatalf("read migrated provider accounts: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, providerID string
		var row accountRow
		if err := rows.Scan(&id, &providerID, &row.label, &row.ref, &row.meta,
			&row.status, &row.created, &row.updated); err != nil {
			t.Fatalf("scan migrated provider account: %v", err)
		}
		accounts[providerID+"/"+id] = row
	}
	if len(accounts) != 2 {
		t.Fatalf("migrated %d provider accounts, want 2", len(accounts))
	}
	cf, ok := accounts["cloudflare/acct-1"]
	if !ok {
		t.Fatalf("cloudflare account did not survive migration: %#v", accounts)
	}
	if cf.label != "Personal" || cf.ref != "cloudflare:acct-1:api-token" ||
		cf.meta != `{"zone_id":"zone-a"}` || cf.status != "authenticated" ||
		cf.created != "2026-01-01T00:00:00Z" || cf.updated != "2026-01-02T00:00:00Z" {
		t.Fatalf("cloudflare account changed during migration: %#v", cf)
	}
	// A pending account must not be quietly promoted by the rebuild.
	if ng := accounts["ngrok/acct-2"]; ng.status != "pending" || ng.created != "2026-01-03T00:00:00Z" {
		t.Fatalf("ngrok account changed during migration: %#v", ng)
	}

	// The new key must actually be in force: the same account ID under a
	// different provider is a different row, and re-upserting one provider's
	// account must not disturb the other's.
	ctx := context.Background()
	if err := s.UpsertProviderAccount(ctx, core.ProviderAccount{
		ID: "acct-1", Provider: "ngrok", Label: "Same name, other provider",
		CredentialRef: "ngrok:acct-1:api-token", Status: core.AccountAuthenticated,
	}); err != nil {
		t.Fatalf("insert same account ID under another provider: %v", err)
	}
	var cfLabel string
	if err := s.DB().QueryRow(
		`SELECT label FROM provider_accounts WHERE provider_id = 'cloudflare' AND id = 'acct-1'`,
	).Scan(&cfLabel); err != nil {
		t.Fatalf("read cloudflare account after cross-provider insert: %v", err)
	}
	if cfLabel != "Personal" {
		t.Fatalf("another provider's account overwrote the Cloudflare one: label = %q", cfLabel)
	}
}

// seedV17ProfileSchema creates a database at schema version 17: the
// service-exposure-only connection_profiles shape, plus a dependent table
// holding an immediate foreign key to it. Migration 18 is the only migration
// left to run, which keeps the fixture focused on the conversion under test.
//
// The dependent row matters: connection_profiles is a parent table, so a
// migration that rebuilt it by dropping it would orphan these rows and fail
// under the DSN's foreign_keys=on.
func seedV17ProfileSchema(t *testing.T, path string, profileRow []any) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE connection_profiles (
    id TEXT PRIMARY KEY, name TEXT NOT NULL, revision INTEGER NOT NULL,
    source_json BLOB NOT NULL, exposure_json BLOB NOT NULL, protection_json BLOB NOT NULL,
    provider_json BLOB NOT NULL, lifecycle_json BLOB NOT NULL, desired_state TEXT NOT NULL,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE operation_plans (
    id TEXT PRIMARY KEY, connection_id TEXT NOT NULL, profile_revision INTEGER NOT NULL,
    provider_id TEXT NOT NULL, intent TEXT NOT NULL, steps_json BLOB NOT NULL, fingerprint TEXT NOT NULL,
    created_at TEXT NOT NULL, expires_at TEXT, UNIQUE(connection_id, fingerprint)
);
CREATE TABLE operations (
    id TEXT PRIMARY KEY, plan_id TEXT NOT NULL, connection_id TEXT NOT NULL,
    state TEXT NOT NULL, started_at TEXT NOT NULL, completed_at TEXT, error_json BLOB,
    FOREIGN KEY(plan_id) REFERENCES operation_plans(id)
);
CREATE TABLE operation_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT, operation_id TEXT NOT NULL, step_id TEXT,
    event_type TEXT NOT NULL, stage TEXT NOT NULL, message TEXT, error TEXT, event_at TEXT NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES operations(id)
);
CREATE TABLE provider_accounts (
    id TEXT PRIMARY KEY, provider_id TEXT NOT NULL, label TEXT NOT NULL, credential_ref TEXT,
    metadata_json BLOB, status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);`); err != nil {
		t.Fatalf("seed v17 schema: %v", err)
	}
	for v := 1; v <= 17; v++ {
		if _, err := db.Exec("INSERT INTO schema_migrations VALUES (?, ?)", v, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("stamp migration %d: %v", v, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO connection_profiles VALUES (?,?,?,?,?,?,?,?,?,?,?)`, profileRow...); err != nil {
		t.Fatalf("seed legacy profile: %v", err)
	}
	// Seed a dependent operation (migration 19 rebuilds the operations table to reference operation_plans).
	if _, err := db.Exec(`INSERT INTO operation_plans VALUES ('plan-1','conn-legacy',3,'cloudflare','open','[]','fp-1','2026-01-01T00:00:00Z',NULL)`); err != nil {
		t.Fatalf("seed dependent plan: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO operations VALUES ('op-1','plan-1','conn-legacy','completed','2026-01-01T00:00:00Z',NULL,NULL)`); err != nil {
		t.Fatalf("seed dependent operation: %v", err)
	}
}

func legacyProfileRow(sourceJSON string) []any {
	return []any{
		"conn-legacy", "legacy connection", 3,
		sourceJSON,
		`{"Mode":"permanent_public","Protocol":"http","RequestedAddress":"legacy.example.com","Expiration":null}`,
		`{"Kind":"email_otp","AllowedEmails":["person@example.com"],"AllowedDomains":null,"SessionTTL":1800000000000}`,
		`{"driver_id":"","provider_id":"cloudflare","account_id":"acct-1"}`,
		`{"AutoStart":true,"OnDisconnect":"keep_alive"}`,
		"open", "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z",
	}
}

const legacySourceJSON = `{"Kind":"existing_service","Existing":{"Network":"tcp","Address":"127.0.0.1:3000","Protocol":"http","Health":{"Enabled":false,"Path":"","Timeout":0,"Interval":0}},"Directory":null,"Command":null,"MCP":null}`

// TestMigration18ConvertsLegacyProfileAndPreservesDependents verifies the
// in-place conversion to versioned tagged-spec storage: the connection kind
// becomes durable rather than inferred, the spec union is stored whole, the
// legacy columns are removed, and dependent foreign-key rows survive.
func TestMigration18ConvertsLegacyProfileAndPreservesDependents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v17.db")
	seedV17ProfileSchema(t, path, legacyProfileRow(legacySourceJSON))

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migration 18): %v", err)
	}
	defer s.Close()

	var kind, specJSON, driverJSON string
	var specVersion int
	if err := s.DB().QueryRow(
		"SELECT kind, spec_version, spec_json, driver_json FROM connection_profiles WHERE id='conn-legacy'",
	).Scan(&kind, &specVersion, &specJSON, &driverJSON); err != nil {
		t.Fatalf("read converted row: %v", err)
	}
	if kind != "service_exposure" {
		t.Fatalf("kind = %q, want service_exposure", kind)
	}
	if specVersion != currentProfileSpecVersion {
		t.Fatalf("spec_version = %d, want %d", specVersion, currentProfileSpecVersion)
	}
	for _, want := range []string{"service_exposure", "127.0.0.1:3000", "legacy.example.com", "person@example.com"} {
		if !strings.Contains(specJSON, want) {
			t.Fatalf("spec_json missing %q: %s", want, specJSON)
		}
	}
	if !strings.Contains(driverJSON, "cloudflare") || !strings.Contains(driverJSON, "acct-1") {
		t.Fatalf("driver_json did not carry the legacy provider selection: %s", driverJSON)
	}

	// The legacy columns must actually be gone: leaving them behind would keep
	// a second, silently divergent source of truth for the same state.
	for _, gone := range []string{"source_json", "exposure_json", "protection_json", "provider_json"} {
		if columnExists(t, s.DB(), "connection_profiles", gone) {
			t.Fatalf("legacy column %s still present after migration 18", gone)
		}
	}

	// The dependent row must survive and the database must be referentially clean.
	var opCount int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM operations WHERE connection_id='conn-legacy'").Scan(&opCount); err != nil {
		t.Fatalf("count dependents: %v", err)
	}
	if opCount != 1 {
		t.Fatalf("dependent operations = %d, want 1", opCount)
	}
	rows, err := s.DB().Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("migration 18 left foreign key violations")
	}

	// The converted row must load back as a complete, valid profile.
	loaded, err := s.LoadProfile(t.Context(), "conn-legacy")
	if err != nil {
		t.Fatalf("LoadProfile after migration: %v", err)
	}
	if loaded.Kind != "service_exposure" {
		t.Fatalf("loaded Kind = %q", loaded.Kind)
	}
	if loaded.Spec.ServiceExposure == nil {
		t.Fatal("loaded ServiceExposure arm is nil")
	}
	if got := loaded.Spec.ServiceExposure.Source.Existing; got == nil || got.Address != "127.0.0.1:3000" {
		t.Fatalf("loaded source = %+v", got)
	}
	if loaded.Spec.ServiceExposure.Exposure.RequestedAddress != "legacy.example.com" {
		t.Fatalf("loaded exposure address = %q", loaded.Spec.ServiceExposure.Exposure.RequestedAddress)
	}
	if loaded.Driver.ProviderID != "cloudflare" {
		t.Fatalf("loaded driver provider = %q", loaded.Driver.ProviderID)
	}
	if loaded.Revision != 3 {
		t.Fatalf("loaded revision = %d, want 3", loaded.Revision)
	}
}

// TestMigration18AbortsOnUndecodableLegacyRow ensures a corrupt legacy row
// fails the whole migration rather than being partially converted.
func TestMigration18AbortsOnUndecodableLegacyRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	seedV17ProfileSchema(t, path, legacyProfileRow(`{"Kind":`))

	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("Open succeeded despite an undecodable legacy profile row")
	}
	if !strings.Contains(err.Error(), "conn-legacy") {
		t.Fatalf("migration error does not identify the offending row: %v", err)
	}

	// The transaction must have rolled back, leaving the legacy shape intact.
	db, dbErr := sql.Open("sqlite3", path)
	if dbErr != nil {
		t.Fatalf("reopen: %v", dbErr)
	}
	defer db.Close()
	if !columnExists(t, db, "connection_profiles", "source_json") {
		t.Fatal("migration 18 partially applied: source_json was dropped despite failure")
	}
	var applied int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = 18").Scan(&applied); err != nil {
		t.Fatalf("check migration record: %v", err)
	}
	if applied != 0 {
		t.Fatal("migration 18 was recorded as applied despite failing")
	}
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

// TestMigration18TakesRecoveryBackupAndIsIdempotent covers the two properties a
// destructive migration must have: it leaves a recoverable copy of the
// pre-migration database, and reopening an already-migrated database is a
// no-op rather than a second attempt to add and drop the same columns.
func TestMigration18TakesRecoveryBackupAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v17.db")
	seedV17ProfileSchema(t, path, legacyProfileRow(legacySourceJSON))

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Migration 18 drops columns. The pre-migration backup is the only way back.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var backups []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".backup-") {
			backups = append(backups, e.Name())
		}
	}
	if len(backups) == 0 {
		t.Fatal("no pre-migration backup was taken before a destructive migration")
	}

	// The backup must still carry the legacy shape, otherwise it is not a
	// recovery point for this migration.
	backupDB, err := sql.Open("sqlite3", filepath.Join(dir, backups[0]))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer backupDB.Close()
	if !columnExists(t, backupDB, "connection_profiles", "source_json") {
		t.Fatal("backup does not contain the pre-migration schema")
	}

	// Reopening must not re-run migration 18.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	defer s2.Close()

	loaded, err := s2.LoadProfile(t.Context(), "conn-legacy")
	if err != nil {
		t.Fatalf("LoadProfile after reopen: %v", err)
	}
	if loaded.Kind != "service_exposure" || loaded.Spec.ServiceExposure == nil {
		t.Fatalf("profile degraded across reopen: kind=%q arm=%v", loaded.Kind, loaded.Spec.ServiceExposure)
	}
	var applied int
	if err := s2.DB().QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = 18").Scan(&applied); err != nil {
		t.Fatalf("count migration 18 records: %v", err)
	}
	if applied != 1 {
		t.Fatalf("migration 18 recorded %d times, want 1", applied)
	}
}

// seedPreScopedAccountsDB writes a database carrying the pre-19 account shape.
func seedPreScopedAccountsDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := db.Exec(`
CREATE TABLE provider_accounts (
    id TEXT PRIMARY KEY, provider_id TEXT NOT NULL, label TEXT NOT NULL, credential_ref TEXT,
    metadata_json BLOB, status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
INSERT INTO provider_accounts VALUES
    ('acct-1','cloudflare','Personal','cloudflare:acct-1:api-token','{"zone_id":"z"}','authenticated','c1','u1'),
    ('acct-2','ngrok','Work','ngrok:acct-2:api-token','','pending','c2','u2');`); err != nil {
		db.Close()
		t.Fatalf("seed pre-19 accounts: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}
}

// accountsTableDDL returns the stored CREATE statement for provider_accounts.
func accountsTableDDL(t *testing.T, s *Store) string {
	t.Helper()
	var ddl string
	if err := s.DB().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='provider_accounts'`).Scan(&ddl); err != nil {
		t.Fatalf("read provider_accounts DDL: %v", err)
	}
	return ddl
}

// TestFreshAndMigratedDatabasesHaveTheSameAccountSchema guards against the
// classic migration split, where new installs and upgraded ones diverge and
// only one of them is ever tested.
func TestFreshAndMigratedDatabasesHaveTheSameAccountSchema(t *testing.T) {
	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	fresh, err := Open(freshPath)
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	defer fresh.Close()

	migratedPath := filepath.Join(t.TempDir(), "migrated.db")
	seedPreScopedAccountsDB(t, migratedPath)
	migrated, err := Open(migratedPath)
	if err != nil {
		t.Fatalf("Open migrating: %v", err)
	}
	defer migrated.Close()

	if got, want := accountsTableDDL(t, migrated), accountsTableDDL(t, fresh); got != want {
		t.Fatalf("migrated schema differs from fresh:\nmigrated:\n%s\nfresh:\n%s", got, want)
	}

	// The rebuild must not touch the rows it copies. A pending account being
	// silently promoted here would defeat the verification invariant at the
	// one moment nobody is watching.
	var status, createdAt string
	if err := migrated.DB().QueryRow(
		`SELECT status, created_at FROM provider_accounts WHERE provider_id='ngrok' AND id='acct-2'`,
	).Scan(&status, &createdAt); err != nil {
		t.Fatalf("read migrated pending account: %v", err)
	}
	if status != "pending" || createdAt != "c2" {
		t.Fatalf("migration altered a pending account: status=%q created_at=%q", status, createdAt)
	}
}

// TestProviderAccountMigrationIsIdempotent ensures reopening an already
// migrated database neither reruns the rebuild nor disturbs the rows.
func TestProviderAccountMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repeat.db")
	seedPreScopedAccountsDB(t, path)

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	ddl := accountsTableDDL(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	defer second.Close()

	if got := accountsTableDDL(t, second); got != ddl {
		t.Fatalf("schema changed on reopen:\n%s\n---\n%s", got, ddl)
	}
	var applied int
	if err := second.DB().QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 19`).Scan(&applied); err != nil {
		t.Fatalf("read migration record: %v", err)
	}
	if applied != 1 {
		t.Fatalf("migration 19 recorded %d times, want 1", applied)
	}
	var accounts int
	if err := second.DB().QueryRow(`SELECT COUNT(*) FROM provider_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accounts != 2 {
		t.Fatalf("accounts after reopen = %d, want 2", accounts)
	}
}
