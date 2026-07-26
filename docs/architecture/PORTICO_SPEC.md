# Portico Specification

This document is the authoritative specification for Portico implementation.

## Key Architecture Principles

1. **Supervisor is the only process allowed to mutate connection state**
2. **Supervisor is the only process allowed to open SQLite**
3. **TUI never calls provider, connector, or database directly**
4. **CLI never duplicates controller logic**
5. **Every mutation is an immutable operation plan**
6. **Provider behavior confined to adapter packages**
7. **Closing TUI disconnects client, not connection**
8. **Profile = desired state, Runtime = observed state**
9. **Rendering is pure - no I/O in View()**
10. **Provider capabilities are structured constraints**

## Core Components

- **TUI/CLI**: Client applications using supervisor API
- **Supervisor**: Manages connections, database, processes
- **Controller**: Provider-neutral planning and reconciliation
- **Providers**: Mock, Cloudflare, ngrok, Tailscale, zrok

## Implementation Phases

1. Architecture lock (Phase 0)
2. Donor characterization (Phase 1)
3. Core and mock provider (Phase 2)
4. Store and supervisor (Phase 3)
5. Static TUI vertical slice (Phase 4)
6. TUI connected to supervisor (Phase 5)
7. Cloudflare adapter extraction (Phase 6)
8. Discovery and recommendation (Phase 7)
9. Diagnostics and repair (Phase 8)
10. Motion and telemetry (Phase 9)
11. Hardening and release (Phase 10)

For full specification, see: https://github.com/B-A-M-N/portico/blob/main/SPEC.md
