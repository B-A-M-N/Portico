# Client-mediated tunnels — design contract

Status: accepted (architect pass, Tier 3)
Scope: audit item 5 — connecting a local MCP server to ChatGPT.

## Invariant

A connection whose purpose is private client-mediated access must never be
satisfied by publishing the service at a public address. If the private path
cannot be established, the connection fails; it does not fall back.

## Verified external facts

Taken from OpenAI's own documentation, not inferred:

- The customer-run agent is a binary named `tunnel-client`.
- Subcommands include `init`, `run` and `doctor`.
- The control-plane credential is supplied through the environment variable
  `CONTROL_PLANE_API_KEY`.
- `tunnel-client init` accepts `--tunnel-id`, `--profile`, `--sample`,
  `--mcp-command` for stdio MCP servers and `--mcp-server-url` for HTTP MCP
  servers.
- The client exposes `/healthz`, `/readyz`, `/metrics` and a loopback-only
  admin UI at `/ui`.
- The connection is outbound-only HTTPS to OpenAI; no inbound port is opened.
- Tunnels are created and managed in the OpenAI platform's organization
  settings, not by the client alone.

Anything not in that list is not assumed. In particular Portico does not claim
to create tunnels, register ChatGPT apps, enumerate MCP tools, or detect
registration completion, because none of those were verified.

## Decisions

### D1 — A distinct connection kind, not an exposure mode

This is not a service exposure with a different provider. There is no public
address, no DNS record and no hostname. Modelling it as `service_exposure` would
put it through planning code whose whole vocabulary is public addressing, and
whose failure modes end in publishing something.

`ConnectionClientTunnel` is added to the existing tagged union with a
`ClientTunnelSpec`. The versioned `spec_json` schema already stores any arm, so
no migration is required.

### D2 — Capabilities declare private-only, and nothing else

The adapter declares `PrivateExposure` supported and `TemporaryAddresses`,
`CustomHostnames` and `ManagedDNS` unsupported. The recommendation engine
already treats capabilities as hard constraints, so this alone prevents the
adapter being offered for a public exposure and prevents a public provider being
offered for this kind.

### D3 — The ChatGPT-side handoff is explicit, not simulated

Creating a tunnel and registering an app happen on OpenAI's platform. Portico
reports that step as an outstanding user action with the exact place to perform
it, and does not claim to have completed or verified it. Reporting an unverified
step as done is the failure this audit found in the ngrok adapter.

### D4 — The credential never enters argv

`CONTROL_PLANE_API_KEY` is passed through the process environment only,
matching the fix applied to ngrok. Process arguments are world-readable.

### D5 — Experimental until exercised against the real client

Every capability is declared experimental and the provider is catalogued as
requiring explicit opt-in. Portico has not been run against a live tunnel, and
declaring otherwise would repeat exactly the over-claim this audit was written
to correct.

## What is implemented

- The connection kind and spec, validated like the others.
- A provider adapter that plans and executes: verify the local MCP endpoint,
  verify the client is installed, verify the credential is present, start the
  client, verify it reports ready.
- Client detection and readiness probing against `/healthz` and `/readyz`.
- Catalog registration so the provider is visible with setup actions when it
  cannot be used.

## What is deliberately not implemented

Tool enumeration, permission review, app-registration detection, revocation and
read-only versus write policy. Each requires behaviour that could not be
verified from documentation. They are listed here rather than stubbed, because a
successful no-op is worse than an absent feature.

## Required tests

- A client-tunnel profile round-trips through persistence.
- The adapter never plans a public address, and never emits DNS, hostname or
  access steps.
- Planning fails when the profile carries no MCP endpoint or command.
- The credential is passed through the environment and never appears in argv.
- Verification fails when the local MCP endpoint is unreachable, before the
  client is started.
- The recommendation engine will not offer this provider for a public exposure,
  and will not offer a public provider for a client tunnel.
