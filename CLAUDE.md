# Portico — Connection Manager

**Connection manager for local services.** Discovers local services, creates
provider-backed connections (Cloudflare, ngrok, Tailscale, zrok), keeps them
alive after TUI closes, diagnoses failures.

## First commands

```text
go build -o portico .       # Build binary
go test ./... -v            # Run all tests
./portico                   # Launch TUI
./portico supervisor run    # Start supervisor daemon
```

## Structure

```
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
  origin/            # Local service backends
  discovery/         # ss-based enumeration + HTTP probing
  diagnostics/       # Route-segment diagnostic engine
  credentials/       # Token resolution (env, keyring, memory)
  session/           # Legacy Flare sessions
  tunnel/            # cloudflared subprocess wrapper
  app/               # XDG path resolution + bootstrap
  testutil/          # Test helpers
cmd/                 # Legacy flare-cli Cobra commands (hidden under "portico legacy")
```

## Architecture

**Read `SPEC.md` first** — it is the authoritative architecture contract.
All non-obvious conventions are documented in `AGENTS.md`.

## Portico audit remediation

### Principles

Quality high, cost low, thrash minimal. The workflow scales with task
complexity: cheap models do most of the work; expensive models do the
least but most important work. Don't run a 30-minute pipeline on a
5-minute fix.

The main session owns the whole work package. It does not `/model` switch
between phases. Advisors are dispatched as subagents when their specific
strength is needed, not as mandatory phase gates.

### Strengths routing

For this remediation, only these named project subagents may be invoked
for design and adversarial-review work: `portico-architect` and
`portico-auditor`. Built-in, user-level, and plugin agents may be
installed but must not be used for this remediation. Confirm
`CLAUDE_CODE_SUBAGENT_MODEL` is unset before dispatching.

- **Main session (qwen3.6-35b default)** — mechanical-to-moderate Go
  edits, test writing, running gates, debugging from concrete errors.
  Sole writer of source. Sole committer.
- **portico-architect (GLM-5.2 subagent)** — dependency ordering, schema
  design, lifecycle invariants, naming the smallest coherent correction.
  Dispatched for invariant-level design or after two failed implementation
  attempts. Returns a contract doc. Never edits source.
- **portico-auditor (kimi-k2.7-code subagent)** — falsifying completed
  work, tracing cross-package call paths, surfacing failure modes the
  implementer is too close to see. Modes: RECON (path unclear), TRIAGE
  (failure with multiple plausible causes), REVIEW (sign-off on a
  complex completed package). Never edits source.

### Counterbalance

The separation is load-bearing, not ceremonial:

- The architect prevents the main session from applying isolated patches
  that violate wider invariants. Architect contracts are the
  authoritative constraint; the main session implements within them.
- The main session prevents the architect from detaching from repository
  reality. Every architect recommendation must be verified against
  current code before editing.
- The auditor attempts to falsify the main session's completed
  implementation using concrete failure paths.

No model may approve its own architectural assumptions without repository
or test evidence.

### Triage (30-second decision, written down)

At the start of every work item, classify:

- **Tier 1 — mechanical.** One file, one function, clear fix. No
  cross-package callers change. Examples: missing check, null path,
  regression test, log format, dead code. Target: minutes.

- **Tier 2 — package-level.** Multi-file within a cluster, or a state
  transition used by callers in another internal package. Examples:
  origin+process+supervisor lifecycle, controller+store plan
  enforcement, IPC+provider security. Target: an hour or two.

- **Tier 3 — architectural.** Top-level invariant, schema/migration,
  lifecycle boundary, cross-package invariant. Examples: origin
  ownership model, persistence schema, compensation rewrite. Target:
  a day.

Reclassify mid-task if scope changes; announce it.

### Hard policy gates

These force a dispatch. They are not advisory.

1. **Architect-first for Tier 3.** Before any source edit on a Tier 3
   task, dispatch `portico-architect` for a design contract. Write the
   accepted result to `docs/<PACKAGE>_DESIGN.md`. The main session
   implements the contract; it does not invent architecture.

2. **RECON at the third package.** If implementation is about to touch a
   third internal package, pause and dispatch `portico-auditor` RECON
   (or `portico-architect` if the cross-package concern is an invariant,
   not just a path).

3. **TRIAGE after two fails.** If a test fails twice on the same defect
   with no supported root cause, dispatch `portico-auditor` TRIAGE
   before another fix attempt.

4. **REVIEW for complex Tier 2/3 commits.** When a Tier 2 or Tier 3
   change is complex or complicated (lifecycle, schema, invariant,
   security boundary), it cannot be committed without a passing
   `portico-auditor` REVIEW. Mechanical Tier 2/3 work (e.g. a clear
   multi-file rename, an obvious propagation) skips REVIEW; note
   "mechanical, no REVIEW" in the commit.

