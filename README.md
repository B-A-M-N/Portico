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
- **Create connections** with various source types:
  - `local:http` — existing HTTP/TCP service
  - `local:command` — command that starts a service
  - `docker:container` — Docker container
  - `docker:compose-service` — Docker Compose service
  - `builtin:static` — static file server
  - `builtin:file-browser` — file browser with upload/download
- **Cloudflare provider** with full API and Quick Tunnel support
- **Temporary (Quick Tunnel)** and **Permanent (named tunnel + DNS)** exposure modes
- **Protection modes**: none, email OTP, identity provider, service token, private network
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
- **Repair workflow** — targeted repair plans with confirmation
- **SSE event stream** — real-time operation progress with replay support

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
```

### TUI
- Interactive connection list with navigation (j/k, up/down)
- Inspect view with route visualization
- Plan preview with step-by-step breakdown
- Operation progress with SSE events
- Provider status and discovery

## Current Provider Support

| Provider | Status | Exposure Modes | Protection |
|----------|--------|----------------|------------|
| Cloudflare | ✅ Full | Temporary, Permanent | None, Email OTP, Identity Provider, Service Token, Private Network |
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

# Run the TUI
./portico

# Start supervisor in background
portico supervisor run
```

## Requirements

- Go 1.25+
- Linux (for Unix sockets, process identity via /proc)
- `cloudflared` binary in PATH (for Cloudflare provider)
- `cloudflared` tunnel token or API token for full Cloudflare features

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