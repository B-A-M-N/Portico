package screens

import (
	"context"
	"fmt"
	"net"
	"net/mail"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
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
	client         ConnectionCreator
	state          WizardState
	selected       int
	input          string
	err            error
	fullCloudflare bool
	accounts       []ipc.ProviderAccountDTO
}

// WizardState holds the state for the new connection wizard.
type WizardState struct {
	Step           int
	SourceType     string
	Name           string
	SourceAddress  string
	SourceProtocol string
	Port           string
	CommandArgs    []string
	WorkingDir     string
	Hostname       string
	ExposureMode   string
	Protection     string
	AllowedEmails  []string
	AllowedDomains []string
	Provider       string
	AccountID      string
}

// Wizard step constants
const (
	WizardStepIntent int = iota
	WizardStepName
	WizardStepSource
	WizardStepPort
	WizardStepCommandArgs
	WizardStepCommandWorkingDir
	WizardStepExposure
	WizardStepHostname
	WizardStepProtection
	WizardStepProtectionRules
	WizardStepProvider
	WizardStepAccount
	WizardStepReview
	WizardStepCreating
	WizardStepComplete
)

// Valid enum values (see internal/core/connection.go).
var (
	wizardSourceKinds = []string{"existing_service", "directory", "command", "mcp_server"}
	wizardProviders   = []string{"cloudflare"}
)

// NewWizard creates a new wizard model.
func NewWizard(client ConnectionCreator, fullCloudflare bool, accounts []ipc.ProviderAccountDTO) *WizardModel {
	return &WizardModel{
		client:         client,
		fullCloudflare: fullCloudflare,
		accounts:       append([]ipc.ProviderAccountDTO(nil), accounts...),
		state:          WizardState{Step: WizardStepIntent, Provider: wizardProviders[0]},
	}
}

// NewWizardForService starts the normal wizard with a discovery result already
// selected, so a user never has to retype a port discovered by Portico.
func NewWizardForService(client ConnectionCreator, fullCloudflare bool, accounts []ipc.ProviderAccountDTO, address, protocol string) *WizardModel {
	m := NewWizard(client, fullCloudflare, accounts)
	m.state.SourceType = "existing_service"
	m.state.SourceAddress = address
	m.state.SourceProtocol = protocol
	m.state.Step = WizardStepName
	return m
}

func (m *WizardModel) exposures() []string {
	if m.fullCloudflare {
		return []string{"temporary_public", "permanent_public"}
	}
	return []string{"temporary_public"}
}

