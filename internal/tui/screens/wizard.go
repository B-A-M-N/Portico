package screens

import (
	"context"
	"fmt"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// ConnectionCreator is the subset of the IPC client the wizard needs.
// The root model passes its client through; tests can pass a fake.
type ConnectionCreator interface {
	CreateConnection(ctx context.Context, req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error)
	PlanOpen(ctx context.Context, connID string) (*ipc.PlanDTO, error)
	ApplyPlan(ctx context.Context, planID string) (*ipc.OperationDTO, error)
	GetOperation(ctx context.Context, operationID string) (*ipc.OperationDTO, error)
}

// ConnectionCreatedMsg is delivered when the async create command finishes.
type ConnectionCreatedMsg struct {
	ID         string
	Connection *ipc.ConnectionDTO
	Err        error
}

// WizardPlanLoadedMsg is delivered when the wizard's open plan request completes.
type WizardPlanLoadedMsg struct {
	Plan *ipc.PlanDTO
	Err  error
}

// WizardPlanAppliedMsg is delivered when the wizard's apply command completes.
type WizardPlanAppliedMsg struct {
	Operation *ipc.OperationDTO
	Err       error
}

// WizardOperationLoadedMsg is delivered when the wizard polls operation status.
type WizardOperationLoadedMsg struct {
	Operation *ipc.OperationDTO
	Err       error
}

// WizardModel is the wizard for creating new connections.
type WizardModel struct {
	client         ConnectionCreator
	ctx            context.Context // application lifetime context for IPC calls
	state          WizardState
	selected       int
	input          string
	err            error
	fullCloudflare bool
	accounts       []ipc.ProviderAccountDTO

	// streamConnected reports whether the root model's event stream is live.
	// When it is, operation progress arrives as events and polling is only a
	// slow safety net; when it is not, polling is the sole source of progress.
	streamConnected bool

	// Post-creation flow state
	createdID       string
	plan            *ipc.PlanDTO
	operation       *ipc.OperationDTO
	openAfterCreate bool // true if user chose "Review and open"
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
	DirectoryMode  string
	DirectorySPA   bool
	AllowUpload    bool
	AllowDelete    bool
	MCPCommand     bool
	MCPTransport   string
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
	// WizardStepOutcome asks what the user is trying to achieve before asking
	// how. The wizard previously opened on "What should be reachable?", a
	// source-type question, so a user had to know Portico's internal model
	// before stating their goal.
	WizardStepOutcome int = iota
	WizardStepIntent
	WizardStepName
	WizardStepMCPMode
	WizardStepSource
	WizardStepPort
	WizardStepProtocol
	WizardStepCommandArgs
	WizardStepCommandWorkingDir
	WizardStepDirectoryMode
	WizardStepDirectorySPA
	WizardStepMCPTransport
	WizardStepExposure
	WizardStepHostname
	WizardStepProtection
	WizardStepProtectionRules
	WizardStepProvider
	WizardStepAccount
	WizardStepReview
	WizardStepCreating
	WizardStepCreated       // Profile created, ask user what to do next
	WizardStepPlanPreview   // Show the open plan for approval
	WizardStepApplying      // Plan is being applied
	WizardStepOperationWait // Waiting for operation to complete
	WizardStepComplete
)

// Valid enum values (see internal/core/connection.go).
var (
	wizardSourceKinds    = []string{"existing_service", "directory", "command", "mcp_server"}
	wizardProviders      = []string{"cloudflare"}
	wizardDirectoryModes = []directoryModeChoice{
		{mode: "read", label: "Read-only static site"},
		{mode: "writes", label: "File browser (read only)"},
		{mode: "writes", label: "File browser (allow uploads)", allowUpload: true},
		{mode: "writes", label: "File browser (allow uploads and deletes)", allowUpload: true, allowDelete: true},
	}
)

// wizardRecipe is an outcome the user can pick, together with the answers that
// outcome already determines. Choosing a recipe skips the questions it answers
// rather than asking them again in provider terminology.
type wizardRecipe struct {
	Label string
	// Explanation states, in plain language, what will happen and who will be
	// able to reach the service.
	Explanation string
	SourceType  string
	Exposure    string
	Protection  string
	// Advanced sends the user to the original source-type question instead of
	// presetting anything.
	Advanced bool
	// Unavailable states why an outcome cannot be delivered yet. An outcome
	// Portico cannot honour must say so rather than quietly producing a
	// different one.
	Unavailable string
}

// wizardRecipes are ordered by how commonly they are wanted.
var wizardRecipes = []wizardRecipe{
	{
		Label:       "Share a web app on this computer, temporarily",
		Explanation: "Portico creates a temporary address. Anyone with the link can reach the service while the connection is open, and the address changes each time you open it.",
		SourceType:  "existing_service",
		Exposure:    "temporary_public",
		Protection:  "none",
	},
	{
		Label:       "Publish a web app at a stable address",
		Explanation: "Portico creates a tunnel and a DNS record for a hostname you choose. The address stays the same. You will be asked who should be allowed to reach it.",
		SourceType:  "existing_service",
		Exposure:    "permanent_public",
	},
	{
		Label:       "Share a folder of files",
		Explanation: "Portico serves a directory and publishes it. You choose whether it is read-only.",
		SourceType:  "directory",
	},
	{
		Label:       "Run a command and share what it serves",
		Explanation: "Portico starts a command, waits for it to listen, and publishes it. Portico stops the command when the connection closes.",
		SourceType:  "command",
	},
	{
		Label:       "Connect an MCP server to ChatGPT",
		Explanation: "A local MCP server should reach ChatGPT over a private, client-mediated tunnel rather than a public address.",
		SourceType:  "mcp_server",
		Unavailable: "This uses OpenAI's Secure MCP Tunnel, which is experimental in Portico and off by default. " +
			"Install tunnel-client, create a tunnel in the OpenAI platform, export CONTROL_PLANE_API_KEY, " +
			"and set PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL=1. " +
			"Portico will not publish your MCP server at a public address instead: that would expose it to anyone who finds the URL.",
	},
	{
		Label:       "Something else (choose the source yourself)",
		Explanation: "Pick the kind of source directly and answer every question.",
		Advanced:    true,
	},
}

type directoryModeChoice struct {
	mode        string
	label       string
	allowUpload bool
	allowDelete bool
}

// NewWizard creates a new wizard model.
func NewWizard(client ConnectionCreator, fullCloudflare bool, accounts []ipc.ProviderAccountDTO) *WizardModel {
	return &WizardModel{
		client:         client,
		ctx:            context.Background(), // default; root model should call WithContext
		fullCloudflare: fullCloudflare,
		accounts:       append([]ipc.ProviderAccountDTO(nil), accounts...),
		state:          WizardState{Step: WizardStepOutcome, Provider: wizardProviders[0]},
	}
}

// WithContext sets the application context for IPC calls made by the wizard.
// This ensures commands are cancelled when the TUI exits.
func (m *WizardModel) WithContext(ctx context.Context) *WizardModel {
	if ctx != nil {
		m.ctx = ctx
	}
	return m
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
	if m.state.SourceType == "mcp_server" && m.state.MCPTransport == "sse" {
		if m.fullCloudflare {
			return []string{"permanent_public"}
		}
		return nil
	}
	if m.fullCloudflare {
		return []string{"temporary_public", "permanent_public"}
	}
	return []string{"temporary_public"}
}

func (m *WizardModel) mcpTransports() []string {
	transports := []string{"http", "streamable_http"}
	if m.fullCloudflare {
		transports = append(transports, "sse")
	}
	return transports
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
	return m.state.SourceType == "existing_service" || m.isCommandOrigin()
}

func (m *WizardModel) isCommandOrigin() bool {
	return m.state.SourceType == "command" ||
		(m.state.SourceType == "mcp_server" && m.state.MCPCommand)
}

// HandleKey processes key input for the wizard. It never performs I/O
// itself; any side effect is returned as a tea.Cmd.
func (m *WizardModel) HandleKey(key string) tea.Cmd {
	switch m.state.Step {
	case WizardStepOutcome:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(wizardRecipes)-1 {
				m.selected++
			}
		case "enter":
			recipe := wizardRecipes[m.selected]
			if recipe.Unavailable != "" {
				m.err = fmt.Errorf("%s", recipe.Unavailable)
				return nil
			}
			m.err = nil
			if recipe.Advanced {
				m.state.Step = WizardStepIntent
				m.selected = 0
				return nil
			}
			// Apply everything the outcome already determines so those
			// questions are never asked again.
			m.state.SourceType = recipe.SourceType
			m.state.ExposureMode = recipe.Exposure
			m.state.Protection = recipe.Protection
			m.state.Step = WizardStepName
			m.selected = 0
			m.input = m.state.Name
		}

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
			m.state.Step = WizardStepOutcome
			m.selected = 0
		case "enter":
			if strings.TrimSpace(m.input) == "" {
				m.err = fmt.Errorf("name is required")
				return nil
			}
			m.err = nil
			m.state.Name = strings.TrimSpace(m.input)
			if m.state.SourceType == "mcp_server" {
				m.state.Step = WizardStepMCPMode
				m.selected = boolIndex(m.state.MCPCommand)
			} else {
				m.state.Step = WizardStepSource
				m.input = m.state.SourceAddress
			}
		default:
			m.input = editInput(m.input, key)
		}

	case WizardStepMCPMode:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < 1 {
				m.selected++
			}
		case "enter":
			m.state.MCPCommand = m.selected == 1
			m.state.Step = WizardStepSource
			m.input = m.state.SourceAddress
		case "esc":
			m.state.Step = WizardStepName
			m.input = m.state.Name
		}

	case WizardStepSource:
		switch key {
		case "esc":
			if m.state.SourceType == "mcp_server" {
				m.state.Step = WizardStepMCPMode
				m.selected = boolIndex(m.state.MCPCommand)
			} else {
				m.state.Step = WizardStepName
				m.input = m.state.Name
			}
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
			} else if m.state.SourceType == "directory" {
				m.state.Step = WizardStepDirectoryMode
				m.selected = m.directoryModeIndex()
			} else if m.state.SourceType == "mcp_server" {
				m.state.Step = WizardStepMCPTransport
				m.selected = mcpTransportIndex(m.mcpTransports(), m.state.MCPTransport)
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
			if port == "" && m.isCommandOrigin() {
				m.err = fmt.Errorf("a command source requires a local port")
				return nil
			}
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
			if m.isCommandOrigin() {
				m.state.Step = WizardStepCommandArgs
				m.input = commandArgsInput(m.state.CommandArgs)
			} else if m.state.SourceType == "existing_service" {
				m.state.Step = WizardStepProtocol
				m.selected = 0 // default to HTTP
			} else {
				m.state.Step = WizardStepExposure
				m.selected = 0
			}
		default:
			m.input = editInput(m.input, key)
		}

	case WizardStepProtocol:
		protocols := []string{"http", "https"}
		switch key {
		case "esc":
			m.state.Step = WizardStepPort
			m.input = m.state.Port
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(protocols)-1 {
				m.selected++
			}
		case "enter":
			m.state.SourceProtocol = protocols[m.selected]
			m.state.Step = WizardStepExposure
			m.selected = 0
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
			if m.state.SourceType == "mcp_server" {
				m.state.Step = WizardStepMCPTransport
				m.selected = mcpTransportIndex(m.mcpTransports(), m.state.MCPTransport)
			} else {
				m.state.Step = WizardStepExposure
				m.selected = 0
			}
		default:
			m.input = editInput(m.input, key)
		}

	case WizardStepDirectoryMode:
		choices := m.directoryModeChoices()
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(choices)-1 {
				m.selected++
			}
		case "enter":
			choice := choices[m.selected]
			m.state.DirectoryMode = choice.mode
			m.state.AllowUpload = choice.allowUpload
			m.state.AllowDelete = choice.allowDelete
			if choice.mode == "read" {
				m.state.Step = WizardStepDirectorySPA
				m.selected = boolIndex(m.state.DirectorySPA)
			} else {
				m.state.DirectorySPA = false
				m.state.Step = WizardStepExposure
				m.selected = 0
			}
		case "esc":
			m.state.Step = WizardStepSource
			m.input = m.state.SourceAddress
		}

	case WizardStepDirectorySPA:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < 1 {
				m.selected++
			}
		case "enter":
			m.state.DirectorySPA = m.selected == 1
			m.state.Step = WizardStepExposure
			m.selected = 0
		case "esc":
			m.state.Step = WizardStepDirectoryMode
			m.selected = m.directoryModeIndex()
		}

	case WizardStepMCPTransport:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(m.mcpTransports())-1 {
				m.selected++
			}
		case "enter":
			m.state.MCPTransport = m.mcpTransports()[m.selected]
			m.state.Step = WizardStepExposure
			m.selected = 0
		case "esc":
			m.state.Step = WizardStepSource
			m.input = m.state.SourceAddress
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
			if m.isCommandOrigin() {
				m.state.Step = WizardStepCommandWorkingDir
				m.input = m.state.WorkingDir
			} else if m.state.SourceType == "directory" {
				if m.state.DirectoryMode == "read" {
					m.state.Step = WizardStepDirectorySPA
					m.selected = boolIndex(m.state.DirectorySPA)
				} else {
					m.state.Step = WizardStepDirectoryMode
					m.selected = m.directoryModeIndex()
				}
			} else if m.state.SourceType == "mcp_server" {
				m.state.Step = WizardStepMCPTransport
				m.selected = mcpTransportIndex(m.mcpTransports(), m.state.MCPTransport)
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
			// Two outcomes: save closed (default) or open (next step)
			m.openAfterCreate = (m.selected == 1)
			m.state.Step = WizardStepCreating
			return createConnectionCmd(m.client, m.ctx, m.buildRequest())
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < 1 {
				m.selected++
			}
		case "esc":
			m.state.Step = WizardStepProvider
			m.selected = 0
		}

	case WizardStepPlanPreview:
		switch key {
		case "enter":
			// Apply the plan
			if m.plan != nil {
				m.state.Step = WizardStepApplying
				return m.applyPlanCmd()
			}
		case "esc":
			// Cancel — go to complete (connection is saved closed)
			m.state.Step = WizardStepComplete
		}

	case WizardStepApplying:
		// No input while applying — wait for the operation to start
		return nil

	case WizardStepOperationWait:
		switch key {
		case "esc":
			// User can bail out of waiting — connection may still be opening
			m.state.Step = WizardStepComplete
		}
	}

	return nil
}

