# ADR 0002: HTTP/SSE over Unix Socket IPC

## Status

Accepted

## Context

TUI, CLI, and JSON agents need to communicate with the supervisor.

## Decision

Use HTTP/1.1 over Unix domain socket with:
- JSON request/response bodies
- Server-Sent Events (SSE) for event streaming

Endpoints:
```
GET    /v1/snapshot     # Full state snapshot
GET    /v1/events        # SSE event stream
GET    /v1/connections  # List connections
POST   /v1/connections  # Create connection
GET    /v1/connections/{id}
PATCH  /v1/connections/{id}
POST   /v1/connections/{id}/plan/open
POST   /v1/connections/{id}/plan/close
POST   /v1/connections/{id}/plan/repair
POST   /v1/plans/{id}/apply
GET    /v1/operations/{id}
GET    /v1/providers
GET    /v1/discovery
```

SSE events include monotonic sequence numbers. Client sends `Last-Event-ID` on reconnect.

## Consequences

- Standard library implementation
- Easy CLI/agent reuse
- Easy inspection with local tooling
- No generated RPC code needed

## Security

- Same-UID peer validation
- 0700 parent directory
- Bounded request bodies
- Request deadlines