// protections returns only choices that the selected, configured provider can
// actually create. Cloudflare Access is available only for named (permanent)
// tunnels, so a temporary Quick Tunnel never offers an unusable choice.
func (m *WizardModel) protections() []string {
	if m.fullCloudflare && m.state.ExposureMode == "permanent_public" {
		return []string{"none", "email_otp"}
	}
	return []string{"none"}
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
			if m.state.SourceType == "existing_service" && port != "" {
				if _, err := existingServiceAddress(m.state.SourceAddress, port); err != nil {
					m.err = err
					return nil
				}
			}
			m.err = nil
			m.state.Port = port
			if m.state.SourceType == "command" {
				m.state.Step = WizardStepCommandArgs
				m.input = commandArgsInput(m.state.CommandArgs)
			} else {
				m.state.Step = WizardStepExposure
				m.selected = 0
			}
		default:
			m.input = editInput(m.input, key)
		}

	case WizardStepCommandArgs:
		switch key {
		case "esc":
			m.state.Step = WizardStepPort
			m.input = m.state.Port
		case "enter":
			args, err := parseCommandArgs(m.input)
			if err != nil {
				m.err = err
				return nil
			}
			m.err = nil
			m.state.CommandArgs = args
			m.state.Step = WizardStepCommandWorkingDir
			m.input = m.state.WorkingDir
		default:
			m.input = editInput(m.input, key)
		}

	case WizardStepCommandWorkingDir:
		switch key {
		case "esc":
			m.state.Step = WizardStepCommandArgs
			m.input = commandArgsInput(m.state.CommandArgs)
		case "enter":
			m.err = nil
			m.state.WorkingDir = strings.TrimSpace(m.input)
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
			if m.selected < len(m.exposures())-1 {
				m.selected++
			}
		case "enter":
			m.state.ExposureMode = m.exposures()[m.selected]
			if m.state.ExposureMode == "permanent_public" {
				m.state.Step = WizardStepHostname
				m.input = m.state.Hostname
			} else {
				m.state.Hostname = ""
				m.state.Step = WizardStepProtection
				m.selected = 0
			}
		case "esc":
			if m.state.SourceType == "command" {
				m.state.Step = WizardStepCommandWorkingDir
				m.input = m.state.WorkingDir
			} else if m.hasPortStep() {
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
			if m.selected < len(m.protections())-1 {
				m.selected++
			}
		case "enter":
			m.state.Protection = m.protections()[m.selected]
			if m.state.Protection == "email_otp" {
				m.state.Step = WizardStepProtectionRules
				m.input = protectionRulesInput(m.state.AllowedEmails, m.state.AllowedDomains)
				m.err = nil
				return nil
			}
			m.state.AllowedEmails = nil
			m.state.AllowedDomains = nil
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

	case WizardStepProtectionRules:
		switch key {
		case "esc":
			m.state.Step = WizardStepProtection
			m.selected = protectionIndex(m.protections(), m.state.Protection)
		case "enter":
			emails, domains, err := parseProtectionRules(m.input)
			if err != nil {
				m.err = err
				return nil
			}
			m.err = nil
			m.state.AllowedEmails = emails
			m.state.AllowedDomains = domains
			m.state.Step = WizardStepProvider
			m.selected = 0
		default:
			m.input = editInput(m.input, key)
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
			switch len(m.accounts) {
			case 0:
				m.state.AccountID = ""
				m.state.Step = WizardStepReview
			case 1:
				m.state.AccountID = m.accounts[0].ID
				m.state.Step = WizardStepReview
			default:
				m.state.Step = WizardStepAccount
			}
			m.selected = 0
		case "esc":
			if m.state.Protection == "email_otp" {
				m.state.Step = WizardStepProtectionRules
				m.input = protectionRulesInput(m.state.AllowedEmails, m.state.AllowedDomains)
			} else {
				m.state.Step = WizardStepProtection
				m.selected = protectionIndex(m.protections(), m.state.Protection)
			}
		}

	case WizardStepAccount:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(m.accounts)-1 {
				m.selected++
			}
		case "enter":
			m.state.AccountID = m.accounts[m.selected].ID
			m.state.Step = WizardStepReview
			m.selected = 0
		case "esc":
			m.state.Step = WizardStepProvider
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
		if normalized, err := existingServiceAddress(addr, s.Port); err == nil {
			addr = normalized
		}
		protocol := s.SourceProtocol
		if protocol == "" {
			protocol = "http"
		}
		src.Existing = &ipc.ExistingSourceDTO{Address: addr, Protocol: protocol}
	case "directory":
		src.Directory = &ipc.DirectorySourceDTO{Path: s.SourceAddress}
	case "command":
		port, _ := strconv.Atoi(s.Port)
		src.Command = &ipc.CommandSourceDTO{
			Executable: s.SourceAddress,
			Args:       append([]string(nil), s.CommandArgs...),
			WorkingDir: s.WorkingDir,
			Port:       port,
			Protocol:   "http",
		}
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
			Kind:           s.Protection,
			AllowedEmails:  append([]string(nil), s.AllowedEmails...),
			AllowedDomains: append([]string(nil), s.AllowedDomains...),
		},
		Provider: ipc.ProviderSelectionDTO{
			ProviderID: s.Provider,
			AccountID:  s.AccountID,
		},
	}
}

// existingServiceAddress adds a separately entered port without confusing an
// IPv6 host for an address that already includes a port. Existing host:port
// values are retained exactly, so a discovered address is never rewritten.
func existingServiceAddress(address, port string) (string, error) {
	address = strings.TrimSpace(address)
	port = strings.TrimSpace(port)
	if port == "" {
		return address, nil
	}
	if address == "" {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address, nil
	}

	host := strings.TrimPrefix(strings.TrimSuffix(address, "]"), "[")
	if net.ParseIP(host) != nil {
		return net.JoinHostPort(host, port), nil
	}
	if strings.Contains(address, ":") {
		return "", fmt.Errorf("address %q is not a host or valid IPv6 literal; use host:port or [IPv6]:port", address)
	}
	return net.JoinHostPort(address, port), nil
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
	case WizardStepCommandArgs:
		return m.withError(renderInput("Command arguments (comma-separated; empty to skip):", m.input))
	case WizardStepCommandWorkingDir:
		return m.withError(renderInput("Working directory (empty to use Portico's):", m.input))
	case WizardStepExposure:
		return m.renderExposure()
	case WizardStepHostname:
		return m.withError(renderInput("Enter the hostname to use:", m.input))
	case WizardStepProtection:
		return m.renderProtection()
	case WizardStepProtectionRules:
		return m.withError(renderInput("Allow emails or domains (comma-separated; @example.com permits a domain):", m.input))
	case WizardStepProvider:
		return m.renderProvider()
	case WizardStepAccount:
		return m.renderAccount()
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
		return "Enter the command executable to run:"
	case "mcp_server":
		return "Enter the MCP server endpoint:"
	default:
		return "Enter the address of your service (host or host:port):"
	}
}

func (m *WizardModel) renderIntent() string {
	options := []string{
		"A service already running on this computer",
		"A directory Portico should serve",
		"An HTTP command Portico should run",
		"An HTTP MCP server",
	}
	return renderMenu("What should be reachable?", options, m.selected)
}

