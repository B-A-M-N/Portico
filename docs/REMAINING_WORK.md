# Remaining Work

**Baseline:** `da0667e` (`feat: harden Portico connection lifecycle`)

**Last reviewed against:** working tree, 2026-09-07. The release-audit P0s
(wizard→provider-setup handoff, setup/Help navigation, canonical Staticcheck
gate, prior-release upgrade selection) are fixed as of `bebd0f6`/`696d685`; the
action-authority footers (`c80ef37`), the editable tunnel-client path including
the config-clear persistence defect (`ae3b2e3`), provider setup semantics on
the DTO (`9561493`), and declared potential capabilities with potential-filtered
wizard advice (`ca4f434`) have also landed. The second audit's five release
blockers are fixed as of `5b6ceee` (account-removal pending state, PTY-safe TUI
logging, race-free E2E waits), `cb5a574` (one meaning per key —
`ActionSet.ValidateUniqueBindings` and per-state coverage), `3a9d3fe` (semantic
protection classification from the capability DTO), and `e882a90` (one
cell-aware wrap facility; six-size sentence-reconstruction tests for Home,
Providers and the wizard). The compiled-binary PTY mutation matrix has since
been driven through the real terminal: edit→apply→restart→verify and
toggle-open→reconcile (`5ede8ec`), repair→apply with an independent byte-level
proof that the freed port carries the origin's canary again (`c7ac6f2`),
credential and encryption-key rotation both surviving a supervisor restart
(`007cc68`), the `/proc` canary proof that a stored credential never reaches a
process argv or any log/export (`a0e1e23`), and the six-size critical-screen
matrix with full-sentence reconstruction at 200×60 through 60×18 (`733c63e`).
SSE disconnect→reconnect→gap-free-replay→snapshot evidence is covered at the
supervisor/SSE integration layer by
`internal/ipc/event_stream_integration_test.go`. The Cloudflare zone model is
now connection-scoped: a permanent connection selects its own DNS zone from
every zone the account's credential can see (wizard zone picker +
`ListProviderAccountZones`), so an account-default or credential change cannot
silently retarget it; legacy account-bound zones still work via a per-account
fallback. Still open: the true
TUI-driven Cloudflare live qualification (needs a live account; the CLI/provider
path is qualified by `scripts/qualification/cloudflare.sh`, the beginner-facing
TUI path is not). Sections marked Done below were
verified against the tests named in them at this review; where a claim is only partly
true, it says which part. Several entries were stale before that review — rotate-in-place and the
429/5xx/malformed/timeout coverage were both listed as open after they had landed — so
treat any unqualified item as unverified rather than as an open gap, and check the named
test before trusting a Done.

Earlier note, retained: two audit remediations have landed since the
baseline. Items below may already be done — each section states its current
state, and `ACCEPTANCE_MATRIX.md` maps completed requirements to the tests that
hold them. Treat an unqualified item here as unverified rather than as an open
gap.

This is the operational backlog after the durability, recovery, reconciliation,
credential, process, IPC, and TUI fixes in that commit. It separates verified
gaps from deliberate v0.1 exclusions in `SPEC.md`; an exclusion is not silently
treated as an implementation defect.

## How to use this document

- Work top-to-bottom within a priority unless a safety issue is discovered.
- Do not mark an item done because a unit test passes. Complete its acceptance
  checks with the named integration or failure-injection coverage.
- Keep provider credentials secret: no plan, event, runtime, log, IPC response,
  or diagnostic evidence may contain secret material.
- Preserve the hard boundaries in `SPEC.md` §2.1. In particular, only the
  supervisor opens the database or mutates provider state.

## P0 — make multi-account Cloudflare usable end to end

### Account setup and selected-account UX

**Current state**

- `provider_accounts` and encrypted `provider_credentials` exist, and migration
  16 stores API credentials by opaque reference.
- `cloudflare.AccountsProvider` creates an account-bound adapter per stored
  account. The controller fails closed when no selected account can be resolved.
- IPC provider snapshots carry non-secret account summaries, and the wizard
  auto-selects exactly one account or requires an explicit choice for several.
  The selected ID is included in its create request and review screen.
