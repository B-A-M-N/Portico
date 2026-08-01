# CAS design — runtime persistence

**Scope.** Audit findings #8 (blanket upsert with no revision guard) and #16
(post-reconcile blanket save races terminal commits). Authored by GLM-5.2 in
architect mode; the implementation model (qwen3.6-35b) executes this verbatim
after verifying against the repository.

**Verified repository state (2026-07-27).**

- `connection_runtime` schema (`internal/store/sqlite.go:65-79`): 13 columns,
  no revision column, `connection_id TEXT PRIMARY KEY`.
- Max migration version: **16** (`internal/store/sqlite.go:484-499`).
- `addColumnIfNotExists` helper at `internal/store/sqlite.go:559`.
- `upsertRuntime` at `internal/store/sqlite.go:1574` — shared write helper
  used by `SaveRuntime` and `CommitConnectorRuntimeEvent`. Does
  `INSERT ... ON CONFLICT(connection_id) DO UPDATE SET ...` with 13 columns,
  no CAS.
- `SaveRuntime` at `internal/store/sqlite.go:1628` — thin wrapper over
  `upsertRuntime` under `s.mu.Lock()`.
- `LoadRuntime` at `internal/store/sqlite.go:1655` — selects 13 columns.
- `readSnapshotRuntimes` at `internal/store/sqlite.go:1482` — same 13 columns.
- `CommitOpenSuccess` (~2638), `CommitCloseSuccess` (~2719),
  `CommitRepairSuccess` (~2805), `CommitDeleteSuccess` (~2825),
  `CommitOperationFailure` (~2780): each issues its own direct
  `UPDATE connection_runtime SET ...` inside an existing transaction. They
  do **not** go through `upsertRuntime`.
- Blanket save loop: `internal/supervisor/supervisor.go:291-296` —
  `for _, rt := range s.controller.ListRuntimes() { s.store.SaveRuntime(...) }`
  after the reconcile pass. This is the primary race source.
- `controller.Observe` (`internal/controller/controller.go:1170-1235`)
  mutates the in-memory `c.runtimes[connID]` (bumps `ObservedRevision`,
  refreshes origin/connector fields) but **does not persist**. Persistence
  has been delegated to the blanket loop in the supervisor. Removing the loop
  therefore requires observation to persist itself, or the observed state
  never reaches disk until the next connector event.
- Existing durable observation path: `store.CommitConnectorRuntimeEvent`
  (`internal/store/sqlite.go:1635`) — already wraps `upsertRuntime` plus the
  event append in one transaction. The supervisor uses this for connector
  process events (`supervisor.go:149`).
- `ConnectionRuntime` (`internal/core/runtime.go:39-52`) currently has
  `ObservedRevision uint64` (in-memory, not persisted).

## 1. Migration 17

Add **one** column to `connection_runtime`. Persisted CAS clock only;
`ObservedRevision` stays in-memory (it is bumped by `controller.Observe` and
has no durable meaning on its own).

```go
{
    version: 17,
    onApply: func(tx *sql.Tx) error {
        return addColumnIfNotExists(tx, "connection_runtime", "runtime_revision", "INTEGER NOT NULL DEFAULT 0")
    },
},
```

Rationale:

- `INTEGER NOT NULL DEFAULT 0` back-fills every existing row to revision 0.
  The first CAS write after upgrade will see `0` and succeed; subsequent
  writes require the caller to have observed that 0 and incremented.
- No `observed_revision` column. Persisting it would couple the durable
  schema to an in-memory counter that has no correctness meaning (the
  counter is just "how many times has `Observe` run in this process").
- Idempotent via `addColumnIfNotExists`, matching the pattern at migrations
  3, 4, 5, 6, 12.

## 2. `ConnectionRuntime` struct change

Add **one** field. Distinct from `ObservedRevision`.

```go
// In internal/core/runtime.go, ConnectionRuntime struct:
type ConnectionRuntime struct {
    ConnectionID     ConnectionID
    RuntimeRevision  uint64 // persisted CAS clock; bumped on every successful write
    ObservedRevision uint64 // in-memory observation counter; not persisted
    State            RuntimeState
    // ... unchanged ...
}
```

`DeepCopy` already does `cr := *r`, so the new field is copied by value.
No explicit copy line is required; do **not** add one. (The prior broken
edit added `cr.RuntimeRevision = r.RuntimeRevision` redundantly — `cr := *r`
already covers it.)

