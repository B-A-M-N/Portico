package screens

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/paoloanzn/portico/internal/ipc"
)

// ConnectionCreator is the subset of the IPC client the wizard needs.
// The root model passes its client through; tests can pass a fake.
type ConnectionCreator interface {
	CreateConnection(ctx context.Context, req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error)
}

// ConnectionCreatedMsg is delivered when the async create command finishes.
type ConnectionCreatedMsg struct {
	ID         string
	Connection *ipc.ConnectionDTO
	Err        error
}

// WizardModel is the wizard for creating new connections.
type WizardModel struct {
	client   ConnectionCreator
	state    WizardState
	selected int
	input    string
	err      error
}

// WizardState holds the state for the new connection wizard.
type WizardState struct {
	Step          int
	SourceType    string
	Name          string
	SourceAddress string
	Port          string
	Hostname      string
	ExposureMode  string
	Protection    string
	Provider      string
}

// Wizard step constants
const (
	WizardStepIntent int = iota
	WizardStepName
	WizardStepSource
	WizardStepPort
	WizardStepExposure
	WizardStepHostname
	WizardStepProtection
	WizardStepProvider
	WizardStepReview
	WizardStepCreating
	WizardStepComplete
)

// Valid enum values (see internal/core/connection.go).
var (
	wizardSourceKinds = []string{"existing_service", "directory", "command", "mcp_server", "existing_service"}
	wizardExposures   = []string{"temporary_public", "permanent_public", "private_only"}
	wizardProtections = []string{"none", "email_otp", "identity_provider", "service_token"}
	wizardProviders   = []string{"cloudflare", "mock"}
)

// NewWizard creates a new wizard model.
func NewWizard(client ConnectionCreator) *WizardModel {
	return &WizardModel{
		client: client,
		state:  WizardState{Step: WizardStepIntent, Provider: wizardProviders[0]},
	}
}

// Step returns the current wizard step.
func (m *WizardModel) Step() int { return m.state.Step }

// hasPortStep reports whether the port step applies to the chosen source.
func (m *WizardModel) hasPortStep() bool {
	return m.state.SourceType == "existing_service" || m.state.SourceType == "command"
}

// HandleKey processes key input for the wizard. It never performs I/O
// itself; any side effect is returned as a tea.Cmd.
func (m *WizardModel) HandleKey(key string) tea.Cmd {
	switch m.state.Step {
	case WizardStepIntent:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(wizardSourceKinds)-1 {
				m.selected++
			}
		case "enter":
			m.state.SourceType = wizardSourceKinds[m.selected]
			m.state.Step = WizardStepName
			m.selected = 0
			m.input = m.state.Name
		}

	case WizardStepName:
		switch key {
		case "esc":
			m.state.Step = WizardStepIntent
			m.selected = 0
		case "enter":
			if strings.TrimSpace(m.input) == "" {
				m.err = fmt.Errorf("name is required")
				return nil
			}
			m.err = nil
			m.state.Name = strings.TrimSpace(m.input)
			m.state.Step = WizardStepSource
			m.input = m.state.SourceAddress
		default:
			m.input = editInput(m.input, key)
		}

	case WizardStepSource:
		switch key {
		case "esc":
			m.state.Step = WizardStepName
			m.input = m.state.Name
		case "enter":
			if strings.TrimSpace(m.input) == "" && m.state.SourceType != "existing_service" {
				m.err = fmt.Errorf("value is required")
				return nil
			}
			m.err = nil
			m.state.SourceAddress = strings.TrimSpace(m.input)
			if m.hasPortStep() {
				m.state.Step = WizardStepPort
				m.input = m.state.Port
			} else {
				m.state.Step = WizardStepExposure
				m.selected = 0
			}
		default:
			m.input = editInput(m.input, key)
		}

	case WizardStepPort:
		switch key {
		case "esc":
			m.state.Step = WizardStepSource
			m.input = m.state.SourceAddress
		case "enter":
			port := strings.TrimSpace(m.input)
			if port != "" {
				n, err := strconv.Atoi(port)
				if err != nil || n < 1 || n > 65535 {
					m.err = fmt.Errorf("port must be a number between 1 and 65535")
					return nil
				}
			}
			if port == "" && m.state.SourceType == "existing_service" && m.state.SourceAddress == "" {
				m.err = fmt.Errorf("enter a port or go back and enter an address")
				return nil
			}
			m.err = nil
			m.state.Port = port
			m.state.Step = WizardStepExposure
			m.selected = 0
		default:
			m.input = editInput(m.input, key)
		}

	case WizardStepExposure:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(wizardExposures)-1 {
				m.selected++
			}
		case "enter":
			m.state.ExposureMode = wizardExposures[m.selected]
			if m.state.ExposureMode == "permanent_public" {
				m.state.Step = WizardStepHostname
				m.input = m.state.Hostname
			} else {
				m.state.Hostname = ""
				m.state.Step = WizardStepProtection
				m.selected = 0
			}
		case "esc":
			if m.hasPortStep() {
				m.state.Step = WizardStepPort
				m.input = m.state.Port
			} else {
				m.state.Step = WizardStepSource
				m.input = m.state.SourceAddress
			}
		}

	case WizardStepHostname:
		switch key {
		case "esc":
			m.state.Step = WizardStepExposure
			m.selected = 0
		case "enter":
			if strings.TrimSpace(m.input) == "" {
				m.err = fmt.Errorf("permanent exposure requires a hostname")
				return nil
			}
			m.err = nil
			m.state.Hostname = strings.TrimSpace(m.input)
			m.state.Step = WizardStepProtection
			m.selected = 0
		default:
			m.input = editInput(m.input, key)
		}

	case WizardStepProtection:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(wizardProtections)-1 {
				m.selected++
			}
		case "enter":
			m.state.Protection = wizardProtections[m.selected]
			m.state.Step = WizardStepProvider
			m.selected = 0
		case "esc":
			if m.state.ExposureMode == "permanent_public" {
				m.state.Step = WizardStepHostname
				m.input = m.state.Hostname
			} else {
				m.state.Step = WizardStepExposure
				m.selected = 0
			}
		}

	case WizardStepProvider:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(wizardProviders)-1 {
				m.selected++
			}
		case "enter":
			m.state.Provider = wizardProviders[m.selected]
			m.state.Step = WizardStepReview
			m.selected = 0
		case "esc":
			m.state.Step = WizardStepProtection
			m.selected = 0
		}

	case WizardStepReview:
		switch key {
		case "enter":
			m.state.Step = WizardStepCreating
			return createConnectionCmd(m.client, m.buildRequest())
		case "esc":
			m.state.Step = WizardStepProvider
			m.selected = 0
		}
	}

	return nil
}

