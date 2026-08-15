# WAVE 0 — CONSOLIDATED AUDIT REPORT

**Date:** 2026-08-14
**Auditors:** A (connection-model), B (TUI), C (provider), D (observability), E (adversarial)

---

## 1. Connection-Model Audit (Agent A)

### Verified findings

| Finding | File:Line | Verdict |
|---------|-----------|---------|
| `CreateConnectionRequest` missing `PrivateNetwork` and `ClientTunnel` arms | `ipc/dto.go:438-450` | CONFIRMED |
| Supervisor refuses `private_network` and `client_tunnel` with "not implemented" | `supervisor/supervisor.go:809-815` | CONFIRMED |
| Persistence works for all kinds (encodeProfileSpec/decodeProfileSpec) | `store/sqlite.go` | CONFIRMED |
| `EffectiveKind()` defaults to `service_exposure` | `core/connection.go:174,182` | CONFIRMED |
| Wizard hardcodes `Kind: "service_exposure"` | `tui/screens/wizard.go:969` | CONFIRMED |
| CLI create doesn't set Kind at all | `cli/handler.go:914-924` | CONFIRMED |
| Capability checker defaults to service_exposure | `core/capability.go:234` | CONFIRMED |

### False positive (caught by Agent E)

| Finding | Status |
|---------|--------|
| `ClientTunnelSpecDTO` MCP field missing | **FALSE** — field IS present at `dto.go:111` |

### Exact changes needed in `internal/ipc/dto.go`

```go
type CreateConnectionRequest struct {
    // existing fields...
    ServiceExposure *ServiceExposureSpecDTO `json:"service_exposure,omitempty"`
    PortForward     *PortForwardSpecDTO     `json:"port_forward,omitempty"`
    PrivateNetwork  *PrivateNetworkSpecDTO  `json:"private_network,omitempty"`  // ADD
    ClientTunnel    *ClientTunnelSpecDTO    `json:"client_tunnel,omitempty"`    // ADD
}
```

---

## 2. TUI Workflow Audit (Agent B)

### Screen/action map

| Screen | Actions | Reachable |
|--------|---------|-----------|
| Home | Space (toggle), e (edit), c (copy), d (delete), r (repair), Enter (inspect) | yes |
| Inspect | tabs, edit, copy | yes (partial — missing lifecycle) |
| Wizard | kind selection (broken — hardcoded service_exposure) | broken |
| Operations | view, cancel | yes |
| Providers | add, remove (missing repair/reverify) | partial |
| Discovery | Enter → Share | yes |
| Diagnostics | view findings | yes |
| Plan Preview | apply, cancel | yes |
| Repair | plan, apply | yes |
| Settings | — | yes |

### Critical gaps

| Gap | File:Line | Severity |
|-----|-----------|----------|
| Wizard only creates `service_exposure` | `wizard.go:969` | P0 |
| 9+ unreachable DTO fields (PortForward, PrivateNetwork, ClientTunnel) | `dto.go` | P0 |
| Inspect missing lifecycle actions (Space, r, d) | `screens/inspect.go` | P1 |
| Provider screen missing reverify/replace-credential | `screens/providers.go` | P0 |
| `s`/`p` keys blocked on modal screens | `app.go` | P2 |

---

## 3. Provider Layer Audit (Agent C)

### Availability/stability collapse

| Current | File:Line | Problem |
|---------|-----------|---------|
| `Selectable()` only true for `AvailabilityReady` | `registry.go:71` | Blocks experimental providers even when enabled |
| Ngrok always returns `AvailabilityExperimental` | `ngrok/definition.go:96,123` | Never selectable even when working |
| OpenAI tunnel always returns `AvailabilityExperimental` | `openaitunnel/definition.go:76` | Same |

### Exact fix for ngrok

```go
// ngrok/definition.go:Activate()
if d.cfg.Enabled {
    entry.Availability = provider.AvailabilityReady
} else {
    entry.Availability = provider.AvailabilityExperimental
}
```

### Provider capability matrix

| Provider | Availability | Selectable | Kinds | Notes |
|----------|--------------|------------|-------|-------|
| cloudflare | Ready (with account) | yes | ServiceExposure | Full features |
| portforward | Ready | yes | ServiceExposure | No account needed |
| ngrok | Experimental (always) | **no** | ServiceExposure | Should be selectable when enabled |
| openai_tunnel | Experimental (always) | **no** | ClientTunnel | Should be selectable when enabled |
| tailscale | NotImplemented | no | — | Catalog only |
| zrok | NotImplemented | no | — | Catalog only |

### Tailscale adapter requirements

- New package: `internal/provider/tailscale/`
- Must implement `Definition` + `Provider` interfaces
- `Kinds = []ConnectionKind{ConnectionPrivateNetwork}`
- `PrivateExposure.Supported = true`
- `TemporaryAddresses.Supported = false`
- `CustomHostnames.Supported = false`
- `ManagedDNS.Supported = false`
- Protection: `ProtectionOnly` (Tailscale handles ACLs)
- Check for `tailscale` binary in `Activate()`

---

## 4. Observability Audit (Agent D)

### Log pipeline — WORKS end-to-end

```
actor.launch() → rotating log files → supervisor.HandleConnectionLogs()
    → IPC → TUI connectionLogsCmd() → InspectModel.LogTail → renderLogs()
```

