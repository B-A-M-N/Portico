package screens

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// ConnectionCreator is the subset of the IPC client the wizard needs.
// The root model passes its client through; tests can pass a fake.
type ConnectionCreator interface {
	CreateConnection(ctx context.Context, req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error)
	// RecommendProvider evaluates the stated requirements against every
	// provider, so the wizard presents a decision with reasons rather than a
	// list with no guidance.
	RecommendProvider(ctx context.Context, req ipc.ProviderRecommendationRequest) (
		*ipc.ProviderRecommendationResponse, error)
	PlanOpen(ctx context.Context, connID string) (*ipc.PlanDTO, error)
	ApplyPlan(ctx context.Context, planID string) (*ipc.OperationDTO, error)
	ApplyPlanWithIdempotency(ctx context.Context, planID, idempotencyKey string) (*ipc.OperationDTO, error)
	GetOperation(ctx context.Context, operationID string) (*ipc.OperationDTO, error)
}

// ConnectionCreatedMsg is delivered when the async create command finishes.
type ConnectionCreatedMsg struct {
	WizardID   WizardID
	Generation WizardGeneration
	ID         string
	Connection *ipc.ConnectionDTO
	Err        error
}

// WizardID uniquely identifies a wizard instance. Async operations carry this ID
// so that late replies from an abandoned wizard can be discarded.
type WizardID uint64

// WizardGeneration tracks the generation of a wizard for request correlation.
type WizardGeneration uint64

// WizardPlanLoadedMsg is delivered when the wizard's open plan request completes.
type WizardPlanLoadedMsg struct {
	WizardID   WizardID
	Generation WizardGeneration
	Plan       *ipc.PlanDTO
	Err        error
}

// WizardPlanAppliedMsg is delivered when the wizard's apply command completes.
type WizardPlanAppliedMsg struct {
	WizardID   WizardID
	Generation WizardGeneration
	Operation  *ipc.OperationDTO
	Err        error
}

// WizardOperationLoadedMsg is delivered when the wizard polls operation status.
type WizardOperationLoadedMsg struct {
	WizardID   WizardID
	Generation WizardGeneration
	Operation  *ipc.OperationDTO
	Err        error
}

// WizardModel is the wizard for creating new connections.
type WizardModel struct {
	id         WizardID         // unique ID for this wizard instance
	generation WizardGeneration // current generation for request correlation
	client     ConnectionCreator
	ctx        context.Context // application lifetime context for IPC calls
	state      WizardState
	selected   int
	// field is the text entry for whichever question is currently asked. It
	// replaced a plain string that could only append and truncate.
	field textinput.Model
	err   error
	// caps derives every menu from what providers declare, replacing a single
	// "is Cloudflare configured" boolean that decided what the user was shown.
	caps providerCapabilities

	// Recommendation state. The fingerprint records what the in-flight request
	// was built from, so an answer arriving after the user has changed
	// something is recognised as stale and dropped rather than describing a
	// connection they are no longer creating.
	recommendation       *ipc.ProviderRecommendationResponse
	recommendFingerprint string
	recommendPending     bool
	recommendErr         error
	// preferredProvider remembers a choice made before the providers changed,
	// so a provider that still fits is returned to rather than replaced.
	preferredProvider string
	// setupProviderID is a provider the user asked to configure from the
	// provider question. The root model owns provider setup — it is the only
	// component holding the IPC client for it — so the wizard records the
	// request and the root model performs it, returning here afterwards with
	// every answer intact.
	setupProviderID string

	// Discovery, for the question that asks which running service to publish.
	// discovered is nil until a scan has been attempted, which is what
	// distinguishes "not asked yet" from "asked and found nothing".
	discovered      []ipc.DiscoveredServiceDTO
	discoverPending bool
	discoverErr     error

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
	// AutoStart and OnDisconnect are the lifecycle decisions. They were
	// hardcoded in buildRequest, so every connection opened by itself at
	// supervisor startup and outlived the interface without being asked.
	AutoStart    bool
	OnDisconnect string
	// Advanced records that the user chose to describe the connection
	// themselves rather than picking a prepared outcome, which is what decides
	// whether the intent question belongs in the sequence.
	Advanced bool
	// ConnectionKind selects the kind of connection to create.
	// Defaults to service_exposure for backward compatibility.
	ConnectionKind string
	// PortForward fields are used when ConnectionKind is "port_forward".
	PortForwardLocalPort  string
	PortForwardRemoteHost string
	PortForwardRemotePort string
	PortForwardProtocol   string
	// TunnelID and TunnelProfile are used when ConnectionKind is
	// "client_tunnel". The tunnel already exists: Portico manages a client
	// against a tunnel created in the platform's own settings, and cannot create
	// one — so the ID is collected rather than generated.
	TunnelID      string
	TunnelProfile string
	// CommandUseShell and CommandEnv configure a command Portico runs. Both are
	// carried by the source DTO, honoured by the origin manager and exposed by the
	// CLI; the wizard could set neither, so a command needing an API key or a
	// shell pipeline could not be created from the interface.
	CommandUseShell bool
	CommandEnv      map[string]string
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
	// WizardStepDiscovery asks which already-running service to publish, from a
	// scan, rather than requiring the user to have found an address elsewhere
	// first.
	WizardStepDiscovery
	WizardStepSource
	WizardStepPort
	WizardStepProtocol
	WizardStepCommandArgs
	WizardStepCommandWorkingDir
	// WizardStepCommandShell asks whether the command runs through a shell, and
	// WizardStepCommandEnv collects its environment.
	WizardStepCommandShell
	WizardStepCommandEnv
	WizardStepDirectoryMode
	WizardStepDirectorySPA
	WizardStepMCPTransport
	WizardStepExposure
	WizardStepHostname
	WizardStepProtection
	WizardStepProtectionRules
	WizardStepProvider
	WizardStepAccount
	// WizardStepPortForwardLocalPort asks for the local listening port.
	WizardStepPortForwardLocalPort
	// WizardStepPortForwardRemoteHost asks for the remote host.
	WizardStepPortForwardRemoteHost
	// WizardStepPortForwardRemotePort asks for the remote port.
	WizardStepPortForwardRemotePort
	// WizardStepPortForwardProtocol asks for the protocol (TCP/UDP).
	WizardStepPortForwardProtocol
	// The client-tunnel questions. They adopt a tunnel that already exists: its
	// ID, the local MCP server it should reach, and the client profile to run.
	WizardStepTunnelID
	WizardStepTunnelMCP
	WizardStepTunnelProfile
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
	// RequiresProvider names a provider this outcome cannot be delivered without.
	// Whether it is present is read from the snapshot, so the refusal is a fact
	// about the machine rather than a string written here — which would be wrong
	// the moment the provider became available.
	RequiresProvider string
	// ConnectionKind selects the connection kind to create.
	ConnectionKind string
}

