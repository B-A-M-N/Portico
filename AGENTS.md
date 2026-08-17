# AGENTS.md — Portico

**Connection manager for local services.** Discovers local services, creates provider-backed connections, keeps them alive after TUI closes, diagnoses failures.

See the provider status table below for what is actually implemented. Naming
providers Portico does not implement, alongside ones it does, is how the
previous version of this document came to be wrong in both directions.

---

**Critical: READ `SPEC.md` first.** The SPEC is the authoritative architecture contract — this file only captures what's non-obvious from reading the code + SPEC together.

---

## Commands

| Command | Purpose |
|---|---|
| `go build -o portico .` | Build binary |
| `go test ./... -v` | Run all tests |
| `go test ./internal/controller/... -v -run TestFullLifecycle` | Specific test |
| `./portico` | Launch TUI |
| `./portico supervisor run` | Start supervisor daemon |
| `./portico list` | CLI list connections |

**Systemd service:** `scripts/portico-supervisor.service` — install with `cp scripts/portico-supervisor.service ~/.config/systemd/user/` then `systemctl --user enable --now portico-supervisor`.

**Note:** `make build` builds the `portico` binary with version ldflags; `go build -o portico .` also works.

## Structure

```
main.go              # Entry: cli.NewCLI()
internal/
  core/              # Pure domain types — no non-stdlib imports
  controller/        # Orchestrates providers, plans, operations
  supervisor/        # Daemon: DB, IPC server, process mgmt, reconciliation
  ipc/               # HTTP+SSE over Unix socket (client + server + DTOs)
  cli/               # New Portico CLI (talks to supervisor via IPC)
  tui/               # Bubble Tea TUI (talks to supervisor via IPC)
  provider/          # Provider interface + registry
    cloudflare/      # Cloudflare adapter and account-scoped provider router
    mock/            # Mock provider for controller tests
  store/             # SQLite (WAL mode, embedded migrations)
  process/           # Connector subprocess mgmt (identity-verified signaling)
  origin/            # Local service backends (local:http, builtin:static, docker, command)
  discovery/         # `ss`-based enumeration + HTTP probing
  diagnostics/       # Route-segment diagnostic engine
  credentials/       # Token resolution (env, keyring, memory)
  session/           # Legacy Flare sessions
  tunnel/            # cloudflared subprocess wrapper
  app/               # XDG path resolution + bootstrap
  testutil/          # Test helpers
  ui/                # Legacy CLI output formatting
```

## Non-obvious gotchas

1. **`make build` outputs `portico`** with version ldflags; `go build -o portico .` also works.
2. **Git history starts at the Portico baseline commit.** Repo originated from `paoloanzn/flare-cli`; pre-rename history is gone.
3. **`portico legacy ...` still active.** Old flare-cli commands preserved in `cmd/`, hidden from help.
4. **Provider status.** Derived from `internal/provider/builtin/catalog.go` and the
   adapters themselves. Check that file before trusting this table.

   Line counts are deliberately not cited: they would be wrong after the next
   edit to any adapter, and a number nobody re-checks is how documentation
   starts lying.

   | Provider | Adapter | Enabled by default | Account setup | Notes |
   |---|---|---|---|---|
   | cloudflare | yes, full adapter | yes | declarative flow: account ID, optional zone, API token | Tunnels, DNS, Access. Contract tests in `internal/tunnel`, `internal/dns`, `internal/access`. |
   | ngrok | yes, full adapter | no — experimental opt-in | agent auth token | Built against the real agent, not a stub. |
   | openai_tunnel | yes, full adapter | no — experimental opt-in | guidance only; Portico cannot hold the credential | Client-mediated, no public address. |
   | port_forward | yes, full adapter | yes | none needed | Local forwards only; remote forwards are refused with a reason. |
   | tailscale | no | n/a | n/a | Catalog entry only, so the UI can explain the gap rather than omit it. |
   | zrok | no | n/a | n/a | Catalog entry only. |
   | mock | test only | n/a | n/a | Used by controller tests. |

   A catalog entry is not an implementation. Tailscale and zrok appear in the
   provider list so a user is told Portico does not implement them, which is a
   different thing from them being available.
5. **Tunnel logs use Portico's XDG state log directory.** Legacy Flare paths are compatibility-read only.
6. **Credentials never serialized** into plans, events, logs, runtimes, or UI. Resolved by reference at operation time; durable tunnel credentials are keyed by exact connection, provider, and tunnel identity. Provider account credentials are encrypted behind opaque references, and profiles bind an account explicitly when one is selected.
7. **Plan fingerprints are SHA-256** of canonical JSON (`core/plan.go:ComputeFingerprint`).
8. **Process identity uses 4 fields** (PID, StartTime, ExecutablePath, CommandHash) — never signal on PID alone.
9. **Restart backoff:** 5 attempts in 10-min window (1s, 2s, 5s, 10s, 30s), then mark unstable.
10. **Resource ownership** (`managed` / `adopted` / `external`) — only `managed` resources auto-deleted.
11. **Space opens/closes, never deletes.** Delete is a separate destructive workflow with preview.
12. **TUI `View()` is pure** — no I/O, no clock reads, no mutations (SPEC rule #9).
13. **Profile != Runtime** — never merge desired state and observed state into one object (SPEC rule #8).
14. **Closing TUI never closes connections** (SPEC rule #7). Connections survive TUI exit.
15. **XDG fallback:** if `XDG_RUNTIME_DIR` unset, socket goes to `/tmp/portico-$UID/`.
16. **Go toolchain:** 1.25.13+ required (security floor).

## Testing

- **Mock provider** (`provider/mock/`) implements `core.Provider` in-memory — used in controller tests and CLI handler.
- **Ngrok provider tests** (`provider/ngrok/adapter_test.go`) cover identity, capabilities, planning, step execution (start/stop/verify/protection/delete), observation, repair, and removal. The adapter is real, built against the ngrok agent; it is not a stub.
- **Controller tests** use `newTestController()` + `testProfile()` + `awaitOp()` patterns; `executor_failure_test.go` injects step-commit and terminal-commit failures with fake committers.
- **Store tests** cover atomic step commits (`commit_test.go`) and migration-8 duplicate-ownership fixtures (`migration_test.go`).
- **Also covered:** process manager (adoption, backoff, rotation), IPC SSE journal (replay across restart, monotonic sequences), TUI state machine (fake client, no socket), discovery/diagnostics (fake enumerators/probers), core deep-copy isolation.
- **Integration:** `test/integration/supervisor_lifecycle_test.go` verifies supervisor lifecycle behavior.
- **Architecture:** `test/architecture/import_boundaries_test.go` enforces core import boundaries.

## Key files to start with

| File | Why |
|---|---|
| `SPEC.md` | **Read this first** — entire architecture contract |
| `internal/core/connection.go` | Central domain model |
| `internal/core/provider.go` | `Provider` interface |
| `internal/core/plan.go` | `OperationPlan` + fingerprint |
| `internal/controller/controller.go` | Controller orchestration |
| `internal/provider/cloudflare/adapter.go` | Reference provider implementation |
| `internal/provider/mock/provider.go` | Mock for testing |
| `internal/supervisor/supervisor.go` | Supervisor lifecycle |
| `internal/ipc/server.go` + `client.go` | IPC transport |
| `internal/store/sqlite.go` | Persistence + migrations |