### Stale fallback

| File:Line | Text | Verdict |
|-----------|------|---------|
| `tui/screens/inspect.go:355-368` | "Log capture is not implemented." | **FALSE** — feature exists. DELETE. |

### Doctor command bugs (`cli/handler.go:798-848`)

| Line | Bug | Fix |
|------|-----|-----|
| 804 | Hardcoded `portico-key.bin` path | Use `paths.SecretStorePath()` |
| 808-812 | Exits 0 when supervisor down | Return error / non-zero exit |
| 820-831 | Uses `Authenticated` not `Availability/Readiness` | Use snapshot DTO fields |
| 842-846 | Discovery error collapsed with empty | Separate error vs zero results |

### Telemetry gap

| Component | Status |
|-----------|--------|
| `core.Capabilities.Telemetry` | Defined |
| ngrok adapter `Telemetry()` | Implemented |
| `TelemetryProvider` interface | **MISSING** — no standard contract |
| Supervisor telemetry sampling | **MISSING** |
| IPC telemetry DTO | **MISSING** |
| TUI Activity rendering | **MISSING** |

### Secret handling — CORRECT

- `RedactingWriter` at log write (actor) AND log read (supervisor)
- `zeroBytes()` after account config
- `Resolver` interface never serializes secrets
- Credential detection reports presence only, never values

---

## 5. Adversarial Audit (Agent E) — CRITICAL MISSED BUGS

### Security regression (CRITICAL — missed by A-D)

| File:Line | Bug | Impact |
|-----------|-----|--------|
| `provider/cloudflare/adapter.go:Plan()` | Uses `GetSource()`, `GetExposure()`, `GetProtection()`, `IsProtected()` | For non-service-exposure kinds: empty source, empty protection → treated as **unprotected public service** |
| `provider/ngrok/adapter.go:Plan()` | Same backward-compat accessors | Same security regression |
| `controller/controller.go:PlanOpen()` | Calls `GetSource()` for all kinds | Silent misconfiguration |

**Correct behavior:** `portforward` and `openai_tunnel` adapters check `profile.Kind` and access `profile.Spec.*` arms directly. Cloudflare and ngrok must do the same.

### Duplicate authorities (HIGH)

| Concept | Authority 1 | Authority 2 | Authority 3 |
|---------|-------------|-------------|-------------|
| Connection kind | `core/connection.go:EffectiveKind()` | `ipc/dto.go:CreateConnectionRequest` | `tui/wizard.go:buildRequest()` |
| Provider selection | `registry.go:Selectable()` | `recommendation.go` | `wizard_choices.go` |

### Stale-response race (MEDIUM)

| File:Line | Bug |
|-----------|-----|
| `supervisor/supervisor.go:HandleApplyPlan()` | Late responses from a previous wizard session can overwrite a newer attempt if idempotency keys collide |

### Other missed bugs

| Bug | File:Line | Severity |
|-----|-----------|----------|
| Recommendation engine checks exposure mode for all kinds | `recommendation.go` | MEDIUM |
| `describeOriginOwnership` fallthrough | `supervisor/supervisor.go` | MEDIUM |
| Edit plan uses bad accessors | `controller/edit.go` | MEDIUM |
| No provider executes `private_network`/`port_forward` | all adapters | HIGH |
| `HandleCreateConnection` fallthrough bug | `supervisor/supervisor.go` | LOW |
| `Capability.Executes()` default assumes service_exposure | `core/capability.go:234` | LOW |

---

## 6. Cross-cutting invariants to enforce

1. **One authority per concept** — connection kind, provider selection, availability
2. **No backward-compat accessors for new kinds** — `GetSource()/GetExposure()/GetProtection()` must not be used in provider `Plan()` for non-service-exposure kinds
3. **Explicit kind branching** — every provider adapter must switch on `profile.Kind` and access the correct spec arm
4. **Stability != Availability** — experimental providers can be ready+selectable
5. **Unavailable != Empty** — DTOs carry `Available`+`Unavailable` reason; TUI must render both distinctly
6. **Secrets never serialized** — verified correct, maintain defense-in-depth

---

## 7. Wave 0 deliverables

| Deliverable | Location |
|-------------|----------|
| Agent A report | `/home/bamn/Portico/CONNECTION_MODEL_AUDIT_REPORT.md` |
| Agent B report | delegation transcripts |
| Agent C report | delegation transcripts |
| Agent D report | delegation transcripts |
| Agent E report | delegation transcripts |
| This consolidated report | `/home/bamn/Portico/WAVE0_CONSOLIDATED_REPORT.md` |

---

## 8. Recommended Wave 1 order (adjusted from adversarial findings)

1. **Separate availability from stability** (registry.go, dto.go, supervisor.go, tui/app.go)
2. **Complete creation tagged unions** (dto.go — PrivateNetwork, ClientTunnel arms)
3. **Propagate provider-account failure reasons** (UnusableReason through IPC)
4. **Fix Cloudflare/ngrok Plan() accessors** (security regression — use kind-specific spec arms)
5. **Add port-forward TUI creation** (wizard.go)
6. **Fix provider repair/reverification UX** (providers screen)
7. **Fix Doctor/key/log state correctness** (handler.go, inspect.go)
8. **Add inspect lifecycle actions** (inspect.go)
