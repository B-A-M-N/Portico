# Gateway late-binding and lifecycle ownership — design contract

Status: accepted (main session, after architect subagent exhausted its budget;
every statement below was verified against current code before writing).

This covers audit findings P0-2, P0-3, P0-4, P0-5, P0-6 as one package: they
share the gateway lifecycle boundary.

## D1. Symbolic effective-origin reference (P0-2)

Problem: `PlanOpen` consults `effectiveGatewayTarget` at preview time; no
gateway runs then, so the immutable plan carries the raw origin forever, and
starting the gateway at apply cannot help.

Mechanism: execution-time target resolution against an immutable symbolic
reference.

- `internal/core/provider.go`: add `const GatewayTargetRef = "portico://gateway"`
  and field `GatewayRequired bool` on `DesiredConnection`.
- `internal/core/connection.go`: add `func (p *ConnectionProfile) RequiresGateway() bool`
  implementing the single authority rule (openai_compatible service exposure →
  true; openai_mcp client tunnel with ClientOpenAISecureMCPTunnel → true).
  `supervisor.gatewayNeeded` becomes a wrapper over it.
- `PlanOpen`: when `profile.RequiresGateway()` and the resolver reports no live
  gateway, set `desired.GatewayRequired = true` (endpoint stays empty).
- Cloudflare and ngrok `Plan`: if `GatewayRequired` and `GatewayEndpoint == ""`,
  write `origin_url = core.GatewayTargetRef` into every traffic-carrying step.
  If `GatewayEndpoint != ""`, keep today's behaviour (concrete endpoint).
- `executeStep` (`internal/controller/executor.go`): in the provider branch,
  before calling `prov.ExecuteStep`, replace any `Technical.Parameters` value
  equal to `GatewayTargetRef` with the resolver's endpoint; if the resolver
  returns empty, fail the step closed with "gateway required by this profile
  is not running". Raw-origin fallback is refused — it is a security-policy
  downgrade, not degradation.
- Fingerprints stay valid: the symbol is deterministic, so preview fingerprint
  equals apply fingerprint.
- `TestNoGatewayMeansRawOriginPlanning` is replaced by two tests: planning
  without a gateway produces the symbolic ref (never the raw origin); applying
  such a plan without starting the gateway fails closed.

## D2. Origin resolution via ResolvedOrigin (P0-6)

`gatewaySpecFor(p)` currently calls `profileOriginURL(p)` (Source.Existing
only). Change the signature to `gatewaySpecFor(p, upstream string)`; callers
resolve the origin through the controller's canonical path
(`prepareOriginForConnection` / `originManager.Plan`, which handles Existing,
Directory, Command and MCP sources) and pass the URL. `profileOriginURL` is
deleted — one source-to-URL authority remains.

## D3. Durable gateway credential (P0-3)

Provisioning order at apply (in `HandleApplyPlan`) and at restart (reconcile):

1. Resolve upstream per D2.
2. If `spec.AuthRequired`: load the credential from the store
   (`SaveProviderCredential`/`LoadProviderCredential`, providerID `"gateway"`,
   credentialRef `"gateway/" + connID` — connection-scoped, AES-GCM under the
   SecretStore installation key with context-bound AAD; `RotateSecretKey`
   re-encrypts these rows like all others).
3. On first start only, generate 32 random bytes hex-encoded, store encrypted,
   never log/plan/event it. Pass plaintext transiently in
   `GatewayStartSpec.AuthTokens`; persist only the opaque ref.
4. Restart loads the same credential — silent regeneration is a defect.

Reveal path: new IPC action `POST /v1/connections/{id}/gateway/credential`
returns the plaintext once, for copy-configuration. It never appears in plans,
events, logs, exports, argv or ordinary DTOs.

## D4. Runtime projection (P0-4)

`gatewayHandle` gains `authEnabled bool` and `credentialRef string`.
`Runtime()` projects `AuthEnabled` and `CredentialRef` alongside
Endpoint/Upstream/StartedAt. Tests assert auth_enabled=true over IPC while the
plaintext appears nowhere in runtime JSON/logs/events.

## D5. Single lifecycle owner (P0-5)

Supervisor owns every gateway start/stop. `RuntimeServices.Gateways` is
removed; `clienttunnel.Provider` stops constructing gateways and receives
the already-resolved effective MCP endpoint (its spec/planning path reads the
same resolver-projected endpoint). Cleanup/compensation rides the same
controller operation semantics as other steps. `clienttunnel` keeps no gateway
knowledge.

## Implementation order

1. Core types (D1 consts/fields, RequiresGateway) + unit tests.
2. Provider symbol emission (cloudflare, ngrok) + tests that plans carry the
   symbol and never the raw origin for gateway-required profiles.
3. Executor resolution + fail-closed test.
4. HandleApplyPlan ordering (already correct) + vertical: preview→apply with
   real capture of the connector start target == started gateway.
5. D2 origin resolution + command-source test.
6. D3 credential flow + restart persistence test + reveal endpoint test.
7. D4 projection + secret-absence assertions.
8. D5 ownership migration + RuntimeServices change + architecture import test.

## Vertical tests demanded

- `TestPreviewedPlanAppliesAgainstTheNewlyStartedGateway` — create
  openai_compatible connection, preview open with NO gateway running, apply
  exactly that plan, capture the connector start target: it is the freshly
  started gateway endpoint; the raw origin never reaches the transport.
- `TestApplyWithoutRequiredGatewayFailsClosed`.
- `TestGatewayCredentialSurvivesRestart` — stop/start supervisor-equivalent
  path, same decrypted token, usable against the gateway.
- `TestCredentialNeverCrossesUnsafeBoundaries` — scan plan JSON, events,
  logs, runtime DTO for the plaintext.