// wizardRecipes are ordered by how commonly they are wanted.
var wizardRecipes = []wizardRecipe{
	{
		Label:          "Share a web app on this computer, temporarily",
		Explanation:    "Portico creates a temporary address. Anyone with the link can reach the service while the connection is open, and the address changes each time you open it.",
		SourceType:     "existing_service",
		Exposure:       "temporary_public",
		Protection:     "none",
		ConnectionKind: "service_exposure",
	},
	{
		Label:          "Publish a web app at a stable address",
		Explanation:    "Portico creates a tunnel and a DNS record for a hostname you choose. The address stays the same. You will be asked who should be allowed to reach it.",
		SourceType:     "existing_service",
		Exposure:       "permanent_public",
		ConnectionKind: "service_exposure",
	},
	{
		Label:          "Share a folder of files",
		Explanation:    "Portico serves a directory and publishes it. You choose whether it is read-only.",
		SourceType:     "directory",
		ConnectionKind: "service_exposure",
	},
	{
		Label:          "Run a command and share what it serves",
		Explanation:    "Portico starts a command, waits for it to listen, and publishes it. Portico stops the command when the connection closes.",
		SourceType:     "command",
		ConnectionKind: "service_exposure",
	},
	{
		Label:          "Forward a local port",
		Explanation:    "Portico forwards a local port to a remote host and port. No public address, no DNS, no access protection — just a TCP forward.",
		ConnectionKind: "port_forward",
	},
	{
		Label: "Connect an MCP server to ChatGPT",
		Explanation: "A local MCP server should reach ChatGPT over a private, client-mediated tunnel " +
			"rather than a public address. Portico manages a tunnel you have already created in the " +
			"OpenAI platform; it does not create one.",
		SourceType: "mcp_server",
		// Availability is not stated here. Whether this can be delivered is a fact
		// about the machine — whether the provider is registered, which its own
		// definition decides — and a hardcoded refusal here was a second answer to
		// that question, wrong the moment the provider became available.
		RequiresProvider: "openai_tunnel",
		ConnectionKind:   "client_tunnel",
	},
	{
		Label:          "Something else (choose the source yourself)",
		Explanation:    "Pick the kind of source directly and answer every question.",
		Advanced:       true,
		ConnectionKind: "service_exposure",
	},
}

type directoryModeChoice struct {
	mode        string
	label       string
	allowUpload bool
	allowDelete bool
}

// nextWizardID generates a unique wizard ID.
var nextWizardID WizardID