The field is read by:

- The supervisor, before calling `SaveRuntimeCAS`, to populate the
  `WHERE runtime_revision = ?` parameter.
- The store, on `LoadRuntime`/`readSnapshotRuntimes`, so callers see the
  current persisted revision.

The field is written by:

- `LoadRuntime` and `readSnapshotRuntimes` (after SELECT).
- `RuntimeCommitResult` consumers in the supervisor (after a terminal
  commit) — but they don't need to mirror the new revision back into
  memory if the next `Observe` cycle will reload it. See §6.

## 3. `upsertRuntime` semantics

**Decision: `upsertRuntime` does NOT bump the revision.** Only
`SaveRuntimeCAS` and the terminal commit methods touch `runtime_revision`.

Reasoning:

- `upsertRuntime` is shared between `SaveRuntime`, `SaveRuntimeCAS`, and
  `CommitConnectorRuntimeEvent`. If it bumped unconditionally, the CAS guard
  in `SaveRuntimeCAS` would have nothing to compare against.
- The terminal commits already do direct UPDATEs that bypass `upsertRuntime`
  entirely; they will bump in their own SET clause (§5).
- Keeping `upsertRuntime` revision-agnostic means callers explicitly choose
  CAS vs. non-CAS by selecting which entry point to call. That is the
  property we want.

**Concrete change to `upsertRuntime`:** none. Leave the SQL exactly as it
is at line 1603-1615. The new `runtime_revision` column is simply not
touched by this path; existing rows being upserted retain whatever
revision they had.

> Note: this leaves a window where a `CommitConnectorRuntimeEvent` write
> (connector-driven) and a terminal commit on a different goroutine can
> both touch the same row without either seeing the other's revision bump.
> That is acceptable because (a) `CommitConnectorRuntimeEvent` and the
> terminal commits are serialised externally by `c.operationMu` /
> `c.mu` in the controller, and (b) the connector-event payload is
> complementary to terminal state, not competing. If this assumption
> breaks in review, escalate to the architect before changing the design.

## 4. `SaveRuntimeCAS` — new method

Single-transaction CAS check against the persisted revision.

```go
// SaveRuntimeCAS persists a connection runtime with optimistic concurrency
// control. The stored runtime_revision must equal rt.RuntimeRevision; on
// success the persisted revision is bumped by 1 and rt.RuntimeRevision is
// updated in place to match.
//
// Returns ErrRuntimeRevisionMismatch when another writer persisted between
// the caller's load and this call. Callers must reload and re-apply.
var ErrRuntimeRevisionMismatch = errors.New("runtime: revision mismatch")

func (s *Store) SaveRuntimeCAS(ctx context.Context, rt *core.ConnectionRuntime) error {
    s.mu.Lock()
    defer s.mu.Unlock()

    // ... existing JSON marshalling (same as upsertRuntime) ...

    res, err := s.db.ExecContext(ctx, `
        UPDATE connection_runtime SET
            runtime_revision = runtime_revision + 1,
            runtime_state = ?,
            provider_id = ?,
            public_address = ?,
            private_address = ?,
            connector_json = ?,
            provider_runtime_json = ?,
            endpoint_json = ?,
            diagnostics_json = ?,
            active_operation_id = ?,
            error_json = ?,
            last_observation = ?,
            last_transition = ?
        WHERE connection_id = ? AND runtime_revision = ?`,
        /* 13 value args */,
        rt.ConnectionID, rt.RuntimeRevision,
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
    rt.RuntimeRevision++ // mirror the bump for the caller
    return nil
}
```

Placement: directly after `SaveRuntime` (around line 1633).

Caller flow:

1. `rt, _ := store.LoadRuntime(ctx, id)` — `rt.RuntimeRevision` is populated.
2. Mutate `rt` in memory.
3. `err := store.SaveRuntimeCAS(ctx, rt)` — bumps in DB, mirrors in struct.
4. On `ErrRuntimeRevisionMismatch`: reload, re-apply, retry once. Escalate
   if the second attempt also conflicts (this indicates a hot loop, not a
   casual race).

`errors` is already imported in `sqlite.go` (line 10).

## 5. Terminal commit methods

