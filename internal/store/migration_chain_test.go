package store

import (
	"context"
	"path/filepath"
	"testing"
)

// The migration chain itself.
//
// Individual migrations are covered by their own tests. Nothing checked the
// shape of the list they live in, and the list is edited by hand every time the
// schema changes — which is exactly the kind of thing that ships broken. A
// duplicate version silently skips a migration, because the runner records
// versions and skips any it has already applied. A gap or an out-of-order entry
// makes the on-disk version number mean something different from what the
// newer-schema refusal at runMigrations assumes.

// TestTheMigrationChainIsWellFormed pins the properties the runner depends on.
func TestTheMigrationChainIsWellFormed(t *testing.T) {
	if len(migrations) == 0 {
		t.Fatal("there are no migrations")
	}

	seen := map[int]bool{}
	for i, m := range migrations {
		if m.version <= 0 {
			t.Errorf("migration at index %d has version %d", i, m.version)
		}
		if seen[m.version] {
			// The runner skips a version it has already recorded, so the second
			// migration claiming a version never runs — on a fresh database as
			// well as an upgraded one.
			t.Errorf("version %d appears more than once", m.version)
		}
		seen[m.version] = true

		if m.sql == "" && m.onApply == nil {
			t.Errorf("migration %d does nothing", m.version)
		}
		if i > 0 && m.version <= migrations[i-1].version {
			t.Errorf("migration %d follows %d: the list must be strictly increasing",
				m.version, migrations[i-1].version)
		}
	}

	// Contiguity. A gap makes "schema version N" ambiguous about which
	// migrations have run, which is what the newer-than-supported refusal in
	// runMigrations compares against.
	for v := 1; v <= migrations[len(migrations)-1].version; v++ {
		if !seen[v] {
			t.Errorf("version %d is missing from the chain", v)
		}
	}
}

// TestAFreshDatabaseAppliesEveryMigrationExactlyOnce pins that a new install
// ends up at the current schema, with each migration recorded once.
//
// This is the path every first-time user takes, and the one least likely to be
// exercised by a developer whose database was created months ago.
func TestAFreshDatabaseAppliesEveryMigrationExactlyOnce(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	rows, err := st.db.Query("SELECT version, COUNT(*) FROM schema_migrations GROUP BY version")
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()

	applied := map[int]int{}
	for rows.Next() {
		var version, count int
		if err := rows.Scan(&version, &count); err != nil {
			t.Fatal(err)
		}
		applied[version] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, m := range migrations {
		switch applied[m.version] {
		case 1:
			// as expected
		case 0:
			t.Errorf("migration %d did not run on a fresh database", m.version)
		default:
			t.Errorf("migration %d ran %d times", m.version, applied[m.version])
		}
	}
	if len(applied) != len(migrations) {
		t.Errorf("a fresh database recorded %d migrations, want %d", len(applied), len(migrations))
	}
}

// TestReopeningADatabaseAppliesNothingFurther pins that migrations are not
// re-run on every start, which would make a data-transforming migration apply
// its transformation repeatedly.
func TestReopeningADatabaseAppliesNothingFurther(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var before int
	if err := first.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	var after int
	if err := second.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("reopening applied %d further migrations", after-before)
	}
}

// TestAFreshDatabaseIsImmediatelyUsable pins that the schema a new install ends
// up with actually supports the operations Portico performs on it. A chain that
// applies cleanly and leaves an unusable schema is still a broken release.
func TestAFreshDatabaseIsImmediatelyUsable(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "usable.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	state, err := st.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("a fresh database cannot be read: %v", err)
	}
	if len(state.Profiles) != 0 {
		t.Fatalf("a fresh database reports %d connections", len(state.Profiles))
	}
	if _, err := st.ListRecentOperations(ctx, 10); err != nil {
		t.Fatalf("a fresh database cannot report operations: %v", err)
	}
}
