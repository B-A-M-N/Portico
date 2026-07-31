# Provider activation — design contract

Status: accepted, implementation staged.
Tier 3.

Reconciles a `portico-architect` contract with the user's corrective direction.
Where the two differ, the user's ownership model wins and the divergence is
recorded below with the reason. Every repository claim was verified by the main
session before implementation; the verification results are stated inline.

## The invariant

> Given the same provider definitions, durable account rows, credentials,
> process dependencies and environment, a clean supervisor startup and an
> in-process account add, update or removal must install the same provider
> runtime, account projection, capabilities and availability state.

Corollaries:

- A provider declaring setup must not require a provider-ID branch in
  `factory.go` or `supervisor.go` to become usable.
- Only accounts whose credential resolves may become usable runtime accounts.
- Pending, revoked, expired and credential-unavailable accounts stay visible
  and are never selectable.
- Runtime construction happens before durable mutation. A failed construction
  leaves both the registry and durable state unchanged.
- Runtime installation and account projection replace under one registry lock.
- Startup and live mutation call the same activation function.

## The defect

Activation is fused into five bespoke functions — `registerProviderCatalog`,
`registerOpenAITunnel`, `registerCloudflareWithAccounts`,
`registerNgrokWithAccounts`, and an inline `portforward.New()` — each
implementing its own partial subset of: gate evaluation, binary lookup, account
loading, credential decryption, adapter construction, registry installation and
catalog fallback. Adding a provider means writing a sixth. Changing an account
means having written a provider-specific rebuild.

**The drift is not hypothetical.** `buildCloudflareChildren` claims in its own
doc comment to be "the single construction path, shared by startup registration
and rebuild, so the two cannot drift". `registerCloudflareWithAccounts` does not
call it — it duplicates the loop inline. They have already diverged: with zero
usable accounts, startup falls back to a Quick Tunnel adapter while
`RebuildCloudflareProvider` removes the provider entirely. The same durable
state yields different capabilities depending on how it was reached. That is the
invariant above, violated, with a comment asserting it holds.

## Verified before designing

- `metadata_json` is a `BLOB` holding marshalled JSON. A new provider needs no
  schema change. **No migration 20.**
- `controller.SetAccounts` replaces its whole list and has one production
  caller. `AuthenticatedAccounts()` has **no production consumers at all** —
  the field is written and never read. The architect flagged a possible "union
  bug"; there is none, because the state is dead. The coordinator therefore
  does not call it, and no union is computed. Recorded so a future reader does
  not reintroduce it.
- `configureDeclaredAccount` is currently unreachable for every shipped
  provider: Cloudflare is routed to `configureCloudflareAccount`, the OpenAI
  tunnel is refused as guidance, and no other provider declares a flow. The gap
  is real but is a third-provider gap, not a live break.
- No shipped provider implements `core.SetupValidator`. Every account created
  through the generic path is therefore pending, and pending is not usable — so
  generic activation *alone* would still never produce a usable account. The
  promoter path is part of this package.
- `ApplyPlan` resolves the adapter once before execution and passes the
  instance into `execPlan`, so a registry replacement cannot swap the adapter
  under a running operation. No new locking on the operation path.

## Ownership: definitions, not supervisor-side factories

The architect recommended activation implementations live in
`internal/supervisor`, to keep adapters untouched. **Overridden.** That keeps
account-identity semantics — which field is the account ID, what becomes
metadata, how the credential reference is derived — in the supervisor, where
they are guesses about a provider. The current `configureDeclaredAccount`
assumes every non-reserved field becomes metadata; that is adequate scaffolding
and not a production contract.

A provider owns what only it can know:

```go
type Definition interface {
    Identity() core.ProviderIdentity
    CatalogEntry() CatalogEntry
    Activate(context.Context, ActivationRequest) (Installation, error)
}

type SetupDefinition interface {
    Definition
    SetupFlow() core.SetupFlow
    PrepareAccount(values map[string]string) (PreparedAccount, error)
}

type SetupVerifier interface {
    VerifyAccount(context.Context, PreparedAccount) (core.SetupValidation, error)
}
```

Provider owns: account identity extraction, label defaulting, metadata mapping,
credential-reference derivation, normalisation, construction, verification.

Supervisor owns: persistence, status assignment, secret zeroing, registry
installation, error mapping, response semantics.

The cost is real and is not hidden: this touches every adapter package, which
the architect listed as a non-goal. It is accepted because the alternative
leaves provider semantics in the supervisor permanently.

**Import direction:** `provider/<name>` imports `internal/provider` and
`internal/core` only. `internal/provider` must not import any adapter package;
`internal/provider/builtin` is the one place that imports both. No provider
package may import `internal/store` — the existing architecture test is
extended to enforce it.

## Activation is a pure read

`ActivationRequest` carries already-resolved material:

```go
type AccountMaterial struct {
    Account core.ProviderAccount
    Secret  []byte   // decrypted; the coordinator zeroes it after Activate returns
}

type ActivationRequest struct {
    Accounts []AccountMaterial   // usable accounts only
    Services RuntimeServices
}
```

`RuntimeServices` exposes interfaces and plain strings — `core.ConnectorProcessService`,
connector and log directory paths, an executable lookup function, an environment
lookup function. It carries **no store handle**, which is what makes "activation
never writes" structural rather than documentary.