Each terminal commit adds `runtime_revision = runtime_revision + 1` to the
SET clause of its existing `UPDATE connection_runtime SET ...`. **No switch
to `SaveRuntimeCAS`** — these methods already run inside their own
transaction with `connection_profiles` and event rows; pulling them out
would break atomicity.

| Method | Line | Change |
|---|---|---|
| `CommitOpenSuccess` | ~2669 | Add `runtime_revision = runtime_revision + 1,` as first SET entry. |
| `CommitCloseSuccess` | ~2752 | Same. |
| `CommitRepairSuccess` | ~2830 | Same. |
| `CommitOperationFailure` | ~2780 | Same (its UPDATE may be the `error_json` one; verify line). |
| `CommitDeleteSuccess` | n/a | `CommitDeleteSuccess` does not UPDATE the runtime row in a way that needs a revision bump — it clears `active_operation_id` then deletes the row on connection deletion. Leave it. |

Verification step before editing each method: read the current UPDATE
statement, confirm it is the runtime-state transition (not the connector
JSON patch or the active-op clear), and insert the revision bump as the
first SET entry. The connector JSON patches at `CommitOpenSuccess:2684`
and `CommitCloseSuccess:2763` are separate UPDATEs and do **not** get a
revision bump — they are subordinate to the state transition that already
bumped.

## 6. Blanket save loop removal

**Decision: delete the loop at `supervisor.go:291-296` entirely.**

What replaces it:

- **Terminal commits** already persist the runtime row inside their own
  transaction (§5). Operation completion is the authoritative persistence
  point for state transitions.
- **Connector events** are already persisted via
  `store.CommitConnectorRuntimeEvent` at `supervisor.go:149`. Connector
  exits and restarts reach disk through this path.
- **Observation persistence** is the gap. `controller.Observe`
  (`controller.go:1170`) mutates the in-memory runtime but does not
  persist. Today the blanket loop is what durably saves observed state.
  After removing the loop, observation must persist itself.

Concrete change to `controller.Observe`:

```go
// At the end of the locked block in controller.Observe, after mutating
// c.runtimes[connID] in memory, persist via the RuntimeSaver. The saver
// is the supervisor-injected store handle. Use SaveRuntime (not CAS):
// observation is not a state transition and may legitimately run
// concurrently with another observation; last-writer-wins is acceptable
// because the observed fields are derived from the same provider call.
if c.runtimeSaver != nil {
    snapshot := rt.DeepCopy()
    c.mu.Unlock()
    if err := c.runtimeSaver.SaveRuntime(ctx, snapshot); err != nil {
        slog.Warn("persist observed runtime", "connection", connID, "err", err)
    }
    return observed, nil
}
c.mu.Unlock()
return observed, nil
```

Notes:

- Use `SaveRuntime` (the existing upsert path), **not** `SaveRuntimeCAS`.
  Observation produces derived fields (PID, status, origin health); if two
  observations race, either winner is acceptable because both are reading
  the same provider state. CAS would force spurious conflicts.
- This does **not** race with terminal commits: terminal commits take
  `c.mu` in the controller for the duration of the operation record
  update, and `Observe` also takes `c.mu`. The locks serialise; the SQL
  writes happen outside the lock but the in-memory state is consistent.
- The runtime row is guaranteed to exist (created by `CreateProfile` via
  `connectionStorer.CreateConnection`), so `SaveRuntime`'s ON CONFLICT
  path is the one that fires.

Risk to flag in review: if `Observe` is called during a close operation
between `CommitCloseSuccess`'s SQL UPDATE and the controller's
in-memory update, `Observe`'s `SaveRuntime` could overwrite the closed
state with the pre-close observation. Mitigation: `Observe` checks
`rt.ActiveOperation != nil` and skips persistence when an operation is
in flight. Add this guard:

```go
if c.runtimeSaver != nil && rt.ActiveOperation == nil {
    // ... persist ...
}
```

When an operation is running, the operation's terminal commit is the
persistence authority; `Observe` defers.

## 7. `controller.Observe` persistence path

Answered in §6. Summary:

- Today: `controller.Observe` does not persist; the supervisor's blanket
  loop does it after reconcile.
- After fix: `controller.Observe` persists itself via `runtimeSaver` (the
  existing `RuntimeSaver` interface, already wired by
  `SetRuntimeSaver` at `controller.go:601`). Uses `SaveRuntime`
  (non-CAS), guarded by `rt.ActiveOperation == nil`.