// NewWizard creates a new wizard model.
func NewWizard(client ConnectionCreator, providers []ipc.ProviderDTO) *WizardModel {
	nextWizardID++
	return &WizardModel{
		id:         nextWizardID,
		generation: 0,
		client:     client,
		ctx:        context.Background(), // default; root model should call WithContext
		caps:       providerCapabilities{providers: providers},
		state:      WizardState{Step: WizardStepOutcome},
		field:      NewField(),
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
func NewWizardForService(client ConnectionCreator, providers []ipc.ProviderDTO, address, protocol string) *WizardModel {
	m := NewWizard(client, providers)
	m.state.ConnectionKind = "service_exposure"
	m.state.SourceType = "existing_service"
	m.state.SourceAddress = address
	m.state.SourceProtocol = protocol
	m.state.Step = WizardStepName
	return m
}

// NewWizardForManualAddress starts the wizard for an existing local service
// whose address the user will type.
//
// Discovery finding nothing was a dead end: the screen reported that nothing was
// found and offered no way on. A service on a port the scan cannot see is still
// a service, and this is the path to publishing it.
func NewWizardForManualAddress(client ConnectionCreator, providers []ipc.ProviderDTO) *WizardModel {
	m := NewWizard(client, providers)
	m.state.ConnectionKind = "service_exposure"
	m.state.SourceType = "existing_service"
	m.state.Step = WizardStepName
	return m
}

// exposureChoices lists every way the connection can be reachable, including
// the ones that are not available yet and why.
func (m *WizardModel) exposureChoices() []wizardChoice {
	return m.caps.exposureChoices(m.state.SourceType, m.state.MCPTransport)
}

// exposures returns the values that can currently be picked.
func (m *WizardModel) exposures() []string {
	return availableValues(m.exposureChoices())
}

func (m *WizardModel) mcpTransports() []string {
	transports := []string{"http", "streamable_http"}
	// SSE needs an address that does not change, so it is offered only when
	// some provider can supply one.
	if m.caps.hasCapability(supportsCustomHostname) {
		transports = append(transports, "sse")
	}
	return transports
}

// protectionChoices lists who may reach the connection, including options that
// need something first.
func (m *WizardModel) protectionChoices() []wizardChoice {
	return m.caps.protectionChoices(m.state.ExposureMode)
}

// protections returns the values that can currently be picked.
func (m *WizardModel) protections() []string {
	return availableValues(m.protectionChoices())
}

// Step returns the current wizard step.
func (m *WizardModel) Step() int { return m.state.Step }

// SelectedIndex exposes the cursor for tests that drive the wizard through the
// root model rather than reaching into its state.
func (m *WizardModel) SelectedIndex() int { return m.selected }

// WizardRecipeCount reports how many prepared outcomes exist.
func WizardRecipeCount() int { return len(wizardRecipes) }

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
			if reason := m.recipeUnavailable(recipe); reason != "" {
				m.err = fmt.Errorf("%s", reason)
				return nil
			}
			m.err = nil
			// Set from the recipe every time, not only when advanced. Leaving
			// it true after switching to a prepared outcome put the intent
			// question back in the sequence, so going back from the name
			// question landed on a question that path never asked.
			m.state.Advanced = recipe.Advanced
			// Set the connection kind from the recipe.
			m.state.ConnectionKind = recipe.ConnectionKind
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
			// Route based on connection kind.
			switch recipe.ConnectionKind {
			case "port_forward":
				m.state.Step = WizardStepName
				m.selected = 0
				m.setInput(m.state.Name)
			default:
				m.state.Step = WizardStepName
				m.selected = 0
				m.setInput(m.state.Name)
			}
		}

	case WizardStepPortForwardLocalPort:
		switch key {
		case "esc":
			m.goBack()
		case "enter":
			port := strings.TrimSpace(m.inputValue())
			if port == "" {
				m.err = fmt.Errorf("local port is required")
				return nil
			}
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				m.err = fmt.Errorf("port must be a number between 1 and 65535")
				return nil
			}
			m.err = nil
			m.state.PortForwardLocalPort = port
			m.state.Step = WizardStepPortForwardRemoteHost
			m.setInput(m.state.PortForwardRemoteHost)
		default:
			m.setInput(editInput(m.inputValue(), key))
		}

	case WizardStepPortForwardRemoteHost:
		switch key {
		case "esc":
			m.goBack()
		case "enter":
			host := strings.TrimSpace(m.inputValue())
			if host == "" {
				m.err = fmt.Errorf("remote host is required")
				return nil
			}
			m.err = nil
			m.state.PortForwardRemoteHost = host
			m.state.Step = WizardStepPortForwardRemotePort
			m.setInput(m.state.PortForwardRemotePort)
		default:
			m.setInput(editInput(m.inputValue(), key))
		}

	case WizardStepPortForwardRemotePort:
		switch key {
		case "esc":
			m.goBack()
		case "enter":
			port := strings.TrimSpace(m.inputValue())
			if port == "" {
				m.err = fmt.Errorf("remote port is required")
				return nil
			}
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				m.err = fmt.Errorf("port must be a number between 1 and 65535")
				return nil
			}
			m.err = nil
			m.state.PortForwardRemotePort = port
			m.state.Step = WizardStepPortForwardProtocol
			m.selected = 0
		default:
			m.setInput(editInput(m.inputValue(), key))
		}

	case WizardStepPortForwardProtocol:
		choices := portForwardProtocolChoices()
		switch key {
		case "esc":
			m.goBack()
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(choices)-1 {
				m.selected++
			}
		case "enter":
			choice, ok := choiceAt(choices, m.selected)
			if !ok {
				return nil
			}
			if !choice.Available {
				m.err = fmt.Errorf("%s: %s", choice.Label, choice.Reason)
				return nil
			}
			m.err = nil
			m.state.PortForwardProtocol = choice.Value
			m.state.Provider = "portforward"
			m.state.Step = WizardStepReview
			m.selected = 0
		}

	case WizardStepIntent:
		switch key {
		case "esc":
			m.goBack()
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
			m.setInput(m.state.Name)
		}

	case WizardStepName:
		switch key {
		case "esc":
			m.goBack()
		case "enter":
			if strings.TrimSpace(m.inputValue()) == "" {
				m.err = fmt.Errorf("name is required")
				return nil
			}
			m.err = nil
			m.state.Name = strings.TrimSpace(m.inputValue())
			// Route based on connection kind.
			switch m.state.ConnectionKind {
			case "port_forward":
				m.state.Step = WizardStepPortForwardLocalPort
				m.setInput(m.state.PortForwardLocalPort)
			case "client_tunnel":
				// The tunnel already exists, so the first question is which one.
				m.state.Step = WizardStepTunnelID
				m.setInput(m.state.TunnelID)
			default: // service_exposure
				switch {
				case m.state.SourceType == "mcp_server":
					m.state.Step = WizardStepMCPMode
					m.selected = boolIndex(m.state.MCPCommand)
				case m.state.SourceType == "existing_service" && m.state.SourceAddress == "":
					// Publishing something already running: Portico looks for it
					// rather than asking the user to go and find its address.
					// An address already known — from the Home discovery screen —
					// skips the question.
					m.state.Step = WizardStepDiscovery
					m.selected = 0
					return m.ensureDiscoveryStarted()
				default:
					m.state.Step = WizardStepSource
					m.setInput(m.state.SourceAddress)
				}
			}
		default:
			m.setInput(editInput(m.inputValue(), key))
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
			m.setInput(m.state.SourceAddress)
		case "esc":
			m.goBack()
		}

	case WizardStepDiscovery:
		return m.handleDiscoveryKey(key)

	case WizardStepCommandShell:
		return m.handleCommandShellKey(key)

	case WizardStepCommandEnv:
		return m.handleCommandEnvKey(key)

	case WizardStepTunnelID:
		return m.handleTunnelIDKey(key)

	case WizardStepTunnelMCP:
		return m.handleTunnelMCPKey(key)

	case WizardStepTunnelProfile:
		return m.handleTunnelProfileKey(key)

	case WizardStepSource:
		switch key {
		case "esc":
			m.goBack()
		case "enter":
			if strings.TrimSpace(m.inputValue()) == "" && m.state.SourceType != "existing_service" {
				m.err = fmt.Errorf("value is required")
				return nil
			}
			m.err = nil
			m.state.SourceAddress = strings.TrimSpace(m.inputValue())
			m.advanceFromSource()
		default:
			m.setInput(editInput(m.inputValue(), key))
		}

	case WizardStepPort:
		switch key {
		case "esc":
			m.goBack()
		case "enter":
			port := strings.TrimSpace(m.inputValue())
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
				m.setInput(commandArgsInput(m.state.CommandArgs))
			} else if m.state.SourceType == "existing_service" {
				m.state.Step = WizardStepProtocol
				m.selected = 0 // default to HTTP
			} else {
				m.state.Step = WizardStepExposure
				m.selected = firstAvailable(m.exposureChoices())
			}
		default:
			m.setInput(editInput(m.inputValue(), key))
		}

	case WizardStepProtocol:
		protocols := []string{"http", "https"}
		switch key {
		case "esc":
			m.goBack()
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
			m.selected = firstAvailable(m.exposureChoices())
		}

	case WizardStepCommandArgs:
		switch key {
		case "esc":
			m.goBack()
		case "enter":
			args, err := parseCommandArgs(m.inputValue())
			if err != nil {
				m.err = err
				return nil
			}
			m.err = nil
			m.state.CommandArgs = args
			m.state.Step = WizardStepCommandWorkingDir
			m.setInput(m.state.WorkingDir)
		default:
			m.setInput(editInput(m.inputValue(), key))
		}

	case WizardStepCommandWorkingDir:
		switch key {
		case "esc":
			m.goBack()
		case "enter":
			m.err = nil
			m.state.WorkingDir = strings.TrimSpace(m.inputValue())
			if m.state.SourceType == "mcp_server" {
				m.state.Step = WizardStepMCPTransport
				m.selected = mcpTransportIndex(m.mcpTransports(), m.state.MCPTransport)
			} else {
				// How the command runs and what it is given, before how it is
				// exposed: they are properties of the thing being published.
				m.state.Step = WizardStepCommandShell
				m.selected = boolIndex(m.state.CommandUseShell)
			}
		default:
			m.setInput(editInput(m.inputValue(), key))
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
				m.selected = firstAvailable(m.exposureChoices())
			}
		case "esc":
			m.goBack()
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
			m.selected = firstAvailable(m.exposureChoices())
		case "esc":
			m.goBack()
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
			m.selected = firstAvailable(m.exposureChoices())
		case "esc":
			m.goBack()
		}

	case WizardStepExposure:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(m.exposureChoices())-1 {
				m.selected++
			}
		case "enter":
			// The list includes options that cannot be picked yet, so choosing
			// one explains what it needs instead of silently doing nothing.
			choice, ok := choiceAt(m.exposureChoices(), m.selected)
			if !ok {
				return nil
			}
			if !choice.Available {
				m.err = fmt.Errorf("%s: %s", choice.Label, choice.Reason)
				return nil
			}
			m.err = nil
			m.state.ExposureMode = choice.Value
			// Protection depends on the address being stable, so changing the
			// address can invalidate a protection already chosen. Leaving it
			// set would carry a combination core validation rejects all the way
			// to create, where the failure is far from the decision that caused
			// it.
			m.discardProtectionIfUnavailable()
			if m.state.ExposureMode == "permanent_public" {
				m.state.Step = WizardStepHostname
				m.setInput(m.state.Hostname)
			} else {
				m.state.Hostname = ""
				m.state.Step = WizardStepProtection
				m.selected = firstAvailable(m.protectionChoices())
			}
		case "esc":
			m.goBack()
		}

	case WizardStepHostname:
		switch key {
		case "esc":
			m.goBack()
		case "enter":
			if strings.TrimSpace(m.inputValue()) == "" {
				m.err = fmt.Errorf("permanent exposure requires a hostname")
				return nil
			}
			m.err = nil
			m.state.Hostname = strings.TrimSpace(m.inputValue())
			m.state.Step = WizardStepProtection
			m.selected = firstAvailable(m.protectionChoices())
		default:
			m.setInput(editInput(m.inputValue(), key))
		}

	case WizardStepProtection:
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(m.protectionChoices())-1 {
				m.selected++
			}
		case "enter":
			choice, ok := choiceAt(m.protectionChoices(), m.selected)
			if !ok {
				return nil
			}
			if !choice.Available {
				m.err = fmt.Errorf("%s: %s", choice.Label, choice.Reason)
				return nil
			}
			m.err = nil
			m.state.Protection = choice.Value
			if m.state.Protection == "email_otp" {
				m.state.Step = WizardStepProtectionRules
				m.setInput(ProtectionRulesInput(m.state.AllowedEmails, m.state.AllowedDomains))
				m.err = nil
				return nil
			}
			m.state.AllowedEmails = nil
			m.state.AllowedDomains = nil
			m.state.Step = WizardStepProvider
			m.selected = 0
			// Every requirement is now answered, so this is the first moment a
			// recommendation can be about the connection actually being made.
			return m.recommendCmd()
		case "esc":
			m.goBack()
		}

	case WizardStepProtectionRules:
		switch key {
		case "esc":
			m.goBack()
		case "enter":
			emails, domains, err := ParseProtectionRules(m.inputValue())
			if err != nil {
				m.err = err
				return nil
			}
			m.err = nil
			m.state.AllowedEmails = emails
			m.state.AllowedDomains = domains
			m.state.Step = WizardStepProvider
			m.selected = 0
			return m.recommendCmd()
		default:
			m.setInput(editInput(m.inputValue(), key))
		}

	case WizardStepProvider:
		switch key {
		case "s":
			// A provider that needs configuring is configured from here. The
			// user was previously required to abandon the wizard, set the
			// provider up elsewhere, and answer every question again.
			choice, ok := choiceAt(m.providerChoices(), m.selected)
			if !ok || choice.Value == "" {
				return nil
			}
			if choice.Available {
				m.err = fmt.Errorf("%s is already usable", choice.Label)
				return nil
			}
			m.err = nil
			m.RequestProviderSetup(choice.Value)
			return nil
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(m.providerChoices())-1 {
				m.selected++
			}
		case "enter":
			choice, ok := choiceAt(m.providerChoices(), m.selected)
			if !ok {
				return nil
			}
			if !choice.Available {
				// Every blocking reason is a fact about this provider, not a
				// preference, so proceeding would create a connection that
				// cannot open. Refusing here is a clear message; refusing at
				// apply time is one after a row has been saved.
				m.err = fmt.Errorf("%s: %s", choice.Label, choice.Reason)
				return nil
			}
			m.err = nil
			m.state.Provider = choice.Value
			// selectAccountFor decides the next question and positions the
			// cursor for it, so the cursor must not be reset afterwards.
			m.selectAccountFor(choice.Value)
		case "esc":
			m.goBack()
		}

	case WizardStepAccount:
		accounts := m.accountsFor(m.state.Provider)
		switch key {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(accounts)-1 {
				m.selected++
			}
		case "enter":
			if m.selected < len(accounts) {
				m.state.AccountID = accounts[m.selected].ID
			}
			m.state.Step = WizardStepReview
			m.selected = 0
		case "esc":
			m.goBack()
		}

	case WizardStepReview:
		switch key {
		case "L":
			// The lifecycle decisions are editable here rather than being
			// applied silently from a default the user was never shown.
			m.cycleLifecycle()
			return nil
		case "enter":
			// A connection with no provider cannot open, and saving it would
			// leave a row that fails the moment anyone tries. The wizard
			// reaches here only if provider selection was skipped or refused,
			// so it says so rather than creating something inert.
			if m.state.Provider == "" {
				m.err = fmt.Errorf("no provider was chosen, so this connection could not open")
				return nil
			}
			// Two outcomes: save closed (default) or open (next step)
			m.openAfterCreate = (m.selected == 1)
			m.state.Step = WizardStepCreating
			req, err := m.buildRequest()
			if err != nil {
				m.err = err
				m.state.Step = WizardStepReview
				return nil
			}
			return createConnectionCmd(m.client, m.ctx, req, m.id, m.generation)
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < 1 {
				m.selected++
			}
		case "esc":
			m.goBack()
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
// Discards stale replies from a previous wizard generation or a different
// wizard instance.
func (m *WizardModel) HandleCreated(msg ConnectionCreatedMsg) tea.Cmd {
	if msg.WizardID != m.id || msg.Generation != m.generation {
		return nil // stale reply
	}
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
// Discards stale replies from a previous wizard generation.
func (m *WizardModel) HandlePlanLoaded(msg WizardPlanLoadedMsg) tea.Cmd {
	if msg.WizardID != m.id || msg.Generation != m.generation {
		return nil // stale reply
	}
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
// Discards stale replies from a previous wizard generation.
func (m *WizardModel) HandlePlanApplied(msg WizardPlanAppliedMsg) tea.Cmd {
	if msg.WizardID != m.id || msg.Generation != m.generation {
		return nil // stale reply
	}
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
// Discards stale replies from a previous wizard generation.
func (m *WizardModel) HandleOperationLoaded(msg WizardOperationLoadedMsg) tea.Cmd {
	if msg.WizardID != m.id || msg.Generation != m.generation {
		return nil // stale reply
	}
	if msg.Err != nil {
		m.err = msg.Err
		m.state.Step = WizardStepComplete
		return nil
	}
	m.operation = msg.Operation

	// An operation reaches "completed", never "succeeded" — that is a step's
	// word. Waiting for it meant a connection that had already opened kept
	// polling, and the wizard went on saying it was still working.
	if msg.Operation != nil && ipc.OperationTerminal(msg.Operation.State) {
		m.state.Step = WizardStepComplete
		return nil
	}

	// Still running — schedule another poll
	return m.pollOperationCmd()
}

// buildRequest assembles the create request from the wizard state.
// Returns an error if the request cannot be built (e.g. invalid port).
func (m *WizardModel) buildRequest() (ipc.CreateConnectionRequest, error) {
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
			UseShell:   s.CommandUseShell,
			Env:        maps.Clone(s.CommandEnv),
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
				UseShell:   s.CommandUseShell,
				Env:        maps.Clone(s.CommandEnv),
			}
		} else {
			src.MCP.Endpoint = s.SourceAddress
		}
	}

	// Build the request based on connection kind.
	kind := s.ConnectionKind
	if kind == "" {
		kind = "service_exposure"
	}

	req := ipc.CreateConnectionRequest{
		Version: 1,
		Kind:    kind,
		Name:    s.Name,
		Provider: ipc.ProviderSelectionDTO{
			ProviderID: s.Provider,
			AccountID:  s.AccountID,
		},
		Lifecycle: ipc.LifecycleDTO{
			AutoStart:    s.AutoStart,
			OnDisconnect: onDisconnectValue(s.OnDisconnect),
		},
	}

	switch kind {
	case "port_forward":
		localPort, err := strconv.Atoi(s.PortForwardLocalPort)
		if err != nil || localPort < 1 || localPort > 65535 {
			return ipc.CreateConnectionRequest{}, fmt.Errorf("invalid local port: %s", s.PortForwardLocalPort)
		}
		remotePort, err := strconv.Atoi(s.PortForwardRemotePort)
		if err != nil || remotePort < 1 || remotePort > 65535 {
			return ipc.CreateConnectionRequest{}, fmt.Errorf("invalid remote port: %s", s.PortForwardRemotePort)
		}
		protocol := s.PortForwardProtocol
		if protocol == "" {
			protocol = "tcp"
		}
		req.PortForward = &ipc.PortForwardDTO{
			LocalPort:  localPort,
			RemoteHost: s.PortForwardRemoteHost,
			RemotePort: remotePort,
			Protocol:   protocol,
			Direction:  "local",
		}
	case "client_tunnel":
		// Adopting a tunnel that already exists. Portico does not create it — the
		// adapter refuses to plan without an ID for exactly that reason — so the
		// request carries the ID the user gave rather than a blank to be filled.
		if s.TunnelID == "" {
			return ipc.CreateConnectionRequest{}, fmt.Errorf(
				"a tunnel ID is required: Portico manages a tunnel you created in the " +
					"OpenAI platform, and cannot create one")
		}
		if s.SourceAddress == "" {
			return ipc.CreateConnectionRequest{}, fmt.Errorf(
				"the tunnel needs an MCP server to forward to")
		}
		req.ClientTunnel = &ipc.ClientTunnelSpecDTO{
			Client:   "openai_secure_mcp_tunnel",
			TunnelID: s.TunnelID,
			Profile:  s.TunnelProfile,
			MCP: ipc.MCPSourceDTO{
				// The transport is HTTP: the client reaches a local MCP server over
				// it, and the endpoint the user gave is that server's address.
				Transport: "http",
				Endpoint:  s.SourceAddress,
			},
		}
	default: // service_exposure
		req.Source = src
		req.Exposure = ipc.ExposureDTO{
			Mode:             s.ExposureMode,
			RequestedAddress: s.Hostname,
		}
		req.Protection = ipc.ProtectionDTO{
			Kind:           s.Protection,
			AllowedEmails:  append([]string(nil), s.AllowedEmails...),
			AllowedDomains: append([]string(nil), s.AllowedDomains...),
		}
	}

	return req, nil
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
// never reads or mutates wizard state. The wizard's identity and generation
// are captured so a late reply from an abandoned wizard is discarded.
func createConnectionCmd(client ConnectionCreator, ctx context.Context, req ipc.CreateConnectionRequest, wizID WizardID, gen WizardGeneration) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return ConnectionCreatedMsg{WizardID: wizID, Generation: gen, Err: fmt.Errorf("no supervisor connection")}
		}
		conn, err := client.CreateConnection(ctx, req)
		if err != nil {
			return ConnectionCreatedMsg{WizardID: wizID, Generation: gen, Err: err}
		}
		return ConnectionCreatedMsg{WizardID: wizID, Generation: gen, ID: conn.ID, Connection: conn}
	}
}

