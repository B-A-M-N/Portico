package screens

import (
	"fmt"

	"github.com/paoloanzn/portico/internal/ipc"
)

// InspectModel is the model for the inspect screen.
type InspectModel struct {
	Connection  *ipc.ConnectionDTO
	selectedTab int
	Diagnostics []ipc.DiagnosticDTO
	Logs        []string
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
		fmt.Sprintf("State:      %s", conn.UserState),
		fmt.Sprintf("Provider:   %s", conn.ProviderID),
		fmt.Sprintf("Public:     %s", conn.PublicAddress),
		fmt.Sprintf("Private:    %s", conn.PrivateAddress),
	}
	if conn.ConnectorPID > 0 {
		lines = append(lines, fmt.Sprintf("Connector:  PID %d (%s)", conn.ConnectorPID, conn.ConnectorState))
	}
	if conn.Error != "" {
		lines = append(lines, "", "Error:", conn.Error)
	}
	return lines
}

func (m *InspectModel) renderRoute() []string {
	lines := []string{"ROUTE VISUALIZATION", ""}
	if m.Connection == nil {
		lines = append(lines, "No connection data")
		return lines
	}
	if m.Connection.PublicAddress != "" {
		lines = append(lines, "  localhost → ◈ → "+m.Connection.PublicAddress)
	} else if m.Connection.RuntimeState == "closed" {
		lines = append(lines, "  localhost → ○ (closed)")
	} else {
		lines = append(lines, "  Route not established")
	}
	return lines
}

func (m *InspectModel) renderActivity() []string {
	return []string{"ACTIVITY", "", "Request rate: --", "Error count:  --", "Latency:      --", "", "Activity data not available"}
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
	if len(m.Diagnostics) > 0 {
		lines = append(lines, "", "Diagnostics:")
		for _, d := range m.Diagnostics {
			lines = append(lines, fmt.Sprintf("  [%s] %s: %s", d.Severity, d.Segment, d.Summary))
		}
	}
	return lines
}

func (m *InspectModel) renderLogs() []string {
	if len(m.Logs) == 0 {
		return []string{"LOGS", "", "No logs available"}
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