## 8. Test plan

New tests in `internal/store/commit_test.go` and
`internal/supervisor/reconcile_test.go`. Style: existing
`TestRuntimeCommitResults` at `commit_test.go:387`.

### `internal/store/commit_test.go`

1. **`TestSaveRuntimeCASBumpsRevision`** — save runtime, load it,
   `SaveRuntimeCAS` with the loaded revision, assert the persisted revision
   is `loaded+1` and the in-memory `rt.RuntimeRevision` was mirrored.

2. **`TestSaveRuntimeCASRejectsStaleRevision`** — save runtime, load it
   (revision R), perform a second `SaveRuntimeCAS` that bumps to R+1, then
   attempt `SaveRuntimeCAS` with the stale R. Assert
   `errors.Is(err, ErrRuntimeRevisionMismatch)` and that the persisted row
   is unchanged (still R+1).

3. **`TestCommitOpenSuccessBumpsRuntimeRevision`** — extend the existing
   `TestRuntimeCommitResults` setup: save runtime at revision 0, call
   `CommitOpenSuccess`, reload, assert `runtime_revision == 1`. Same for
   `CommitCloseSuccess` and `CommitRepairSuccess`. One test per method, or
   one parametrised test — implementer's choice.

4. **`TestMigration17AddsRuntimeRevision`** — open a store seeded at
   migration 16 (use the existing `newTestStore` helper, then manually
   `DROP COLUMN` if needed, or use a fresh DB), run migrations, assert
   `PRAGMA table_info(connection_runtime)` contains `runtime_revision`
   with type `INTEGER` and default 0. (Skip if no harness exists for
   pre-migration seeding; the column-existence check on a fresh store is
   the minimum.)

### `internal/supervisor/reconcile_test.go`

> **Not written.** The three tests prescribed below do not exist under these or
> any other names. `reconcile_test.go` covers narrow drift repair thoroughly,
> but nothing asserts the runtime-persistence rule this contract specifies.
> A design contract listing tests that were never written is a claim of
> coverage that is not there; it is recorded here rather than quietly dropped.

5. **`TestObservePersistsRuntimeWhenNoOperationInFlight`** — wire a real
   store + controller, call `Observe`, assert the runtime row in the DB
   reflects the observed connector status. (This replaces the blanket
   loop's role.)

6. **`TestObserveSkipsPersistenceWhenOperationInFlight`** — set
   `rt.ActiveOperation = &someOpID` in memory, call `Observe`, assert
   the persisted row was **not** updated (revision unchanged). This is
   the close-race mitigation from §6.

7. **`TestReconcileNoLongerPersistsBlanketRuntime`** — assert that
   `reconcileAll` does not call `SaveRuntime` on runtimes that have no
   state change. Implement with a counting `RuntimeSaver` shim or by
   inspecting `store.SaveRuntime` call count via a wrapper. The assertion
   is "the loop is gone," so any post-reconcile save must come from the
   observation path (covered by test 5).

### Gating

Per `CLAUDE.md` work-package rules, this is **Tier 2 — package-level**:
crosses store + supervisor + controller. Pipeline:

1. State invariant (this document).
2. Inspect current code (done above).
3. RECON: not needed; call paths are clear.
4. Add focused regression tests (above 7).
5. Implement correction.
6. Run targeted package tests.
7. Run `go test ./...` at the package boundary.
8. `portico-auditor` REVIEW once.
9. Correct evidence-backed findings.
10. Rerun affected tests.
11. Commit independently.

## Out of scope (do not pull in)

- Idempotency keys (finding #12).
- Shutdown mutation gate (finding #5).
- Event-driven reconciliation queue (finding #14).
- Removing `ObservedRevision` from the struct (it is still useful as an
  in-memory freshness indicator for tests and logs).

## Acceptance

This design is complete when:

- Migration 17 adds `runtime_revision` and existing tests pass.
- `SaveRuntimeCAS` exists with `ErrRuntimeRevisionMismatch` and the two
  CAS tests pass.
- Terminal commits bump `runtime_revision`.
- The blanket save loop is removed and `controller.Observe` persists
  observations itself, guarded by `ActiveOperation == nil`.
- All 7 named tests pass.
- `go test ./...` is green; `go test -race ./...` is green for the
  affected packages.
