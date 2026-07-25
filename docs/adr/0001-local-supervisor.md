# ADR 0001: Local Supervisor Process Model

## Status

Accepted

## Context

Portico requires a long-lived background process to:
- Maintain connections when TUI closes
- Manage SQLite database
- Own connector processes
- Reconcile desired vs observed state

## Decision

The same binary (`portico`) operates in two modes:
1. **Client mode** (default): Starts supervisor if needed, then launches TUI/CLI
2. **Supervisor mode**: `portico supervisor run`

Startup sequence:
1. Try Unix socket
2. If unavailable, acquire startup lock
3. Re-exec as `portico supervisor run`
4. Detach with new session
5. Redirect stdout/stderr to logs
6. Wait for ready handshake
7. Start TUI client

## Consequences

- TUI exit does not terminate connections
- Supervisor restart reconstructs runtime state
- Process identity validation prevents killing unrelated PIDs

## Paths

```
Socket:   $XDG_RUNTIME_DIR/portico/portico.sock
          Fallback: /tmp/portico-$UID/portico.sock

Database: $XDG_DATA_HOME/portico/portico.db
          Fallback: ~/.local/share/portico/portico.db
```