// HandleCreated applies the result of the async create command.
func (m *WizardModel) HandleCreated(msg ConnectionCreatedMsg) {
	if msg.Err != nil {
		m.err = msg.Err
		m.state.Step = WizardStepReview
		return
	}
	m.err = nil
	m.state.Step = WizardStepComplete
}

// buildRequest assembles the create request from the wizard state.
func (m *WizardModel) buildRequest() ipc.CreateConnectionRequest {
	s := m.state

	src := ipc.SourceDTO{Kind: s.SourceType}
	switch s.SourceType {
	case "existing_service":
		addr := s.SourceAddress
		if s.Port != "" {
			if addr == "" {
				addr = "127.0.0.1:" + s.Port
			} else if !strings.Contains(addr, ":") {
				addr = addr + ":" + s.Port
			}
		}
		src.Existing = &ipc.ExistingSourceDTO{Address: addr}
	case "directory":
		src.Directory = &ipc.DirectorySourceDTO{Path: s.SourceAddress}
	case "command":
		port, _ := strconv.Atoi(s.Port)
		src.Command = &ipc.CommandSourceDTO{Executable: s.SourceAddress, Port: port}
	case "mcp_server":
		src.MCP = &ipc.MCPSourceDTO{Transport: "http", Endpoint: s.SourceAddress}
	}

	return ipc.CreateConnectionRequest{
		Version: 1,
		Name:    s.Name,
		Source:  src,
		Exposure: ipc.ExposureDTO{
			Mode:             s.ExposureMode,
			RequestedAddress: s.Hostname,
		},
		Protection: ipc.ProtectionDTO{
			Kind: s.Protection,
		},
		Provider: ipc.ProviderSelectionDTO{
			ProviderID: s.Provider,
		},
	}
}

// createConnectionCmd returns a command that performs the IPC call off the
// update loop. The request is built before the closure runs so the command
// never reads or mutates wizard state.
func createConnectionCmd(client ConnectionCreator, req ipc.CreateConnectionRequest) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return ConnectionCreatedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		conn, err := client.CreateConnection(context.Background(), req)
		if err != nil {
			return ConnectionCreatedMsg{Err: err}
		}
		return ConnectionCreatedMsg{ID: conn.ID, Connection: conn}
	}
}

