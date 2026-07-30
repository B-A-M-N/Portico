# Remaining Work

**Baseline:** `da0667e` (`feat: harden Portico connection lifecycle`)

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

1. Add account revoke/remove and update endpoints with a plan that protects
   profiles still bound to that account. Do not remove credentials until no
   profile or recovery item needs them.
2. Replace environment-token login with stdin/protected-file-descriptor input
   where practical, while keeping secrets out of command arguments, logs,
   events, and responses.
3. Add account lifecycle controls to the provider-management screen; it already
   lists account labels/statuses, and inspect shows the selected account ID.
4. Migrate the one-account config bootstrap into the account repository once.
   Do not keep a permanent config-file token fallback after successful
   migration. Retain the old read path only behind an explicit migration action
   with a visible result.

**Acceptance**

- Two accounts with different tokens/zones can be added, listed, selected in
  the TUI and CLI, restarted, and used without cross-account API calls.
- A profile created while one account exists retains that account after another
  is added.
- A missing/revoked account produces a typed availability error before any
  provider request.
- Tests cover selection, restart reconstruction, wrong-account rejection, and
  encryption-key rotation of every account credential.

## P0 — provider contract and live Cloudflare confidence

### Complete Cloudflare contract testing

**Current state**

The Cloudflare adapter has direct unit tests and exact-resource observation,
but no fake HTTP API suite that proves request construction and response
classification through the actual Cloudflare client.

**Implementation**

1. Add a fake Cloudflare API server/transport fixture for tunnel, DNS, Access
   application, and Access policy endpoints.
2. Exercise exact-ID observation after a reconstructed supervisor, including
   404, 401/403, 429, 5xx, malformed JSON, and timeouts.
3. Verify DNS-only and Access-only drift repair makes the smallest update and
   does not recreate an intact tunnel or connector.
4. Verify credential-file creation, readiness, and cleanup for every connector
   start failure path using the real adapter flow.

**Acceptance**

- No live Cloudflare account is needed in CI.
- Tests prove that unauthorized, rate-limited, and transient observations are
  never interpreted as a missing managed resource.
- Tests prove terminal verification uses remote state, not adapter memory.

### Remove or isolate obsolete provider execution paths

**Current state**

`core.Provider` no longer requires `Apply`, `Repair`, or `Remove`, but both the
Cloudflare and mock adapters still contain those old asynchronous execution
implementations. `portico legacy` also remains a second mutation architecture.

**Implementation**

1. Add characterization tests for every legacy command that is intentionally
   retained during migration.
2. Delete adapter `Apply`, `Repair`, and `Remove` once no callers remain, or
   move them into a clearly isolated legacy-only adapter that cannot be reached
   by the supervisor registry.
3. Remove `portico legacy` only after equivalent new supervisor workflows and
   characterization coverage exist. Until then, warn that it does not share
   Portico lifecycle ownership and block unsafe resource mutations if needed.

**Acceptance**

- A repository search finds no production call path that mutates Cloudflare
  outside `ExecuteStep` under controller/supervisor journaling.
- The documented legacy removal gate in `SPEC.md` §3.2 is satisfied.

## P1 — finish the beginner workflow

### Complete wizard source configuration

**Current state**

The wizard can create existing-service, directory, command, and endpoint-based
MCP profiles. Command sources collect an executable, port, comma-separated
arguments, and working directory. Directory sources collect static-site versus
file-browser mode, upload/delete permissions, and SPA fallback. Endpoint MCP
sources collect HTTP, streamable HTTP, or (for permanent Cloudflare exposure)
SSE transport. The wizard also supports command-owned MCP servers. It still
lacks environment references and shell-mode configuration.

**Implementation**

1. Make wizard fields capability- and source-specific rather than a static
   sequence. Reuse the IPC source DTOs; do not construct a parallel model.
2. Add safe tokenized input for command arguments and environment *references*
   (not raw secrets). Validate every screen before advancing.
3. Provide a directory mode chooser, file-browser permissions, and SPA option.
4. Keep transport choices constrained to combinations the selected provider can
   carry, including when an MCP command is owned by Portico.
5. Add a final review that states owned-process behavior, chosen account,
   public/private exposure, protection, and destructive implications.

**Acceptance**

- Every field accepted by the production source DTO is either configurable in
  the wizard or explicitly unavailable with an explanation.
- TUI state-machine tests cover back navigation, validation, request mapping,
  IPv6 addresses, multi-account selection, and no duplicate submission.

### Connection editing and account/provider visibility

**Current state**

The TUI provides create, open/close, inspect, diagnostics, and repair basics;
it does not yet present a complete edit workflow for existing desired state or
provider-account lifecycle.

**Implementation**

1. Add profile edit/clone flows backed by revision-aware update endpoints.
2. Show provider, selected account, origin ownership, and unresolved findings
   in inspect and provider screens.
3. When an edit changes remote desired state, route through normal plan,
   confirmation, apply, operation progress, and verification—not a direct save.

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

Portico writes its XDG TOML configuration and reads/migrates legacy Flare
configuration and credential files for compatibility. This is safer than the
old plaintext fallback but leaves two concepts of account setup.

**Implementation**

1. Define the final Portico-only configuration contract in user documentation:
   `PORTICO_*`, XDG config/state/runtime paths, and account repository.
2. Add an explicit, idempotent migration command that imports legacy settings
   and reports exactly what it moved. Back up before changing legacy files.
3. Remove automatic legacy reads after a defined compatibility release window.
4. Delete instructions that tell users to run `flare` commands.

**Acceptance**

- A fresh install never creates or needs a Flare path.
- A legacy install migrates once without exposing plaintext tokens and reports
  any ambiguous values for user confirmation.

### Complete key lifecycle operations

**Current state**

Installation-key rotation and tunnel/provider credential re-encryption exist,
but there is no supported CLI/API command, status view, or operational runbook
for rotation, backup, or recovery from a rejected key file.

**Implementation**

1. Add a supervisor-owned rotate-key operation with durable event/audit output.
2. Expose only status and rotation result, never key material or ciphertext.
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
applies no access protection. Tailscale and zrok remain architectural targets,
not implementations.

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
command-owned MCP sources. Older Docker and builtin origin packages remain,
but are not represented by the current profile source union or wizard.

**Implementation**

1. Connect Docker/static/file-browser implementations through one source
   contract and origin manager, with lifecycle persistence and diagnostics; or
2. Remove unused implementations and claims until their source contract is
   designed.
3. Decide whether deterministic directory ports should gain a user-configured
   override or a safe reservation mechanism for collisions.

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

CI and a release workflow exist, but release publishing, artifact signing, and
an installed-binary smoke test are not yet proven in a tag build.

**Implementation**

1. Exercise the release workflow on a prerelease tag and verify exact archive
   names/checksums expected by `install.sh`.
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
go1.25.12 test -race ./...
go1.25.12 vet ./...
staticcheck ./...
git diff --check
```
