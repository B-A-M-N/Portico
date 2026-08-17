# Portico

**Terminal-native connection manager for local services.**

Portico discovers local services, creates and manages provider-backed
connections, keeps them alive after the TUI closes, and provides diagnostics for
connection failures.

Cloudflare and local port forwards work out of the box. ngrok and OpenAI's
Secure MCP Tunnel are compiled in but disabled by default. See
[Provider support](#current-provider-support) for what each can actually do.

```
portico
```

## Quick Start

```bash
# Launch the TUI
portico

# Or use CLI commands
portico list
portico open <connection-id>
portico close <connection-id>
portico delete <connection-id>
portico plan open <connection-id>
portico doctor
portico discover
```

## Architecture

Portico follows a strict layered architecture:

```
┌─────────────────────────────────────┐
│ TUI / CLI / JSON Client            │
└──────────────┬──────────────────────┘
               │ HTTP + SSE over Unix socket
               ▼
┌─────────────────────────────────────┐
│ Local Supervisor                   │
│ - Database (SQLite WAL)            │
│ - Process Manager (identity-verified)
│ - Reconciliation Loop              │
└──────────────┬──────────────────────┘
               │
               ▼
┌─────────────────────────────────────┐
│ Provider-Neutral Controller         │
│ - Step Sequencing                   │
│ - Compensation & Rollback           │
│ - State Persistence                 │
└──────────────┬──────────────────────┘
               │
        ┌──────┴───────┬───────────┬──────────────┐
        ▼              ▼           ▼              ▼
   Cloudflare       ngrok      port forward   OpenAI tunnel
   (cloudflared)   (agent)     (in-process)  (tunnel-client)
```

Tailscale and zrok appear in the provider list as catalog entries with no
adapter, so the interface can explain the gap rather than omit them. A mock
provider exists for controller tests.

## Current Features (Implemented)

### Connection Management
- **Create connections** for an existing local HTTP/HTTPS service, a directory
  served by Portico, an owned HTTP command, or an HTTP/streamable/SSE MCP
  endpoint. Portico starts and stops owned directory/command origins as part
  of the same reviewed plan as the provider connector.
- **Cloudflare provider** with Quick Tunnel support by default, named tunnels
  when an account and API token are configured, and DNS plus Access protection
  when a zone is configured as well. The zone is optional: an account without
  one gets managed tunnels with temporary addresses.
- **Local port forwards**, which bind a loopback port and carry traffic to a
  remote endpoint. Creatable through the wizard or the API (`POST /v1/connections`
  with `kind: port_forward`). Only TCP is supported; UDP and remote forwards
  are refused.
- **Temporary (Quick Tunnel)** and, when configured, **Permanent (named
  tunnel + DNS)** exposure modes.
- **Access protection** by email passcode, naming the people or domains
  allowed. Service tokens and identity-provider policies are deferred.
- **Edit and copy** an existing connection. An edit is previewed as a plan and
  carries the revision it was built from, so an edit prepared against a stale
  view is refused rather than overwriting someone else's change. A copy is made
  by the supervisor's deep copy and asks for its own hostname, since two
  connections cannot share one.
- **Lifecycle control**: auto-start, keep-alive on disconnect

### Reliability & Correctness
- **Atomic operations** — all state changes in single SQLite transactions
- **Identity-verified process management** — PID + start time + executable + command hash
- **Compensation & rollback** — reverse-order execution on step failure
- **Desired-state reconciliation** — supervisor restarts desired-open connections on restart
- **Immediate state persistence** — runtime transitions survive supervisor crashes
- **No TUI dependency** — connections survive TUI close; supervisor runs detached

### Diagnostics & Observability
- **Route segment diagnostics** — local service → connector → provider edge → DNS → endpoint
- **Finding classification** — errors, warnings with evidence and repair options
- **Repair workflow** — diagnostics followed by a targeted repair-plan preview,
  verified afterwards by which findings were resolved, which remain and which
  are new, rather than by counting them
- **SSE event stream** — real-time operation progress with bounded replay and
  snapshot resynchronization after reconnects
- **Operation history** with the journal of what each one did, and an explicit
  statement of whether the list is complete or capped
- **Support export** — a redacted report for a bug report, from the CLI
  (`portico support export`) or the setup screen. It carries no credentials,
  authorization headers, cookies, private keys or command environments, and is
  written readable only by you

### CLI Interface
```bash
portico list                    # list all connections
portico inspect <id>            # show connection details
portico open <id>               # open connection (plan + apply)
portico close <id>              # close connection
portico delete <id>             # delete connection
portico plan open <id>          # preview open plan
portico plan close <id>         # preview close plan
portico plan repair <id>        # preview repair plan
portico apply <plan-id>         # apply a previewed plan
portico create                  # create a connection without opening it
portico repair <id>             # diagnose and repair
portico doctor                  # validate prerequisites and environment
portico discover                # discover local services
portico logs                    # show supervisor logs

portico provider list                              # providers and their accounts
portico provider login <provider>                  # configure an account
portico provider remove-account <provider> <id>    # forget a stored account

portico support export --output report.json        # redacted diagnostic report

# Serve a directory through a Portico-owned local origin
portico serve docs --source-type directory --source ./public --yes

# Start an HTTP command, wait for its explicit port, then tunnel it
portico serve app --source-type command --source ./my-server \
  --source-port 8080 --source-arg=--listen=8080 --yes

# Expose an already-running HTTP MCP server
portico serve tools --source-type mcp_server --source http://127.0.0.1:3000/mcp --yes
```

### TUI
- Interactive connection list with navigation (j/k, up/down)
- Inspect view with route visualization
- Plan preview with step-by-step breakdown
- Operation progress with SSE events
- Edit (`e`) and copy (`c`) an existing connection
- Provider and account management, including removing a stored account —
  refused while any connection or outstanding cleanup still needs it, and the
  refusal names them
- Text fields with a cursor, word motion and paste; credentials are masked as
  they are typed
- Scrolling on every screen, with an indicator saying how much is above and
  below (`pgup`/`pgdn`, `home`/`end`)
- Provider status, local-service discovery, directory/command/MCP creation,
  repair, and delete-plan previews

## Current Provider Support

| Provider | Status | Exposure Modes | Protection |
|----------|--------|----------------|------------|
| Cloudflare | ✅ Implemented, enabled | Temporary; Permanent when a zone is configured | Email OTP with explicit allow rules, in the TUI and the API |
| ngrok | ✅ Implemented, disabled by default | Temporary; custom hostname with a reserved domain | Not applied — see below |
| Port forward | ✅ Implemented, enabled | Local only — binds loopback, no public address | Not applicable; reachable only from this machine |
| Tailscale | ❌ Not implemented — catalog entry, no adapter | — | — |
| zrok | ❌ Not implemented — catalog entry, no adapter | — | — |
| OpenAI Secure MCP Tunnel | ⚠️ Experimental — not usable | Private only (no public address) | Mediated by OpenAI; Portico applies none |

> Cloudflare and local port forwards are usable out of the box. ngrok and the
> OpenAI tunnel are compiled in and disabled by default. Tailscale and zrok
> appear in the provider list so the interface can say Portico does not
> implement them — a catalog entry is not an implementation.
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
ngrok connection is reachable by anyone with its URL. The capability is declared
unsupported rather than advertised.

The agent authenticates from `NGROK_AUTHTOKEN`, a Portico-configured ngrok
account, or its own `ngrok config add-authtoken` configuration.

**OpenAI Secure MCP Tunnel is experimental and disabled by default.** It connects
a local MCP server to ChatGPT over an outbound-only tunnel, with no public
address and no inbound port. Portico can start and observe the `tunnel-client`
process, but it does **not** create tunnels, enumerate MCP tools, or verify that
the app has been registered in ChatGPT — those happen on OpenAI's platform and
are reported as outstanding user actions rather than inferred.

Enable it with `PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL=1` after installing
`tunnel-client`, creating a tunnel in the OpenAI platform, and exporting
`CONTROL_PLANE_API_KEY`. It has not been exercised against a live tunnel.

## Development

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

# Install as `portico` for the current user (~/.local/bin must be on PATH)
make install

# Run the TUI
./portico

# Start supervisor in background
portico supervisor run
```

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
  portico provider login cloudflare --account-id <id>
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

> **Note:** If `XDG_RUNTIME_DIR` is unset (common on non-systemd systems or some WSL configurations), the Unix socket will be created in `/tmp/portico-$UID/`. This directory is cleared on reboot, meaning clients must reconnect to the supervisor after restart. The supervisor daemon itself survives because it's managed by systemd user service or similar.

## Testing

```bash
# All tests
go test ./...

# With race detector
go test -race ./...

# Specific package
go test ./internal/controller/... -v

# Static analysis
go vet ./...
staticcheck ./...
```

Two audits and an independent review have been worked through in full.
`docs/ACCEPTANCE_MATRIX.md` and `docs/AUDIT_ACCEPTANCE_MATRIX.md` map every
requirement to the test that holds it, the command that runs that test alone,
and the commit that introduced it. `make acceptance` runs every one of those
commands and fails if a row cites a test that does not exist — the matrix is
checked, not trusted.

`internal/docs` holds tests that fail when this README disagrees with the code,
including the provider table above.

## License

MIT
