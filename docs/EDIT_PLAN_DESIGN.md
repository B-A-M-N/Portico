# Connection editing — design contract

Status: accepted (architect pass, Tier 3)
Scope: audit item 17 — changing a connection's runtime-affecting fields.

## Invariant

An edit either fully applies or leaves the connection exactly as it was. At no
point may a connection hold a new profile alongside provider resources that the
old profile created and the new one does not describe.

## Why close-then-edit-then-open is not enough

The obvious approach — close the connection, change the profile, open it again —
is unsafe as the code stands, and the reason is worth recording:

- `PlanClose` stops the connector and deliberately keeps provider resources.
- `PlanOpen` plans from the profile alone. It never consults existing
  resources, so the Cloudflare adapter emits `create_tunnel` unconditionally.

Editing a hostname and reopening would therefore create a second tunnel and a
second DNS record while the originals stayed behind at the provider, owned by
nobody. The user would see a working connection and an invisible leak.

An edit must therefore reconcile the resources it invalidates, in the same
operation that changes the profile.

## Decisions

### D1 — One operation, one plan

An edit is a single planned operation with intent `edit`, not a sequence the
user drives. A failure halfway through a user-driven sequence leaves a
connection whose profile and resources disagree, which is the state this
contract exists to prevent.

### D2 — The previous profile is retained until the whole operation commits

The proposed profile is not written when the plan is built. It is applied by a
step near the end, after invalidated resources have been removed. Until that
step commits, the connection still has its old profile and a caller reading it
sees the old state. A failure before that step leaves nothing to undo at the
profile level.

### D3 — Resource impact is classified from the delta, not guessed

Each managed resource is classified against the proposed profile:

| Change | Tunnel | Route/DNS | Access policy | Connector |
|---|---|---|---|---|
| Name only | keep | keep | keep | keep |
| Origin/source | keep | keep | keep | restart |
| Protection | keep | keep | **replace** | restart |
| Hostname | keep | **replace** | **replace** | restart |
| Exposure mode | **replace** | **replace** | **replace** | restart |
| Provider or account | **replace** | **replace** | **replace** | restart |

"Replace" means the existing resource is deleted in this operation and the
reopen recreates it. A resource is only deleted when Portico owns it; an
adopted or external resource is reported and left alone.

### D4 — Pause and resume follow the connection's own desired state

If the connection is open, the plan stops the connector first and reopens at the
end. If it is closed, it stays closed and the edit only mutates the profile and
resources. An edit never opens a connection the user had closed.

### D5 — A no-op edit is a no-op

An edit whose delta is empty produces a plan with no steps and is reported as
such, rather than pointlessly closing and reopening a working connection.

### D6 — Name-only edits stay on the fast path

Renaming touches nothing at the provider and requires no plan. The existing
update path keeps handling it, so the common case does not become a
preview-and-confirm workflow.

## Plan shape

For an open connection changing its hostname:

```
1. edit-stop-connector      stop the connector
2. edit-delete-dns          delete the DNS record the old hostname created
3. edit-delete-access       delete the access policy bound to the old hostname
4. edit-apply-profile       commit the proposed profile   <- previous retained until here
5. …open steps from the new profile…
```

Steps 2 and 3 carry compensation, and step 4 is the commit boundary.

## Out of scope

Changing a connection's *kind* remains refused. That is a migration between
different lifecycles rather than an edit, and clone plus delete expresses it
without inventing a cross-kind conversion.

## Required tests

- A no-op edit produces no steps.
- A hostname change plans deletion of the DNS record and access policy, and not
  of the tunnel.
- A provider change plans deletion of every managed resource.
- An open connection's plan stops the connector first and reopens last; a closed
  connection's plan does neither.
- The previous profile is still readable until the apply step commits.
- A failure before the apply step leaves the original profile intact.
- An adopted resource is never planned for deletion.
- Changing the connection kind is refused.
