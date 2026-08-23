package tui

import (
	"fmt"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// The properties of each connection kind.
//
// The edit screen offered a name, a hostname, a protection mode, its identities,
// two lifecycle answers and an account. The controller has classified changes to
// the source, the protocol, the exposure mode, the local port, the remote target
// and the forward direction since those kinds existed — so a connection created
// with the wrong address could only be deleted and made again.
//
// Each kind's rows are its own. A property another kind has is not offered with a
// reason it cannot be used; it is simply not that kind's property, and the rows
// reflect the connection in front of the user.

// serviceExposureRows are the properties of a published service.
func (s *editState) serviceExposureRows() []editRow {
	exposed := s.detail.DesiredSpec.ServiceExposure
	rows := []editRow{}

	// What is being published. Only an existing service has an address to change:
	// a directory has a path and a command has an executable, and changing those
	// is a different operation from repointing at another address.
	if existing := exposed.Source.Existing; existing != nil {
		rows = append(rows, editRow{
			field: editSourceAddress, label: "Service address", current: existing.Address,
			pending: derefString(s.sourceAddress), editable: true,
			explain: "Where the service Portico publishes is listening, as host:port.",
		}, editRow{
			field: editSourceProtocol, label: "Reached over", current: existing.Protocol,
			pending:  derefString(s.sourceProtocol),
			choices:  []string{"http", "https"},
			editable: true,
			explain:  "How Portico connects to your service — not how the public reaches it.",
		})
	} else {
		rows = append(rows, editRow{
			field: editSourceAddress, label: "Source", editable: false,
			current: sourceDescription(exposed.Source),
			reason: "Portico runs this source itself, so there is no address to repoint. " +
				"Copy the connection to publish something else.",
		})
	}

	// How it is reached. The available modes are the ones the provider declares,
	// so a mode this provider cannot deliver is not offered.
	modes := s.exposureModeChoices()
	rows = append(rows, editRow{
		field: editExposureMode, label: "Address behaviour",
		current: exposureModeWord(exposed.Exposure.Mode),
		pending: exposureModeWord(derefString(s.exposureMode)),
		choices: modes, editable: len(modes) > 1,
		reason:  "this provider offers only one kind of address",
		explain: "Whether the address stays the same between openings or is assigned each time.",
	})

	// The hostname, when the mode has one to set.
	hostname := exposed.Exposure.RequestedAddress
	if s.effectiveExposureMode() != "permanent_public" {
		rows = append(rows, editRow{
			field: editHostname, label: "Hostname", current: hostname,
			editable: false,
			reason:   "this connection uses a temporary address, which the provider assigns",
			explain:  "Set the address behaviour to a stable address to choose a hostname.",
		})
	} else {
		rows = append(rows, editRow{
			field: editHostname, label: "Hostname", current: hostname,
			pending: derefString(s.hostname), editable: true,
			explain: "The name the service is reached at. Portico manages its DNS record.",
		})
	}

	// Who may reach it. The choices come from what the provider declares it can
	// enforce, not from a fixed cycle: a provider with no access control must not
	// offer a policy it cannot apply.
	protections := s.protectionChoices()
	rows = append(rows, editRow{
		field: editProtection, label: "Who can reach it",
		current: protectionWord(exposed.Protection.Kind),
		pending: protectionWord(derefString(s.protection)),
		choices: protections, editable: len(protections) > 1,
		reason:  "this provider cannot enforce an access policy",
		explain: "Whether anyone with the address can reach it, or only people you name.",
	})
	if s.effectiveProtection() == "email_otp" {
		rows = append(rows, editRow{
			field: editProtectionRules, label: "Who can sign in",
			current: screens.ProtectionRulesInput(
				exposed.Protection.AllowedEmails, exposed.Protection.AllowedDomains),
			pending:  derefString(s.protectionRules),
			editable: true,
			explain: "Email addresses, or @domain to admit anyone with an address there. " +
				"Separate them with commas.",
		})
	}
	return rows
}

// portForwardRows are the properties of a local forward.
func (s *editState) portForwardRows() []editRow {
	forward := s.detail.DesiredSpec.PortForward
	return []editRow{
		{
			field: editLocalPort, label: "Local port",
			current: fmt.Sprint(forward.LocalPort), pending: derefString(s.localPort),
			editable: true,
			explain:  "The port on this machine that forwards to the remote end.",
		},
		{
			field: editRemoteHost, label: "Remote host",
			current: forward.RemoteHost, pending: derefString(s.remoteHost),
			editable: true,
			explain:  "The host the forward connects to.",
		},
		{
			field: editRemotePort, label: "Remote port",
			current: fmt.Sprint(forward.RemotePort), pending: derefString(s.remotePort),
			editable: true,
			explain:  "The port on that host.",
		},
		{
			field: editForwardProtocol, label: "Protocol",
			current: forward.Protocol, pending: derefString(s.forwardProtocol),
			// TCP only. UDP is not offered because Portico cannot forward it: the
			// relay is connection-oriented and has no datagram path, so listing it
			// would be an option that fails at apply.
			choices:  []string{"tcp"},
			editable: false,
			reason:   "Portico can only forward TCP",
			explain:  "The transport the forward carries.",
		},
		{
			// Direction is the spec's own field and the controller classifies a
			// change to it, but only local forwarding is implemented: a remote
			// forward needs a provider to terminate the far side. Offering it would
			// be an edit that plans and then fails.
			field: editHostname, label: "Direction",
			current:  directionWord(forward.Direction),
			editable: false,
			reason: "Portico forwards from this machine outward only; publishing a local " +
				"port at a remote endpoint needs a provider to terminate the far side",
		},
	}
}

// accountRow is the account selector, offered only where accounts apply.
func (s *editState) accountRow() editRow {
	summary := s.detail.Summary
	// A provider with no accounts is accountless: showing an account selector for
	// it offers a choice between nothing.
	if len(s.usable) == 0 {
		return editRow{
			field: editAccount, label: "Account", editable: false,
			current: accountRowCurrent(summary.ProviderAccountID),
			reason:  "this provider does not use accounts",
		}
	}
	if len(s.usable) == 1 && string(s.usable[0].ID) == summary.ProviderAccountID {
		return editRow{
			field: editAccount, label: "Account", editable: false,
			current: s.accountLabelFor(summary.ProviderAccountID),
			reason:  "this is the only usable account for this provider",
		}
	}

	choices := make([]string, 0, len(s.usable))
	for _, account := range s.usable {
		choices = append(choices, string(account.ID))
	}
	return editRow{
		field:   editAccount,
		label:   "Account",
		current: s.accountLabelFor(summary.ProviderAccountID),
		pending: s.accountLabelFor(derefString(s.accountID)),
		choices: choices, editable: true,
		explain: "Which of this provider's accounts owns the resources for this connection.",
	}
}

// accountRowCurrent names the account on a row that cannot be changed.
func accountRowCurrent(id string) string {
	if id == "" {
		return "none"
	}
	return id
}

// exposureModeWord says what an exposure mode does.
func exposureModeWord(mode string) string {
	switch mode {
	case "temporary_public":
		return "a new address each time"
	case "permanent_public":
		return "a stable address you choose"
	case "private":
		return "reachable only on your private network"
	default:
		return mode
	}
}

// protectionWord says who a protection mode admits.
func protectionWord(kind string) string {
	switch kind {
	case "none", "":
		return "anyone with the address"
	case "email_otp":
		return "only people you name"
	case "private_network":
		return "only devices on your private network"
	case "service_token":
		return "only callers with a service token"
	default:
		return kind
	}
}

// directionWord names a forward's direction.
func directionWord(direction string) string {
	switch direction {
	case "local":
		return "from this machine outward"
	case "remote":
		return "from a remote endpoint inward"
	default:
		return direction
	}
}

// sourceDescription says what a source Portico runs itself is.
//
// Only an existing service has an address to repoint. A directory has a path and
// a command has an executable, and changing those is a different operation from
// pointing the connection at somewhere else.
func sourceDescription(source ipc.SourceDTO) string {
	switch {
	case source.Directory != nil:
		return "the files in " + source.Directory.Path
	case source.Command != nil:
		return "whatever " + source.Command.Executable + " serves"
	case source.MCP != nil:
		if source.MCP.Command != nil {
			return "an MCP server Portico runs"
		}
		return "the MCP server at " + source.MCP.Endpoint
	case source.Existing != nil:
		return source.Existing.Address
	default:
		return "a service on this machine"
	}
}