// HandleCreated applies the result of the async create command.
func (m *WizardModel) HandleCreated(msg ConnectionCreatedMsg) tea.Cmd {
	if msg.Err != nil {
		m.err = msg.Err
		m.state.Step = WizardStepReview
		return nil
	}
	m.err = nil
	m.createdID = msg.ID

	if m.openAfterCreate {
		// Transition to plan preview — request the open plan asynchronously.
		m.state.Step = WizardStepPlanPreview
		return m.requestPlanCmd()
	}

	// Save closed — go directly to complete.
	m.state.Step = WizardStepComplete
	return nil
}

// HandlePlanLoaded applies the result of the plan request.
func (m *WizardModel) HandlePlanLoaded(msg WizardPlanLoadedMsg) tea.Cmd {
	if msg.Err != nil {
		m.err = msg.Err
		m.state.Step = WizardStepComplete
		return nil
	}
	m.plan = msg.Plan
	m.err = nil
	if msg.Plan != nil && msg.Plan.Noop {
		// Already open — skip apply
		m.state.Step = WizardStepComplete
		return nil
	}
	// Stay on plan preview — user must approve
	return nil
}

// HandlePlanApplied applies the result of the apply command.
func (m *WizardModel) HandlePlanApplied(msg WizardPlanAppliedMsg) tea.Cmd {
	if msg.Err != nil {
		m.err = msg.Err
		m.state.Step = WizardStepComplete
		return nil
	}
	m.operation = msg.Operation
	m.err = nil
	m.state.Step = WizardStepOperationWait
	// Start polling the operation
	return m.pollOperationCmd()
}

