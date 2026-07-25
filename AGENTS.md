# AGENTS.md — Portico

**Connection manager for local services.** Discovers local services, creates provider-backed connections (Cloudflare, ngrok, Tailscale, zrok), keeps them alive after TUI closes, diagnoses failures.

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

**Gotcha:** Makefile is stale — builds `flare` binary with old ldflags. Use `go build -o portico .` directly.

## Structure

```
main.go              # Entry: cli.NewCLI() + legacy command tree
cmd/                 # Legacy flare-cli Cobra commands (hidden under "portico legacy")
internal/
  core/              # Pure domain types — no non-stdlib imports
  controller/        # Orchestrates providers, plans, operations
  supervisor/        # Daemon: DB, IPC server, process mgmt, reconciliation
  ipc/               # HTTP+SSE over Unix socket (client + server + DTOs)
  cli/               # New Portico CLI (talks to supervisor via IPC)
  tui/               # Bubble Tea TUI (talks to supervisor via IPC)
  provider/          # Provider interface + registry
    cloudflare/      # Cloudflare adapter (adapter.go only — SPEC envisions split)
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

1. **Makefile outputs `flare`, not `portico`.** Use `go build -o portico .` directly.
2. **No git history.** Repo was cloned from `paoloanzn/flare-cli` then renamed; `.git` is gone.
3. **`portico legacy ...` still active.** Old flare-cli commands preserved in `cmd/`, hidden from help.
4. **Only Cloudflare + mock providers exist.** Ngrok, Tailscale, zrok directories are SPEC-only stubs.
5. **Tunnel logs use `~/.config/flare-cli/logs`.** Legacy path not yet migrated to XDG state dir.
6. **Credentials never serialized** into plans, events, logs, runtimes, or UI. Resolved by reference at operation time.
7. **Plan fingerprints are SHA-256** of canonical JSON (`core/plan.go:ComputeFingerprint`).
8. **Process identity uses 4 fields** (PID, StartTime, ExecutablePath, CommandHash) — never signal on PID alone.
9. **Restart backoff:** 5 attempts in 10-min window (1s, 2s, 5s, 10s, 30s), then mark unstable.
10. **Resource ownership** (`managed` / `adopted` / `external`) — only `managed` resources auto-deleted.
11. **Space opens/closes, never deletes.** Delete is a separate destructive workflow with preview.
12. **TUI `View()` is pure** — no I/O, no clock reads, no mutations (SPEC rule #9).
13. **Profile != Runtime** — never merge desired state and observed state into one object (SPEC rule #8).
14. **Closing TUI never closes connections** (SPEC rule #7). Connections survive TUI exit.
15. **XDG fallback:** if `XDG_RUNTIME_DIR` unset, socket goes to `/tmp/portico-$UID/`.

## Testing

- **Mock provider** (`provider/mock/`) implements `core.Provider` in-memory — used in controller tests and CLI handler.
- **Controller tests** use `newTestController()` + `testProfile()` + `awaitOp()` patterns.
- **No integration/e2e tests yet** — directories exist but empty.
- **No import-boundary tests yet** — SPEC requires them but not implemented.

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