### Pipeline by tier

**Tier 1 — mechanical**

1. State invariant and finding.
2. Add regression test(s).
3. Implement.
4. Run targeted tests.
5. Commit.

No subagents. No RECON. No REVIEW.

**Tier 2 — package-level**

1. Read relevant audit and plan sections.
2. State package invariant.
3. Inspect current implementation.
4. Optional RECON if cross-package path unclear.
5. Add focused regression tests.
6. Implement.
7. Run targeted package tests during implementation.
8. `go test ./...` at package boundary.
9. If complex: `portico-auditor` REVIEW once. Correct evidence-backed
   findings. Rerun affected tests.
10. Commit.

**Tier 3 — architectural**

1. Read audit, spec, and current code.
2. State package invariant.
3. Optional RECON if ownership or mutation sites unclear.
4. Dispatch `portico-architect` for design contract. Write accepted
   result to `docs/<PACKAGE>_DESIGN.md`.
5. Add focused regression tests.
6. Implement the contract; verify each assumption against the repo
   before editing.
7. Run targeted package tests during implementation.
8. Full gate set at package boundary:
   `go test ./... && go test -race ./... && go vet ./... && staticcheck ./...`
9. `portico-auditor` REVIEW once. Correct evidence-backed findings.
10. Rerun tests and gates.
11. Commit.

### Escalation

Per work item, when failing:

- **Attempts 1-2**: fix at current tier.
- **Attempts 3-4**: dispatch `portico-auditor` TRIAGE; main session
  implements the fix.
- **Attempts 5-6**: dispatch `portico-architect` for a corrected
  contract; main session re-implements.
- **Architect fails twice**: stop, report to user.

New work item starts back at its declared tier.

### De-escalation

- Tier 3 design contract comes back trivial (one-file fix): execute at
  Tier 1 pace, skip full gates and REVIEW, note in commit.
- Tier 2 task shrinks to mechanical: drop REVIEW, commit at Tier 1
  pace, note in commit.

### Work packages

In order:

1. Origin lifecycle and ownership (findings 1, 3, 4, 16, 17)
2. Mutation and durable-state safety (findings 2, 5, 6, 12)
3. Plan correctness and process identity (findings 7, 8, 9)
4. Security boundaries (findings 10, 11, 13, 15)
5. Reconciliation, provider, legacy, release confidence (14, 18, 19 + docs)

One package at a time. Commit before starting the next. Combine findings
that share an interface or lifecycle correction; don't implement them
individually.

The overall remediation architecture lives in `docs/REMEDIATION_PLAN.md`
(produced by the initial `portico-architect` invocation). Per-package
design contracts produced under Tier 3 live in
`docs/<PACKAGE>_DESIGN.md` (e.g. `docs/CAS_DESIGN.md`). Both are
implementation contracts — verify each statement against current code
before treating it as established fact.

### Subagent payloads

Focused: invariant, applicable findings, named files or symbols,
bounded diff or investigation question, tests already run. Never the
full audit, never the repo transcript. Never invoke both agents on the
same question.

`portico-auditor` findings are accepted only with concrete failure
evidence supported by repository state.

`portico-architect` recommendations are implementation contracts. The
main session verifies each assumption against the repo before editing.

### Thrash controls

- Triage is a 30-second decision, not a meeting.
- Stop after one review/fix cycle unless a release blocker remains.
- Don't expand into unrelated technical debt.
- Don't rewrite docs until behavior is settled.
- Don't begin the next package before committing the current one.
- Concurrency: at most one subagent runs at a time unless two
  investigations are completely independent and read-only. Normal
  operation uses zero or one subagent. Do not ask multiple agents the
  same question.

### Testing

During implementation: specific changed package tests + exact
regression tests. No repo-wide gates after every edit.

At every work-package boundary: `go test ./...`.

Full gate set (`go test ./... && go test -race ./... && go vet ./... &&
staticcheck ./...`) after packages 2 (before midpoint replan), 4, and 5.

A package may not be committed with failing targeted tests or a failing
`go test ./...`.

`portico-auditor` (Kimi) analyzes condensed failures only when the main
session cannot identify the root cause after two focused attempts.

### Midpoint replan

After package 2, dispatch `portico-architect` once. Limited to:
comparing completed implementation against the original plan, detecting
changed assumptions affecting packages 3-5, updating remaining
dependency order and contracts. Skip if no assumption changes surfaced.

### Acceptance

Before declaring remediation complete, produce a matrix mapping every
audit acceptance requirement to: test name, location, behavior,
command, passing result, relevant commit.

Documentation claims corrected only after the matrix is complete.
