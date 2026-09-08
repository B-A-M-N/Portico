# Compiled-binary TUI acceptance

Portico has three distinct test boundaries:

1. Component tests exercise models, screens and controller logic.
2. Integration tests exercise the supervisor, store, IPC server and local
   fixtures.
3. `test/tui_e2e` launches the compiled `portico` binary in a Linux PTY and
   drives terminal bytes against it.

The third boundary is the release-facing TUI check. It does not instantiate a
Bubble Tea model and it does not call IPC methods directly for the behavior
under test. The harness creates isolated XDG/HOME directories, enables the
explicit development-only mock provider, starts the real supervisor through the
real launcher, sends bytes such as `\r`, `ESC`, arrow keys, paging keys and
Ctrl+C, captures the raw terminal stream, and reconstructs the current semantic
screen after ANSI cursor movement and incremental redraws.

Run the deterministic gate with:

```bash
make tui-e2e
```

The target builds `./portico` first and sets `PORTICO_TUI_E2E=1`. Direct
`go test` runs leave this suite opt-in so an ordinary package test cannot leave a
detached supervisor behind. When a test fails, artifacts are written below
`artifacts/tui-e2e/<test>/` (or `PORTICO_TUI_E2E_ARTIFACT_DIR`):

- `terminal.raw` — unmodified PTY bytes;
- `terminal.txt` — the reconstructed semantic screen;
- `commands.log` — physical input bytes and resize events;
- `supervisor.log` — the isolated supervisor log;
- `state.db` — the isolated SQLite state at failure; and
- `failure.txt` — dimensions, cursor and final screen.

## Current deterministic coverage

| Scenario | Evidence | Boundary exercised |
|---|---|---|
| Fresh install | `TestFreshInstallTUIHomeHelpAndNavigation` | Auto-bootstrap, meaningful empty Home, contextual Help, paging/home/end, discovery/providers/setup/settings/history/wizard routes, Escape and quit |
| Connection lifecycle | `TestTUIPlanCancelApplyOpenAndSupervisorRestart` | Real local HTTP fixture, CLI fixture seed, TUI inspect, open-plan cancel, open-plan apply, operation progress, resize, post-operation back navigation, TUI restart and supervisor restart/reconciliation |
| Existing service creation | `TestTUIExistingServiceManualHTTPAndHTTPS` | Wizard discovery, manual-row navigation, pasted address, local-port choice, HTTP and HTTPS menu choices, health choice, provider recommendation, review and closed save |
| Directory and owned-command creation | `TestTUIWizardDirectoryAndCommandFields` | TUI-only directory path and hostname fields, directory mode/SPA choices, owned command executable/args/working directory/shell/environment, review paging and closed save |
| MCP creation | `TestTUIMCPTransportMenuAndCreation` | TUI-only already-running MCP endpoint, HTTP and Streamable HTTP transport menus, evaluated provider selection, review and closed save |
| Local forwarding effect | `TestTUILocalForwardCarriesTrafficAndClosesIt` | TUI-only TCP forward creation, actual HTTP request through the loopback listener, fixture-side request assertion, close cleanup, and UDP refusal |
| Provider setup and settings | `TestTUIProviderSetupSecretAndSettingsNavigation` | Provider/account cursor, disabled-action reasons, provider-declared setup fields, masked secret, support export redaction, guidance-only provider, startup setting and encryption-key rotation across restart |
| Failure and recovery | `TestTUIFailedCommandOperationAndDiagnosisNavigation`, `TestTUIMalformedSupervisorSocketRecovery` | Real failed owned-command operation, error/repair/help navigation, malformed supervisor socket recovery and retry after fixture removal |
| PTY reconstruction | `TestTerminalParserRejectsImpossibleDimensions` | ANSI clear/cursor semantics used by the black-box screen assertions |
| Back-stack regression | `TestBackFromCompletedOperationSkipsClearedPlanPreview` | Completed operation returns to Inspect instead of a stale “Loading the plan…” screen |
| Edit applies and survives restart | `TestTUIEditAppliesAndSurvivesRestart` | Edit→preview→apply→supervisor restart→verify final durable state, plus toggle-open→apply→reconcile |
| Repair applies with independent proof | `TestTUIRepairAppliesAndRestoresTransport` | Physical fault (squatter claims the forward port after supervisor restart), diagnose, preview, apply through the TUI, and a byte-level proof the same port relays the origin's canary again |
| Account mutation boundary | `TestTUIAccountRemovalRefusalThenSuccess` | Dependency-named removal refusal, dependency removal, then confirmed removal; the CLI's independent view agrees |
| Credential and key rotation across restart | `TestTUICredentialReplacementThroughRealSurface`, `TestTUIInstallationKeyRotationKeepsOldSecretsUsable` | Replacement and installation-key rotation both prove the re-encrypted credential stays usable after a supervisor restart |
| Process-level secret proof | `TestTUISecretNeverEntersProcessCommandLines` | `/proc` scan asserts a canary credential never reaches supervisor or connector argv, supervisor.log, or a support export |
| Six-size critical screens | `TestTUICriticalScreensSurviveEveryNarrowSize` | Fresh Home and Providers at 200×60, 120×40, 100×30, 80×24, 70×20 and 60×18: no row overflows the real inner width and critical instructional prose (a sentence ending in `CONTROL_PLANE_API_KEY`) reconstructs unamputated |

