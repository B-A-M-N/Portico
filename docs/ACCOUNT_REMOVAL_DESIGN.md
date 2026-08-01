# Provider account removal — design contract

Status: accepted (architect pass, Tier 3)
Scope: independent review item 5.3, treated as an extension of work package 2
(mutation and durable-state safety).

## The question

An independent review found that removing a provider account contradicts a
stated invariant: every mutation is represented by an immutable plan the user
previews and approves, then applied as a recorded operation. Removal instead
deleted credential state directly, behind a confirmation screen.

The obvious correction is to make removal a plan. It was declined, and the
reasoning is worth recording because the same argument will come up again for
the next mutation that is not connection-shaped.

## Why account removal is not a plan

`core.OperationPlan` is connection-scoped by construction, not by accident.
`Controller.ApplyPlan` looks up the profile by `plan.ConnectionID` and refuses
without one; `operation_plans.connection_id` is `NOT NULL` with
`UNIQUE(connection_id, fingerprint)`; `computeFingerprint` hashes the connection
ID; and the executor uses it as the addressing key for operation reservation,
step journalling, resource outcomes, runtime error marking and cleanup
recording. Admitting a connectionless plan makes every one of those conditional.

The plan boundary exists for mutations with three properties:

1. They change provider-visible state or the inventory of resources Portico owns
   at a provider.
2. They execute as an ordered sequence that can stop between steps.
3. They therefore need compensation and a re-observed expected outcome.

Account removal has none of them. It changes local durable state only; it is one
SQLite transaction with no intermediate state; nothing at a provider is touched,
so there is nothing to compensate and nothing to re-observe. It is only ever
permitted when nothing depends on the account, so it has no provider-visible
consequence at all.

Two pieces of evidence that this boundary was already drawn rather than invented
to justify the decision:

- **Account creation writes the same durable credential state with no plan.**
  `HandleConfigureProviderAccount` calls `UpsertProviderAccountCredential`
  directly. If removal violates the invariant, creation violates it equally.
- **`EDIT_PLAN_DESIGN.md` already exempts name-only edits** from the plan
  boundary, explicitly because they touch nothing at the provider.

So the review named the right property and the wrong mechanism. The property is
real and was missing. The mechanism is a plan only because a plan is where
Portico keeps that property for connections.

## What removal must have instead

Three properties — what "previewed immutable operation" actually buys:

- **P1. The preview is derived from the same evidence as the decision**, not
  composed independently by each client.
- **P2. The preview binds the apply.** If the subject or its dependencies
  changed since, the apply refuses and shows what is true now.
- **P3. The removal leaves a durable record**, committed in the same transaction
  as the deletion.

## What was actually wrong

- **The preview was client-composed and made a claim nothing checked.** The
  confirmation screen said "Portico will forget the credential it stored for
  this account" from a cached row. An account row always names a credential
  reference; the referenced row may not exist. For such an account the screen
  promised to forget something Portico does not hold.
- **The CLI had no preview at all**, so "the preview describes what applying
  does" was vacuous there — there was nothing to compare against.
- **Nothing bound preview to apply.** A confirmation opened before an account
  was removed and re-added under the same `(provider_id, id)` would, on enter,
  delete the new one.
- **No durable record.** The rows disappeared and nothing recorded that they had
  existed, so a support export could not answer "was this credential removed,
  and when".

## The design

`Store.PreviewProviderAccountRemoval` returns an `AccountRemovalPreview`:
identity, label, status, dependencies, `Removable`, `CredentialStored`, and a
`Fingerprint`. It calls `accountDependenciesTx` — the same unexported helper the
removal decides on. **There is exactly one dependency query in the codebase.**
Two would eventually disagree, and the disagreement would be a preview
describing a removal that does something else.

`fingerprintAccountRemoval` hashes the provider and account IDs, the credential
reference, the account's `updated_at`, and the dependency set canonicalised by
`(Kind, ID)`. Display text is excluded: a relabelled account is the same
removal, and a preview must not expire because a sentence changed.

`Store.DeleteProviderAccountIfUnused` takes the fingerprint as a **required**
parameter. Empty is refused, not treated as "skip the check" — otherwise every
caller that forgets it is silently unchecked. Inside one transaction it
recomputes the preview, reports dependencies first (what blocks it is more
useful than the fact that a preview aged), then compares fingerprints, then
deletes, then writes the audit row. Decide-and-delete remains inseparable.

`provider_account_removals` (migration 21) records provider, account, label,
credential reference, whether a stored credential was removed, the confirmed
fingerprint and the time. No foreign key: the referent is what was deleted.
Refusals are not recorded — a refusal changes nothing, and recording them lets a
retry loop grow the table without bound. The reference is stored, never a
secret.

The supervisor composes the consequence sentences in `describeAccountRemoval`,
including whether this is the provider's last usable account. **No client
composes a factual claim about what removal does.**

## Invariants and what enforces each

| Invariant | Enforced by |
|---|---|
| A removal cannot be applied without a preview | `previewFingerprint` is a required parameter; the client signature change makes every call site prove it |
| The preview and the removal see the same dependencies | One `accountDependenciesTx`, called by both |
| Deciding and deleting cannot be separated | Both inside one `BeginTx` |
| A changed subject is refused, not removed | Fingerprint compared inside the deleting transaction |
| A record cannot claim a removal that did not happen | Audit row inserted in the deleting transaction |
| No client invents a consequence | `Consequences` composed by the supervisor |

## Non-goals

1. No `OperationPlan` change: no new intent, no step kind, no connectionless
   plan, no subject abstraction.
2. No steps, executor or compensation for account removal. If a future change
   needs ordered account operations with partial failure, that is when
   generalising the plan's subject gets reconsidered — with a second subject in
   hand rather than on speculation.
3. No `operations` record. `provider_account_removals` is an audit row, not an
   operation, and does not get a state machine.
4. No change to what blocks removal.
5. No SPEC edit. If §2.1(5) still reads as over-broad after this, that is a
   documentation item once behaviour has settled.

## Found while implementing this

`Controller.ApplyPlan`'s intent dispatch listed open, close, repair and delete.
`core.IntentEdit` was absent, while `PlanEdit` built plans carrying it — so
every edit plan fell to the default branch and returned "unknown plan intent:
edit". **The entire edit feature could be planned, previewed and approved, and
never applied.**

Every test covering editing asserted on the plan; none applied one. That is how
a flow that could not work looked covered, and it is the same shape as the
transport defects found earlier in this remediation: a boundary that tests
approach from both sides and never cross.

Fixed, with `TestAnEditPlanCanActuallyBeApplied`, which drives `PlanEdit`,
`ApplyPlan`, waits for the operation to reach a terminal state, and asserts the
profile actually changed. It fails against the previous code.