`app.Paths` is deliberately not used: `internal/app` imports `internal/tui` and
bubbletea, and putting it in a provider-facing type would drag the TUI into the
provider import graph.

## Installation is one registry operation

`Replace` + `SetAccountInfo` + `AddCatalogEntry` take separate locks, so a
concurrent snapshot can observe a new adapter with an old account list. One
operation replaces all three under one lock:

```go
type Installation struct {
    Provider core.Provider   // nil means catalog-only
    Catalog  CatalogEntry
    Accounts []AccountInfo   // usable and pending both
}

func (r *registry) Install(inst Installation)
```

Pending accounts are passed through. Today the register paths build
`AccountInfo` only from successfully constructed children, so `PendingAccounts`
is always empty after a restart — defeating the split added in the previous
package.

`AccountInfo` gains an effective-usability reason distinct from durable status:
an authenticated account whose credential will not decrypt is not usable, but
that is not the same fact as never having been verified, and the user needs to
be told which.

## Coordinator sequence

Per definition, fixed and written once:

1. Install the catalog entry first, so the provider is visible before anything
   can fail.
2. Evaluate the gate (environment opt-in).
3. Resolve the required binary.
4. Load usable accounts and decrypt their credentials.
5. `Activate` under panic recovery.
6. Install, or update the catalog entry with a reason.

Failure semantics:

- A provider never vanishes. Every failure updates the entry's availability and
  reason.
- Build fully, then install. If `Activate` fails, the previously installed
  adapter is **retained** — dropping a working adapter because a re-activation
  failed would break `Observe` for live connections. The one case that
  legitimately installs nothing is an explicit catalog-only result.
- A panic in construction is recovered and becomes `Degraded`. Today it kills
  `RunSupervisor` at startup or an IPC handler goroutine on configure.
- Errors reach a catalog reason and an IPC response, so they carry account IDs
  and labels but never credential material.
- **No background retry.** Activation runs at startup, immediately after a
  configure or remove, and on an explicit user retry. The failure modes are
  "binary missing" and "credential rejected"; neither resolves on a timer, and
  a loop would periodically re-decrypt secrets for nothing.

`activationMu` serialises activation. It is not the operation path and does not
extend `operationMu`.

## Restart elimination

`RestartRequired` becomes false for every account change and stays on the wire
as the signal for the cases where activation could not be attempted at all:
environment-gate changes and anything `config.Init()` reads once at startup.
The message must name the variable.

`RebuildCloudflareProvider` is deleted, not wrapped. `ActivateProvider(ctx, id)`
serves every provider.

Superseded adapters are **not torn down**. Activation replaces a registry
reference and nothing else; connector lifetime belongs to
`core.ConnectorProcessService`. This is what keeps a live connection alive
across a reactivation.

## The Quick Tunnel asymmetry, resolved deliberately

With zero usable accounts, Cloudflare activation constructs the Quick Tunnel
adapter. Startup behaviour wins; `RebuildCloudflareProvider` removing the
provider was the bug. Removing a working capability because an account was
deleted is a regression, and the same durable state must produce the same
capabilities however it was reached. Pinned by a named test asserting Quick
Tunnel survives removing the last account, immediately and after restart.

## Account status

> Activation consumes only accounts for which `AccountInfo.Usable()` is true.
> Activation performs no write to the account store, ever. A pending account is
> never promoted by being activated, skipped, or by any adapter behaviour after
> activation.

The three open-coded `account.Status != core.AccountAuthenticated` comparisons
are replaced by the single `Usable()` predicate.

`core.SetupValidator` (via `SetupVerifier`) remains the only promoter. A
provider that stores accounts but cannot verify still stores pending and still
reports it — and the response must say the account **cannot be selected until
verified**, not that it will be tested when a connection opens. The latter is
what the current message says and it is false: `Usable()` excludes pending, so
the controller refuses before the provider is ever reached.

## The environment import gap, closed

The bootstrap currently records an unverified environment token as
authenticated. Validating every account on every boot would make offline startup
depend on provider APIs, which is unacceptable. Instead:

- Existing authenticated rows are trusted as previously verified.
- A **new** environment-bootstrap account is verified once, through the
  definition's `VerifyAccount`, before an authenticated row is written.
- If that verification cannot complete because the provider is unreachable, no
  authenticated row is written; the provider stays unconfigured and the import
  is retried on a later start.

## Staging

Each stage is independently green and committable.

- **A.** `provider.Definition` and friends, `registry.Install`, the
  `AccountInfo` usability split. Types and registry only; nothing wired.
- **B.** Definitions for every built-in provider, the `builtin` catalog, the
  supervisor coordinator, `RunSupervisor` using it. Deletes the five bespoke
  registration functions and the duplicated Cloudflare loop.
- **C.** Live configure and remove through the same path;
  `RebuildCloudflareProvider` deleted; `RestartRequired` false.
- **D.** Verified environment import.

## Non-goals

- No plugin or out-of-process boundary; no dynamic discovery; no `init()`
  self-registration. One explicit built-in manifest is a composition root, not
  a failure of genericity.
- No change to `core.Provider`, `core.ProviderAccountID`, or any IPC wire field.
- No schema migration.
- No activation-time promotion of pending accounts.
- No background retry loop.
- No ngrok lifecycle rebuild; it stays experimental and gated.
- No attempt to make the OpenAI tunnel account-backed.
- `controller.SetAccounts` is not maintained by the coordinator; it is dead
  state and is left alone rather than extended.