- `portico provider login cloudflare --account-id … --zone-id …` reads a token
  only from the environment and sends it over the authenticated local socket.
  The supervisor transactionally writes the encrypted credential and account
  row without returning the token. A controlled restart is explicitly required
  because account-bound adapters are constructed at startup.

**Implementation**

1. **Done.** Account removal exists end to end: a `DELETE` route, a client
   method, `portico provider remove-account`, and an `[x]` action on the
   providers screen. It refuses while any connection selects the account or any
   outstanding cleanup obligation needs its credential — the check and the
   delete happen in one transaction, so a connection bound in between cannot be
   stranded — and the refusal names what is in the way. It is not expressed as
   a plan, deliberately: see `ACCOUNT_REMOVAL_DESIGN.md`. What it has instead is
   a preview computed from the same evidence the removal decides on, a
   fingerprint binding preview to apply, and a durable record written in the
   deleting transaction.

   **Done since.** Rotate-in-place exists: `PUT
   /v1/providers/{id}/accounts/{accountID}/credential`, a client method, and a
   `Replace credential` action on the providers screen. The identity
   `(provider_id, account_id)` is preserved — every connection stores it, so
   creating a second account would leave them all pointing at the one being
   replaced — and the new credential is validated before anything is committed,
   so a rejected one leaves the working credential untouched. Covered in
   `internal/supervisor/account_credential_test.go`, including an assertion that
   no response body carries the secret.

   Re-verification is implemented alongside it, against the supervisor-owned
   credential store: it fetches the credential without exposing it, validates it
   through the provider's own validator, and updates the durable account status
   in both directions.

2. **Not done.** The token still comes from the environment. Command arguments
   are refused with the reason, and the secret stays out of logs, events and
   responses, but stdin or a protected file descriptor would be better.

3. **Mostly done.** The providers screen selects a provider and an account,
   draws the cursor, and offers removal. Inspect shows the selected account.
   **Still missing:** nothing — re-verify (`v`) and replace-credential (`c`)
   are rendered on the providers screen from the action registry, and the
   rotation form presents identity as read-only context.

4. **Not done.** The one-account config bootstrap has not been migrated into the
   account repository, and the old read path is still in place.

**Acceptance**

- Two accounts with different tokens/zones can be added, listed, selected in
  the TUI and CLI, restarted, and used without cross-account API calls.
  *(Holds. Account identity is the `(provider_id, id)` pair since migration 19;
  before that a colliding account ID silently reassigned another provider's
  account.)*
- A profile created while one account exists retains that account after another
  is added. *(Holds.)*
- A missing/revoked account produces a typed availability error before any
  provider request. *(Holds, and an edit that would point a connection at an
  unusable account is now refused at plan time whatever the connection's
  state — a closed connection generates no reopen steps, so nothing else
  consulted the provider.)*
- Tests cover selection, restart reconstruction, wrong-account rejection, and
  encryption-key rotation of every account credential. *(Holds for the
  supervisor-owned rotation action and its durable lifecycle events.)*

## P0 — provider contract and live Cloudflare confidence

### Complete Cloudflare contract testing

**Current state — partly done.**

There is now a fake HTTP API suite driving the real Cloudflare client through a
`cf.BaseURL` seam, in `internal/tunnel/contract_test.go`,
`internal/dns/contract_test.go` and `internal/access/contract_test.go`. It pins
request construction (remotely-managed tunnels, proxied CNAMEs pointing at the
tunnel, Access policies carrying the allowed identities) and the two
classifications the repair path branches on: a 404 is an absence, a 401/403 is
not. The first one written found a live defect — a wrapped-error type assertion
that never matched, so a resource deleted at the provider made every repair fail
on the lookup instead of recreating it.

**What remains**

1. **Done.** Fake API fixtures for tunnel, DNS, Access application and policy.
2. **Done, except exact-ID observation after a reconstructed supervisor.**
   404 and 401/403 were already covered for every manager. 429, 5xx, malformed
   JSON and timeouts now are, through the fake HTTP API and the real client:
   `internal/dns/failure_test.go`, `internal/access/failure_test.go`,
   `internal/tunnel/failure_test.go`, and
   `internal/provider/cloudflare/classify_test.go` for the shared classifier.

   The invariant those hold is that nothing unclassified is ever reported as
   missing, because a resource reported missing gets recreated. Writing them
   found two defects. `dns.GetRecord` dereferenced `record.Proxied`, a `*bool`
   that is nil when the field is absent, so a proxy's HTML error page returned
   with a 200 crashed the supervisor. And the Cloudflare client retries a 429
   itself and then reports exhaustion rather than the status, so `errors.As`
   could not see it and a rate limit was classified as an unclassified transient
   failure — `tunnel.IsRateLimitExhaustion` is the single place that is now
   recognised, used by both the tunnel manager and the adapter's classifier.

   **Still missing:** exact-ID observation after a reconstructed supervisor.
