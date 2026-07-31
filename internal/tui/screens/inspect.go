package screens

import (
	"fmt"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// InspectModel is the model for the inspect screen.
//
// Connection is the list summary the screen opens with. Detail is the
// authoritative view loaded from the supervisor; it arrives asynchronously and
// is nil until it does. Rendering must distinguish "not loaded yet" from
// "loaded and genuinely empty", so that an unavailable subsystem is never drawn
// as an authoritative empty state.
type InspectModel struct {
	Connection  *ipc.ConnectionDTO
	Detail      *ipc.ConnectionDetailDTO
	selectedTab int
	Diagnostics []ipc.DiagnosticDTO
	Logs        []string
	// LogTail is the authoritative log response, distinguishing "read and
	// empty" from "could not be read".
	LogTail *ipc.ConnectionLogsDTO
}

// NewInspect creates a new inspect model.
func NewInspect(conn *ipc.ConnectionDTO) *InspectModel {
	return &InspectModel{
		Connection: conn,
	}
}

// HandleKey processes key input for the inspect screen.
func (m *InspectModel) HandleKey(key string) {
	switch key {
	case "left", "h":
		if m.selectedTab > 0 {
			m.selectedTab--
		}
	case "right", "l":
		if m.selectedTab < 4 {
			m.selectedTab++
		}
	}
}

// View renders the inspect screen.
func (m *InspectModel) View() string {
	tabs := []string{"Overview", "Route", "Activity", "Technical", "Logs"}
	lines := []string{}

	// Tab bar (horizontal rendering)
	tabLine := ""
	for i, tab := range tabs {
		prefix := "  "
		if i == m.selectedTab {
			prefix = " ["
		} else {
			prefix = "  "
		}
		suffix := " "
		if i == m.selectedTab {
			suffix = "] "
		}
		tabLine += prefix + tab + suffix
	}
	lines = append(lines, tabLine)
	lines = append(lines, "")

	// Content based on selected tab
	switch m.selectedTab {
	case 0:
		lines = append(lines, m.renderOverview()...)
	case 1:
		lines = append(lines, m.renderRoute()...)
	case 2:
		lines = append(lines, m.renderActivity()...)
	case 3:
		lines = append(lines, m.renderTechnical()...)
	case 4:
		lines = append(lines, m.renderLogs()...)
	}

	lines = append(lines, "", "← → Switch tabs  Esc Back")

	result := ""
	for i, line := range lines {
		if i > 0 {
			result += "\n"
		}
		result += line
	}
	return result
}

func (m *InspectModel) renderOverview() []string {
	conn := m.Connection
	if conn == nil {
		return []string{"No connection data"}
	}
	lines := []string{
		"CONNECTION DETAILS",
		"",
		fmt.Sprintf("Name:       %s", conn.Name),
		fmt.Sprintf("Kind:       %s", ConnectionKindLabel(conn.Kind)),
		fmt.Sprintf("State:      %s", conn.UserState),
		fmt.Sprintf("Provider:   %s", conn.ProviderID),
	}

	// Address lines are printed only when there is an address, and labelled by
	// what the address is. Printing "Public:" unconditionally gave every port
	// forward and client tunnel a blank public address, which reads as one that
	// has not been assigned yet rather than one that will never exist.
	if conn.PublicAddress != "" {
		lines = append(lines, fmt.Sprintf("Public:     %s", conn.PublicAddress))
	}
	if conn.PrivateAddress != "" {
		lines = append(lines, fmt.Sprintf("%-11s %s", PrivateAddressLabel(conn.Kind)+":", conn.PrivateAddress))
	}
	if conn.PublicAddress == "" && conn.PrivateAddress == "" {
		lines = append(lines, "Address:    none yet")
	}
	if conn.ProviderAccountID != "" {
		lines = append(lines, fmt.Sprintf("Account:    %s", conn.ProviderAccountID))
	}
	if conn.ConnectorPID > 0 {
		lines = append(lines, fmt.Sprintf("Connector:  PID %d (%s)", conn.ConnectorPID, conn.ConnectorState))
	}
	if conn.Error != "" {
		lines = append(lines, "", "Error:", conn.Error)
	}
	return lines
}

// renderRoute draws the observed route from the authoritative detail: the local
// origin, the connector process, each managed provider resource, and the public
// endpoint. It renders the segments the supervisor actually reported rather
// than a fixed "localhost → ◈ → address" shape that is drawn identically
// whether or not the underlying resources exist.
func (m *InspectModel) renderRoute() []string {
	lines := []string{"ROUTE", ""}
	if m.Connection == nil {
		return append(lines, "No connection data")
	}
	if m.Detail == nil {
		return append(lines, "Loading route detail...")
	}

	// The route is the segment chain the supervisor computed. This screen used
	// to build a second chain of its own from the source, processes, resources
	// and endpoints — which restated the same route from kind-blind parts, and
	// so drew a provider tunnel and a public endpoint for connections that have
	// neither. There is one authority for what the hops are, and this is a view
	// of it.
	if len(m.Detail.Segments) == 0 {
		lines = append(lines, "No route is established for this connection.")
		if m.Connection.RuntimeState != "" {
			lines = append(lines, "Runtime state: "+m.Connection.RuntimeState)
		}
		return lines
	}

	for i, seg := range m.Detail.Segments {
		if i > 0 {
			lines = append(lines, "        ↓")
		}
		label := seg.Label
		if label == "" {
			label = seg.ID
		}
		lines = append(lines, fmt.Sprintf("%s  [%s]", label, seg.Status))
		if seg.Error != "" {
			lines = append(lines, "  "+seg.Error)
		}
	}

	if len(m.Detail.Resources) > 0 {
		lines = append(lines, "", "PROVIDER RESOURCES")
		for _, r := range m.Detail.Resources {
			lines = append(lines, fmt.Sprintf("  %-20s %s · %s", r.Type, r.ExternalID, r.Ownership))
		}
	}

	return lines
}

// ConnectionKindLabel names the kind in the words a user would use.
func ConnectionKindLabel(kind string) string {
	switch kind {
	case "port_forward":
		return "Port forward"
	case "private_network":
		return "Private network"
	case "client_tunnel":
		return "Client tunnel (no public address)"
	case "service_exposure", "":
		return "Published service"
	default:
		return kind
	}
}

// PrivateAddressLabel says what the non-public address is for this kind, since
// "Private" describes a tunnel's fallback address but not a forward's listener.
func PrivateAddressLabel(kind string) string {
	switch kind {
	case "port_forward":
		return "Listening"
	case "private_network":
		return "Network"
	default:
		return "Private"
	}
}

// renderActivity reports lifecycle observability rather than fabricated traffic
// metrics.
//
// Portico does not currently collect request rate, error count or latency from
// any provider. Rendering those labels with "--" values presents an unbuilt
// feature as a merely empty one, so the screen states plainly that traffic
// telemetry is unavailable and shows the liveness evidence that does exist.
func (m *InspectModel) renderActivity() []string {
	lines := []string{"ACTIVITY", ""}
	if m.Connection == nil {
		return append(lines, "No connection data")
	}
	if m.Detail == nil {
		return append(lines, "Loading activity detail...")
	}

	lines = append(lines, "Traffic telemetry is not collected for this provider.", "")
	lines = append(lines, "LIVENESS")

	if m.Detail.LastVerified != "" {
		lines = append(lines, "  Last verified:   "+m.Detail.LastVerified)
	} else {
		lines = append(lines, "  Last verified:   never")
	}
	lines = append(lines, "  Runtime state:   "+valueOrUnknown(m.Connection.RuntimeState))
	lines = append(lines, "  Connector state: "+valueOrUnknown(m.Connection.ConnectorState))

	if len(m.Detail.Processes) == 0 {
		lines = append(lines, "  Processes:       none running")
	}
	for _, p := range m.Detail.Processes {
		lines = append(lines, fmt.Sprintf("  Process:         PID %d · %s", p.PID, p.Status))
	}

	if op := m.Detail.LastOperation; op != nil {
		lines = append(lines, "", "LAST OPERATION")
		lines = append(lines, fmt.Sprintf("  %s · %s", op.Intent, op.State))
	}

	if len(m.Detail.Findings) > 0 {
		lines = append(lines, "", "OPEN FINDINGS")
		for _, f := range m.Detail.Findings {
			lines = append(lines, fmt.Sprintf("  [%s] %s: %s", f.Severity, f.Segment, f.Summary))
		}
	}
	return lines
}

func valueOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (m *InspectModel) renderTechnical() []string {
	if m.Connection == nil {
		return []string{"No connection data"}
	}
	lines := []string{
		"TECHNICAL DETAILS",
		"",
		fmt.Sprintf("Connection ID: %s", m.Connection.ID),
		fmt.Sprintf("Desired state: %s", m.Connection.DesiredState),
		fmt.Sprintf("Runtime state: %s", m.Connection.RuntimeState),
	}
	if m.Connection.ProviderAccountID != "" {
		lines = append(lines, fmt.Sprintf("Provider account: %s", m.Connection.ProviderAccountID))
	}

	if m.Detail == nil {
		lines = append(lines, "", "Loading provider resource detail...")
	} else {
		lines = append(lines, fmt.Sprintf("Profile revision: %d", m.Detail.Revision))
		if m.Detail.Driver.ProviderID != "" {
			lines = append(lines, fmt.Sprintf("Driver: %s", m.Detail.Driver.ProviderID))
		}

		// Exact external identifiers belong here: they are what a support
		// export or a manual provider-side cleanup has to act on.
		lines = append(lines, "", "Provider resources:")
		if len(m.Detail.Resources) == 0 {
			lines = append(lines, "  none recorded")
		}
		for _, r := range m.Detail.Resources {
			lines = append(lines, fmt.Sprintf("  %s  %s  (%s)", r.Type, r.ExternalID, r.Ownership))
		}

		lines = append(lines, "", "Processes:")
		if len(m.Detail.Processes) == 0 {
			lines = append(lines, "  none running")
		}
		for _, p := range m.Detail.Processes {
			lines = append(lines, fmt.Sprintf("  PID %d  %s  %s", p.PID, p.Status, p.ExecutablePath))
		}
	}

	if len(m.Diagnostics) > 0 {
		lines = append(lines, "", "Diagnostics:")
		for _, d := range m.Diagnostics {
			lines = append(lines, fmt.Sprintf("  [%s] %s: %s", d.Severity, d.Segment, d.Summary))
		}
	}
	return lines
}

func (m *InspectModel) renderLogs() []string {
	if m.LogTail != nil {
		lines := []string{"LOGS", ""}
		if !m.LogTail.Available {
			reason := m.LogTail.Unavailable
			if reason == "" {
				reason = "logs could not be read"
			}
			return append(lines,
				"Logs are unavailable.",
				"",
				"Reason: "+reason,
				"",
				"This does not mean the connector produced no output.")
		}
		if len(m.LogTail.Lines) == 0 {
			return append(lines, "The connector has not written any output yet.")
		}
		if m.LogTail.Truncated {
			lines = append(lines, "(showing the most recent lines)", "")
		}
		for _, entry := range m.LogTail.Lines {
			lines = append(lines, fmt.Sprintf("  [%s] %s", entry.Stream, entry.Text))
		}
		return lines
	}
	if len(m.Logs) == 0 {
		// Portico does not yet capture connector output into a per-connection
		// buffer, so there is nothing to tail. "No logs available" reads as an
		// authoritative empty state for a feature that does not exist; say
		// which it is.
		return []string{
			"LOGS",
			"",
			"Log capture is not implemented.",
			"",
			"Portico does not currently retain connector output per connection,",
			"so this view cannot be populated. Use the supervisor's own log",
			"output until per-connection capture exists.",
		}
	}
	lines := []string{"LOGS", ""}
	lines = append(lines, m.Logs...)
	return lines
}

// RepairModel is the model for the repair screen.
type RepairModel struct {
	Connection *ipc.ConnectionDTO
	Findings   []ipc.DiagnosticDTO
	Selected   int
}

// NewRepair creates a new repair model.
func NewRepair(conn *ipc.ConnectionDTO, findings []ipc.DiagnosticDTO) *RepairModel {
	return &RepairModel{Connection: conn, Findings: findings}
}

// HandleKey processes key input for the repair screen.
func (m *RepairModel) HandleKey(key string) {
	switch key {
	case "up", "k":
		if m.Selected > 0 {
			m.Selected--
		}
	case "down", "j":
		if m.Selected < len(m.Findings)-1 {
			m.Selected++
		}
	}
}

// View renders the repair screen.
func (m *RepairModel) View() string {
	lines := []string{"REPAIR", ""}
	if m.Connection == nil {
		lines = append(lines, "No connection available")
	} else if len(m.Findings) == 0 {
		lines = append(lines, "Running diagnostics...")
		if m.Connection.Error != "" {
			lines = append(lines, "", "Last error:", m.Connection.Error)
		}
	} else {
		lines = append(lines, "Issues found:", "")
		for i, f := range m.Findings {
			prefix := "  "
			if i == m.Selected {
				prefix = "▸ "
			}
			lines = append(lines, prefix+f.Summary)
			lines = append(lines, "   "+f.Explanation, "")
		}
		lines = append(lines, "↑↓ Navigate  Enter Repair  R Diagnostics  Esc Back")
	}
	result := ""
	for i, line := range lines {
		if i > 0 {
			result += "\n"
		}
		result += line
	}
	return result
}
