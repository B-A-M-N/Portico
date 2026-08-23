package screens

import "strings"

// Plain language for the model's vocabulary.
//
// The review step printed the wire values: "service_exposure", "temporary_public",
// "email_otp", "keep_alive", "existing_service". Those are Portico's internal
// names for things, and a user confirming what is about to be created has to be
// able to read what they are confirming.
//
// These are the words for those values. They are here, in the screens package,
// because they are presentation — the wire values remain the wire values, and
// nothing here changes what is sent.

// SourceSentence says what will be reachable.
func SourceSentence(state WizardState) string {
	switch state.SourceType {
	case "existing_service":
		addr := state.SourceAddress
		if addr == "" {
			addr = "a service on this machine"
		}
		return "The service already listening at " + addr
	case "directory":
		path := state.SourceAddress
		if path == "" {
			path = "a directory"
		}
		return "The files in " + path
	case "command":
		cmd := state.SourceAddress
		if len(state.CommandArgs) > 0 {
			cmd += " " + strings.Join(state.CommandArgs, " ")
		}
		if cmd == "" {
			cmd = "a command"
		}
		return "Whatever " + cmd + " serves once Portico starts it"
	case "mcp_server":
		if state.MCPCommand {
			return "An MCP server Portico starts by running " + state.SourceAddress
		}
		if state.SourceAddress != "" {
			return "The MCP server already answering at " + state.SourceAddress
		}
		return "An MCP server on this machine"
	default:
		return "A service on this machine"
	}
}

// AddressSentence says how it will be reached.
func AddressSentence(state WizardState) string {
	switch state.ExposureMode {
	case "temporary_public":
		return "At a temporary public address the provider chooses. " +
			"It changes every time the connection is opened."
	case "permanent_public":
		if state.Hostname != "" {
			return "At https://" + state.Hostname + ", which stays the same every time."
		}
		return "At a hostname you own, which stays the same every time."
	case "private":
		return "Only from inside your private network. There is no public address."
	default:
		return "The provider decides the address."
	}
}

// AccessSentence says who will be able to reach it.
//
// This is the question a user most needs answered before creating something,
// and the one "Protection: none" answered least clearly.
func AccessSentence(state WizardState) string {
	switch state.Protection {
	case "email_otp":
		who := describeIdentities(state.AllowedEmails, state.AllowedDomains)
		return "Only " + who + ". Each person proves who they are with a code emailed to them."
	case "private_network":
		return "Only devices on your private network."
	case "none", "":
		if state.ExposureMode == "private" {
			return "Anyone who can reach your private network."
		}
		return "Anyone who has the address. There is no sign-in and no restriction."
	default:
		return "Controlled by the " + strings.ReplaceAll(state.Protection, "_", " ") + " policy."
	}
}

// describeIdentities names who an email-OTP policy admits.
func describeIdentities(emails, domains []string) string {
	var parts []string
	if len(emails) > 0 {
		parts = append(parts, strings.Join(emails, ", "))
	}
	for _, domain := range domains {
		parts = append(parts, "anyone with an @"+strings.TrimPrefix(domain, "@")+" address")
	}
	if len(parts) == 0 {
		return "the people you name"
	}
	return strings.Join(parts, ", and ")
}

// ManagedSentence says what Portico will create and manage.
//
// A user should know before agreeing that Portico is about to create a DNS
// record in their zone, or that it will be running and stopping a process on
// their behalf.
func ManagedSentence(state WizardState) []string {
	var out []string
	switch state.ConnectionKind {
	case "port_forward":
		out = append(out, "A listener on 127.0.0.1:"+state.PortForwardLocalPort+
			", relaying to "+state.PortForwardRemoteHost+":"+state.PortForwardRemotePort+".")
		return out
	case "client_tunnel":
		out = append(out, "A tunnel client process, connecting outward. Nothing is published.")
		return out
	}

	out = append(out, "A tunnel from the provider to this machine.")
	if state.ExposureMode == "permanent_public" && state.Hostname != "" {
		out = append(out, "A DNS record for "+state.Hostname+" in your zone.")
	}
	if state.Protection == "email_otp" {
		out = append(out, "An access policy naming who is allowed through.")
	}
	if state.SourceType == "command" || (state.SourceType == "mcp_server" && state.MCPCommand) {
		out = append(out, "The command itself: Portico starts it, waits for it to listen, "+
			"and stops it when the connection closes.")
	}
	if state.SourceType == "directory" {
		out = append(out, "A local web server for the directory, run by Portico.")
	}
	return out
}

// ClosingSentence says what happens when the connection is closed.
func ClosingSentence(state WizardState) string {
	ownsProcess := state.SourceType == "command" ||
		(state.SourceType == "mcp_server" && state.MCPCommand)
	switch {
	case ownsProcess && state.ExposureMode == "permanent_public":
		return "The tunnel stops and the command Portico started is stopped. " +
			"The hostname and its DNS record are kept, so opening it again reuses them."
	case ownsProcess:
		return "The tunnel stops and the command Portico started is stopped. " +
			"The temporary address is gone for good."
	case state.ExposureMode == "permanent_public":
		return "The tunnel stops. Your service keeps running, and the hostname is kept " +
			"for the next time you open it."
	case state.ExposureMode == "temporary_public":
		return "The tunnel stops and your service keeps running. The temporary address is " +
			"gone for good — reopening produces a different one."
	default:
		return "The tunnel stops. Your service keeps running."
	}
}