// editInput applies a single key press to a text input value.
func editInput(value, key string) string {
	switch key {
	case "backspace":
		if len(value) > 0 {
			return value[:len(value)-1]
		}
		return value
	case "space":
		return value + " "
	default:
		if len([]rune(key)) == 1 {
			return value + key
		}
		return value
	}
}

// View renders the wizard screen.
func (m *WizardModel) View() string {
	switch m.state.Step {
	case WizardStepIntent:
		return m.renderIntent()
	case WizardStepName:
		return m.withError(renderInput("Name this connection:", m.input))
	case WizardStepSource:
		return m.withError(renderInput(m.sourcePrompt(), m.input))
	case WizardStepPort:
		return m.withError(renderInput("Local port (empty to skip):", m.input))
	case WizardStepExposure:
		return m.renderExposure()
	case WizardStepHostname:
		return m.withError(renderInput("Enter the hostname to use:", m.input))
	case WizardStepProtection:
		return m.renderProtection()
	case WizardStepProvider:
		return m.renderProvider()
	case WizardStepReview:
		return m.renderReview()
	case WizardStepCreating:
		return "Creating connection..."
	case WizardStepComplete:
		return "Connection created!\n\nEnter Return home"
	}
	return ""
}

func (m *WizardModel) withError(view string) string {
	if m.err == nil {
		return view
	}
	return view + "\n\nError: " + m.err.Error()
}

func (m *WizardModel) sourcePrompt() string {
	switch m.state.SourceType {
	case "directory":
		return "Enter the directory path to serve:"
	case "command":
		return "Enter the command to run:"
	case "mcp_server":
		return "Enter the MCP server endpoint:"
	default:
		return "Enter the address of your service (host or host:port):"
	}
}

func (m *WizardModel) renderIntent() string {
	options := []string{
		"A service already running on this computer",
		"A folder of files",
		"A command or application",
		"An MCP server",
		"Something else (enter manually)",
	}
	return renderMenu("What should be reachable?", options, m.selected)
}

func (m *WizardModel) renderExposure() string {
	options := []string{
		"Temporarily, with a generated address",
		"Permanently, with my own hostname",
		"Privately, only from my devices",
	}
	return renderMenu("How should it be reachable?", options, m.selected)
}

func (m *WizardModel) renderProtection() string {
	options := []string{
		"Anyone with the address",
		"People who verify their email",
		"Specific identity providers",
		"Service tokens",
	}
	return renderMenu("Who should be able to reach it?", options, m.selected)
}

func (m *WizardModel) renderProvider() string {
	options := []string{
		"Cloudflare (default)",
		"Mock (testing)",
	}
	return renderMenu("Which provider should carry the connection?", options, m.selected)
}

func (m *WizardModel) renderReview() string {
	lines := []string{
		"REVIEW",
		"",
		fmt.Sprintf("Name:       %s", m.state.Name),
		fmt.Sprintf("Source:     %s", m.state.SourceType),
		fmt.Sprintf("Address:    %s", m.state.SourceAddress),
	}
	if m.state.Port != "" {
		lines = append(lines, fmt.Sprintf("Port:       %s", m.state.Port))
	}
	lines = append(lines, fmt.Sprintf("Exposure:   %s", m.state.ExposureMode))
	if m.state.Hostname != "" {
		lines = append(lines, fmt.Sprintf("Hostname:   %s", m.state.Hostname))
	}
	lines = append(lines,
		fmt.Sprintf("Protection: %s", m.state.Protection),
		fmt.Sprintf("Provider:   %s", m.state.Provider),
		"",
		"Portico will:",
		"  1. Create connection",
		"  2. Start connector",
		"  3. Verify endpoint",
	)
	if m.err != nil {
		lines = append(lines, "", "Error: "+m.err.Error())
	}
	lines = append(lines, "", "Enter Create  Esc Back")
	return strings.Join(lines, "\n")
}

func renderMenu(title string, options []string, selected int) string {
	lines := []string{title, ""}
	for i, opt := range options {
		prefix := "  "
		if i == selected {
			prefix = "▸ "
		}
		lines = append(lines, prefix+opt)
	}
	lines = append(lines, "", "↑↓ Navigate  Enter Select  Esc Back")
	return strings.Join(lines, "\n")
}

func renderInput(prompt, value string) string {
	lines := []string{prompt, "", "> " + value, "", "Enter to continue  Esc Back"}
	return strings.Join(lines, "\n")
}
