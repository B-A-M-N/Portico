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
- [x] Adversarial Gate 2
- [x] Wave 3 — Observability (telemetry contract: internal/provider/telemetry.go)
- [ ] Wave 4 — Provider expansion (4A-4E)
- [ ] Adversarial Gate 3
- [ ] Wave 5 — TUI decomposition
- [ ] Wave 6 — Legacy removal and release

## Critical architectural correction (2026-08-15)

**"OpenAI tunnel" is NOT a provider. It's a connection profile that runs over transport providers.**

The current `internal/provider/openaitunnel/` is architecturally wrong. It needs to become a profile (like a service-exposure profile) layered on top of transport providers: Cloudflare, ngrok, Tailscale, SSH.

Portico architecture:
```
                         PORTICO

                  ┌──────────────────┐
                  │ Connection Intent │
                  └────────┬─────────┘
                           │
                 ┌─────────▼──────────┐
                 │ Profiles            │
                 │ OpenAI / HTTP / SSH │
                 └─────────┬──────────┘
                           │
                 ┌─────────▼─────────┐
                 │ Security/Gateway   │
                 │ Auth / SSE / proxy │
                 └─────────┬─────────┘
                           │
              ┌────────────▼────────────┐
              │ Transport Provider       │
              ├──────────────────────────┤
              │ Cloudflare │ ngrok       │
              │ Tailscale  │ SSH         │
              └────────────┬─────────────┘
                           │
                ┌──────────▼──────────┐
                │ Runtime Supervisor   │
                │ process/state/health │
                └──────────┬──────────┘
                           │
                   ┌───────▼───────┐
                   │ Remote Endpoint │
                   └───────────────┘
```

### Three-state health

Portico distinguishes:
- **PROCESS** — Is the connector alive?
- **TRANSPORT** — Is the externally reachable tunnel alive?
- **SERVICE** — Does the intended application actually work?

Not just "PID alive."

### Portico Gateway

A local auth/SSE proxy between client and tunnel. Prevents accidentally exposing unauthenticated local servers. Transparent to streaming — no buffering, flush chunks immediately.

### Provider order

1. **Common connection/spec/capability model**
2. **Common process supervisor**
3. **Health/reconciliation subsystem**
4. **Portico auth/streaming gateway**
5. **Cloudflare named tunnels**
6. **OpenAI-compatible profile over Cloudflare**
7. **Full remote streaming verification**
8. **Cloudflare Quick Tunnel as explicit dev-only mode**
9. **ngrok**
10. **OpenAI profile over ngrok — should require almost no OpenAI-specific new code** (key architectural test)
11. **Tailscale Serve**
12. **Tailscale Funnel**
13. **OpenAI profile over Tailscale**
14. **SSH local/remote forwarding**
15. **SOCKS/proxy connection profiles**
16. Later providers.

---

## Revised development sequence

### Phase 1 — Clean up (current)
1. Freeze + checkpoint the known-good state — **DONE** (0c53e4f)
2. Remove known architectural lies — **DONE: cmd/ removed** (8870ab3)

### Phase 2 — Architectural correction
3. **OpenAI profile refactor** — convert openaitunnel provider to a profile
   - OpenAI profile runs over Cloudflare, ngrok, Tailscale, SSH
   - Portico Gateway for auth/SSE streaming
   - Profile probes: /v1/models, normal request, streaming, auth

### Phase 3 — Harden Cloudflare (reference provider)
4. Cloudflare becomes the reference implementation
5. Creation, configuration, startup, shutdown, reconnection
6. State discovery, credentials, failure handling
7. Orphan cleanup, URL/status reporting, logging, diagnostics

### Phase 4 — Finish OpenAI end-to-end
8. OpenAI profile over Cloudflare
9. Full streaming verification
10. Profile discovery: /v1/models probe, capability detection
11. Usable client output: base URL, API token, env vars

### Phase 5 — Incremental TUI extraction
12. Extract architectural boundaries one at a time, driven by provider work
13. Application shell → state/model → routing → input → orchestration → status

### Phase 6 — Experimental providers
14. Graduate ngrok behind explicit opt-in gating
15. Three maturity states: experimental → supported → default

### Phase 7 — Adversarial pass + release
18. Adversarial remediation pass
19. Clean-machine install/first-run testing
20. Cut v0.1

---

## v0.1 release gate

```
V0.1 RELEASE GATE
[ ] Current work checkpoint committed
[ ] Dead CLI implementation removed                    ← DONE
[ ] CLI behavior parity verified
[ ] Cloudflare workflow fully hardened
[ ] OpenAI profile fully implemented (streaming, auth, verification)
[ ] Experimental-provider gating implemented
[ ] TUI responsibility extraction completed where needed
[ ] clean-machine installation tested
[ ] upgrade/reinstall tested
[ ] configuration persistence tested
[ ] failure/recovery scenarios tested
[ ] orphan-process cleanup tested
[ ] build passes
[ ] vet passes
[ ] static analysis passes
[ ] unit tests pass
[ ] integration tests pass
[ ] race tests pass
[ ] acceptance matrix passes
[ ] documentation matches actual behavior
[ ] version reporting works
[ ] packaged binary/install method verified
```

Tailscale, SSH, SOCKS, generic proxies, etc. should **not** block v0.1.

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
- **Three-state health** — PROCESS, TRANSPORT, SERVICE (not just PID)
- **OpenAI is a profile, not a provider** — runs over transport providers
- **Gateway is streaming-transparent** — no buffering, immediate flush

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

---

## Critical files

| File | Role |
|---|---|
| `internal/core/connection.go` | Central domain model |
| `internal/core/plan.go` | OperationPlan + fingerprint |
| `internal/core/capability.go` | Stability definitions |
| `internal/controller/controller.go` | Orchestration |
| `internal/provider/registry.go` | Provider registration + availability |
| `internal/provider/cloudflare/adapter.go` | Reference provider |
| `internal/provider/openaitunnel/adapter.go` | **TO BE REFACTORED** — profile, not provider |
| `internal/supervisor/supervisor.go` | Supervisor lifecycle |
| `internal/ipc/server.go` + `client.go` | IPC transport |
| `internal/ipc/dto.go` | DTOs |
| `internal/store/sqlite.go` | Persistence + migrations |
| `internal/process/actor.go` + `logs.go` | Connector subprocess |
| `internal/cli/handler.go` | Doctor command |
| `internal/tui/app.go` | Root TUI model |
| `internal/tui/screens/wizard.go` | Creation wizard |
| `internal/tui/screens/inspect.go` | Connection inspect |
| `internal/tui/edit.go` | Connection editing |