func (m *WizardModel) renderExposure() string {
	options := []string{"Temporarily, with a generated address"}
	if m.fullCloudflare {
		options = append(options, "Permanently, with my own hostname")
	}
	return renderMenu("How should it be reachable?", options, m.selected)
}

func (m *WizardModel) renderProtection() string {
	options := []string{"Anyone with the address"}
	if m.fullCloudflare && m.state.ExposureMode == "permanent_public" {
		options = append(options, "Email one-time passcode (limit who can sign in)")
	}
	return renderMenu("Who should be able to reach it?", options, m.selected)
}

func (m *WizardModel) renderProvider() string {
	options := []string{
		"Cloudflare (default)",
	}
	return renderMenu("Which provider should carry the connection?", options, m.selected)
}

func (m *WizardModel) renderAccount() string {
	options := make([]string, 0, len(m.accounts))
	for _, account := range m.accounts {
		label := account.Label
		if label == "" || label == account.ID {
			label = account.ID
		} else {
			label += " (" + account.ID + ")"
		}
		options = append(options, label)
	}
	return renderMenu("Which Cloudflare account should own this connection?", options, m.selected)
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
	if len(m.state.CommandArgs) > 0 {
		lines = append(lines, fmt.Sprintf("Arguments:  %s", strings.Join(m.state.CommandArgs, ", ")))
	}
	if m.state.WorkingDir != "" {
		lines = append(lines, fmt.Sprintf("Working dir: %s", m.state.WorkingDir))
	}
	lines = append(lines, fmt.Sprintf("Exposure:   %s", m.state.ExposureMode))
	if m.state.Hostname != "" {
		lines = append(lines, fmt.Sprintf("Hostname:   %s", m.state.Hostname))
	}
	if len(m.state.AllowedEmails) > 0 {
		lines = append(lines, fmt.Sprintf("Allowed emails:  %s", strings.Join(m.state.AllowedEmails, ", ")))
	}
	if len(m.state.AllowedDomains) > 0 {
		lines = append(lines, fmt.Sprintf("Allowed domains: %s", strings.Join(m.state.AllowedDomains, ", ")))
	}
	if m.state.AccountID != "" {
		lines = append(lines, fmt.Sprintf("Account:    %s", m.accountLabel(m.state.AccountID)))
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

func (m *WizardModel) accountLabel(id string) string {
	for _, account := range m.accounts {
		if account.ID == id && account.Label != "" {
			return account.Label
		}
	}
	return id
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

// parseCommandArgs uses commas as an unambiguous terminal-friendly separator.
// Spaces remain part of one argument, so `--title, hello world` maps to two
// arguments without requiring the user to understand shell quoting rules.
func parseCommandArgs(input string) ([]string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}
	parts := strings.Split(input, ",")
	args := make([]string, 0, len(parts))
	for _, part := range parts {
		arg := strings.TrimSpace(part)
		if arg == "" {
			return nil, fmt.Errorf("command arguments cannot contain an empty entry")
		}
		args = append(args, arg)
	}
	return args, nil
}

func commandArgsInput(args []string) string {
	return strings.Join(args, ", ")
}

// parseProtectionRules accepts explicit email addresses and domains. Domains
// may be entered as example.com or @example.com, making the common intent
// clear without making the user remember provider-specific policy syntax.
func parseProtectionRules(input string) ([]string, []string, error) {
	seenEmails := make(map[string]struct{})
	seenDomains := make(map[string]struct{})
	var emails, domains []string
	for _, raw := range strings.Split(input, ",") {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		if strings.Contains(value, "@") && !strings.HasPrefix(value, "@") {
			parsed, err := mail.ParseAddress(value)
			if err != nil || parsed.Address != value {
				return nil, nil, fmt.Errorf("%q is not a valid email address", raw)
			}
			if _, exists := seenEmails[value]; !exists {
				seenEmails[value] = struct{}{}
				emails = append(emails, value)
			}
			continue
		}

		value = strings.TrimPrefix(value, "@")
		if !validProtectionDomain(value) {
			return nil, nil, fmt.Errorf("%q is not a valid domain", raw)
		}
		if _, exists := seenDomains[value]; !exists {
			seenDomains[value] = struct{}{}
			domains = append(domains, value)
		}
	}
	if len(emails) == 0 && len(domains) == 0 {
		return nil, nil, fmt.Errorf("email passcode protection requires at least one allowed email or domain")
	}
	return emails, domains, nil
}

func validProtectionDomain(value string) bool {
	if len(value) == 0 || len(value) > 253 || !strings.Contains(value, ".") || strings.ContainsAny(value, "/:@ ") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func protectionRulesInput(emails, domains []string) string {
	values := append([]string(nil), emails...)
	values = append(values, domains...)
	return strings.Join(values, ", ")
}

func protectionIndex(options []string, selected string) int {
	for i, option := range options {
		if option == selected {
			return i
		}
	}
	return 0
}
