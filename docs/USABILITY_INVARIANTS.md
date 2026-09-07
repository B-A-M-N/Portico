# Normal-user usability invariants

Status: accepted, implemented, and covered by focused tests.

Portico asks for intent, discovers facts, presents choices, and asks for raw
technical input only when the fact belongs to an external system or lies outside
the local discovery scope.

Every user-facing field that can otherwise become a lookup exercise is classified
as one of:

| Class | Meaning |
| --- | --- |
| `INFERRED` | Derived from a value the user already supplied, such as a port in `host:port`. |
| `DISCOVERED` | Returned by Portico's local scan or a provider control-plane query. |
| `SELECTED` | Chosen from provider- or Portico-owned options. |
| `SUGGESTED` | A safe generated default that is visible and editable. |
| `MANUAL_REQUIRED` | A raw value is genuinely necessary; the UI must explain why. |

`core.UsabilityField.Validate` rejects an unexplained `MANUAL_REQUIRED` field.
The wizard exposes its current classification through `UsabilityFields`, so the
same contract can be asserted independently of rendering.

## Field policy

- Provider accounts and zones are discovered from the provider after credentials
  are supplied, then selected by human labels. A manual ID is an explicit
  fallback, not the normal path.
- Running local services, private-network origins, and local MCP endpoints use
  the shared discovery selector. A manually entered address says that the scan
  did not establish the fact.
- Permanent hostnames start with a generated subdomain inside the selected zone.
  A custom hostname is manual because it is user-owned; validation explains and
  enforces the selected zone boundary.
- Command executables, filesystem paths, remote forward targets, and existing
  external tunnel IDs remain manual because Portico cannot infer user intent or
  inspect the external control plane. Each carries a reason in the flow.
- A command's port is manual unless it was included in the command/address. A
  discovered service's address carries its port, so the port is inferred rather
  than re-asked.
- Technical identifiers are available in Technical/verbose/JSON surfaces. Home,
  provider lists, ordinary Inspect, wizard choices, and default CLI output use
  names and descriptions.

## Acceptance scenarios

The focused acceptance tests cover these normal-user paths:

1. A running service is found and selected from the wizard without typing its
   address, port, or protocol.
2. A Cloudflare token discovers a friendly account and friendly zone; IDs are
   not required in the normal path.
3. A permanent exposure accepts the generated `<connection>.<zone>` hostname
   without requiring a full FQDN.
4. Tailscale expose selects a discovered service without a host:port question.
5. MCP tunnel selects a discovered MCP service; only the externally created
   tunnel reference is pasted.
6. Edit uses explicit provider/account/source choices where the saved connection
   model supports an in-place change, and explains immutable connection kind and
   source type rather than pretending a raw edit is safe.
7. CLI commands resolve exact names and unique prefixes; ambiguous friendly names
   list names and ask for a longer name or ID prefix.
8. Ordinary surfaces omit opaque identifiers. Technical tabs, `--verbose`, and
   `--json` remain the intentional exceptions.

The current focused tests are in `internal/core/usability_test.go`,
`internal/tui/edit_kinds_test.go`, `internal/cli/handler_test.go`,
`internal/tui/selection_visibility_test.go`,
`internal/tui/provider_setup_discovery_test.go`, and the
provider/supervisor setup tests. (An earlier revision of this list named two
test files that never existed in the repository; a coverage list that names
files which do not run is claiming coverage it does not have, so this list is
contract-tested — see `internal/docs/usability_doc_test.go`.)

## Cloudflare zone scope

The current connection model stores one selected Cloudflare zone per account
configuration. That is sufficient for the normal one-connection/one-zone flow:
the wizard discovers all visible zones, the user selects one, and the chosen
zone is persisted with the account metadata. A single account cannot currently
express different zones per connection without reselecting/reconfiguring the
account. Supporting per-connection zone bindings is a future model change, not
a reason to expose zone IDs in the normal UI.
