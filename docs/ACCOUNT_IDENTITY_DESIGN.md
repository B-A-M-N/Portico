# Provider account identity — design contract

Status: accepted, implemented.
Tier 3. Produced by `portico-architect`; every claim below was verified against
the repository by the main session before implementation, and the verification
results are recorded here.

## The invariant

A provider account's identity is the pair `(provider_id, id)`. No write path can
change the provider that owns an existing account row. Two providers may each
hold an account named `default`.

## The defect

`provider_accounts` was keyed on `id` alone. Both upserts declared
`ON CONFLICT(id) DO UPDATE SET provider_id=excluded.provider_id, ...`, so any
writer supplying a colliding ID silently reassigned another provider's account —
its owner, credential reference and status.

The previous package added a pre-check in `configureDeclaredAccount`. **That
guard covered one of four write paths.** Verified: the other three —
`configureCloudflareAccount` (supervisor.go:1892) and the two environment
bootstrap paths (factory.go:330, factory.go:488) — had no such check, so the
takeover remained reachable through ordinary Cloudflare setup. The compensation
was not merely fragile; it was already incomplete. That is the argument for
moving enforcement into the key.

## Verified before implementing

- The store already takes a pre-migration backup with a WAL checkpoint
  (sqlite.go:1057-1088). The migration adds no backup step.
- Reads and deletes are already dual-scoped (`WHERE id = ? AND provider_id = ?`,
  sqlite.go:4842 and 4858). Only the two upserts were unscoped.
- No foreign key, index, trigger or view references `provider_accounts`, so a
  table rebuild is safe. Confirmed by grep.
- Latest migration was 18, so this is 19.
- `credential_ref` is nullable and `ListProviderAccounts` scans it into a plain
  `string`, so a NULL would fail the read today. The migration preserves the
  column exactly as-is and fixtures use `''`, so two defects are not conflated.

## The correction

Composite primary key `(provider_id, id)`, in that column order — the implicit
index then serves the per-provider filtering that startup and the factory
already perform. Rejected a surrogate key: nothing references this table by
rowid, and a surrogate would force every reader through an extra lookup to say
what the composite key says directly.

Migration 19 rebuilds the table inside the migration transaction: create with
identical column names, types and nullability; copy with an explicit column list
and no re-encoding; **check row-count parity and fail if it differs**; drop;
rename. Both upserts then target `ON CONFLICT(provider_id, id)` and **drop
`provider_id` from their SET lists**, making the owning provider structurally
immutable.

### No collision is possible

The old schema declared `id TEXT PRIMARY KEY`, so every existing row already has
a distinct `id`. Widening a key cannot create duplicates: a set of tuples unique
in one component is unique in any superset of components. The count-parity check
proves this at runtime rather than by argument, and turns any surprise into a
refusal with the backup already on disk.

The migration-8 `migration_conflicts` precedent deliberately does **not** apply.
That precedent is for *narrowing* a key; this widens one. A quarantine path here
would be dead code guarded by a condition that cannot be true.

### Rows already corrupted are not repaired

In a takeover both `provider_id` and `credential_ref` were rewritten together,
so the row is internally consistent and indistinguishable from a legitimate one.
Any heuristic repair would invent history. Recovery is from the `.backup-*` file
the store already writes. This is stated in the migration's own comment so a
future reader does not attempt it.

## What does not change

No store method signature changes and none are removed. `core.ProviderAccount`
already carries both `ID` and `Provider`, and both are already validated
non-empty — the pair was always in the argument; only the conflict target
ignored it. No file outside `internal/store` needs to compile differently.

`core.ProviderAccountID` stays a bare string. A provider-qualified type would
change `DriverSelection.AccountID`, which is serialised into
`connection_profiles.driver_json` — requiring a second released-schema migration
— and the `account_id` IPC wire field. The enforcement gain is zero: the pairing
is already structural in `DriverSelection`, the registry's
`map[ProviderID][]ProviderAccountID`, and each Cloudflare child adapter's
binding. The database key was the only place it was not.

## The supervisor guard is removed, not kept

The guard enforced **global** account-ID uniqueness: it rejected any ID owned by
a different provider. The corrected model explicitly permits that. Keeping it
would refuse an ngrok account named `default` when a Cloudflare `default` exists
— exactly what the first required test asserts must work — leaving the store
permissive and the supervisor restrictive. Two contradictory identity models in
one system is the failure class this package exists to remove, and the guard's
advice ("choose a different value") would become actively wrong.