3. **Done at the decision level, not through the fake API.**
   `internal/supervisor/reconcile_test.go` proves that DNS-only, Access
   application-only and Access policy-only drift each produce the narrowest
   repair and do not recreate an intact tunnel or connector
   (`TestReconcileOpenConnectionRepairsOnlyMissingDNS`,
   `TestReconcileOpenConnectionUpdatesOnlyDriftedDNSTarget`,
   `TestReconcileOpenConnectionRepairsOnlyMissingAccessApplication`,
   `TestReconcileOpenConnectionRepairsOnlyMissingAccessPolicy`,
   `TestReconcileOpenConnectionUpdatesOnlyDriftedAccessPolicy`).

   Those exercise `computeReconcileDecision` against constructed observations.
   What is **not** covered is the same drift arriving from the fake HTTP API
   through the real client, so a change in how a response is classified would
   not be caught by them.
4. **Done.** `internal/provider/cloudflare/credential_failure_test.go` covers
   the start failing, readiness never arriving, `start` panicking, an
   uncreatable credential directory, an unset directory, a repeated cleanup, a
   sweep meeting an entry it cannot remove, and a sweep of a directory that does
   not exist yet.

   Verified by mutation: moving the deferred cleanup to after a successful start
   — the obvious way to write it — leaves a token on disk on both the
   start-failure and panic paths, and two of those tests fail naming the file.
   That is why removal is deferred rather than placed on each exit.

   These are still unit-level against `withCredentialFileUntilReady` rather than
   driven through a connector start in the real adapter, which would need a
   process to start and fail on demand.

**Acceptance**

- No live Cloudflare account is needed in CI. *(Holds — the suite is served by
  a local fake.)*
- Tests prove that unauthorized, rate-limited, and transient observations are
  never interpreted as a missing managed resource. *(Unauthorized holds;
  rate-limited and transient are untested.)*
- Tests prove terminal verification uses remote state, not adapter memory.
  *(Not yet.)*

> These tests pin Portico's half of the contract against a local fake. They are
> not evidence about how Cloudflare behaves, and would not catch an undocumented
> change to its API.

### Remove or isolate obsolete provider execution paths

**Current state**

`core.Provider` no longer requires `Apply`, `Repair`, or `Remove`, but both the
Cloudflare and mock adapters still contain those old asynchronous execution
implementations.

**Implementation**

1. ~~Add characterization tests for every legacy command that is intentionally retained during migration.~~ (done — `portico legacy` removed)
2. ~~Delete adapter `Apply`, `Repair`, and `Remove` once no callers remain, or move them into a clearly isolated legacy-only adapter that cannot be reached by the supervisor registry.~~
3. Remove `portico legacy` only after equivalent new supervisor workflows and characterization coverage exist.

**Acceptance**

- A repository search finds no production call path that mutates Cloudflare outside `ExecuteStep` under controller/supervisor journaling.
- The documented legacy removal gate in `SPEC.md` §3.2 is satisfied.

## P1 — finish the beginner workflow

### Complete wizard source configuration

**Current state**

The wizard can create existing-service, directory, command, and endpoint-based
MCP profiles. Command sources collect an executable, port, comma-separated
arguments, and working directory. Directory sources collect static-site versus
file-browser mode, upload/delete permissions, and SPA fallback. Endpoint MCP
sources collect HTTP, streamable HTTP, or (for permanent Cloudflare exposure)
SSE transport. The wizard also supports command-owned MCP servers.
Environment references (`env:NAME`) and shell-mode configuration are
implemented: `WizardStepCommandShell` and `WizardStepCommandEnv` exist, with
`ParseCommandEnv` accepting `env:` references and refusing secret literals.

