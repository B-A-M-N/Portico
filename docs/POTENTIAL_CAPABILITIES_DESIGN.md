# PotentialCapabilities design contract (P1-4)

Produced by `portico-architect`; accepted and verified against the working
tree before implementation. Statements below were each checked against
current code.

## Invariant

A provider's *potential* capabilities — what it could deliver after its
declared setup flow completes — are a static property of the provider's
definition, independent of any constructed adapter or account. The TUI must
never infer potential from current capability, and setup advice must only
ever name providers whose static potential includes the capability being
advised on.

## Declaration: new optional definition interface

    // internal/provider/definition.go
    type PotentialDefinition interface {
        Definition
        PotentialCapabilities(ctx context.Context) (core.Capabilities, error)
    }

The answer must be static: no constructed adapter, no account, no I/O.

Per-provider implementation (all verified as static literals reading no
receiver fields — the mock's `Definition.SetupFlow()` already uses this
exact pattern):

- cloudflare: `cloudflareIntrinsicCapabilities(ctx)` (readiness.go; full-mode
  `Capabilities()` branch is a static literal, adapter.go ~302).
- ngrok, portforward, tailscale, clienttunnel, mock: `(&Provider{}).Capabilities(ctx)`.
- Unimplemented (zrok): NOT implemented — Portico ships no adapter, so it
  cannot declare what one would do.

## Transport: extend CapabilitySetDTO (omitempty)

    PotentialTemporaryAddresses bool     `json:"potential_temporary_addresses,omitempty"`
    PotentialCustomHostnames    bool     `json:"potential_custom_hostnames,omitempty"`
    PotentialPrivateExposure    bool     `json:"potential_private_exposure,omitempty"`
    PotentialManagedDNS         bool     `json:"potential_managed_dns,omitempty"`
    PotentialProtectionModes    []string `json:"potential_protection_modes,omitempty"`

Old clients unaffected (fields absent when zero).

## Supervisor mapping

`potentialCapabilitiesFor(id)` in provider_activation.go mirrors
`setupKindFor`: definition lookup, type-assert `PotentialDefinition`,
errors degrade to `ok == false` (never propagate — potential is advisory).
Mapped in HandleSnapshot after current capabilities; guard
`dto.Capabilities == nil` first.

## Wizard predicates (wizard_choices.go)

- Keep `supportsCustomHostname` / `supportsProtection` as the CURRENT
  predicates.
- Add `couldSupportCustomHostname` and `couldSupportProtection(kind)`
  reading the Potential fields.
- Replace the disjunctive setup-advice predicate
  `!usableProvider(p) || !supportsCustomHostname(p)` with
  `couldSupportCustomHostname` — the load-bearing fix.
- Three states for exposureChoices' permanent branch and
  protectionChoices' OTP branch:
  1. Available now — current capability present.
  2. Available after setup — potential present, current absent; Reason
     "needs a provider account"; Detail names only potential-capable
     providers' setup actions.
  3. Unsupported — neither; Reason "no installed provider offers
     permanent hostnames" (or "sign-in protection"); no setup advice.

## Tests

Supervisor (potential_capabilities_test.go, following setup_kind_test.go):
1. Snapshot carries potential from the definition (accountless cloudflare:
   PotentialCustomHostnames true, PotentialProtectionModes has email_otp;
   portforward false; mock true).
2. Potential survives a failed activation (static, no adapter needed).
3. Unimplemented provider serves no potential.

Wizard (wizard_choices_test.go):
4. Permanent-hostname advice names only potential-capable providers (no
   tunnel-client/ngrok/tailscale advice).
5. Unsupported state shows no setup advice.
6. OTP guidance names only potential-OTP providers.
7. Available-now still works when potential is also present (no advice).

Extend quickTunnelOnlySnapshot with the potential fields.

## Non-goals

No change to core.Capabilities, the registry, IPC endpoints, the
recommendation engine, the store, Unimplemented, or suitabilityFingerprint
(potential changes never alter provider fit — only advice).