Its test is inverted rather than deleted: the new assertion is that configuring
provider B with provider A's account ID succeeds and leaves A's row intact.

## What the adversarial review found

A `portico-auditor` REVIEW returned FAIL with three findings. All three were
verified against repository state before being acted on; all three were real,
and all three were in the write paths this contract claimed to have audited.
The composite-key correction itself was confirmed sound: the migration is
faithful, fails closed on every branch, is safe under `foreign_keys=on` and WAL,
and fresh and upgraded databases converge on byte-identical DDL.

1. **The bootstrap could promote a pending account.** Both environment
   bootstrap paths wrote `Status: AccountAuthenticated` unconditionally, and the
   upsert's SET list includes `status`. So an account deliberately saved as
   pending — because its credential could not be checked — was promoted to
   authenticated by a restart with the right environment variables set. The
   composite key closed the *owner* takeover and left status writable by a path
   that verifies nothing. This is the previous package's invariant, broken
   through a path neither package had checked.

2. **The bootstrap reverted operator state on every restart.** The same upsert
   overwrote `label` and `metadata_json` wholesale from environment values. A
   Cloudflare account configured through setup with a verified zone would have
   its zone reverted to a stale environment value on the next start, and the
   adapter is then built against the wrong zone — defeating the zone-membership
   check that setup performs precisely to prevent that.

3. **A secret survived its account.** The generic setup path built
   `<provider>:<account>:credential` while the bootstrap built
   `<provider>:<account>:api-token`. Two references, one account. Deletion
   removes only the reference the account currently carries, so the other
   encrypted credential stayed in the database after the user was told the
   account was removed, reachable by nothing.

### The corrections

1 and 2 share a cause and a fix: `CreateProviderAccountIfAbsent` gives the
bootstrap insert-if-absent semantics, so a writer that merely found a
credential can no longer overwrite state it did not establish.

The reviewer offered a second option for 1 — write `AccountPending` from the
bootstrap. **Rejected after checking the consequence:** adapters are only built
from authenticated accounts (`factory.go:244`, `:353`, `:511`), so that would
have disabled environment-based setup outright. The two options were
alternatives, not both, and insert-if-absent closes the finding without the
regression.

3 is fixed at its source: one `providerCredentialRef` helper that every writer
derives from, so a second reference for one account cannot be constructed.

### A correction the fix for 3 made necessary

Unifying the credential reference removed the orphaned secret and, in the same
move, created a sharper defect. With one reference per account, the bootstrap's
`SaveProviderCredential` — an upsert on that reference, called *before* the
guarded account insert — no longer wrote a separate row. It overwrote the
validated one. The account kept its label, zone and status while the adapter was
built from a stale environment token: an orphaned secret traded for a silently
swapped one.

The account row and its secret are therefore written as one unit by
`CreateProviderAccountCredentialIfAbsent`: insert the account with
`ON CONFLICT DO NOTHING`, and write the credential only if that insert created a
row, inside the same transaction, so a failed credential write rolls the account
back. Querying first would be a race; reversing the two calls would leave an
account pointing at a credential that never persisted.

`CreateProviderAccountIfAbsent` — the account-only variant — is deleted rather
than kept, because using it means writing the credential separately, which is
the defect. Both provider bootstraps now share one `seedBootstrapAccount`
helper so they cannot drift apart again.

### Residual gap, stated rather than hidden

An account *created* by the bootstrap is still recorded as authenticated on the
strength of a token nobody checked. That is this path's existing behaviour and
is unchanged here. Closing it means verifying at startup — a network call on
boot, which can fail offline — and is separate work. What is now guaranteed is
narrower and worth stating exactly: the bootstrap cannot change the status,
label or metadata of an account that already exists.

## Out of scope, recorded so they are not lost

- `SaveProviderCredential` rewrites `provider_id` on conflict, but refs are
  provider-prefixed and `LoadProviderCredential` already refuses a ref whose
  stored provider does not match, so it fails closed on read.
- `ListProviderAccounts` scanning a nullable `credential_ref` into a plain
  string is a separate Tier 1 item.
- `DeleteProviderAccount`'s doc comment names `CountConnectionsUsingAccount`,
  which does not exist. Fixed opportunistically.