Since this was written, the wizard's question sequence became derived rather
than hand-written: each step declares the condition under which it is asked, so
going back is the inverse of going forward by construction. Text fields have a
cursor, word motion and paste. **The wizard now supports port-forward creation,
and an "OpenAI-compatible API" outcome that probes the endpoint's protocol
compatibility before review.** The never-applied client-profile question was
removed: Portico is the single configuration authority for a client tunnel.

**Implementation**

1. **Done.** Steps are derived from predicates over the answers so far
   (`internal/tui/screens/wizard_steps.go`), not a static sequence.
2. **Done.** Port-forward creation supported in wizard.
3. Add safe tokenized input for command arguments and environment *references*
   (not raw secrets). Validate every screen before advancing.
4. Provide a directory mode chooser, file-browser permissions, and SPA option.
5. Keep transport choices constrained to combinations the selected provider can
   carry, including when an MCP command is owned by Portico.
5. Add a final review that states owned-process behavior, chosen account,
   public/private exposure, protection, and destructive implications.

**Acceptance**

- Every field accepted by the production source DTO is either configurable in
  the wizard or explicitly unavailable with an explanation.
- TUI state-machine tests cover back navigation, validation, request mapping,
  IPv6 addresses, multi-account selection, and no duplicate submission.

### Connection editing and account/provider visibility

**Current state — mostly done; one item remains.**

1. **Done.** Edit and clone flows exist, reachable with `e` and `c` from the
   list and the inspect screen. The edit carries the revision it was built
   from, and `HandlePlanEdit` refuses a stale one
   (`TestAnEditCarriesTheRevisionItWasBuiltFrom`,
   `TestPlanEditRefusesAStaleRevision`). Copying is done by the supervisor's
   deep copy rather than rebuilt from the detail DTO, which would lose a
   command's environment and an existing service's health check
   (`TestTheCopyIsMadeBySupervisorNotRebuiltFromTheDetail`).

2. **Done.** The inspect screen shows the provider, the selected account,
   unresolved findings, and what closing the connection does to the local
   service — whether Portico started it or merely connected to one already
   running. That is derived from the runtime where one exists, because the
   runtime knows what Portico is actually running while the profile only says
   what was asked for (`TestOriginOwnershipSaysWhatClosingDoes`). The close
   preview says the same thing in its own words, since that is where the
   decision is taken.

3. **Done — and it did not work when this was first written.** An edit produces
   a plan and goes through the normal preview, approval, apply and progress
   path rather than a direct save (`TestAConnectionCanBeEdited`).

   `ApplyPlan`'s intent dispatch omitted `IntentEdit`, so every edit plan
   returned "unknown plan intent: edit" and no edit could ever be applied. It
   was covered by tests that all asserted on the plan and never applied one.
   `TestAnEditPlanCanActuallyBeApplied` now crosses that boundary. A refused edit stays on the screen that made
   it with its values intact, and abandoning the preview returns to the edit
   rather than discarding it (`TestARefusedEditStaysOnTheScreenThatMadeIt`,
   `TestAbandoningThePreviewReturnsToTheEdit`).

See `ACCEPTANCE_MATRIX.md` §6 for the full mapping.

**Acceptance**

- Revision conflicts are understandable and non-destructive.
- Editing an account-bound connection cannot accidentally switch accounts.

### Improve doctor and user-facing recovery

**Current state**

Doctor is substantially more passive than the old implementation, but it
should become the single actionable entry point for configuration, migration,
credential, socket, database, and cleanup health.

**Implementation**

1. Add checks for provider-account credential references, zone metadata,
   unresolved cleanup items, retained-event health, and account adapter
   availability.
2. Keep default doctor read-only. Put state-changing remediations under an
   explicit `doctor repair` command with a preview.
3. Convert startup failures—especially supervisor early exit—into concise
   diagnostics that point to the relevant log and a next command.

**Acceptance**

- `portico doctor` does not start a supervisor or mutate state.
- A bad database, insecure key file, stale socket, unavailable account, or
  unresolved cleanup item returns a clear remediation path.

## P1 — complete configuration and credential migration

### Retire split Portico/Flare configuration

**Current state**

