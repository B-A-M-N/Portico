# Zone scope design — connection-scoped DNS zone (finding 7)

**Authority:** Accepted implementation contract from `portico-architect`
(invoked for this Tier 3 change per CLAUDE.md hard gate 1), verified against
the repository by the main session before implementation. The architect's
contract is extended in one place (the wizard zone picker) where the audit's
own acceptance requires per-connection zone choice; that deviation is justified
below.

**Finding:** second-audit finding 7. The DNS zone is modeled as metadata bound
to the provider account, so one Cloudflare account carries exactly one selected
zone. Two connections under one identity that need different zones cannot
coexist. The credential belongs to the account; the zone belongs to the
connection.

## Restored invariant

The account is the long-lived unit of credential + identity + verified
capabilities. The connection is the unit of zone + hostname. A zone must not be
pinned to the account, because the zone's lifetime is bound to the connection's
intent, not to the credential's identity. Updating account credentials or the
account-default zone must never silently retarget a connection's planned zone.

## Existing failure mechanism (verified)

`internal/provider/cloudflare/definition.go:135,140` reads
`zone := account.Metadata["zone_id"]` at activation and builds a zone-bound
child adapter via `newWithOptions(..., zone, ...)`. Every DNS operation in that
child uses `p.zoneID` (`adapter.go:816, 1230, 1367, 1569`). Because `Activate`
runs once per account, all connections under that account share one zone.
Changing the account's zone (via `HandleSelectProviderAccountZone`,
`account_zone.go:90-92`) silently retargets every connection under that
account.

## Design

### Durable slot — no SQL migration

The connection already carries a per-connection free-form slot:
`DriverSelection.Options map[string]string` (`internal/core/connection.go:160`),
returned by `GetProvider()` (`connection.go:291-293`) and persisted verbatim at
create (`supervisor.go:1643`) and edit (`supervisor.go:2325-2329`). The
connection profile is stored as versioned JSON blobs (`exposure_json`,
`provider_json`; `internal/store/sqlite.go:62-73`), so the zone occupies
`Driver.Options["zone_id"]` with no column or migration. The chosen zone is the
connection's when present; otherwise the adapter falls back to the account
zone.

**Legacy account zone migration:** none needed. Existing connections under an
account with `metadata["zone_id"]` carry no `Options["zone_id"]`; the adapter
fallback to the account zone preserves their behavior exactly at read time.
Copying the account zone into existing connections would create a second
authority that can drift — rejected.

### Adapter: per-connection zone (core change)

All within `internal/provider/cloudflare/adapter.go`:

1. **`Plan`** (line 384) resolves the zone once for a permanent-exposure
   connection:
   `zoneID := profile.GetProvider().Options["zone_id"]; if zoneID == "" { zoneID = p.zoneID }`.
   The permanent-exposure guard currently `if p.zoneID == "" { … }` (line 484)
   must use the resolved zone, so a connection bearing its own zone plans even
   when the account default is empty (Quick-Tunnel-only account).
2. **Plan step parameters** carry the zone: add `"zone_id": zoneID` to the
   `cf-validate` step (line ~508) and `cf-dns` step `Technical.Parameters`.
3. **`ExecuteStep`** (line 1109) resolves the zone per step:
   `zoneID := step.Technical.Parameters["zone_id"]; if zoneID == "" { zoneID = p.zoneID }`,
   and uses it in the DNS handlers `StepCreateDNSRecord` (line 1230 →
   `p.dns.CreateCNAME(ctx, zoneID, …)`), the update handler (line 1367 →
   `UpdateCNAME(ctx, zoneID, …)`), and `StepDeleteRecord` (line 1569 →
   `DeleteRecord(ctx, zoneID, …)`).
4. **DNS resource metadata** carries the zone so post-restart observation
   reconciles in the right zone: `StepCreateDNSRecord` result embeds
   `Metadata: map[string]string{"zone_id": zoneID}` on the DNS
   `ProviderResource`. `provider_resources.metadata_json` already persists it
   (`internal/store/sqlite.go:111`).
5. **`ObserveWithResources`** (line ~816) resolves the zone from the DNS
   resource's `Metadata["zone_id"]`, falling back to `p.zoneID`.

`StepValidateAccount` (line 1119) may additionally verify zone membership when
a `zone_id` parameter is present, reusing the `validator.VerifyZone` pattern
(`account_zone.go:72`) — a fail-closed guard, not required for correctness.

### Wizard: choose zone only for permanent Cloudflare exposure

The hostname step is already gated on permanent exposure
(`internal/tui/screens/wizard_steps.go:92-93`). Two changes:

- The wizard must set `req.Provider.Options["zone_id"]` on the create/edit
  request when the connection is a permanent Cloudflare exposure, so the zone
  becomes connection-scoped rather than defaulting to the account.
- `hostnameChoiceRows()` (`internal/tui/screens/wizard.go:2593-2607`) currently
  offers `<name>.<account.ZoneName>` rows from the account's single selected
  zone. The audit's acceptance requires a connection in zone B under an account
  whose default is zone A.

**Deviation from the architect contract (documented):** the architect scoped
out "zone discovery on the snapshot path" and left open whether a
`ListAccountZones` endpoint is needed. The audit's own acceptance — "create
connection A in zone A; create connection B in zone B" — cannot be satisfied
from the account's single selected zone alone: a typed `<host>.<zone>` hostname
cannot supply the zone ID DNS needs. Therefore a thin `ListAccountZones`
supervisor/IPC endpoint is added, and the hostname step offers the account's
discoverable zones (each tagged with its ID) ahead of the manual-entry row.
When the account carries no zone list, the fallback remains the account's
selected zone or manual entry — a backward-compatible degradation.

### Compensation and failure semantics

DNS create/update/delete compensation already routes through the same
`step.Technical.Parameters`, so a compensation step executed after a restart
also carries the connection's zone (the compensation step is built from the
same plan step parameters). If `Options["zone_id"]` names a zone the credential
cannot access, the Cloudflare API call fails closed.

### Dependency ordering

1. Adapter zone resolution (`Plan`, `ExecuteStep`, `ObserveWithResources`,
   DNS resource metadata) — all within `adapter.go`.
2. `ListAccountZones` IPC endpoint (supervisor + ipc DTO + route).
3. Wizard: zone choice + `Options["zone_id"]` on the create/edit request.
4. No store migration.

## Completion criteria

- Two connections under one Cloudflare account can plan and execute against
  different zones (`cf-dns` step of each carries its own `zone_id`).
- Updating account credentials or the account-default zone does not change any
  connection's planned zone.
- A permanent connection's `Options["zone_id"]` survives create → snapshot →
  profile JSON → adapter `GetProvider().Options`.
- Post-restart observation of a DNS resource reconciles against the zone in the
  resource's metadata, not the account default.
- Quick Tunnel (temporary) connections never carry a zone and never ask for
  one.
- Legacy connections without `Options["zone_id"]` keep planning against the
  account zone (fallback is the migration).
- Full gate set passes.

## Explicit non-goals

- Do not remove `zone_id` from account metadata (it remains the fallback
  default).
- Do not add a store migration or new column.
- Do not change the `Provider` interface signature.
- Do not touch `HandleSelectProviderAccountZone` (it still sets the account
  default).
- Do not touch other providers.