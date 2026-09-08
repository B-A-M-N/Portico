# Portico

[![CI](https://github.com/B-A-M-N/Portico/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/B-A-M-N/Portico/actions/workflows/ci.yml)
[![Go 1.25.13](https://img.shields.io/badge/go-1.25.13-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platforms](https://img.shields.io/badge/platforms-Linux-4c566a)](README.md)
[![License: MIT](https://img.shields.io/badge/license-MIT-4c566a)](LICENSE)

Terminal-native connection manager for local services.

Portico discovers local services, creates and manages provider-backed
connections — Cloudflare, ngrok, Tailscale, local port forwards — keeps them
alive after the TUI closes, and tells you exactly which hop broke when one
fails.

> **Trust boundary — local supervisor.** Portico runs a local supervisor over
> a Unix socket that survives the TUI closing. Connections, credentials and
> state live in a local SQLite database; provider traffic and secrets never
> leave this machine except to the provider you chose.

**Discover local services · One command to expose them · Permanent DNS ·
Fine-grained access · Route-segment diagnostics · Honest provider status**

## Why I made it

I am honestly bad at this kind of setup. The mechanics of exposing a local
service — picking a provider, creating a tunnel, wiring DNS, setting up access
— are a pile of small, fiddly, easy-to-get-wrong steps that are genuinely
overwhelming, and I did not want to spend the hours it takes to memorize all
the minutiae of each provider's console just to do something that should be
routine. So I built a tool that asks one plain question — *what are you trying
to do?* — and handles the painstaking part.

Portico came from that: take a common, valuable operation (share something
running on your machine) and make it one reviewed plan instead of a chore
list. It is my attempt to simplify setting up and managing these things so a
person does not have to become a Cloudflare or ngrok expert to use them.

## What it provides

- Local-service discovery, so you point at what is already running rather
  than inventing an address.
- One prepared-outcome wizard: "share a web app temporarily", "publish it
  permanently at a hostname you choose" — the interface asks the consequence,
  not the provider jargon.
- Provider-backed connections that survive the TUI closing; the local
  supervisor reconciles and reopens what you asked to stay open.
- Permanent hostnames with your own DNS zone, and email-passcode access rules
  attached to them, when your Cloudflare account and a zone are configured.
- Route-segment diagnostics that say *which hop* failed — local service,
  connector, provider edge, DNS, endpoint — and a targeted repair plan
  verified afterwards by the findings it actually resolved.
- A redacted support report for a bug report, from the CLI or the setup
  screen, that carries no credentials.

## What it does—and what it never does

Portico is opinionated about staying honest. It:

- plans every state change as a reviewed plan you approve, then applies it in
  one atomic SQLite transaction with compensation and rollback on failure;
- verifies process identity before acting, so it does not signal a stranger's
  process;
- refuses to advertise a capability the provider cannot currently deliver —
  ngrok's access protection, for instance, is declared unsupported rather
  than implied;
- keeps unknown telemetry unknown instead of fabricating a zero or an
  overconfident conclusion.

It never lies about what closing, deleting, or removing an account will do,
and it never pretends a provider or feature is usable when it is not. The
provider table below is checked against the code by a test: if the README
disagrees, the build fails.

## How it fits

```
                         provider traffic
Local service ────────>  Cloudflare / ngrok / tailnet ...
       │
       │ local discovery + status
       ▼
 Portico TUI ──> Unix socket ──> Local supervisor ──> decisions, state, processes
                                  (SQLite, reconciler, identity-verified)
```

Portico does not run in the path between your user and the provider — it helps
you choose a provider, creates the connection, keeps it alive and diagnoses it
when it breaks.

## A short example

```text
$ portico
What are you trying to do?
▸ Share a web app temporarily
  Publish it permanently at a hostname you choose
  ...

$ portico list
demo-http    cloudflare   quick-tunnel    open   https://xxx.trycloudflare.com

$ portico repair demo-http
DNS failed: the record is missing
preview repair → apply → verified: DNS fixed, connector untouched

$ portico support export --output report.json
redacted report written to report.json
```

The output is illustrative. Missing telemetry stays unknown, and every
diagnosis names the evidence it was built from rather than a guess.

## Install

Download a release binary and run `portico` — see the release notes. Or build
and install from source:

```bash
make install      # install as `portico` for the current user
portico doctor    # validate prerequisites and environment
portico           # launch the TUI
```

The local supervisor runs on demand: any Portico client command bootstraps it
detached, and closing the TUI does not stop it. The one-file installer does
not install a boot-persistence unit; `scripts/portico-supervisor.service` is
available if you want the supervisor to start at login via systemd, but
connections still reconcile from durable state either way.

Keep API tokens in the environment or a secrets manager, never in a config
file, and never in a command argument.

## Current Provider Support

| Provider | Status | Exposure Modes | Protection |
|----------|--------|----------------|------------|
| Cloudflare | ✅ Implemented, enabled | Temporary; Permanent when a zone is configured | Email OTP with explicit allow rules, in the TUI and the API |
| ngrok | ✅ Implemented, disabled by default | Temporary; custom hostname with a reserved domain | Not applied — see below |
| Port forward | ✅ Implemented, enabled | Local only — binds loopback, no public address | Not applicable; reachable only from this machine |
| Tailscale | ✅ Implemented (beta) — uses the machine's own tailnet membership | Private only — published through Serve to your tailnet | Not applied; reachability is governed by your tailnet's ACLs |
| zrok | ❌ Not implemented — catalog entry, no adapter | — | — |
| OpenAI Secure MCP Tunnel | ⚠️ Experimental — not usable | Private only (no public address) | Mediated by OpenAI; Portico applies none |

> Cloudflare and local port forwards are usable out of the box. ngrok and the
> OpenAI tunnel are compiled in and disabled by default. Tailscale is compiled
> in and works with the machine's existing tailnet membership. zrok appears in
> the provider list so the interface can say Portico does not implement it — a
> catalog entry is not an implementation.
>
> Only local port forwards are supported. A remote forward is refused with the
> reason rather than accepted and left inert.

**Ngrok is disabled by default and must be enabled explicitly** with
`PORTICO_ENABLE_EXPERIMENTAL_NGROK=1`. The adapter drives the real ngrok agent
and is verified against it end to end: it creates a real tunnel with the
agent-assigned identifier, forwards to the connection's own origin, correlates
by a per-connection tunnel name, reads the assigned URL from the agent's local
API, removes the tunnel on close, rebuilds observed state after a supervisor
restart, and reports the agent's traffic counters.

**Portico applies no access protection to ngrok connections.** ngrok applies
protection through a traffic policy that Portico does not generate yet, so an
ngrok connection is reachable by anyone with its URL. The capability is
declared unsupported rather than advertised.

The agent authenticates using ngrok's own mechanisms: `NGROK_AUTHTOKEN` or the
agent's `ngrok config add-authtoken` configuration. Portico has no ngrok setup
flow and does not store ngrok credentials; it runs the agent with whatever
authentication the machine already has.

**Cloudflare DNS zones are a per-connection decision.** A permanent hostname
is bound to the DNS zone *you choose for that connection* — from every zone
the account's credential can see — so changing the account's default zone or
rotating its token never silently moves an existing connection. An account
without a zone still gets managed tunnels with temporary addresses.

**OpenAI Secure MCP Tunnel is experimental and disabled by default.** It
connects a local MCP server to ChatGPT over an outbound-only tunnel, with no
public address and no inbound port. Portico can start and observe the
`tunnel-client` process, but it does **not** create tunnels, enumerate MCP
tools, or verify that the app has been registered in ChatGPT — those happen on
OpenAI's platform and are reported as outstanding user actions rather than
inferred.

Enable it with `PORTICO_ENABLE_CLIENT_TUNNEL=1` (the legacy
`PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL` name is still accepted) after
installing `tunnel-client`, creating a tunnel in the OpenAI platform, and
running `portico provider login client_tunnel` to store the control-plane key
(`CONTROL_PLANE_API_KEY` remains an environment fallback). It has not been
exercised against a live tunnel.

## Requirements

- Go 1.25.13+ (earlier 1.25 patch releases have known standard-library vulnerabilities)
- Linux (for Unix sockets, process identity via /proc)
- `cloudflared` in `PATH` (for Cloudflare provider)
- `ngrok` in `PATH` (only for the experimental, disabled-by-default Ngrok provider)
- For managed Cloudflare tunnels: an account ID and an API token. The token is
  read from `PORTICO_CLOUDFLARE_API_TOKEN` or `CLOUDFLARE_API_TOKEN` — never
  from a command argument, which would be recorded in shell history and visible
  in the process list.

  ```bash
  export CLOUDFLARE_API_TOKEN=...
  portico provider login cloudflare        # discovers your accounts and zones
  ```

  For scripts, pass field values generically (`--set key=value`) and the
  credential through stdin:

  ```bash
  printf '%s\n' "$CLOUDFLARE_API_TOKEN" |
    portico provider login cloudflare --credential-stdin --set account_id=<id>
  ```

- A **zone ID is optional**. Without one you get managed tunnels with temporary
  addresses; with one you also get permanent hostnames, DNS records and Access
  protection. `provider login` reports which you ended up with.

### XDG Directory Fallback

Portico follows the [XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/latest/). When environment variables are not set, it falls back to these defaults:

| Variable | Fallback | Purpose |
|----------|----------|---------|
| `XDG_RUNTIME_DIR` | `/tmp/portico-$UID/` | Unix socket (supervisor IPC) |
| `XDG_DATA_HOME` | `$HOME/.local/share` | SQLite database, persistent state |
| `XDG_CONFIG_HOME` | `$HOME/.config` | Configuration file (`config.toml`) |
| `XDG_STATE_HOME` | `$HOME/.local/state` | Logs, connector output |

> **Note:** If `XDG_RUNTIME_DIR` is unset (common on non-systemd systems or some WSL configurations), the Unix socket will be created in `/tmp/portico-$UID/`. This directory is cleared on reboot, so clients must reconnect to the supervisor after a machine restart.

## Development

Two audits and an independent review have been worked through in full.
`docs/ACCEPTANCE_MATRIX.md` and `docs/AUDIT_ACCEPTANCE_MATRIX.md` map every
requirement to the test that holds it, the command that runs that test alone,
and the commit that introduced it. `make acceptance` runs every one of those
commands and fails if a row cites a test that does not exist — the matrix is
checked, not trusted.

```bash
# Everything a release must pass: format, build, vet, staticcheck, tests, race
make validate

# Check the acceptance matrix against the tests it cites
make acceptance

# Run tests
go test ./...

# Run tests with race detector
go test -race ./...

# Build
go build -o portico .
```

`internal/docs` holds tests that fail when this README disagrees with the code,
including the provider table above.

## Documenting

| Topic | Documentation |
| --- | --- |
| Architecture | [SPEC.md](SPEC.md) |
| Remediation protocol | [Remediation plan](docs/REMEDIATION_PLAN.md) |
| First audit acceptance | [Production-readiness matrix](docs/AUDIT_ACCEPTANCE_MATRIX.md) |
| Beginner-trust acceptance | [Acceptance matrix](docs/ACCEPTANCE_MATRIX.md) |
| Remaining and verified gaps | [Remaining work](docs/REMAINING_WORK.md) |
| TUI end-to-end coverage | [TUI E2E](docs/TUI_E2E.md) |

## Useful commands

```bash
portico list                               # list all connections
portico open <id>                          # open a connection (plan + apply)
portico close <id>                         # close a connection
portico delete <id>                        # delete a connection
portico plan open <id>                     # preview an open plan
portico plan repair <id>                   # preview a repair plan
portico doctor                             # validate prerequisites and environment
portico discover                           # discover local services
portico support export --output report.json # redacted diagnostic report
```

## Independent community project

> **Unofficial and independent.** Portico is an independent open-source
> project. It is not affiliated with, endorsed by, or sponsored by Cloudflare,
> ngrok, Tailscale, zrok, OpenAI, or FreeInference; the providers and services
> it connects to or acknowledges are named only to be accurate about what it
> uses, and nothing here implies their sponsorship, partnership, or
> endorsement.

I built Portico with the help of freely accessible inference, and I would not
have been able to put this much care into it without a public, affordable
source of capable models. If you found Portico useful at all, please consider
supporting the people who make that inference available: donate to
[FreeInference](https://freeinference.org/). FreeInference provides a genuinely
valuable public resource — capable models anyone can use to learn, build, and
ship things like this — and it deserves support so it stays available. FreeInference
did not commission or fund Portico; I am asking you to support them because
they helped make this kind of project possible, not out of any obligation.

## License

MIT