The indefinite split read is gone. `migrateLegacyConfig` imports a legacy
`config.yaml` once — through a separate viper instance, so the global one is never
left pointing at the old file — writes it to the Portico path in TOML, and renames
the original to `config.yaml.migrated` rather than deleting it.

That renaming is what makes it happen once. Previously viper was pointed at the
legacy file and left there, so the settings were re-read on every start: the
installation never moved, and a user editing the file Portico documents saw no
effect. A mutation test holds this — skipping the rename reproduces the old
behaviour, including a setting the user changed after migrating being overwritten
by the old file on the next start (`internal/config/legacy_migration_test.go`).

The credential path already migrated explicitly: `LoadCredential` reads a legacy
plaintext credential, re-saves it encrypted, and removes the original.

**Implementation**

1. Define the final Portico-only configuration contract in user documentation:
   `PORTICO_*`, XDG config/state/runtime paths, and account repository.
   **Still open** — this is a documentation task, not a code one.
2. ~~Add an explicit, idempotent migration command that imports legacy settings
   and reports exactly what it moved.~~ **Done**, as a migration at startup
   rather than a separate command: a user should not have to know to run one. It
   is idempotent because the source is renamed, and the original is kept rather
   than backed up separately.
3. Remove automatic legacy reads after a defined compatibility release window.
   **Still open**, and now a smaller change: there is one read, at one site,
   which can be deleted when the window closes.
4. Delete instructions that tell users to run `flare` commands. **Still open.**

**Acceptance**

- A fresh install never creates or needs a Flare path. *(Holds —
  `TestAFreshInstallationIsNotAFailure` also pins that it writes no config file
  for settings nobody has set.)*
- A legacy install migrates once without exposing plaintext tokens.
  *(`TestLegacySettingsAreImportedOnce`, `TestASecondStartDoesNotMigrateAgain`.)*
- **Not met:** reporting ambiguous values for user confirmation. The migration
  imports what it finds and reports a file it could not read; it does not ask
  about a value it is unsure of.

### Complete key lifecycle operations

**Current state**

Installation-key rotation and tunnel/provider credential re-encryption exist,
and rotation is now reachable through the supervisor API, CLI, and TUI. The
backup/recovery runbook exists: `docs/KEY_RECOVERY_RUNBOOK.md` covers which
file protects which secrets, mode expectations, backup/restore pairs, and
post-rotation key disposal.

**Implementation**

1. Done. The supervisor owns the rotate-key operation and records durable
   started/completed/failed lifecycle events without key material.
2. Done. Only status and rotation result are exposed, never key material or
   ciphertext.
3. Document backup/restore semantics, key-file ownership/mode checks, and the
   recovery procedure for a corrupt key.

**Acceptance**

- Rotation is idempotent/recoverable across interruption and re-encrypts both
  tunnel and provider-account credentials.
- Doctor detects a key/database mismatch without replacing any key.

## P2 — product scope, docs, release, and test depth

### Align supported-provider claims with implementation

**Current state**

Cloudflare and ngrok are real providers; mock is development-only. The ngrok
adapter was rebuilt against the real agent and is verified end to end, though it
applies no access protection. Tailscale is implemented for private-network
connections; zrok remains an architectural target.

**Implementation**

Choose one of the following before a public release:

1. Narrow all product copy to “Cloudflare in v0.1; additional providers
   planned”; or
2. Implement each advertised provider behind the same plan/observe/execute/
   reconcile/credential contract, with capability-specific UI filtering.

**Acceptance**

- README, `--help`, TUI, release notes, and runtime capability output agree.
- No provider is selectable unless its complete lifecycle is available.

### Resolve orphaned or duplicate origin implementations

**Current state**

The supervisor-owned origin manager now runs directories, commands, and
command-owned MCP sources. Directory origins receive a durable loopback port
allocation in the persisted profile, with deterministic fallback for legacy
profiles and collision avoidance for new profiles. Older Docker and unused
builtin origin packages remain, but are not represented by the current profile
source union or wizard.

**Implementation**

1. Connect Docker/static/file-browser implementations through one source
   contract and origin manager, with lifecycle persistence and diagnostics; or
2. Remove unused implementations and claims until their source contract is
   designed.
3. Remove or quarantine the remaining unused origin implementations before
   advertising them; the directory-port reservation gap is closed for new
   profiles.

