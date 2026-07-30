# Connection profile persistence — design contract

Status: accepted (architect pass, Tier 3)
Scope: `internal/store/sqlite.go` profile schema and codecs.

This is an implementation contract. Every statement below was checked against
the repository at the time of writing; the implementer must re-verify each
assumption before editing.

## Invariant

A connection profile's durable representation must record its connection kind
explicitly and store exactly one spec union arm, in a versioned encoding that a
future binary can either read or refuse. No profile may be reconstructed by
inferring its kind from the schema shape.

## Verified repository facts

These were confirmed before the contract was written.

1. `connection_profiles` is service-exposure-specific: it has `source_json`,
   `exposure_json`, `protection_json`, `provider_json`, and no kind or spec
   column (`internal/store/sqlite.go:51`).
2. Every read and write of those columns is confined to
   `internal/store/sqlite.go`. No other package selects from the table. The only
   external reference is a legacy fixture in `internal/store/migration_test.go`.
3. `HandleCreateConnection` hardcodes `Kind: core.ConnectionServiceExposure`
   (`internal/supervisor/supervisor.go`), so no other kind can currently be
   created through any supported path.
4. Migrations are versioned, applied in a single transaction each, and the
   runner takes a WAL-checkpointed file backup before any pending migration
   (`runMigrations`). A schema newer than the binary is already refused.
5. Migration 15 establishes the table-rebuild pattern
   (`CREATE ... _new` / `INSERT SELECT` / `DROP` / `RENAME`) used in this
   codebase for incompatible shape changes.
6. `SourceSpec` carries no JSON struct tags; `ServiceExposureSpec` and
   `ConnectionSpec` do. Existing `source_json` values were produced by
   marshalling `SourceSpec` directly, so re-marshalling through Go preserves the
   exact encoding.
7. Post Phase 0, the store refuses non-service-exposure kinds with a typed
   error, and both load paths currently *infer*
   `Kind = ConnectionServiceExposure`. That inference is the durable-state
   defect this package removes.

## Decisions

### D1 — Migrate in place with ADD COLUMN / backfill / DROP COLUMN

The legacy columns are `NOT NULL`. Adding `spec_json` alongside them and leaving
them populated would create two sources of truth for the same state and a
silent-divergence class of bug. The legacy columns must actually go away.

**A table rebuild cannot be used here.** The first draft of this contract
specified the migration 15 rebuild pattern
(`CREATE _new` / `INSERT SELECT` / `DROP` / `RENAME`). That was falsified
empirically before implementation:

- `connection_profiles` is a *parent* table; `operations`, `resources` and
  `findings` hold immediate (non-deferred) foreign keys to
  `connection_profiles(id)`.
- With `_foreign_keys=on` (set in the DSN), `DROP TABLE` performs an implicit
  `DELETE FROM`, which violates those child constraints. A probe reproduced
  this exactly: `FOREIGN KEY constraint failed`.
- `PRAGMA foreign_keys` is a no-op inside a transaction, and the migration
  runner executes every migration inside one, so FK enforcement cannot be
  suspended without changing the migration machinery.

Migration 15's precedent does not transfer because `tunnel_credentials` has no
dependents.

The bundled SQLite is 3.46.1, which supports `ALTER TABLE ... DROP COLUMN`.
Migration 18 therefore migrates the table in place:

1. `ALTER TABLE ... ADD COLUMN` for `kind`, `spec_version`, `spec_json`,
   `driver_json`.
2. Backfill every row in Go.
3. `ALTER TABLE ... DROP COLUMN` for `source_json`, `exposure_json`,
   `protection_json`, `provider_json`.

This never drops the parent table, so dependent rows are never orphaned. A probe
confirmed it succeeds inside the migration transaction with foreign keys
enabled, leaving `PRAGMA foreign_key_check` clean and child rows intact.

`DROP COLUMN` refuses indexed, primary-key and unique columns. Verified: the
only index on `connection_profiles` is the `id` primary key, so all four legacy
columns are droppable.

Note that `ADD COLUMN ... NOT NULL` requires a default; the new columns are
added with empty/zero defaults and populated by the backfill.

### D2 — Kind and spec version are columns, not envelope fields

