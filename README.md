# Portico

**Terminal-native connection manager for local services.**

Portico discovers local services, creates and manages provider-backed connections (Cloudflare), keeps them alive after the TUI closes, and provides diagnostics for connection failures.

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
        ┌──────┴──────┐
        ▼             ▼
   Mock Adapter  Cloudflare Adapter
                 ▼
           cloudflared
```

## Current Features (Implemented)

### Connection Management
- **Create connections** for an existing local HTTP/HTTPS service, a directory
  served by Portico, an owned HTTP command, or an HTTP/streamable/SSE MCP
  endpoint. Portico starts and stops owned directory/command origins as part
  of the same reviewed plan as the provider connector.
- **Cloudflare provider** with Quick Tunnel support by default and named
  tunnels/DNS when a Cloudflare account, zone, and API token are configured.
- **Temporary (Quick Tunnel)** and, when configured, **Permanent (named
  tunnel + DNS)** exposure modes.
- The beginner TUI offers only configurations it can complete. Private
  exposure, service tokens, and identity-provider policies are deferred.
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
- **Repair workflow** — diagnostics followed by a targeted repair-plan preview
- **SSE event stream** — real-time operation progress with bounded replay and
  snapshot resynchronization after reconnects

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
portico doctor                  # system health check
portico discover                # discover local services
portico logs                    # show supervisor logs

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
- Provider status, local-service discovery, directory/command/MCP creation,
  repair, and delete-plan previews

## Current Provider Support

| Provider | Status | Exposure Modes | Protection |
|----------|--------|----------------|------------|
| Cloudflare | ✅ | Temporary; Permanent when configured | None in the TUI; email OTP with explicit allow rules in the API |
| ngrok | 🔄 Planned | | |
| Tailscale | 🔄 Planned | | |
| zrok | 🔄 Planned | | |

## Development

```bash
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

- Go 1.25.12+ (earlier 1.25 patch releases have known standard-library vulnerabilities)
- Linux (for Unix sockets, process identity via /proc)
- `cloudflared` in `PATH`
- For permanent Cloudflare connections: `CLOUDFLARE_API_TOKEN`, an account ID,
  and a zone ID. Run `portico provider login cloudflare --account-id … --zone-id …`
  to encrypt and persist this setup locally, then restart the supervisor.

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

## License

MIT