// HandleOperationLoaded applies the result of the operation poll.
func (m *WizardModel) HandleOperationLoaded(msg WizardOperationLoadedMsg) tea.Cmd {
	if msg.Err != nil {
		m.err = msg.Err
		m.state.Step = WizardStepComplete
		return nil
	}
	m.operation = msg.Operation

	// Check if the operation reached a terminal state
	if msg.Operation != nil {
		switch msg.Operation.State {
		case "succeeded", "failed", "cancelled":
			m.state.Step = WizardStepComplete
			return nil
		}
	}

	// Still running — schedule another poll
	return m.pollOperationCmd()
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
		src.Directory = &ipc.DirectorySourceDTO{
			Path:        s.SourceAddress,
			Mode:        s.DirectoryMode,
			SPAFallback: s.DirectorySPA,
			AllowUpload: s.AllowUpload,
			AllowDelete: s.AllowDelete,
		}
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
		transport := s.MCPTransport
		if transport == "" {
			transport = "http"
		}
		src.MCP = &ipc.MCPSourceDTO{Transport: transport}
		if s.MCPCommand {
			port, _ := strconv.Atoi(s.Port)
			src.MCP.Command = &ipc.CommandSourceDTO{
				Executable: s.SourceAddress,
				Args:       append([]string(nil), s.CommandArgs...),
				WorkingDir: s.WorkingDir,
				Port:       port,
				Protocol:   "http",
			}
		} else {
			src.MCP.Endpoint = s.SourceAddress
		}
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
		Lifecycle: ipc.LifecycleDTO{
			AutoStart:    true,
			OnDisconnect: "keep_alive",
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
func createConnectionCmd(client ConnectionCreator, ctx context.Context, req ipc.CreateConnectionRequest) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return ConnectionCreatedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		conn, err := client.CreateConnection(ctx, req)
		if err != nil {
			return ConnectionCreatedMsg{Err: err}
		}
		return ConnectionCreatedMsg{ID: conn.ID, Connection: conn}
	}
}

