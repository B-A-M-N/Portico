# Provider setup — design contract

Status: accepted, implemented.
Tier 3. Written by the main session; the architect gate could not be dispatched
(`model: default` is unresolvable for every agent under `.claude/agents/`), and
the user accepted main-session ownership of the design in that knowledge.

Every statement below was verified against the repository before implementation.
Where a claim came from reading the consuming code rather than from a comment or
document, the file and symbol are named so the next reader can re-check it.

## The invariant

An account is persisted with `core.AccountAuthenticated` only after its
credential has been checked against the real provider.

This was established earlier in the audit. `HandleConfigureProviderAccount`
carries the reason in a comment: recording an account as authenticated on the
strength of a non-empty string is how Portico came to advertise providers that
could not perform a single operation.

Generalising setup must not weaken it. The failure mode to design against is a
provider being configured through a form that validates nothing, and then
appearing ready.

## What was actually wrong

The declarative flow existed but was half-wired.

- `core.SetupFlow` and `core.ProviderSetup` were complete.
- Cloudflare and the OpenAI tunnel both declared a `SetupFlow()`.
- `supervisorHandler.HandleProviderSetupFlow` converted a flow to a DTO — and
  had no IPC route and no client method. Nothing could reach it.
- `HandleConfigureProviderAccount` checked that a provider implements
  `core.ProviderSetup`, then immediately rejected anything that was not
  Cloudflare.
- The TUI hardcoded Cloudflare's four fields in a five-step state machine and
  gated `enter` on `selected.ID == "cloudflare"`.

So the extension point was designed, declared by two providers, and unreachable.

## The correction

Three provider classes, distinguished by what completing the flow actually does.
This is the load-bearing decision in this contract: the previous design assumed
one class, and that assumption is what made the OpenAI tunnel's flow misleading.

### 1. Account providers Portico can validate — Cloudflare

Portico stores the account, checks the credential against the provider, and
records `AccountAuthenticated`. Unchanged behaviour.

### 2. Account providers Portico cannot validate

Portico stores the account and records `core.AccountPending`, which already
exists in `internal/core/finding.go` — no new status constant, no migration.
The response says the credential was not verified, and the UI must repeat it.

Storing an unverified credential is acceptable; *claiming* it works is not.

### 3. Providers whose credential Portico cannot deliver — the OpenAI tunnel

Verified in `internal/provider/clienttunnel/adapter.go`: `validateClient` and
`clientProcessSpec` both read `CONTROL_PLANE_API_KEY` from the supervisor's own
process environment. Nothing reads the account store. The client receives the
key because the supervisor already has it in its environment.

Therefore a stored credential for this provider would change nothing. Setup
would report success and the connection would still fail with
"CONTROL_PLANE_API_KEY is not set".

Its `tunnel_id` field is also not account state: it belongs to the connection
(`profile.Spec.ClientTunnel.TunnelID`, checked in `connectionBlockers`), and the
adapter reads it from step parameters, not from an account.

So the OpenAI tunnel has no account concept at all. Its flow must present as
instructions — what to export and what it buys — not as a form that pretends to
save. A form here would be the same class of defect as the stale catalog text
this audit already corrected.

### Declaring the class

`core.SetupFlow` gains three fields, so the class is a provider's own
declaration rather than a provider ID the supervisor recognises:

- `Kind` — `SetupAccount` or `SetupGuidance`. Guidance is rendered, never
  submitted.
- `IdentityField` — the field whose value identifies the account. Empty means a
  single implicit account.
- `SecretField` — the field carrying the credential.

Everything else a flow declares is carried in `ProviderAccount.Metadata`, which
is a free-form map. **No schema migration**: the store's
`UpsertProviderAccountCredential` already takes a `core.ProviderAccount` with
`Provider`, `Label`, `CredentialRef`, `Metadata` and `Status`.

### Where validation lives

An optional provider-side capability, `core.SetupValidator`. Only a provider
knows how to check its own credential.

Cloudflare's existing validation stays where it is, behind
`supervisorHandler.accountValidator`. It is injected in tests
(`recovery_test.go` at the account-configuration cases), and moving it would
churn those tests for no behavioural gain.