The audit sketched a JSON envelope carrying `version` and `kind` inside the
blob. Columns are preferred: they are queryable and filterable, and storing the
same fact in both a column and the blob reintroduces a dual source of truth.

`spec_json` therefore holds exactly a marshalled `core.ConnectionSpec`.

### D3 — Convert rows in Go, not in SQL

Backfill runs inside `onApply` in Go rather than via SQLite JSON1 functions.
This avoids depending on a build-tag-dependent extension, and lets a corrupt or
undecodable legacy row fail with a typed, row-identifying error instead of
silently producing a malformed blob.

### D4 — Validate arm cardinality on decode, not the full profile

After decoding, exactly one union arm must be populated and it must match the
`kind` column. The implementer must *not* call `ConnectionProfile.Validate()` on
load: that method also enforces name, provider and desired-state rules, and
applying it at load time would make an already-persisted row permanently
unloadable after any future tightening of those rules.

### D5 — Reject unknown spec versions with a typed error

`currentProfileSpecVersion = 1`. A row whose `spec_version` exceeds it is
refused with a distinguishable error. This guards the encoding axis, which is
finer-grained than the existing schema-version guard.

## Target schema after migration 18

`connection_profiles` ends with these columns:

```text
id            TEXT PRIMARY KEY
name          TEXT NOT NULL
revision      INTEGER NOT NULL
kind          TEXT NOT NULL          -- added by 18
spec_version  INTEGER NOT NULL       -- added by 18
spec_json     BLOB NOT NULL          -- added by 18
driver_json   BLOB NOT NULL          -- added by 18, replaces provider_json
lifecycle_json BLOB NOT NULL
desired_state TEXT NOT NULL
created_at    TEXT NOT NULL
updated_at    TEXT NOT NULL
```

Dropped by 18: `source_json`, `exposure_json`, `protection_json`,
`provider_json`.

`provider_json` becomes `driver_json`, which is what it has actually held since
the driver refactor. Phase 0 had to keep writing the legacy name; this migration
is the correct point to retire it.

## Conversion rules

For each legacy row:

1. Decode `source_json`, `exposure_json`, `protection_json` into their structs.
2. Build `ConnectionSpec{ServiceExposure: &ServiceExposureSpec{...}}`.
3. Write `kind = 'service_exposure'`, `spec_version = 1`, `spec_json` = the
   marshalled spec, `driver_json` = the legacy `provider_json` bytes verbatim.
4. A row that fails to decode aborts the migration transaction with an error
   naming the row id. Never partially convert.

## Codec changes

- `serviceExposureForPersist` (added in Phase 0) is replaced by an encode step
  that marshals whichever arm is set and derives `kind` from the profile.
  Non-service-exposure kinds become persistable at the storage layer; they
  remain unreachable because no caller can construct them yet.
- `hydrateServiceExposure` is deleted. Kind comes from the column.
- Both load paths (`loadProfileLocked`, `readSnapshotProfiles`) decode
  `spec_json` and apply D4.

## Out of scope, with rationale

The audit groups four items into its Phase 1. This package deliberately takes
only the first.

- **Tagged IPC DTOs** and **kind-dispatched controller behaviour**
  (`ConnectionKindHandler`) are deferred. They are in-memory refactors that
  carry no migration risk and can land at any time; durable state cannot.
- **Removing the compatibility accessors** is deferred. There are ~110 call
  sites across 12 files. While `HandleCreateConnection` can only produce
  service-exposure profiles (fact 3), removing them changes no behaviour — it is
  pure churn that would obscure the persistence change in review.

These should be done as one package at the point a second connection kind is
actually implemented end to end, which is when the accessors first become
capable of returning a misleading zero value in production.

## Required tests

- Round trip for a service-exposure profile through save, load and snapshot.
- `kind` is read from the column, not inferred: a row whose stored kind is
  absent or unexpected must not silently load as service exposure.
- Migration 18 converts a legacy row and preserves source, exposure, protection
  and driver values exactly.
- Migration 18 preserves a profile that has dependent foreign-key rows.
- A legacy row with undecodable JSON aborts the migration and leaves the
  database unchanged.
- A row with `spec_version` above the supported version is refused with the
  typed error.
- A decoded row with zero or multiple populated arms is refused.
- Existing database opens cleanly after upgrade (migration idempotence).
