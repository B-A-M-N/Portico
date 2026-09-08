# Portico stable-beta criteria

**Authority:** This is the single authoritative readiness checklist for a
stable-beta build. `SPEC.md` remains the design contract; the two acceptance
matrices are evidence indexes; and [`docs/REMAINING_WORK.md`](docs/REMAINING_WORK.md)
is only the residual backlog. None of those documents is a second release
checklist.

**Review basis:** current working tree, 2026-09-07. A checkbox is checked only
when the cited evidence was run against the same revision being released.
Source inspection, a historical pass, or a test that cannot currently compile
is not a stable-beta pass.

**Gate evidence, revision `e882a905649af04f8bc4ac5f0725f2aede27f231`
(branch `remediation/phase0-connection-model`), recorded 2026-09-07:**
Go `go1.25.13` linux/amd64. `make release-check` passed (format, build,
vet, pinned staticcheck 2025.1.1, `go test ./...`, `make acceptance`
(147 matrix commands, none missing or failing), `make artifact-check`,
`make installer-contract`, `make release-tag-contract`, `make tui-e2e`
(compiled-binary PTY suite), `govulncheck@v1.1.4` (no reachable
vulnerabilities), `go mod tidy -diff` clean). `go test -race ./...`
passed on this revision. This evidence is revision-specific: a new
candidate re-runs it and re-records it here. A release candidate's
record must also carry its tag, its binary SHA256, the exact test
command, a pass/fail, and the qualification timestamp — not a settled
`[x]` — because each of those answers is specific to one revision.

Post-`e882a90` compiled-binary PTY matrix, committed and green on
working-tree HEAD `733c63e` (binary SHA256
`1b9a7bb3c0534f8382bb6db22546cea4e2ccbf3656d3ad44f378a67d44148a75`):
edit→apply→restart→verify (`5ede8ec`), repair→apply with independent byte
proof (`c7ac6f2`), credential and encryption-key rotation→restart
(`007cc68`), the `/proc` canary argv/log/export proof (`a0e1e23`), and the
six-size critical-screen sentence-reconstruction matrix (`733c63e`). These
extend the second audit's release-blocker evidence on the same branch.

**Current decision:** **NOT READY — external qualification evidence is
incomplete.** The current working-tree gate passed after the second-audit
blockers were fixed. Remaining evidence includes the controlled live Cloudflare
qualification with its `cloudflared` version recorded, a tagged candidate run
through the CI release workflow (both-architecture build, signed release
assets, clean-user smoke under root, and prior-version installer upgrade), and
final doc-surface reconciliation. No release claim should be made until those
are evidenced on the exact tagged revision.

## Provider and workload positioning

These are the claims stable-beta documentation may make. “Implemented” means
the code path exists; it does not promote a beta or experimental capability to
stable.

| Surface | Accurate position | Stable-beta boundary |
|---|---|---|
| Cloudflare | Default provider. Account-backed named tunnels support managed DNS and email-OTP Access when a zone is configured; accountless Quick Tunnels provide temporary addresses. | Quick Tunnels are development-oriented, beta, and reject SSE. A permanent hostname requires a Cloudflare account, token, and zone. |
| Local port forward | Default, stable local-only provider. It binds loopback and forwards TCP to a reachable remote target. | It creates no public address. Remote forwarding and UDP are refused, not silently accepted. |
| Tailscale | Beta private-network provider. Availability follows the installed client’s actual signed-in/approved state; Portico does not store a Tailscale credential or sign the machine in. | Join and Serve/private routes only. Funnel and public exposure are not implemented or promised. |
| ngrok | Experimental, explicit opt-in with `PORTICO_ENABLE_EXPERIMENTAL_NGROK=1`. The real agent lifecycle and connection/request counters are implemented. | Portico applies no access protection; a URL is reachable by anyone who has it. Custom hostnames remain experimental. Do not describe ngrok as a stable protected path. |
| OpenAI Secure MCP | An OpenAI MCP workload/profile carried by the `client_tunnel` transport registration; OpenAI is not a public transport provider. | Experimental and disabled by default. `tunnel-client` is outbound-only, creates no public address or inbound port, and has not been exercised against a live tunnel. Portico starts/observes it but does not create the OpenAI tunnel, enumerate tools, or register the ChatGPT app. |
| OpenAI-compatible API | A separate OpenAI-compatible workload profile, probed for protocol compatibility and carried through the normal gateway/provider path. | Do not conflate it with the client-mediated Secure MCP tunnel. |
| zrok | Catalog-only entry. | No adapter; never present it as selectable or implemented. |

The canonical client-tunnel names are `client_tunnel`,
`PORTICO_ENABLE_CLIENT_TUNNEL`, and `PORTICO_CLIENT_TUNNEL_BIN`. The old
OpenAI-named environment variables are migration fallbacks only. Tailscale
has no separate Portico opt-in flag in the current composition; the local
client’s status determines whether it is usable.

## Required readiness checklist

### Release and verification gates

- [x] `make release-check` passes on the current working tree. This is the
  canonical aggregate gate: format, build, vet, staticcheck, ordinary tests,
  the three-repeat race suite, acceptance-matrix verification,
  `govulncheck`, and `go mod tidy -diff`.