The lifecycle scenario deliberately seeds one profile through the CLI only to
establish durable fixture state; the behavior being accepted afterward is
driven through the TUI. The mock provider has no external side effects, while
the source is an actual local HTTP server.

## Typed fields and choice menus

The wizard uses a text field for values that are supplied by the user:

- connection name, service/address and port values;
- health path;
- command arguments, working directory and command environment;
- permanent hostname and access-rule text;
- local-forward ports and remote host; and
- private-network address plus client-tunnel ID and MCP endpoint.

These fields share the standard editor: cursor movement, Home/End, deletion and
paste. Enter commits the answer and Escape goes back. The deterministic
component contract is `TestEveryTextQuestionUsesTheTextField`; the compiled
binary additionally types through the local-forward wizard and Edit/Copy.

The wizard uses visible choice menus—not hidden or clickable dropdown widgets—
for prepared outcomes, source/kind selection, protocol, health probing,
directory mode, MCP mode/transport, exposure, protection, provider/account,
private-network mode and lifecycle choices. The selected row is marked; Up/Down
and j/k move it; Enter commits it; unavailable choices remain visible with the
reason they cannot be selected. `TestCreatingAPortForward`, the wizard choice
tests and the PTY local-forward flow cover this contract. The client-tunnel
native profile question is intentionally absent because the launched client does
not apply that value; Back follows the two questions that are actually visited.

## Live boundary

Live provider traffic is never part of `make tui-e2e`. Run it explicitly:

```bash
PORTICO_E2E_LIVE=1 \
PORTICO_TUI_E2E_LIVE_PROVIDER=cloudflare \
PORTICO_TUI_E2E_LIVE_SOURCE=127.0.0.1:8080 \
make tui-e2e-live
```

`make tui-e2e-live` fails unless `PORTICO_E2E_LIVE=1` is present, and the test
fails unless a provider and source are supplied. It does not turn missing live
credentials into a passing or silently skipped live result. Live resource
cleanup and provider-specific qualification remain operator-owned until the
provider's dedicated live contract is supplied.

## Remaining acceptance work

The deterministic gate is now a real compiled-binary PTY gate and covers the
creation paths named above plus the currently implemented plan, account,
mutation, rotation and process-level surfaces. It is still not a claim that
every item in the external acceptance mandate is complete. The edit/repair/
removal/rotation final-state matrix, the six-size critical-screen sentence
matrix, and the `/proc` argv/log/export secret proof have all been driven
through the compiled binary. SSE disconnect→reconnect→gap-free-replay→snapshot
evidence lives at the supervisor/SSE integration layer in
`internal/ipc/event_stream_integration_test.go` rather than as a PTY visual,
which is the correct boundary for transport-level resynchronization.

Before stable-beta, extend the same harness with what remains genuinely open:

- live provider qualification with real credentials and provider-side cleanup
  (the beginner-facing TUI path, not just the CLI/provider path that
  `scripts/qualification/cloudflare.sh` qualifies);
- connector/origin kill, supervisor death during an operation, malformed-fixture
  and reconciliation scenarios across every provider;
- all six supported sizes during every form, preview and operation, with every
  advertised action driven at least once on each applicable screen (the current
  six-size matrix covers fresh Home and Providers, not every form/preview);
- repairing and re-verifying the other providers' paths with the same
  independent byte-level proof the local-forward repair now has; and
- the required manual exploratory pass on the release candidate.

Those remain explicit gaps rather than being represented by model-only tests or
by the deterministic mock-provider pass.