// requestPlanCmd returns a command that requests an open plan for the created connection.
func (m *WizardModel) requestPlanCmd() tea.Cmd {
	client := m.client
	connID := m.createdID
	ctx := m.ctx
	return func() tea.Msg {
		if client == nil {
			return WizardPlanLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		plan, err := client.PlanOpen(ctx, connID)
		if err != nil {
			return WizardPlanLoadedMsg{Err: err}
		}
		return WizardPlanLoadedMsg{Plan: plan}
	}
}

// applyPlanCmd returns a command that applies the current plan.
func (m *WizardModel) applyPlanCmd() tea.Cmd {
	client := m.client
	planID := ""
	if m.plan != nil {
		planID = m.plan.ID
	}
	ctx := m.ctx
	return func() tea.Msg {
		if client == nil {
			return WizardPlanAppliedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		if planID == "" {
			return WizardPlanAppliedMsg{Err: fmt.Errorf("no plan to apply")}
		}
		op, err := client.ApplyPlan(ctx, planID)
		if err != nil {
			return WizardPlanAppliedMsg{Err: err}
		}
		return WizardPlanAppliedMsg{Operation: op}
	}
}

// SetStreamConnected tells the wizard whether operation events are arriving.
func (m *WizardModel) SetStreamConnected(connected bool) {
	m.streamConnected = connected
}

// OperationID returns the operation the wizard is currently tracking, so the
// root model can route matching events to it.
func (m *WizardModel) OperationID() string {
	if m.operation == nil {
		return ""
	}
	return m.operation.ID
}

// RefreshOperationCmd fetches the authoritative operation state immediately.
// It is issued in response to an operation event rather than on a timer.
func (m *WizardModel) RefreshOperationCmd() tea.Cmd {
	return m.operationFetchCmd(0)
}

// pollOperationCmd is the fallback path. While the event stream is connected it
// backs off to a slow safety poll, because progress arrives as events; when the
// stream is down it polls at the original interval so a disconnected wizard
// still makes progress.
func (m *WizardModel) pollOperationCmd() tea.Cmd {
	delay := 750 * time.Millisecond
	if m.streamConnected {
		delay = 5 * time.Second
	}
	return m.operationFetchCmd(delay)
}

func (m *WizardModel) operationFetchCmd(delay time.Duration) tea.Cmd {
	client := m.client
	opID := ""
	if m.operation != nil {
		opID = m.operation.ID
	}
	ctx := m.ctx
	return func() tea.Msg {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return WizardOperationLoadedMsg{Err: ctx.Err()}
			}
		}
		select {
		case <-ctx.Done():
			return WizardOperationLoadedMsg{Err: ctx.Err()}
		default:
		}
		if client == nil {
			return WizardOperationLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		if opID == "" {
			return WizardOperationLoadedMsg{Err: fmt.Errorf("no operation to poll")}
		}
		op, err := client.GetOperation(ctx, opID)
		if err != nil {
			return WizardOperationLoadedMsg{Err: err}
		}
		return WizardOperationLoadedMsg{Operation: op}
	}
}

// editInput applies a single key press to a text input value.
//
// Backspace removes one whole rune. Slicing a byte off the end split multi-byte
// code points, leaving invalid UTF-8 in the field for accented Latin, Arabic,
// CJK and emoji input.
func editInput(value, key string) string {
	switch key {
	case "backspace":
		if value == "" {
			return value
		}
		_, size := utf8.DecodeLastRuneInString(value)
		return value[:len(value)-size]
	case "space":
		return value + " "
	default:
		if utf8.RuneCountInString(key) == 1 {
			r, _ := utf8.DecodeRuneInString(key)
			if r == utf8.RuneError || r < 0x20 || r == 0x7f {
				return value
			}
			return value + key
		}
		return value
	}
}

// View renders the wizard screen.
func (m *WizardModel) View() string {
	switch m.state.Step {
	case WizardStepOutcome:
		return m.withError(m.renderOutcome())
	case WizardStepIntent:
		return m.renderIntent()
	case WizardStepName:
		return m.withError(renderInput("Name this connection:", m.input))
	case WizardStepMCPMode:
		return renderMenu("How does the MCP server run?", []string{"Already running at an HTTP endpoint", "A command Portico should run"}, m.selected)
	case WizardStepSource:
		return m.withError(renderInput(m.sourcePrompt(), m.input))
	case WizardStepPort:
		if m.isCommandOrigin() {
			return m.withError(renderInput("Local port for the command (required):", m.input))
		}
		return m.withError(renderInput("Local port (empty to skip):", m.input))
	case WizardStepProtocol:
		return m.renderProtocol()
	case WizardStepCommandArgs:
		return m.withError(renderInput(
			"Command arguments (space separated; quote any argument containing spaces; empty to skip):\n"+
				renderArgvPreviewForInput(m.state.SourceAddress, m.input), m.input))
	case WizardStepCommandWorkingDir:
		return m.withError(renderInput("Working directory (empty to use Portico's):", m.input))
	case WizardStepDirectoryMode:
		return m.renderDirectoryMode()
	case WizardStepDirectorySPA:
		return renderMenu("Enable SPA fallback for unknown paths?", []string{"No", "Yes"}, m.selected)
	case WizardStepMCPTransport:
		return m.renderMCPTransport()
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
	case WizardStepPlanPreview:
		return m.renderPlanPreview()
	case WizardStepApplying:
		return "Applying plan..."
	case WizardStepOperationWait:
		return m.renderOperationWait()
	case WizardStepComplete:
		return m.renderComplete()
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
		if m.state.MCPCommand {
			return "Enter the MCP command executable to run:"
		}
		return "Enter the MCP server endpoint:"
	default:
		return "Enter the address of your service (host or host:port):"
	}
}

func (m *WizardModel) renderOutcome() string {
	options := make([]string, 0, len(wizardRecipes))
	for _, recipe := range wizardRecipes {
		label := recipe.Label
		if recipe.Unavailable != "" {
			label += "  (not available yet)"
		}
		options = append(options, label)
	}
	view := renderMenu("What are you trying to do?", options, m.selected)

	// Show the consequence of the highlighted choice before it is made.
	if m.selected >= 0 && m.selected < len(wizardRecipes) {
		view += "\n\n" + wizardRecipes[m.selected].Explanation + "\n"
	}
	return view
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

func (m *WizardModel) renderProtocol() string {
	options := []string{"HTTP", "HTTPS"}
	return renderMenu("What protocol does your service use?", options, m.selected)
}

func (m *WizardModel) renderExposure() string {
	options := make([]string, 0, len(m.exposures()))
	for _, exposure := range m.exposures() {
		switch exposure {
		case "temporary_public":
			options = append(options, "Temporarily, with a generated address")
		case "permanent_public":
			options = append(options, "Permanently, with my own hostname")
		}
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

func (m *WizardModel) renderDirectoryMode() string {
	options := make([]string, 0, len(m.directoryModeChoices()))
	for _, choice := range m.directoryModeChoices() {
		options = append(options, choice.label)
	}
	return renderMenu("How should Portico serve this directory?", options, m.selected)
}

// directoryModeChoices returns the directory mode options filtered by
// provider capabilities. Without a configured Cloudflare account (Quick
// Tunnel only), write-enabled modes are not viable because core validation
// rejects uploads/deletes without protection, and protection requires a
// permanent hostname.
func (m *WizardModel) directoryModeChoices() []directoryModeChoice {
	if !m.fullCloudflare {
		// Quick Tunnel only — offer read-only modes.
		return []directoryModeChoice{
			{mode: "read", label: "Read-only static site"},
			{mode: "writes", label: "File browser (read only)"},
		}
	}
	return wizardDirectoryModes
}

func (m *WizardModel) renderMCPTransport() string {
	options := make([]string, 0, len(m.mcpTransports()))
	for _, transport := range m.mcpTransports() {
		switch transport {
		case "http":
			options = append(options, "HTTP")
		case "streamable_http":
			options = append(options, "Streamable HTTP")
		case "sse":
			options = append(options, "Server-Sent Events (requires permanent exposure)")
		}
	}
	return renderMenu("Which MCP transport does the server use?", options, m.selected)
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
	if m.state.SourceType == "directory" {
		lines = append(lines, fmt.Sprintf("Directory:   %s", directoryModeSummary(m.state)))
		if m.state.DirectorySPA {
			lines = append(lines, "SPA fallback: enabled")
		}
	}
	if m.state.SourceType == "mcp_server" && m.state.MCPTransport != "" {
		mode := "endpoint"
		if m.state.MCPCommand {
			mode = "command"
		}
		lines = append(lines, fmt.Sprintf("MCP mode:      %s", mode))
		lines = append(lines, fmt.Sprintf("MCP transport: %s", m.state.MCPTransport))
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
		fmt.Sprintf("Lifecycle:  auto-start=%v, on-disconnect=%s", true, "keep_alive"),
		"",
		"What should Portico do?",
	)

	// Two explicit outcomes
	options := []string{
		"Save connection (closed — you can open it later)",
		"Save and open connection (review plan first)",
	}
	lines = append(lines, renderMenu("", options, m.selected))

	if m.err != nil {
		lines = append(lines, "", "Error: "+m.err.Error())
	}
	lines = append(lines, "", "Enter Confirm  Esc Back")
	return strings.Join(lines, "\n")
}

func (m *WizardModel) renderPlanPreview() string {
	lines := []string{"OPEN CONNECTION PLAN", ""}

	if m.plan == nil {
		lines = append(lines, "Loading plan...")
		return strings.Join(lines, "\n")
	}

	lines = append(lines, fmt.Sprintf("Intent: %s", m.plan.Intent))
	lines = append(lines, fmt.Sprintf("Provider: %s", m.plan.Provider))
	lines = append(lines, "")
	lines = append(lines, "Steps:")
	for i, step := range m.plan.Steps {
		prefix := "  "
		if step.Destructive {
			prefix = "  [DESTRUCTIVE] "
		}
		lines = append(lines, fmt.Sprintf("%s%d. %s", prefix, i+1, step.Summary))
	}

	if len(m.plan.Warnings) > 0 {
		lines = append(lines, "", "Warnings:")
		for _, w := range m.plan.Warnings {
			lines = append(lines, "  - "+w)
		}
	}

	if m.err != nil {
		lines = append(lines, "", "Error: "+m.err.Error())
	}
	lines = append(lines, "", "Enter Apply plan  Esc Cancel")
	return strings.Join(lines, "\n")
}

func (m *WizardModel) renderOperationWait() string {
	lines := []string{"OPENING CONNECTION", ""}

	if m.operation == nil {
		lines = append(lines, "Waiting for operation to start...")
		return strings.Join(lines, "\n")
	}

	lines = append(lines, fmt.Sprintf("State: %s", m.operation.State))

	if len(m.operation.Steps) > 0 {
		lines = append(lines, "", "Progress:")
		for _, step := range m.operation.Steps {
			lines = append(lines, "  - "+step.Summary)
		}
	}

	if m.err != nil {
		lines = append(lines, "", "Error: "+m.err.Error())
	}
	lines = append(lines, "", "Esc Return to home (connection may still be opening)")
	return strings.Join(lines, "\n")
}

func (m *WizardModel) renderComplete() string {
	lines := []string{"CONNECTION CREATED", ""}

	if m.openAfterCreate && m.operation != nil {
		switch m.operation.State {
		case "succeeded":
			lines = append(lines, "Connection opened successfully!")
		case "failed":
			lines = append(lines, "Connection created but opening failed.")
			if m.err != nil {
				lines = append(lines, "", "Error: "+m.err.Error())
			}
		case "cancelled":
			lines = append(lines, "Connection created but opening was cancelled.")
		default:
			lines = append(lines, "Connection created. Opening is still in progress.")
		}
	} else if m.openAfterCreate && m.plan != nil && m.plan.Noop {
		lines = append(lines, "Connection was already open.")
	} else {
		lines = append(lines, "Connection saved (closed).")
		lines = append(lines, "Use Space on the home screen to open it when ready.")
	}

	lines = append(lines, "", "Enter Return home")
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

// parseCommandArgs splits an argument line using familiar quoting rules.
//
// Commas used to be the separator, which made an argument containing a comma
// impossible to express: "Example, Inc." became two arguments with no way to
// escape it. Quoting is what users already expect from a terminal, and it can
// represent every argument value.
//
// This parses the line into an argv slice only. It never implies shell
// execution: whether the command runs through a shell is a separate explicit
// choice, so quoting here does not silently change how the command is run.
func parseCommandArgs(input string) ([]string, error) {
	var (
		args    []string
		current strings.Builder
		quote   rune
		escaped bool
		started bool
	)

	for _, r := range input {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			// A backslash is literal inside single quotes, as in POSIX shells.
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			// An opening quote starts an argument even if it is empty, so ""
			// yields one empty argument rather than none.
			started = true
		case r == ' ' || r == '\t':
			if started {
				args = append(args, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(r)
			started = true
		}
	}

	if escaped {
		return nil, fmt.Errorf("command arguments end with a trailing backslash")
	}
	if quote != 0 {
		return nil, fmt.Errorf("command arguments have an unclosed %c quote", quote)
	}
	if started {
		args = append(args, current.String())
	}
	return args, nil
}

// commandArgsInput renders an argv slice back into an editable line, quoting
// any argument that would not survive a round trip unquoted.
func commandArgsInput(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, quoteCommandArg(arg))
	}
	return strings.Join(parts, " ")
}

func quoteCommandArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t'\"\\") {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range arg {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// renderArgvPreviewForInput previews the argv that the current input line
// parses to, so quoting mistakes are visible while typing rather than after the
// command has been created.
func renderArgvPreviewForInput(executable, input string) string {
	args, err := parseCommandArgs(input)
	if err != nil {
		return "\n" + err.Error() + "\n"
	}
	return "\n" + renderArgvPreview(executable, args)
}

// renderArgvPreview shows the exact argument vector that will be executed, one
// argument per line, so quoting mistakes are visible before the command runs.
func renderArgvPreview(executable string, args []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Executable: %s\n", executable)
	if len(args) == 0 {
		b.WriteString("Arguments: none\n")
		return b.String()
	}
	b.WriteString("Arguments:\n")
	for i, arg := range args {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, arg)
	}
	return b.String()
}

func (m *WizardModel) directoryModeIndex() int {
	for i, choice := range m.directoryModeChoices() {
		if choice.mode == m.state.DirectoryMode && choice.allowUpload == m.state.AllowUpload && choice.allowDelete == m.state.AllowDelete {
			return i
		}
	}
	return 0
}

func directoryModeSummary(state WizardState) string {
	if state.DirectoryMode == "read" {
		return "read-only static site"
	}
	if state.AllowDelete {
		return "file browser with uploads and deletes"
	}
	if state.AllowUpload {
		return "file browser with uploads"
	}
	return "read-only file browser"
}

func mcpTransportIndex(options []string, selected string) int {
	for i, option := range options {
		if option == selected {
			return i
		}
	}
	return 0
}

func boolIndex(value bool) int {
	if value {
		return 1
	}
	return 0
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