**Acceptance**

- Every compiled origin backend is reachable from a supported profile type or
  intentionally removed.
- Origin collision, readiness failure, restart, close, delete, and supervisor
  shutdown have tests.

### Update stale repository documentation

**Current state**

`AGENTS.md` now correctly references integration and import-boundary tests
(`test/integration/supervisor_lifecycle_test.go` and
`test/architecture/import_boundaries_test.go`). The document still needs a
short account-store and multi-account note once the user-facing flow is
complete.

**Implementation and acceptance**

- ~~Correct those testing claims when the corresponding tests remain in place.~~ (done)
- Keep README capability language aligned with the actual Cloudflare matrix.
- Add a concise operator guide for account setup, backup, key rotation,
  operation recovery, and event resynchronization.

### Completed remediation items

The following audit findings have been addressed in recent commits:

- **CSRF protection** (`builtin_filebrowser.go`): HMAC-based CSRF tokens are
  generated per-form-action, validated with constant-time comparison, and
  embedded in upload and delete forms.
- **Provider PID checks removed** (`cloudflare/adapter.go`): Direct
  `syscall.Kill` liveness checks replaced with process manager observation.
  Process identity belongs to `process.Manager`.
- **Idempotency activated** (`store/sqlite.go`, `ipc/server.go`,
  `supervisor/supervisor.go`): The `idempotency_keys` table is now wired
  through the IPC `Idempotency-Key` header. Duplicate applies return the
  cached operation.
- **Event-driven reconciliation** (`supervisor/supervisor.go`): The reconcile
  loop now responds to connector exit/crash/unstable events via a buffered
  channel with coalescing, in addition to the 15s periodic ticker.
- **File browser HTTP method enforcement** (`builtin_filebrowser.go`): All
  handlers enforce their expected HTTP method (GET for browse/download/api,
  POST for upload/delete).
- **Read-only command validation** (`scripts/validate_readonly_command.py`):
  Standalone script checks command specs for write operations.
- **Descriptor-relative filesystem** (`builtin_filebrowser.go`): File browser
  now uses `openat()` with a pinned root file descriptor (O_PATH) to prevent
  symlink retarget attacks. All path operations are fd-relative, eliminating
  TOCTOU races. Legacy path-based resolution retained for BuiltinStatic.

### Release and operational verification

**Current state**

CI and a release workflow exist. The produced archive is now checked by
`scripts/verify_release_artifact.sh` before publication; release signing,
publisher-authenticity verification, and a clean-account install smoke test
remain unproven in a tag build.

**Implementation**

1. Exercise the release workflow on a prerelease tag and verify exact archive
   names/checksums expected by `install.sh`; the local archive check is done.
2. Sign artifacts/checksums or distribute a trusted public key separately from
   the release origin.
3. Test fresh-install, upgrade, and supervisor migration on a clean Linux
   user account.

**Acceptance**

- Download/install/checksum verification succeeds from the release output.
- `portico`, `portico doctor`, `portico list`, and supervisor startup work on
  a clean account without developer paths.

### Explicitly deferred v0.1 scope

The following remain intentional non-goals from `SPEC.md` §1.2, not defects to
paper over: Kubernetes, web dashboard, remote control plane, plugin
marketplace, arbitrary YAML, generic reverse proxy, stdio-to-network MCP
bridge, public UDP, non-Linux support, provider migration, multi-user local
authorization, and billing optimization. Revisit them only through a new
product decision and architecture plan.

## Verification matrix for all future work

Every item above should add proportional evidence:

| Area | Minimum evidence |
| --- | --- |
| Store/schema | migration fixture, corruption/rollback test, rotation test when secrets are involved |
| Controller/provider | success, permanent failure, timeout/unknown outcome, compensation, restart reconstruction |
| IPC/SSE | HTTP status mapping, durable append failure, reconnect/replay/resync behavior |
| Process/origin | identity/PID reuse, log redaction, shutdown ordering, readiness and cleanup |
| TUI/CLI | state-machine or command test, no secret output, capability/account filtering |
| Release | clean-user install and upgrade smoke test |

Run before each commit at minimum:

```bash
go1.25.13 test -race ./...
go1.25.13 vet ./...
staticcheck ./...
git diff --check
```
