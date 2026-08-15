# PORTICO IMPLEMENTATION PLAN — PERSISTENT TODO

**Created:** 2026-08-14
**Task:** Execute the Portico Code Edit Audit and Implementation Plan to a T.
**Coordinator rules:** One authority per concept, tests precede fixes, no agent approves own work, adversarial review every wave.

---

## Current position

- [x] Wave 0 — Recon and contracts (read-only auditors A-E) — **COMPLETE**
  - Reports: `/home/bamn/Portico/WAVE0_CONSOLIDATED_REPORT.md`
- [x] Wave 1 — Foundational fixes (lanes 1-3)
- [x] Adversarial Gate 1
- [x] Wave 2 — User workflows (lanes A-D)
- [ ] Adversarial Gate 2
- [x] Wave 3 — Observability (telemetry contract: internal/provider/telemetry.go)
- [ ] Wave 4 — Provider expansion (4A-4E)
- [ ] Adversarial Gate 3
- [ ] Wave 5 — TUI decomposition
- [ ] Wave 6 — Legacy removal and release

---

## Recommended implementation order (REVISED per Wave 0 findings)

### Wave 1 — Foundational fixes

1. **Separate availability from stability** (`internal/provider/registry.go`, `internal/core/capability.go`)
   - Add `Stability` to `CatalogEntry` and `ProviderSnapshot`
   - `Selectable()` should mean "can plan/execute now" not "production-stable"
   - Carry stability through `internal/ipc/dto.go` and `internal/supervisor/supervisor.go`
   - Render stability in `internal/tui/app.go` ("Experimental • Ready")
   - Tests: disabled→unavailable, enabled+missing exe→client missing, enabled+missing cred→unconfigured, enabled+exe+cred→selectable+experimental, stable unchanged

2. **Complete creation tagged unions** (`internal/ipc/dto.go`)
   - Add `PrivateNetwork *PrivateNetworkSpecDTO`, `ClientTunnel *ClientTunnelSpecDTO` to `CreateConnectionRequest`
   - Refuse inconsistent combinations exactly as core tagged union does
   - Tests: kind=private_network + no PrivateNetwork arm → rejected, etc.

3. **Propagate provider-account failure reasons** (`internal/ipc/dto.go`)
   - Add `UnusableReason string` to `ProviderAccountDTO`
   - Propagate from `provider.AccountInfo` → supervisor → IPC → TUI
   - Tests: pending/unverified, missing credential, unreadable, multi-account restriction

### Wave 1.5 — Security regression fix (CRITICAL, from Agent E)

4. **Fix Cloudflare/ngrok Plan() accessors** — SECURITY REGRESSION
   - `provider/cloudflare/adapter.go:Plan()` uses `GetSource()`, `GetExposure()`, `GetProtection()`, `IsProtected()` which return zero values for non-service-exposure kinds
   - For `port_forward`/`private_network`/`client_tunnel`: empty source, empty protection → treated as **unprotected public service**
   - Fix: Cloudflare and ngrok must switch on `profile.Kind` and access `profile.Spec.*` arms directly (like portforward/openai_tunnel already do)
   - Also fix `controller/controller.go:PlanOpen()` which calls `GetSource()` for all kinds
   - Tests: non-service-exposure kinds must NOT be planned with empty protection

### Wave 2 — User workflows

5. **Add port-forward TUI creation** (`internal/tui/screens/wizard.go`)
   - Add `ConnectionKind` to `WizardState`
   - Port-forward recipe: name → local port → remote host → remote port → TCP → review
   - Client tunnel recipe: MCP endpoint → transport → provider → config → review
   - Do not ask hostname/protection/provider-account for non-service kinds

6. **Fix provider repair/reverification UX** (`internal/tui/screens/providers.go`)
   - Add Reverify and Replace credential actions
   - Reuses existing configure-account path with prefilled metadata

7. **Fix Doctor/key/log state correctness** (`internal/cli/handler.go`, `internal/tui/screens/inspect.go`)
   - Use `Availability`/`Readiness` instead of `Authenticated`
   - Separate discovery error vs zero results
   - Delete stale "Log capture is not implemented" fallback (inspect.go:355-368)
   - Fix hardcoded key-file path
   - Add `LogsLoading`/`LogsLoaded`/`LogsError` state model

8. **Add inspect lifecycle actions** (`internal/tui/screens/inspect.go`)
   - Space (toggle), r (diagnose/repair), e (edit), c (copy), d (delete preview)
   - Must call exact same plan commands as Home

### Wave 3+ — Observability, provider expansion, TUI decomposition, release

9. Wire telemetry (`TelemetryProvider` interface in `internal/provider/`)
10. Graduate ngrok behind explicit experimental opt-in
11. Implement Tailscale/private-network support (`internal/provider/tailscale/`)
12. Finish OpenAI client-tunnel workflow
13. Design SSH/proxy connection types (`ConnectionProxy`, `ProxySpec`)
14. Decompose TUI god objects (`app.go` ~3945L, `wizard.go` ~1759L)
15. Remove legacy mutation architecture (`cmd/`)
16. Release/install verification
17. Update documentation

---

## Critical files

| File | Role |
|---|---|
| `internal/core/connection.go` | Central domain model |
| `internal/core/plan.go` | OperationPlan + fingerprint |
| `internal/core/capability.go` | Stability definitions |
| `internal/controller/controller.go` | Orchestration |
| `internal/provider/registry.go` | Provider registration + availability |
| `internal/provider/cloudflare/adapter.go` | Reference provider (has security regression in Plan()) |
| `internal/provider/ngrok/adapter.go` | Has same security regression in Plan() |
| `internal/provider/mock/provider.go` | Mock for tests |
| `internal/supervisor/supervisor.go` | Supervisor lifecycle |
| `internal/ipc/server.go` + `client.go` | IPC transport |
| `internal/ipc/dto.go` | DTOs (needs PrivateNetwork, ClientTunnel, Stability, UnusableReason) |
| `internal/store/sqlite.go` | Persistence + migrations |
| `internal/process/actor.go` + `logs.go` | Connector subprocess |
| `internal/cli/handler.go` | Doctor command (4 bugs) |
| `internal/tui/app.go` | Root TUI model (~3945 lines) |
| `internal/tui/screens/wizard.go` | Creation wizard (~1759 lines) |
| `internal/tui/screens/inspect.go` | Connection inspect (stale fallback + missing lifecycle) |
| `internal/tui/edit.go` | Connection editing |

---

## Key invariants

- TUI `View()` stays pure (SPEC rule #9)
- Profile != Runtime, never merge desired/observed (SPEC rule #8)
- Closing TUI never closes connections (SPEC rule #7)
- Credentials never serialized into plans/events/logs/runtimes/UI
- Process identity uses 4 fields (PID, StartTime, ExecutablePath, CommandHash)
- Resource ownership: managed/adopted/external — only managed auto-deleted
- **One authority per concept** — no duplicate kind/provider-selection decisions
- **Stability != Availability** — experimental providers can be ready+selectable
- **Unavailable != Empty** — DTOs carry Available+Unavailable reason

---

## Test requirements

Vertical test for every new connection type:
```
TUI key sequence → Create request → real IPC client → Unix socket →
IPC server → supervisor → controller → provider/mock → stored profile →
snapshot → TUI rendering
```

Release gates:
```bash
go test ./...
go test -race ./... -count=1
go vet ./...
staticcheck ./...
make acceptance
make validate
```