Dispatch order, and the invariant's enforcement point:

1. Provider implements `core.SetupValidator` → use it.
2. Otherwise the provider is Cloudflare → existing `accountValidator`.
3. Otherwise → **no validator**. Persist `AccountPending`, never
   `AccountAuthenticated`, and say so in the response.

Step 3 is the invariant. A provider that cannot be checked cannot be presented
as working.

## Crossing IPC

`ConfigureProviderAccountRequest` gains `Fields map[string]string`, keyed by
`SetupField.ID`. The existing `AccountID` / `ZoneID` / `Label` / `Credential`
fields stay: `internal/cli/handler.go` constructs one, and the supervisor tests
use them. When `Fields` is empty the named fields are mapped onto the reserved
IDs, so both callers work.

Secret handling is unchanged, and unchanged means imperfect: the supervisor
copies the credential into a `[]byte` and zeroes that copy, while the request's
own string remains subject to Go's garbage collector. Moving the value into a
map does not make this worse, and this contract does not claim to fix it.

`HandleProviderSetupFlow` gets the route it never had: `GET
/v1/providers/{id}/setup-flow`, plus a client method.

## Order of work

1. `core` — flow fields, `SetupKind`, `SetupValidator`.
2. Providers declare their class.
3. IPC — DTO fields, setup-flow route, client method.
4. Supervisor — generic configure path, validator dispatch, guidance refusal.
5. TUI — fetch the flow, render fields generically, render guidance.

## What the adversarial review found

A `portico-auditor` REVIEW pass returned FAIL with four findings. All four were
verified against repository state before being acted on; all four were real.

1. **Cross-provider account hijack.** `provider_accounts` is keyed on `id`
   alone and the upsert rewrites `provider_id`. Because a declared provider's
   identity value is free text, entering an existing Cloudflare account's ID
   took over that row: the account changed owner, the Cloudflare adapter lost
   it at the next start, and its connections would have failed with the
   unexplained "provider account unavailable" this audit already fixed once.
   Generalising setup is what made this reachable. Corrected by refusing an
   identity that belongs to another provider, with a test that reproduces the
   takeover.

2. **The unverified signal was generated and dropped.** The supervisor set
   `VerificationUnavailable` and stored `AccountPending` correctly, and nothing
   read it: the TUI's success branch keyed only on `CapabilityLevel`, so a
   provider Portico could not check reported "Account configured." The invariant
   held in the database and broke on the screen, which is where the user is.
   Corrected, with a test.

3. **The Cloudflare writer ignored the values it was validated against.** The
   required-field gate checked the merged field map; the writer then read the
   older named request fields. A caller following the new generic contract
   passed validation and was refused for a missing account ID. Corrected by
   passing the merged map, with a Fields-only Cloudflare test.

4. **A stale flow reply rewound a live form.** Two loads for the same provider
   were indistinguishable, so a late reply to a cancelled load reset the field
   position while keeping typed values. Corrected with a per-load request
   counter. No secret survived this path.

The reviewer also raised two hazards that were not yet defects, both adopted:
the store defaulted an unstated account status to `AccountAuthenticated`, which
made "I forgot to set this" indistinguishable from "this was checked" — it now
defaults to pending; and the guidance reason was asserted centrally rather than
declared, so it is now a `SetupFlow` field the provider fills in.

The reviewer could not resolve its question about the `l` key binding. Checked
directly instead: the setup screen's binding is inside `m.screen == ScreenSetup`
and the inspect screen's inside `m.screen == ScreenInspect`, so they cannot
collide, and while a setup form is open every key is consumed by the form.

## Tests that pin existing behaviour and must keep passing

In `internal/supervisor/recovery_test.go`: Cloudflare account configuration
including the zone-not-visible and missing-token cases, and both
`HandleProviderSetupFlow` cases (the Cloudflare flow, and a provider that
declares none being refused).

The mock provider declares no `SetupFlow`, and must still be refused — that is
the case proving the capability check is real and not decorative.