- [x] The race gate is rerun after the final code change as
  `go test -race ./... -count=3`. A prior `-count=1` run or a pass from an
  earlier concurrent worktree state is not evidence for this release.
- [x] `make acceptance` passes and its report contains no missing test names,
  no commands that match nothing, and no failed commands.
- [x] The packaged binary passes `make artifact-check`; the archive is
  extracted and exercised under isolated XDG paths, including restart from a
  copied install.
- [x] The deterministic compiled-binary TUI gate passes with
  `make tui-e2e`: real PTY input, semantic screen assertions, plan
  cancellation/application, operation progress, resize, TUI restart and
  supervisor restart/reconciliation are exercised under isolated XDG paths.
- [ ] The full black-box TUI acceptance matrix is complete: every advertised
  action and creation path is physically driven, failure/reconciliation,
  account/secret, SSE reconnect, all-size resize, and destructive workflows
  have deterministic evidence, and the release-candidate manual exploratory
  pass is recorded. Current coverage and explicit gaps are in
  [`docs/TUI_E2E.md`](docs/TUI_E2E.md).
- [ ] The release workflow’s clean-user install/upgrade smoke test passes.
- [ ] The release workflow signs `checksums.txt` with a keyless Sigstore
  bundle bound to this repository’s tag workflow, publishes that bundle beside
  the archives, and the installer verifies the bundle before accepting the
  checksum file. Local synthetic fixtures may bypass this only with an explicit
  unsigned-local test flag.

### Default stable-beta behavior

- [ ] Cloudflare temporary and permanent workflows pass their plan, execute,
  observe, restart, close, cleanup, and targeted-repair checks. The suite must
  keep 404 absence distinct from unauthorized, rate-limited, transient, and
  malformed responses.
- [ ] Local port forwards pass creation, loopback binding, reachability,
  close, restart/observation, and refusal checks for unsupported remote and UDP
  forms.
- [ ] Every mutation remains supervisor/controller-owned and journaled; no
  provider has a second production `Apply`/`Repair`/`Remove` orchestration path
  or direct store ownership.
- [ ] Desired state, runtime state, exact external resource identity, process
  identity, compensation, unresolved cleanup, and restart reconstruction remain
  distinct and durable.
- [ ] Account setup, selection, rotation, re-verification, removal, and
  migration are transactional and provider-scoped. Secrets stay out of plans,
  events, logs, diagnostics, IPC responses, argv, and rendered views.
- [ ] The TUI and CLI expose only capability-valid choices, preserve typed
  input and edits, correlate asynchronous replies to the screen/request that
  made them, and distinguish empty, unavailable, failed, and unknown states.
- [ ] Doctor is read-only by default and reports database/key mismatch,
  credential health, cleanup obligations, event history, provider readiness,
  and socket state with an actionable next step.
- [ ] Connector logs are bounded and redacted. Activity reports measured
  counters only when a provider supplies them; otherwise it states why figures
  are unavailable.

### Experimental and beta boundaries

- [ ] Experimental providers remain explicitly gated and visibly labeled.
  Their existence must not make an unsupported capability selectable.
- [ ] Tailscale remains beta and private-only until a deliberate promotion
  decision is backed by the required client/integration evidence.
- [ ] ngrok remains experimental and unprotected until Portico implements and
  verifies the protection it would advertise. Its agent-backed lifecycle must
  not be described as a security boundary.
- [ ] Client-mediated OpenAI MCP remains experimental until a live-tunnel
  exercise proves the client/control-plane path. Platform actions owned by
  OpenAI remain user actions, not inferred Portico success.

## Evidence index

Use the evidence indexes for test-to-requirement detail, but use this document
for the readiness decision:

- [`docs/ACCEPTANCE_MATRIX.md`](docs/ACCEPTANCE_MATRIX.md) — beginner-trust
  behavior and current-path test mappings.
- [`docs/AUDIT_ACCEPTANCE_MATRIX.md`](docs/AUDIT_ACCEPTANCE_MATRIX.md) —
  production-readiness audit findings and historical evidence.
- [`docs/REMAINING_WORK.md`](docs/REMAINING_WORK.md) — open implementation and
  release work, without a second readiness verdict.

## Known limits that remain honest

These are not silently promoted to “done” by unit evidence:

- The provider contract suites use local fakes. They prove Portico’s request
  and classification behavior, not undocumented changes in Cloudflare.
- Tailscale tests use a controlled client/recorded state; private-network
  support remains beta rather than live-network certified.
- Client-tunnel tests exercise invocation, readiness, health-file isolation,
  and repair logic without a live OpenAI tunnel. The platform-created tunnel
  and ChatGPT registration remain outside Portico.
- ngrok’s live tests are behind the `live` build tag and are not part of the
  default release gate. The adapter reports connection/request counts, but no
  Portico access policy is applied.
- Cloudflare, Tailscale, and client-tunnel providers do not claim traffic
  telemetry. A provider without measured counters must continue to render an
  explicit unavailable reason.
- Legacy Flare configuration compatibility and unused donor-origin
  implementations remain migration/cleanup work; they are not current
  stable-beta capabilities.

Stable-beta readiness is achieved only when every required checkbox above is
checked on one reproducible revision and the provider boundaries remain as
stated here.