// requestPlanCmd returns a command that requests an open plan for the created connection.
func (m *WizardModel) requestPlanCmd() tea.Cmd {
	client := m.client
	connID := m.createdID
	ctx := m.ctx
	wizID := m.id
	gen := m.generation
	return func() tea.Msg {
		if client == nil {
			return WizardPlanLoadedMsg{WizardID: wizID, Generation: gen, Err: fmt.Errorf("no supervisor connection")}
		}
		plan, err := client.PlanOpen(ctx, connID)
		if err != nil {
			return WizardPlanLoadedMsg{WizardID: wizID, Generation: gen, Err: err}
		}
		return WizardPlanLoadedMsg{WizardID: wizID, Generation: gen, Plan: plan}
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
	wizID := m.id
	gen := m.generation
	return func() tea.Msg {
		if client == nil {
			return WizardPlanAppliedMsg{WizardID: wizID, Generation: gen, Err: fmt.Errorf("no supervisor connection")}
		}
		if planID == "" {
			return WizardPlanAppliedMsg{WizardID: wizID, Generation: gen, Err: fmt.Errorf("no plan to apply")}
		}
		// The same key on every retry of this plan, so a lost response cannot
		// turn into a second operation.
		op, err := client.ApplyPlanWithIdempotency(ctx, planID, "apply-"+planID)
		if err != nil {
			return WizardPlanAppliedMsg{WizardID: wizID, Generation: gen, Err: err}
		}
		return WizardPlanAppliedMsg{WizardID: wizID, Generation: gen, Operation: op}
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
	wizID := m.id
	gen := m.generation
	return func() tea.Msg {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return WizardOperationLoadedMsg{WizardID: wizID, Generation: gen, Err: ctx.Err()}
			}
		}
		select {
		case <-ctx.Done():
			return WizardOperationLoadedMsg{WizardID: wizID, Generation: gen, Err: ctx.Err()}
		default:
		}
		if client == nil {
			return WizardOperationLoadedMsg{WizardID: wizID, Generation: gen, Err: fmt.Errorf("no supervisor connection")}
		}
		if opID == "" {
			return WizardOperationLoadedMsg{WizardID: wizID, Generation: gen, Err: fmt.Errorf("no operation to poll")}
		}
		op, err := client.GetOperation(ctx, opID)
		if err != nil {
			return WizardOperationLoadedMsg{WizardID: wizID, Generation: gen, Err: err}
		}
		return WizardOperationLoadedMsg{WizardID: wizID, Generation: gen, Operation: op}
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
		return m.withError(m.renderField("Name this connection:"))
	case WizardStepPortForwardLocalPort:
		return m.withError(m.renderField("Local listening port:"))
	case WizardStepPortForwardRemoteHost:
		return m.withError(m.renderField("Remote host:"))
	case WizardStepPortForwardRemotePort:
		return m.withError(m.renderField("Remote port:"))
	case WizardStepPortForwardProtocol:
		return m.withError(renderChoices("Protocol:", portForwardProtocolChoices(), m.selected))
	case WizardStepMCPMode:
		return renderMenu("How does the MCP server run?", []string{"Already running at an HTTP endpoint", "A command Portico should run"}, m.selected)
	case WizardStepDiscovery:
		return m.withError(m.renderDiscovery())
	case WizardStepCommandShell:
		return m.withError(m.renderCommandShell())
	case WizardStepCommandEnv:
		return m.withError(m.renderField(
			"Environment for the command (NAME=VALUE, comma separated; empty for none):\n" +
				"For a secret, write NAME=env:OTHER — Portico reads OTHER from its own\n" +
				"environment when the command starts, so only the name is saved."))
	case WizardStepTunnelID:
		return m.withError(m.renderField(
			"Which tunnel should Portico manage?\n" +
				"Portico does not create tunnels: create one in the OpenAI platform's\n" +
				"organization settings, then paste its ID here."))
	case WizardStepTunnelMCP:
		return m.withError(m.renderField(
			"Where is the MCP server the tunnel should reach?\n" +
				"The address it listens on, such as http://127.0.0.1:8000."))
	case WizardStepTunnelProfile:
		return m.withError(m.renderField(
			"Which client profile should Portico run? (empty to use the default)"))
	case WizardStepSource:
		return m.withError(m.renderField(m.sourcePrompt()))
	case WizardStepPort:
		if m.isCommandOrigin() {
			return m.withError(m.renderField("Local port for the command (required):"))
		}
		return m.withError(m.renderField("Local port (empty to skip):"))
	case WizardStepProtocol:
		return m.renderProtocol()
	case WizardStepCommandArgs:
		return m.withError(m.renderField(
			"Command arguments (space separated; quote any argument containing spaces; empty to skip):\n" +
				renderArgvPreviewForInput(m.state.SourceAddress, m.inputValue())))
	case WizardStepCommandWorkingDir:
		return m.withError(m.renderField("Working directory (empty to use Portico's):"))
	case WizardStepDirectoryMode:
		return m.renderDirectoryMode()
	case WizardStepDirectorySPA:
		return renderMenu("Enable SPA fallback for unknown paths?", []string{"No", "Yes"}, m.selected)
	case WizardStepMCPTransport:
		return m.renderMCPTransport()
	case WizardStepExposure:
		return m.renderExposure()
	case WizardStepHostname:
		return m.withError(m.renderField("Enter the hostname to use:"))
	case WizardStepProtection:
		return m.renderProtection()
	case WizardStepProtectionRules:
		return m.withError(m.renderField("Allow emails or domains (comma-separated; @example.com permits a domain):"))
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

// advanceFromSource moves to whatever question follows the source, given the
// kind of source this is.
//
// It is one function because two questions now arrive here: the typed address and
// the discovered service. A second copy of this branch is exactly the drift that
// made the exposure step's back-navigation disagree with its forward navigation.
func (m *WizardModel) advanceFromSource() {
	switch {
	case m.hasPortStep():
		m.state.Step = WizardStepPort
		m.setInput(m.state.Port)
	case m.state.SourceType == "directory":
		m.state.Step = WizardStepDirectoryMode
		m.selected = m.directoryModeIndex()
	case m.state.SourceType == "mcp_server":
		m.state.Step = WizardStepMCPTransport
		m.selected = mcpTransportIndex(m.mcpTransports(), m.state.MCPTransport)
	default:
		m.state.Step = WizardStepExposure
		m.selected = firstAvailable(m.exposureChoices())
	}
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
		// Availability is read from the snapshot, so an outcome that has become
		// possible stops being marked unavailable without anyone editing a string.
		if m.recipeUnavailable(recipe) != "" {
			label += "  (not available yet)"
		}
		options = append(options, label)
	}
	view := renderMenu("What are you trying to do?", options, m.selected)

	// Show the consequence of the highlighted choice before it is made, and what
	// stands in the way when something does.
	if m.selected >= 0 && m.selected < len(wizardRecipes) {
		recipe := wizardRecipes[m.selected]
		view += "\n\n" + recipe.Explanation + "\n"
		if reason := m.recipeUnavailable(recipe); reason != "" {
			view += "\nThis is not available yet: " + reason + "\n"
		}
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
	return renderChoices("How should it be reachable?", m.exposureChoices(), m.selected)
}

func (m *WizardModel) renderProtection() string {
	return renderChoices("Who should be able to reach it?", m.protectionChoices(), m.selected)
}

func (m *WizardModel) renderDirectoryMode() string {
	options := make([]string, 0, len(m.directoryModeChoices()))
	for _, choice := range m.directoryModeChoices() {
		options = append(options, choice.label)
	}
	return renderMenu("How should Portico serve this directory?", options, m.selected)
}

// directoryModeChoices returns the directory mode options filtered by
// provider capability. Write-enabled modes need protection, protection needs an
// address that does not move, and that needs a provider able to own a hostname —
// so the constraint is about capability, not about which provider supplies it.
func (m *WizardModel) directoryModeChoices() []directoryModeChoice {
	// Upload and delete require protection, and protection requires an address
	// that does not move, so write-enabled modes depend on some provider being
	// able to supply a permanent address — not on any particular provider.
	if !m.caps.hasCapability(supportsCustomHostname) {
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
	accounts := m.accountsFor(m.state.Provider)
	options := make([]string, 0, len(accounts))
	for _, account := range accounts {
		label := account.Label
		if label == "" || label == account.ID {
			label = account.ID
		} else {
			label += " (" + account.ID + ")"
		}
		options = append(options, label)
	}
	// The question names whichever provider was chosen. Naming one provider
	// here was the last place the wizard assumed which one it would be.
	name := m.state.Provider
	for _, p := range m.caps.providers {
		if p.ID == m.state.Provider && p.DisplayName != "" {
			name = p.DisplayName
		}
	}
	if name == "" {
		name = "provider"
	}
	return renderMenu("Which "+name+" account should own this connection?", options, m.selected)
}

// renderPlanPreview shows the plan through the shared presentation, so the
// wizard describes a plan the same way every other surface does.
func (m *WizardModel) renderPlanPreview() string {
	if m.plan == nil {
		return "Loading the plan..."
	}
	view := DescribePlan(m.plan, m.state.Name)
	out := RenderPlan(view)
	if m.err != nil {
		out += "\nError: " + m.err.Error()
	}
	return out
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
		switch {
		case ipc.OperationSucceeded(m.operation.State):
			lines = append(lines, "Connection opened successfully!")
		case m.operation.State == ipc.OperationFailed:
			lines = append(lines, "Connection created but opening failed.")
			if m.err != nil {
				lines = append(lines, "", "Error: "+m.err.Error())
			}
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
	for _, account := range m.accountsFor(m.state.Provider) {
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

// renderField draws a question answered by typing, showing the live field with
// its cursor rather than a snapshot of the value.
func (m *WizardModel) renderField(prompt string) string {
	lines := []string{prompt, "", m.field.View(), "", "Enter to continue  Esc Back"}
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
// ParseProtectionRules is exported so the edit screen parses identities the
// same way the wizard does. Two parsers would eventually disagree about what a
// valid identity is, and the disagreement would appear as an edit that core
// rejects for reasons the screen accepted.
func ParseProtectionRules(input string) ([]string, []string, error) {
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

// ProtectionRulesInput formats stored identities for editing.
func ProtectionRulesInput(emails, domains []string) string {
	values := append([]string(nil), emails...)
	values = append(values, domains...)
	return strings.Join(values, ", ")
}
