package screens

import (
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
)

// The review, around what the user asked for.
//
// The review listed the request's fields in the request's own words: a "Source"
// of "existing_service", an "Exposure" of "temporary_public", a "Protection" of
// "email_otp", a "Lifecycle" of "auto-start=true, on-disconnect=keep_alive" —
// the last two of which were printed as literals rather than read from the
// state, so they said the same thing whatever the user had chosen.
//
// The five questions a person needs answered before agreeing to this are what
// will be reachable, how it will be reached, who can reach it, what Portico is
// about to create, and what happens when it is closed. That is what this says.

// renderReview draws the last question before anything is created.
func (m *WizardModel) renderReview() string {
	var b strings.Builder
	b.WriteString("REVIEW\n\n")
	b.WriteString(m.state.Name + " — " + strings.ToLower(ConnectionKindLabel(m.state.ConnectionKind)) + "\n\n")

	// A refused creation is the one fact this screen must not hide. The error
	// used to render below the save choices — off-screen at every terminal
	// size this screen has — so Enter appeared to do nothing while the create
	// failed silently. Errors lead, consequences follow.
	if m.err != nil {
		b.WriteString("Error: " + m.err.Error() + "\n\n")
	}

	for _, section := range m.reviewSections() {
		b.WriteString(section.title + "\n")
		for _, line := range section.lines {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString("\n")
	}

	// The OpenAI-compatibility probe result, when this outcome asked for one.
	if m.state.ProfileKind == string(core.ProfileOpenAICompatible) {
		if m.probeSummary != "" {
			b.WriteString("COMPATIBILITY CHECK\n  " + m.probeSummary + "\n\n")
		} else if m.err == nil {
			b.WriteString("COMPATIBILITY CHECK\n  checking the endpoint speaks the OpenAI API...\n\n")
		}
	}

	b.WriteString("What should Portico do?\n")
	b.WriteString(m.renderMenu("", []string{
		"Save it, closed — you can open it whenever you want",
		"Save it and open it now — you will see the plan first",
	}, m.selected, m.contentWidth()))
	return b.String()
}

// reviewSection is one labelled group of statements about the connection.
type reviewSection struct {
	title string
	lines []string
}

// reviewSections is the review's content, per connection kind.
func (m *WizardModel) reviewSections() []reviewSection {
	switch m.state.ConnectionKind {
	case "port_forward":
		return m.portForwardReview()
	case "client_tunnel":
		return m.clientTunnelReview()
	case "private_network":
		return m.privateNetworkReview()
	default:
		return m.serviceExposureReview()
	}
}

// serviceExposureReview describes a published service.
func (m *WizardModel) serviceExposureReview() []reviewSection {
	sections := []reviewSection{
		{title: "What will be reachable", lines: []string{SourceSentence(m.state)}},
		{title: "How it will be reached", lines: []string{AddressSentence(m.state)}},
		{title: "Who can reach it", lines: []string{AccessSentence(m.state)}},
		{title: "Through", lines: m.providerLines()},
		{title: "What Portico will create and manage", lines: ManagedSentence(m.state)},
		{title: "When you close it", lines: []string{ClosingSentence(m.state)}},
		{title: "Startup and quitting", lines: m.lifecycleLines()},
	}
	// SPEC §29.4: a publicly reachable directory anyone can change must be
	// named as the risk it is before approval, not described in neutral field
	// words. The consequences are distinct because they are distinct: upload
	// means strangers can add content under your hostname, delete means they
	// can remove files Portico will never bring back. Stating both together is
	// the point — upload-only sounded benign next to a delete-only warning.
	if warning := m.mutableDirectoryWarning(); len(warning) > 0 {
		sections = append(sections, reviewSection{title: "Security warning", lines: warning})
	}
	if extra := m.sourceDetailLines(); len(extra) > 0 {
		sections = append(sections, reviewSection{title: "Details", lines: extra})
	}
	return sections
}

// mutableDirectoryWarning states what a public, write-enabled directory
// exposes, when upload and/or delete is enabled. It is a review section so it
// is conspicuous on the approval screen itself rather than buried in Details.
func (m *WizardModel) mutableDirectoryWarning() []string {
	if m.state.SourceType != "directory" {
		return nil
	}
	// Private exposure carries the same mechanics with no strangers on the
	// path; the warning is about public reachability.
	if m.state.ExposureMode != "permanent_public" && m.state.ExposureMode != "temporary_public" {
		return nil
	}
	var lines []string
	if m.state.AllowUpload {
		lines = append(lines,
			"UPLOAD is enabled: anyone who can reach this address can add or replace "+
				"files in this directory, under your hostname, with no sign-in.")
	}
	if m.state.AllowDelete {
		lines = append(lines,
			"DELETE is enabled: anyone who can reach this address can remove files. "+
				"Deleted files are NOT restored — removal is permanent.")
	}
	if len(lines) > 0 {
		lines = append(lines,
			"Protect it with email sign-in (press Esc and change Protection), or serve "+
				"the directory read-only.")
	}
	return lines
}

// portForwardReview describes a local forward, which has no public address, no
// DNS and no access policy — so it must not be described as though it had them.
func (m *WizardModel) portForwardReview() []reviewSection {
	protocol := strings.ToUpper(m.state.PortForwardProtocol)
	if protocol == "" {
		protocol = "TCP"
	}
	return []reviewSection{
		{title: "What will be reachable", lines: []string{
			m.state.PortForwardRemoteHost + " port " + m.state.PortForwardRemotePort +
				", as if it were on this machine",
		}},
		{title: "How it will be reached", lines: []string{
			"At 127.0.0.1:" + m.state.PortForwardLocalPort + " over " + protocol + ".",
			"There is no public address and no DNS record.",
		}},
		{title: "Who can reach it", lines: []string{
			"Anything running on this machine. The listener is local only.",
		}},
		{title: "What Portico will create and manage", lines: ManagedSentence(m.state)},
		{title: "When you close it", lines: []string{
			"The listener stops. Nothing at the remote end is affected.",
		}},
		{title: "Startup and quitting", lines: m.lifecycleLines()},
	}
}

// providerLines names the provider and account in display terms.
func (m *WizardModel) providerLines() []string {
	name := m.state.Provider
	for _, p := range m.caps.providers {
		if p.ID == m.state.Provider && p.DisplayName != "" {
			name = p.DisplayName
		}
	}
	if name == "" {
		return []string{"No provider chosen. This connection could not be opened."}
	}
	lines := []string{name}
	if m.state.AccountID != "" {
		lines = append(lines, "Account: "+m.accountLabel(m.state.AccountID))
	}
	return lines
}

// lifecycleLines states both lifecycle decisions and how to change them.
func (m *WizardModel) lifecycleLines() []string {
	return []string{
		autoStartSentence(m.state.AutoStart) + ".",
		onDisconnectSentence(m.state.OnDisconnect) + ".",
		"Press L to change either of these.",
	}
}

// sourceDetailLines carries the specifics that matter but do not belong in the
// five headline answers.
func (m *WizardModel) sourceDetailLines() []string {
	var out []string
	if m.state.WorkingDir != "" {
		out = append(out, "The command runs in "+m.state.WorkingDir+".")
	}
	if m.state.SourceType == "directory" {
		out = append(out, directoryModeSummary(m.state)+".")
		if m.state.DirectorySPA {
			out = append(out, "A request for a path that does not exist is answered with the "+
				"index page, which is what a single-page app needs.")
		}
	}
	if m.state.SourceType == "mcp_server" && m.state.MCPTransport != "" {
		out = append(out, "The MCP server is reached over "+m.state.MCPTransport+".")
	}
	if m.state.CommandUseShell {
		// Worth stating: a shell parses the line, so what runs is not exactly the
		// executable named — and shell features are available to it.
		out = append(out, "It runs through a shell, so pipes and redirection work.")
	}
	// What the command is given. A literal value is not printed; a reference is,
	// because the variable's name is not a secret and seeing it is how someone
	// checks they wrote the right one.
	out = append(out, CommandEnvSummary(m.state.CommandEnv)...)
	if m.state.SourceProtocol != "" && m.state.SourceType == "existing_service" {
		out = append(out, fmt.Sprintf("Portico connects to it over %s.", m.state.SourceProtocol))
	}
	return out
}
