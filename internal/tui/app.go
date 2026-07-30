package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/route"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// ScreenID identifies which screen is active.
type ScreenID string

const (
	ScreenBoot              ScreenID = "boot"
	ScreenRecovery          ScreenID = "recovery"
	ScreenHome              ScreenID = "home"
	ScreenNewConnection     ScreenID = "new_connection"
	ScreenInspect           ScreenID = "inspect"
	ScreenPlanPreview       ScreenID = "plan_preview"
	ScreenOperationProgress ScreenID = "operation_progress"
	ScreenOperations        ScreenID = "operations"
	ScreenRepair            ScreenID = "repair"
	ScreenDiscovery         ScreenID = "discovery"
	ScreenProviders         ScreenID = "providers"
	ScreenSettings          ScreenID = "settings"
	ScreenHelp              ScreenID = "help"
	ScreenSetup             ScreenID = "setup"
	ScreenQuit              ScreenID = "quit"
)

// Model is the root Bubble Tea model (SPEC §17.3).
type Model struct {
	width      int
	height     int
	ready      bool
	screen     ScreenID
	prevScreen ScreenID   // screen to return to when leaving overlays (help)
	navStack   []ScreenID // navigation stack for back navigation
	selectedID string     // stable connection ID, not a list index
	listOffset int        // scroll offset for connection list viewport

	snapshot          ipc.SnapshotDTO
	plan              *ipc.PlanDTO
	operation         *ipc.OperationDTO
	applying          bool
	diagnostics       []ipc.DiagnosticDTO
	discovery         []ipc.DiscoveredServiceDTO
	discoverySelected int
	opEvents          []string

	// Connection detail for inspect screen
	connectionDetail *ipc.ConnectionDetailDTO
	connectionLogs   *ipc.ConnectionLogsDTO

	// Operations screen state
	operations     []ipc.OperationDTO
	opsSelectedIdx int
	// operationsAvailable distinguishes "history read, none yet" from
	// "history could not be read". Only the former may be shown as an
	// authoritative empty list.
	operationsAvailable   bool
	operationsUnavailable string

	// Repair verification state
	preRepairDiagnostics       []ipc.DiagnosticDTO
	repairConnectionID         string
	awaitingRepairVerification bool

	// Provider account setup state
	providerSetupStep   int // 0: not in setup, 1: account ID, 2: label, 3: zone ID, 4: credential, 5: confirming
	providerSetupID     string
	providerSetupLabel  string
	providerSetupZoneID string
	providerSetupCred   string
	providerSetupError  string

	lastEventSeq    int64
	stream          *ipc.EventStream
	streamCancel    context.CancelFunc
	streamConnected bool

	keys     KeyMap
	theme    Theme
	useASCII bool

	wizard  *screens.WizardModel
	inspect *screens.InspectModel
	setup   *screens.SetupModel

	client SupervisorClient
	// rootCtx is the application lifetime context. It is cancelled when
	// the Bubble Tea program exits, ensuring in-flight RPCs do not outlive
	// the TUI.
	rootCtx    context.Context
	rootCancel context.CancelFunc
	err        error
	status     string
}

// New creates a new TUI model backed by the real IPC client.
func New(client *ipc.Client) Model {
	var c SupervisorClient
	if client != nil {
		c = client
	}
	return newModel(c)
}

// newModel creates a model against the SupervisorClient interface.
func newModel(client SupervisorClient) Model {
	theme := DefaultTheme
	if os.Getenv("NO_COLOR") != "" || os.Getenv("PORTICO_MONOCHROME") != "" {
		theme = MonochromeTheme
	}
	ctx, cancel := context.WithCancel(context.Background())
	return Model{
		screen:     ScreenBoot, // Start in boot screen
		keys:       DefaultKeyMap,
		theme:      theme,
		useASCII:   os.Getenv("PORTICO_ASCII") != "" || os.Getenv("TERM") == "dumb",
		client:     client,
		rootCtx:    ctx,
		rootCancel: cancel,
	}
}

// --------------- Bubble Tea integration ---------------

// Init returns the startup command (SPEC §17.4).
// Sequenced: load snapshot first, then connect SSE after cursor is set.
func (m Model) Init() tea.Cmd {
	return m.requestSnapshot()
}

// Update handles messages (SPEC §17.7).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKeyPress(msg)

	case snapshotMsg:
		if msg.Err != nil {
			if !m.ready {
				// Initial snapshot failed — transition to recovery screen
				m.err = msg.Err
				m.screen = ScreenRecovery
			} else {
				m.status = statusLine("snapshot refresh failed", msg.Err)
			}
			return m, nil
		}
		m.snapshot = msg.Snapshot
		m.ready = true
		// Transition to home screen on successful initial load
		if m.screen == ScreenBoot {
			m.screen = ScreenHome
			m.clearNav()
		}
		// Preserve selection by stable connection ID. If the previously
		// selected connection was deleted, fall back to the first entry.
		conns := m.ConnectionList()
		if len(conns) > 0 && m.selectedID != "" {
			found := false
			for _, c := range conns {
				if c.ID == m.selectedID {
					found = true
					break
				}
			}
			if !found {
				m.selectedID = conns[0].ID
			}
		} else if len(conns) > 0 && m.selectedID == "" {
			m.selectedID = conns[0].ID
		} else if len(conns) == 0 {
			m.selectedID = ""
		}
		// Advance the event cursor to the snapshot's LastSeq so that
		// SSE replay starts after this point, closing the gap between
		// snapshot capture and stream connection.
		if msg.Snapshot.LastSeq > m.lastEventSeq {
			m.lastEventSeq = msg.Snapshot.LastSeq
		}
		// On first boot, connect the event stream now that the cursor is set.
		if !m.streamConnected {
			m.streamConnected = true
			return m, m.connectEventStream()
		}
		return m, nil

	case planLoadedMsg:
		if msg.Err != nil {
			m.status = statusLine("plan failed", msg.Err)
			return m, nil
		}
		m.status = ""
		if msg.Plan != nil && msg.Plan.Noop {
			m.plan = nil
			m.status = "No repair needed"
			m.pushScreen(ScreenRepair)
			return m, nil
		}
		m.plan = msg.Plan
		m.pushScreen(ScreenPlanPreview)
		return m, nil

	case planAppliedMsg:
		m.applying = false
		if msg.Err != nil {
			m.status = statusLine("apply failed", msg.Err)
			return m, nil
		}
		m.status = ""
		m.operation = msg.Operation
		m.opEvents = nil
		m.plan = nil
		m.pushScreen(ScreenOperationProgress)
		return m, tea.Batch(m.requestSnapshot(), m.getOperationCmd(msg.Operation.ID))

	case operationLoadedMsg:
		if msg.Err != nil {
			m.status = statusLine("operation refresh failed", msg.Err)
			return m, nil
		}
		m.operation = msg.Operation

		// Check if operation completed and we need post-repair verification
		if m.operation != nil && m.awaitingRepairVerification {
			state := m.operation.State
			if state == "succeeded" || state == "failed" || state == "cancelled" {
				// Operation completed, re-run diagnostics to verify repair
				m.awaitingRepairVerification = false
				if m.repairConnectionID != "" {
					m.status = "Verifying repair..."
					return m, m.diagnosticsCmd(m.repairConnectionID)
				}
			}
		}
		return m, nil

	case readinessMsg:
		if m.setup == nil {
			m.setup = screens.NewSetup()
		}
		// A failed check is reported as a failed check, never as a clean bill
		// of health.
		m.setup.Err = msg.Err
		m.setup.Readiness = msg.Readiness
		return m, nil

	case connectionLogsMsg:
		if msg.ConnectionID != m.selectedID {
			return m, nil
		}
		if msg.Err != nil {
			// Report the subsystem as unreadable rather than showing an empty
			// log view that reads as "the connector said nothing".
			m.connectionLogs = &ipc.ConnectionLogsDTO{
				ConnectionID: msg.ConnectionID,
				Unavailable:  msg.Err.Error(),
			}
		} else {
			m.connectionLogs = msg.Logs
		}
		if m.inspect != nil {
			m.inspect.LogTail = m.connectionLogs
		}
		return m, nil

	case connectionDetailMsg:
		// A late reply for a connection the user has already navigated away
		// from must not overwrite the current one's detail.
		if msg.ConnectionID != m.selectedID {
			return m, nil
		}
		if msg.Err != nil {
			// Keep whatever detail is already on screen; report the staleness
			// rather than blanking the view.
			m.status = statusLine("connection detail unavailable", msg.Err)
			return m, nil
		}
		m.connectionDetail = msg.Detail
		if m.inspect != nil {
			m.inspect.Detail = msg.Detail
		}
		return m, nil

	case diagnosticsMsg:
		if msg.Err != nil {
			m.status = statusLine("diagnostics failed", msg.Err)
			return m, nil
		}
		m.status = ""
		m.diagnostics = msg.Findings

		// If we just completed repair verification, show the results
		if len(m.preRepairDiagnostics) > 0 && m.repairConnectionID != "" {
			m.pushScreen(ScreenRepair)
			// Keep preRepairDiagnostics for comparison display
		}
		return m, nil

	case discoveryMsg:
		if msg.Err != nil {
			m.status = statusLine("discovery failed", msg.Err)
			m.screen = ScreenHome
			m.clearNav()
			return m, nil
		}
		m.discovery = msg.Services
		m.discoverySelected = 0
		m.pushScreen(ScreenDiscovery)
		return m, nil

	case operationsLoadedMsg:
		if msg.Err != nil {
			m.operationsAvailable = false
			m.operationsUnavailable = msg.Err.Error()
			m.operations = nil
			m.status = statusLine("operations load failed", msg.Err)
			return m, nil
		}
		m.operations = msg.Operations
		m.operationsAvailable = msg.Available
		m.operationsUnavailable = msg.Unavailable
		m.opsSelectedIdx = 0
		return m, nil

	case providerAccountConfiguredMsg:
		if msg.Err != nil {
			// Keep the answers that were accepted and return to the credential
			// step, which is what validation almost always rejects. Restarting
			// at step one discarded correct input for no reason.
			m.providerSetupError = msg.Err.Error()
			if msg.Response != nil && len(msg.Response.MissingPermissions) > 0 {
				m.providerSetupError += "\n\nThe token is missing:\n  • " +
					strings.Join(msg.Response.MissingPermissions, "\n  • ")
			}
			// The rejected secret must not stay in memory while the user
			// retypes it.
			m.providerSetupCred = ""
			m.providerSetupStep = 4
			return m, nil
		}
		// Success: drop every collected answer, secret included.
		m.clearProviderSetup()
		// Report what the account can actually do, since a zone is required
		// only for DNS and custom hostnames.
		switch {
		case msg.Response == nil:
			m.status = "Account configured."
		case msg.Response.CapabilityLevel == "tunnels_without_dns":
			m.status = "Account verified. Tunnels are available; add a zone to use permanent hostnames."
		case msg.Response.CapabilityLevel == "tunnels_with_dns":
			m.status = "Account verified. Tunnels and permanent hostnames are available."
		default:
			m.status = "Account configured."
		}
		if msg.Response != nil && msg.Response.RestartRequired {
			m.status += " Restart the supervisor to activate it."
		}
		return m, m.requestSnapshot()

	case screens.ConnectionCreatedMsg:
		if m.wizard != nil {
			cmd := m.wizard.HandleCreated(msg)
			if msg.Err != nil {
				m.status = statusLine("create failed", msg.Err)
				return m, nil
			}
			m.status = ""
			return m, tea.Batch(cmd, m.requestSnapshot())
		}
		if msg.Err != nil {
			m.status = statusLine("create failed", msg.Err)
			return m, nil
		}
		m.status = ""
		return m, m.requestSnapshot()

	case screens.WizardPlanLoadedMsg:
		if m.wizard != nil {
			cmd := m.wizard.HandlePlanLoaded(msg)
			if msg.Err != nil {
				m.status = statusLine("plan failed", msg.Err)
			}
			return m, cmd
		}
		return m, nil

	case screens.WizardPlanAppliedMsg:
		if m.wizard != nil {
			cmd := m.wizard.HandlePlanApplied(msg)
			if msg.Err != nil {
				m.status = statusLine("apply failed", msg.Err)
			}
			return m, cmd
		}
		return m, nil

	case screens.WizardOperationLoadedMsg:
		if m.wizard != nil {
			cmd := m.wizard.HandleOperationLoaded(msg)
			return m, cmd
		}
		return m, nil

	case eventMsg:
		if msg.Event.Type == "resync_required" {
			// Close the old stream and request a fresh snapshot.
			// The snapshot handler will set lastEventSeq and reconnect SSE.
			m.closeEventStream()
			m.streamConnected = false
			return m, m.requestSnapshot()
		}
		// Deduplicate events by sequence monotonicity.
		if msg.Event.Sequence > 0 && msg.Event.Sequence <= m.lastEventSeq {
			return m, m.waitForEvent()
		}
		m.lastEventSeq = msg.Event.Sequence
		cmd := m.handleEvent(msg.Event)
		// Schedule next event wait
		return m, tea.Batch(cmd, m.waitForEvent())

	case errorMsg:
		m.err = msg.Err
		return m, nil

	case streamErrorMsg:
		if m.wizard != nil {
			m.wizard.SetStreamConnected(false)
		}
		m.closeEventStream()
		m.status = fmt.Sprintf("event stream interrupted: %v; reconnecting", msg.Err)
		return m, m.reconnectEventStream()

	case reconnectEventStreamMsg:
		return m, m.connectEventStream()

	case eventStreamReadyMsg:
		m.stream = msg.Stream
		m.streamCancel = msg.Cancel
		if m.wizard != nil {
			m.wizard.SetStreamConnected(true)
		}
		return m, m.waitForEvent()

	case resyncMsg:
		// Replay gap — reload snapshot
		return m, m.requestSnapshot()
	}

	return m, nil
}

// View renders the current state (SPEC §17.8 — pure).
func (m Model) View() tea.View {
	// Boot and loading states come first — no snapshot yet.
	if !m.ready && m.screen == ScreenBoot {
		if m.err != nil {
			// Fatal boot error with no recovery path.
			content := m.renderError()
			v := tea.NewView(content)
			v.AltScreen = true
			return v
		}
		content := m.renderLoading()
		v := tea.NewView(content)
		v.AltScreen = true
		return v
	}

	// Dispatch by screen — each screen owns its own error presentation.
	// This ensures ScreenRecovery is reachable even when m.err is set.
	var content string
	switch m.screen {
	case ScreenRecovery:
		content = m.renderRecovery()
	case ScreenHome:
		content = m.renderHome()
	case ScreenNewConnection:
		content = m.renderNewConnection()
	case ScreenInspect:
		content = m.renderInspect()
	case ScreenHelp:
		content = m.renderHelp()
	case ScreenPlanPreview:
		content = m.renderPlanPreview()
	case ScreenOperationProgress:
		content = m.renderOperationProgress()
	case ScreenProviders:
		content = m.renderProviders()
	case ScreenRepair:
		content = m.renderRepair()
	case ScreenDiscovery:
		content = m.renderDiscovery()
	case ScreenOperations:
		content = m.renderOperations()
	case ScreenSetup:
		if m.setup != nil {
			content = m.setup.View()
		} else {
			content = "Checking what Portico needs..."
		}
	case ScreenQuit:
		content = ""
	default:
		if m.err != nil {
			content = m.renderError()
		} else {
			content = m.renderHome()
		}
	}

	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// --------------- message types ---------------

type snapshotMsg struct {
	Snapshot ipc.SnapshotDTO
	Err      error
}

type planLoadedMsg struct {
	Plan *ipc.PlanDTO
	Err  error
}

type planAppliedMsg struct {
	Operation *ipc.OperationDTO
	Err       error
}

type operationLoadedMsg struct {
	Operation *ipc.OperationDTO
	Err       error
}

type operationsLoadedMsg struct {
	Operations  []ipc.OperationDTO
	Available   bool
	Unavailable string
	Err         error
}

type errorMsg struct {
	Err error
}

type eventMsg struct {
	Event ipc.EventDTO
}

type eventStreamReadyMsg struct {
	Stream *ipc.EventStream
	Cancel context.CancelFunc
}

type resyncMsg struct {
	Reason string
}

type diagnosticsMsg struct {
	Findings []ipc.DiagnosticDTO
	Err      error
}

type discoveryMsg struct {
	Services []ipc.DiscoveredServiceDTO
	Err      error
}

type providerAccountConfiguredMsg struct {
	Response *ipc.ConfigureProviderAccountResponse
	Err      error
}

type readinessMsg struct {
	Readiness *ipc.ReadinessDTO
	Err       error
}

type connectionLogsMsg struct {
	ConnectionID string
	Logs         *ipc.ConnectionLogsDTO
	Err          error
}

type connectionDetailMsg struct {
	ConnectionID string
	Detail       *ipc.ConnectionDetailDTO
	Err          error
}

type streamErrorMsg struct{ Err error }

// --------------- commands ---------------
//
// Commands capture what they need before returning the closure so that
// nothing running off the update loop reads or mutates the model.

// connectionDetailCmd loads the authoritative detail view for a connection.
// The inspect screen opens immediately from the list summary and fills in
// detail when it arrives, so a slow supervisor delays content rather than
// blocking navigation.
func (m *Model) connectionDetailCmd(connID string) tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return connectionDetailMsg{ConnectionID: connID, Err: fmt.Errorf("no supervisor connection")}
		}
		detailCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		detail, err := client.GetConnectionDetail(detailCtx, connID)
		return connectionDetailMsg{ConnectionID: connID, Detail: detail, Err: err}
	}
}

// connectionLogsCmd loads a bounded, redacted tail of the connector's output.
// readinessCmd loads the aggregated setup view.
func (m *Model) readinessCmd() tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return readinessMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		readyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		readiness, err := client.Readiness(readyCtx)
		return readinessMsg{Readiness: readiness, Err: err}
	}
}

func (m *Model) connectionLogsCmd(connID string) tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return connectionLogsMsg{ConnectionID: connID, Err: fmt.Errorf("no supervisor connection")}
		}
		logCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		logs, err := client.ConnectionLogs(logCtx, connID, 0)
		return connectionLogsMsg{ConnectionID: connID, Logs: logs, Err: err}
	}
}

func (m *Model) diagnosticsCmd(connID string) tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return diagnosticsMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		// Diagnostics can take a while — use a bounded timeout.
		diagCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		findings, err := client.Diagnostics(diagCtx, connID)
		return diagnosticsMsg{Findings: findings, Err: err}
	}
}

func (m *Model) discoveryCmd() tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return discoveryMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		// Use RefreshDiscovery (POST) for explicit user-initiated scans
		// so that newly launched services are detected immediately.
		discCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		result, err := client.RefreshDiscovery(discCtx)
		if err != nil {
			return discoveryMsg{Err: err}
		}
		return discoveryMsg{Services: result.Services}
	}
}

func (m *Model) loadOperationsCmd() tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return operationsLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		opsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		history, err := client.GetOperationHistory(opsCtx)
		if err != nil {
			return operationsLoadedMsg{Err: err}
		}
		return operationsLoadedMsg{
			Operations:  history.Operations,
			Available:   history.Available,
			Unavailable: history.Unavailable,
		}
	}
}

func (m *Model) configureProviderAccountCmd(providerID string, req ipc.ConfigureProviderAccountRequest) tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return providerAccountConfiguredMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		configCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		resp, err := client.ConfigureProviderAccount(configCtx, providerID, req)
		return providerAccountConfiguredMsg{Response: resp, Err: err}
	}
}

func (m *Model) requestSnapshot() tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return snapshotMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		snapCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		snap, err := client.GetSnapshot(snapCtx)
		if err != nil {
			return snapshotMsg{Err: err}
		}
		return snapshotMsg{Snapshot: *snap}
	}
}

func (m *Model) planOpenCmd(connID string) tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return planLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		planCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		plan, err := client.PlanOpen(planCtx, connID)
		return planLoadedMsg{Plan: plan, Err: err}
	}
}

func (m *Model) planCloseCmd(connID string) tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return planLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		planCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		plan, err := client.PlanClose(planCtx, connID)
		return planLoadedMsg{Plan: plan, Err: err}
	}
}

func (m *Model) planRepairCmd(connID string) tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return planLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		planCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		plan, err := client.PlanRepair(planCtx, connID)
		return planLoadedMsg{Plan: plan, Err: err}
	}
}

func (m *Model) planDeleteCmd(connID string) tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return planLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		planCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		plan, err := client.PlanDelete(planCtx, connID)
		return planLoadedMsg{Plan: plan, Err: err}
	}
}

func (m *Model) applyPlanCmd(planID string) tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return planAppliedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		// Apply is a mutation — use a longer timeout. The server-side
		// operation continues even if this context is cancelled.
		applyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		op, err := client.ApplyPlan(applyCtx, planID)
		return planAppliedMsg{Operation: op, Err: err}
	}
}

func (m *Model) getOperationCmd(operationID string) tea.Cmd {
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return operationLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		operation, err := client.GetOperation(opCtx, operationID)
		return operationLoadedMsg{Operation: operation, Err: err}
	}
}

func (m *Model) connectEventStream() tea.Cmd {
	client := m.client
	lastSeq := m.lastEventSeq
	ctx := m.rootCtx
	return func() tea.Msg {
		if client == nil {
			return streamErrorMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		// The event stream lives for the application lifetime — use rootCtx
		// directly so it is cancelled when the TUI exits.
		streamCtx, cancel := context.WithCancel(ctx)
		stream, err := client.ConnectEventStream(streamCtx, lastSeq)
		if err != nil {
			cancel()
			return streamErrorMsg{Err: fmt.Errorf("event stream: %w", err)}
		}
		return eventStreamReadyMsg{Stream: stream, Cancel: cancel}
	}
}

func (m *Model) waitForEvent() tea.Cmd {
	stream := m.stream
	return func() tea.Msg {
		if stream == nil {
			return nil
		}
		evt, err := stream.Next()
		if err != nil {
			return streamErrorMsg{Err: fmt.Errorf("event read: %w", err)}
		}
		return eventMsg{Event: *evt}
	}
}

func (m *Model) reconnectEventStream() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return reconnectEventStreamMsg{} })
}

type reconnectEventStreamMsg struct{}

func (m *Model) closeEventStream() {
	if m.streamCancel != nil {
		m.streamCancel()
	}
	if m.stream != nil {
		_ = m.stream.Close()
	}
	m.stream = nil
	m.streamCancel = nil
}

// handleEvent reacts to a supervisor event. It only mutates the model on
// the update loop and returns any follow-up work as a command.
func (m *Model) handleEvent(evt ipc.EventDTO) tea.Cmd {
	switch {
	case evt.Type == "supervisor.shutdown":
		m.err = fmt.Errorf("supervisor shut down")
		return nil

	case strings.HasPrefix(evt.Type, "operation."):
		// The wizard tracks its own operation. Deliver progress from the event
		// stream rather than leaving it to poll, so the stream is the primary
		// signal and polling is only a fallback.
		if m.wizard != nil && evt.OperationID != "" && evt.OperationID == m.wizard.OperationID() {
			return tea.Batch(m.requestSnapshot(), m.wizard.RefreshOperationCmd())
		}
		// Only update the visible operation log when the event belongs to
		// the operation currently displayed. Unrelated operation events
		// still trigger a snapshot refresh but must not pollute the
		// active operation screen.
		if m.operation != nil && evt.OperationID == m.operation.ID {
			if data, ok := evt.Data.(map[string]interface{}); ok {
				if summary, ok := data["summary"].(string); ok && summary != "" {
					// Bound the in-memory event list to prevent unbounded growth.
					if len(m.opEvents) < 500 {
						m.opEvents = append(m.opEvents, summary)
					}
				}
			}
			// Operation progress changes connection state — reload both the
			// connection snapshot and the operation itself so the progress view
			// reaches a durable terminal state.
			if m.operation != nil {
				return tea.Batch(m.requestSnapshot(), m.getOperationCmd(m.operation.ID))
			}
			return m.requestSnapshot()
		}
		// Unrelated operation event — refresh the connection list only.
		return m.requestSnapshot()

	case strings.HasPrefix(evt.Type, "connection."):
		// Reload snapshot on change.
		return m.requestSnapshot()
	}
	return nil
}

// --------------- navigation helpers ---------------

// pushScreen saves the current screen to the navigation stack and transitions
// to the given screen. This enables back-navigation with Esc.
func (m *Model) pushScreen(target ScreenID) {
	if m.screen != "" && m.screen != ScreenBoot && m.screen != target {
		m.navStack = append(m.navStack, m.screen)
	}
	m.screen = target
}

// popScreen returns to the previous screen from the navigation stack.
// Returns false if the stack is empty (already at root).
func (m *Model) popScreen() bool {
	if len(m.navStack) == 0 {
		return false
	}
	prev := m.navStack[len(m.navStack)-1]
	m.navStack = m.navStack[:len(m.navStack)-1]
	m.screen = prev
	return true
}

// clearNav resets the navigation stack (e.g., when returning to home explicitly).
func (m *Model) clearNav() {
	m.navStack = nil
}

// --------------- key handling ---------------

func (m Model) handleKeyPress(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	key := msg.String()

	if key == "ctrl+c" {
		// Shutdown must not leave a collected secret resident in the model.
		m.clearProviderSetupSecret()
		if m.rootCancel != nil {
			m.rootCancel()
		}
		return m, tea.Quit
	}

	// While the wizard is active, it owns the keyboard.
	if m.screen == ScreenNewConnection {
		return m.handleWizardKey(key)
	}

	// While provider setup is active, handle it specially.
	if m.providerSetupStep > 0 {
		return m.handleProviderSetupKey(key)
	}

	// While the setup screen is active, it owns navigation.
	if m.screen == ScreenSetup && m.setup != nil {
		switch key {
		case "up", "k", "down", "j":
			m.setup.HandleKey(key)
			return m, nil
		case "r":
			return m, m.readinessCmd()
		case "enter":
			// Setting up the highlighted provider goes through the existing
			// account flow rather than a second, parallel one.
			if selected := m.setup.Selected(); selected != nil && selected.ID == "cloudflare" {
				m.providerSetupStep = 1
				m.pushScreen(ScreenProviders)
			}
			return m, nil
		}
	}

	// While the inspect screen is active, delegate tab navigation to the InspectModel.
	if m.screen == ScreenInspect && m.inspect != nil {
		switch key {
		case "left", "h", "right", "l":
			m.inspect.HandleKey(key)
			return m, nil
		}
	}

	switch key {
	case "q":
		if m.screen == ScreenHome {
			if m.rootCancel != nil {
				m.rootCancel()
			}
			m.clearProviderSetupSecret()
			return m, tea.Quit
		}
		m.screen = ScreenHome
		return m, nil

	case "esc":
		if m.screen == ScreenHelp {
			// Return to the screen that opened help.
			m.screen = m.prevScreen
			m.prevScreen = ""
			return m, nil
		}
		if m.screen == ScreenHome {
			// Can't go back from home
			return m, nil
		}
		// Try to pop the navigation stack
		if !m.popScreen() {
			// Stack empty, go to home
			m.screen = ScreenHome
		}
		return m, nil

	case "up", "k":
		if m.screen == ScreenDiscovery && m.discoverySelected > 0 {
			m.discoverySelected--
		} else if m.screen == ScreenOperations && m.opsSelectedIdx > 0 {
			m.opsSelectedIdx--
		} else if m.screen == ScreenHome {
			// Move selection up by finding the current index and decrementing.
			conns := m.ConnectionList()
			idx := m.selectedConnectionIndex()
			if idx > 0 {
				m.selectedID = conns[idx-1].ID
			}
		}

	case "down", "j":
		if m.screen == ScreenDiscovery {
			if m.discoverySelected < len(m.discovery)-1 {
				m.discoverySelected++
			}
		} else if m.screen == ScreenOperations {
			if m.opsSelectedIdx < len(m.operations)-1 {
				m.opsSelectedIdx++
			}
		} else if m.screen == ScreenHome {
			// Move selection down by finding the current index and incrementing.
			conns := m.ConnectionList()
			idx := m.selectedConnectionIndex()
			if idx >= 0 && idx < len(conns)-1 {
				m.selectedID = conns[idx+1].ID
			} else if idx < 0 && len(conns) > 0 {
				m.selectedID = conns[0].ID
			}
		}

	case "enter":
		if m.screen == ScreenHome && m.SelectedConnection() != nil {
			conn := m.SelectedConnection()
			m.inspect = screens.NewInspect(conn)
			// Detail from the previous connection must not be shown against
			// this one while the fetch is in flight.
			m.connectionDetail = nil
			m.connectionLogs = nil
			m.pushScreen(ScreenInspect)
			return m, tea.Batch(m.connectionDetailCmd(conn.ID), m.connectionLogsCmd(conn.ID))
		} else if m.screen == ScreenPlanPreview && m.plan != nil {
			if m.applying {
				return m, nil
			}
			// Apply the previewed plan asynchronously.
			m.applying = true
			m.status = "Applying plan..."
			// Track if this is a repair for post-repair verification
			if m.plan.Intent == "repair" {
				m.awaitingRepairVerification = true
			}
			return m, m.applyPlanCmd(m.plan.ID)
		} else if m.screen == ScreenRepair && m.SelectedConnection() != nil && len(m.diagnostics) > 0 {
			// Store pre-repair diagnostics for verification
			m.preRepairDiagnostics = make([]ipc.DiagnosticDTO, len(m.diagnostics))
			copy(m.preRepairDiagnostics, m.diagnostics)
			m.repairConnectionID = m.SelectedConnection().ID
			return m, m.planRepairCmd(m.SelectedConnection().ID)
		} else if m.screen == ScreenDiscovery && len(m.discovery) > 0 {
			svc := m.discovery[m.discoverySelected]
			m.wizard = screens.NewWizardForService(m.client, m.hasFullCloudflare(), m.cloudflareAccounts(), svc.Address, svc.Protocol).WithContext(m.rootCtx)
			m.status = ""
			m.pushScreen(ScreenNewConnection)
		}

	case "n":
		if m.screen == ScreenHome {
			m.wizard = screens.NewWizard(m.client, m.hasFullCloudflare(), m.cloudflareAccounts()).WithContext(m.rootCtx)
			m.status = ""
			m.pushScreen(ScreenNewConnection)
		}

	case "r":
		if m.screen == ScreenRecovery {
			// Retry connection to supervisor
			m.err = nil
			m.screen = ScreenBoot
			return m, m.requestSnapshot()
		}
		if m.screen == ScreenRepair && m.SelectedConnection() != nil {
			// Clear pre-repair diagnostics when manually re-running
			m.preRepairDiagnostics = nil
			m.repairConnectionID = ""
			m.diagnostics = nil
			return m, m.diagnosticsCmd(m.SelectedConnection().ID)
		}
		if m.screen == ScreenHome && m.SelectedConnection() != nil {
			m.pushScreen(ScreenRepair)
			m.diagnostics = nil
			return m, m.diagnosticsCmd(m.SelectedConnection().ID)
		}

	case "a":
		if m.screen == ScreenHome {
			m.status = "Discovering local services..."
			return m, m.discoveryCmd()
		}
		if m.screen == ScreenProviders {
			// Start provider account setup
			m.providerSetupStep = 1
			m.providerSetupID = ""
			m.providerSetupLabel = ""
			m.providerSetupZoneID = ""
			m.providerSetupCred = ""
			m.providerSetupError = ""
			return m, nil
		}

	case "d":
		if m.screen == ScreenHome && m.SelectedConnection() != nil {
			return m, m.planDeleteCmd(m.SelectedConnection().ID)
		}
		if m.screen == ScreenRepair && m.SelectedConnection() != nil {
			// Re-run diagnostics.
			m.diagnostics = nil
			return m, m.diagnosticsCmd(m.SelectedConnection().ID)
		}

	case "o":
		if m.screen == ScreenHome {
			m.pushScreen(ScreenOperations)
			return m, m.loadOperationsCmd()
		}

	case "s":
		if m.setup == nil {
			m.setup = screens.NewSetup()
		}
		m.pushScreen(ScreenSetup)
		return m, m.readinessCmd()

	case "p":
		m.pushScreen(ScreenProviders)

	case "?":
		m.prevScreen = m.screen
		m.screen = ScreenHelp

	case "space", " ":
		if m.screen == ScreenHome && m.SelectedConnection() != nil {
			conn := m.SelectedConnection()
			// Plan asynchronously; planLoadedMsg moves to the preview screen.
			if conn.DesiredState == "open" {
				return m, m.planCloseCmd(conn.ID)
			}
			return m, m.planOpenCmd(conn.ID)
		}
	}

	return m, nil
}

func (m *Model) hasFullCloudflare() bool {
	for _, provider := range m.snapshot.Providers {
		if provider.ID == "cloudflare" {
			return provider.Authenticated
		}
	}
	return false
}

func (m *Model) cloudflareAccounts() []ipc.ProviderAccountDTO {
	for _, provider := range m.snapshot.Providers {
		if provider.ID == "cloudflare" {
			return append([]ipc.ProviderAccountDTO(nil), provider.Accounts...)
		}
	}
	return nil
}

// handleWizardKey routes keys to the new-connection wizard.
func (m Model) handleWizardKey(key string) (Model, tea.Cmd) {
	if m.wizard == nil {
		m.screen = ScreenHome
		m.clearNav()
		return m, nil
	}

	switch m.wizard.Step() {
	case screens.WizardStepIntent:
		// Backing out of the first step returns home.
		if key == "esc" || key == "q" {
			m.wizard = nil
			m.screen = ScreenHome
			m.clearNav()
			return m, nil
		}
	case screens.WizardStepComplete:
		// Done — return home and refresh.
		if key == "enter" || key == "esc" || key == "q" {
			m.wizard = nil
			m.screen = ScreenHome
			m.clearNav()
			return m, m.requestSnapshot()
		}
	case screens.WizardStepCreating:
		// Creation in flight; ignore input until the result arrives.
		return m, nil
	}

	return m, m.wizard.HandleKey(key)
}

// clearProviderSetupSecret drops the credential from model memory.
//
// Visual masking hides a secret from the screen, not from the process. The
// credential must not survive leaving the step that collected it, so every exit
// path — cancel, back past the credential step, validation failure, success and
// shutdown — calls this rather than relying on the success path alone.
func (m *Model) clearProviderSetupSecret() {
	m.providerSetupCred = ""
}

// clearProviderSetup resets the whole setup flow, secret included.
func (m *Model) clearProviderSetup() {
	m.providerSetupStep = 0
	m.providerSetupID = ""
	m.providerSetupLabel = ""
	m.providerSetupZoneID = ""
	m.providerSetupError = ""
	m.clearProviderSetupSecret()
}

func (m Model) handleProviderSetupKey(key string) (Model, tea.Cmd) {
	switch m.providerSetupStep {
	case 1: // Account ID
		if key == "esc" {
			// Leaving setup entirely: nothing collected may persist.
			m.clearProviderSetup()
			return m, nil
		}
		if key == "enter" {
			if m.providerSetupID == "" {
				m.providerSetupError = "Account ID cannot be empty"
				return m, nil
			}
			m.providerSetupStep = 2
			m.providerSetupError = ""
			return m, nil
		}
		m.providerSetupID = editString(m.providerSetupID, key)
		return m, nil

	case 2: // Label
		if key == "esc" {
			m.providerSetupStep = 1
			m.providerSetupError = ""
			return m, nil
		}
		if key == "enter" {
			m.providerSetupStep = 3
			m.providerSetupError = ""
			return m, nil
		}
		m.providerSetupLabel = editString(m.providerSetupLabel, key)
		return m, nil

	case 3: // Zone ID
		if key == "esc" {
			m.providerSetupStep = 2
			m.providerSetupError = ""
			return m, nil
		}
		if key == "enter" {
			m.providerSetupStep = 4
			m.providerSetupError = ""
			return m, nil
		}
		m.providerSetupZoneID = editString(m.providerSetupZoneID, key)
		return m, nil

	case 4: // Credential
		if key == "esc" {
			// Moving back past the credential step must not leave the secret
			// resident while the user edits earlier answers.
			m.providerSetupStep = 3
			m.providerSetupError = ""
			m.clearProviderSetupSecret()
			return m, nil
		}
		if key == "enter" {
			if m.providerSetupCred == "" {
				m.providerSetupError = "Credential cannot be empty"
				return m, nil
			}
			m.providerSetupStep = 5
			m.providerSetupError = ""
			return m, nil
		}
		m.providerSetupCred = editString(m.providerSetupCred, key)
		return m, nil

	case 5: // Confirm
		if key == "esc" {
			m.providerSetupStep = 4
			m.providerSetupError = ""
			return m, nil
		}
		if key == "enter" {
			// Submit the configuration
			req := ipc.ConfigureProviderAccountRequest{
				AccountID:  m.providerSetupID,
				Label:      m.providerSetupLabel,
				ZoneID:     m.providerSetupZoneID,
				Credential: m.providerSetupCred,
			}
			return m, m.configureProviderAccountCmd("cloudflare", req)
		}
		return m, nil
	}

	return m, nil
}

// editString is a simple string editor for terminal input.
// editString applies one key press to a text field.
//
// Editing is rune-aware. The previous implementation tested len(key) == 1,
// which silently discarded every multi-byte character, so accented Latin,
// Arabic, CJK and emoji input did nothing at all. Backspace sliced a byte off
// the end, which split multi-byte code points and produced invalid UTF-8.
func editString(s string, key string) string {
	if key == "backspace" {
		if s == "" {
			return s
		}
		_, size := utf8.DecodeLastRuneInString(s)
		return s[:len(s)-size]
	}
	// A printable key press is one rune, however many bytes it occupies.
	// Named keys ("enter", "left", …) are longer than one rune and are not
	// text input.
	if utf8.RuneCountInString(key) == 1 {
		r, _ := utf8.DecodeRuneInString(key)
		// Control characters are commands, not content.
		if r == utf8.RuneError || r < 0x20 || r == 0x7f {
			return s
		}
		return s + key
	}
	return s
}

// --------------- view models ---------------

func (m *Model) ConnectionList() []ipc.ConnectionDTO {
	if m.snapshot.Connections == nil {
		return nil
	}
	return m.snapshot.Connections
}

func (m *Model) SelectedConnection() *ipc.ConnectionDTO {
	conns := m.ConnectionList()
	if m.selectedID == "" {
		return nil
	}
	for i := range conns {
		if conns[i].ID == m.selectedID {
			return &conns[i]
		}
	}
	return nil
}

// selectedConnectionIndex returns the index of the selected connection in the
// current list, or -1 if not found or no selection.
func (m *Model) selectedConnectionIndex() int {
	if m.selectedID == "" {
		return -1
	}
	conns := m.ConnectionList()
	for i := range conns {
		if conns[i].ID == m.selectedID {
			return i
		}
	}
	return -1
}

// --------------- rendering ---------------

func (m *Model) renderLoading() string {
	return lipgloss.NewStyle().
		Foreground(m.theme.Muted).
		Render("Portico — connecting to supervisor...")
}

// renderError presents a failure as an intervention: what happened, why, and
// what to do next, with the original error text kept under technical details
// rather than shown as the primary message.
func (m *Model) renderError() string {
	ufe := describeError(m.err)

	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Intervention).Render(ufe.Summary))
	if ufe.Explanation != "" {
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Text).Render(ufe.Explanation))
	}
	if len(ufe.NextActions) > 0 {
		b.WriteString("\n\nWhat you can do:\n")
		for _, action := range ufe.NextActions {
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Text).Render("  • " + action))
			b.WriteString("\n")
		}
	}
	if ufe.Technical != "" && ufe.Technical != ufe.Summary {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Muted).
			Render("Technical details: " + ufe.Technical))
		b.WriteString("\n")
	}
	if ufe.Code != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Muted).Render("Code: " + ufe.Code))
	}
	return b.String()
}

func (m *Model) renderRecovery() string {
	var b strings.Builder
	b.WriteString(renderHeader(m.width, m.theme, m.useASCII))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().
		Foreground(m.theme.Intervention).
		Render("Unable to connect to supervisor\n"))
	b.WriteString("\n")
	if m.err != nil {
		b.WriteString(lipgloss.NewStyle().
			Foreground(m.theme.Muted).
			Render(fmt.Sprintf("Error: %s\n\n", m.err)))
	}
	b.WriteString(lipgloss.NewStyle().
		Foreground(m.theme.Text).
		Render("This usually means:\n"))
	b.WriteString("  • The supervisor is not running\n")
	b.WriteString("  • The socket file is missing or inaccessible\n")
	b.WriteString("  • Permission denied\n")
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().
		Foreground(m.theme.Stable).
		Render("[r] Retry connection\n"))
	b.WriteString(lipgloss.NewStyle().
		Foreground(m.theme.Muted).
		Render("[q] Quit"))
	return b.String()
}

func (m *Model) renderHome() string {
	if m.width > 0 && Breakpoint(m.width) == LayoutEmergency {
		return m.renderEmergencyHome()
	}
	var b strings.Builder

	b.WriteString(renderHeader(m.width, m.theme, m.useASCII))
	b.WriteString("\n\n")
	b.WriteString(m.theme.Style("header").Render(" CONNECTIONS "))
	b.WriteString("\n")

	// Calculate available height for connection list
	// Reserve space for: header (1) + title (1) + route (3-5) + status (2) + help (2) = ~10 lines
	availableHeight := m.height - 10
	if availableHeight < 5 {
		availableHeight = 5
	}

	// Render connection list with viewport tracking
	conns := m.ConnectionList()
	selectedIdx := m.selectedConnectionIndex()

	// Adjust listOffset so the selected item is always visible.
	if selectedIdx >= 0 {
		if selectedIdx < m.listOffset {
			m.listOffset = selectedIdx
		} else if selectedIdx >= m.listOffset+availableHeight {
			m.listOffset = selectedIdx - availableHeight + 1
		}
	}
	// Clamp offset to valid range.
	if m.listOffset < 0 {
		m.listOffset = 0
	}
	if m.listOffset > len(conns)-1 && len(conns) > 0 {
		m.listOffset = len(conns) - 1
	}

	// Slice the visible window.
	visibleEnd := m.listOffset + availableHeight
	if visibleEnd > len(conns) {
		visibleEnd = len(conns)
	}
	visibleConns := conns
	if m.listOffset < len(conns) {
		visibleConns = conns[m.listOffset:visibleEnd]
	} else {
		visibleConns = nil
	}

	connListStr := renderConnectionList(visibleConns, m.selectedID, m.width, m.theme, m.useASCII)
	lines := strings.Split(connListStr, "\n")

	// Show scroll indicators when list is truncated.
	if m.listOffset > 0 {
		b.WriteString(m.theme.Style("muted").Render(fmt.Sprintf("  ↑ %d above", m.listOffset)))
		b.WriteString("\n")
	}

	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}

	if visibleEnd < len(conns) {
		b.WriteString(m.theme.Style("muted").Render(fmt.Sprintf("  ↓ %d more", len(conns)-visibleEnd)))
		b.WriteString("\n")
	}

	if selected := m.SelectedConnection(); selected != nil {
		state := route.RouteOpen
		switch selected.UserState {
		case "Open":
			state = route.RouteOpen
		case "Closed":
			state = route.RouteClosed
		case "Unstable":
			state = route.RouteDegraded
		default:
			state = route.RouteUnknown
		}
		vm := route.RouteVM{
			LocalLabel:    selected.Name,
			EndpointLabel: selected.PublicAddress,
			State:         state,
		}
		routeStr := route.RenderRoute(vm, m.width, m.useASCII)
		if routeStr != "" {
			b.WriteString("\n")
			b.WriteString(routeStr)
		}
	}

	if m.status != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("intervention").Render("  " + m.status))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(helpRow(m.keys, m.theme))
	return b.String()
}

// renderNewConnection renders the new-connection wizard screen.
func (m *Model) renderNewConnection() string {
	if m.wizard == nil {
		return m.renderHome()
	}
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" NEW CONNECTION "))
	b.WriteString("\n\n")
	b.WriteString(m.wizard.View())
	if m.status != "" {
		b.WriteString("\n\n")
		b.WriteString(m.theme.Style("intervention").Render("  " + m.status))
	}
	return b.String()
}

func (m *Model) renderInspect() string {
	conn := m.SelectedConnection()
	if conn == nil {
		return "No connection selected"
	}

	// Use the composed InspectModel if available (provides tabbed view).
	if m.inspect != nil {
		// Keep the InspectModel's connection data in sync with the snapshot.
		m.inspect.Connection = conn
		m.inspect.Diagnostics = m.diagnostics
		m.inspect.Detail = m.connectionDetail
		m.inspect.LogTail = m.connectionLogs

		var b strings.Builder
		b.WriteString(m.theme.Style("header").Render(fmt.Sprintf(" %s ", conn.Name)))
		b.WriteString("\n\n")
		b.WriteString(m.inspect.View())
		return b.String()
	}

	// Fallback: basic inspect view when InspectModel is not initialized.
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(fmt.Sprintf(" %s ", conn.Name)))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("State:    %s\n", conn.UserState))
	b.WriteString(fmt.Sprintf("Provider: %s\n", conn.ProviderID))
	if conn.ProviderAccountID != "" {
		b.WriteString(fmt.Sprintf("Account:  %s\n", conn.ProviderAccountID))
	}
	if conn.PublicAddress != "" {
		b.WriteString(fmt.Sprintf("Public:   %s\n", conn.PublicAddress))
	}
	if conn.PrivateAddress != "" {
		b.WriteString(fmt.Sprintf("Local:    %s\n", conn.PrivateAddress))
	}
	if conn.Error != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("intervention").Render(conn.Error))
		b.WriteString("\n")
	}
	b.WriteString("\n[esc] back    [q] quit\n")
	return b.String()
}

func (m *Model) renderPlanPreview() string {
	if m.plan == nil {
		return "No plan"
	}
	var b strings.Builder

	// Title with explicit action
	intent := m.plan.Intent
	if intent == "" {
		intent = "unknown"
	}
	b.WriteString(m.theme.Style("header").Render(fmt.Sprintf(" [%s] ", strings.ToUpper(intent))))
	b.WriteString("\n\n")

	// Connection and provider/account
	conn := m.SelectedConnection()
	if conn != nil {
		b.WriteString(fmt.Sprintf("Connection: %s\n", conn.Name))
	}
	b.WriteString(fmt.Sprintf("Provider:   %s\n", m.plan.Provider))
	if conn != nil && conn.ProviderAccountID != "" {
		b.WriteString(fmt.Sprintf("Account:    %s\n", conn.ProviderAccountID))
	}

	// Consequences before implementation terminology: what this achieves, who
	// can reach it, what changes locally and at the provider, and what can be
	// undone. The step list follows as the exact technical plan.
	if m.plan.Outcome != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("header").Render(" OUTCOME "))
		b.WriteString("\n")
		b.WriteString(m.theme.Style("stable").Render("  " + m.plan.Outcome))
		b.WriteString("\n")
	}
	if m.plan.Access != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("header").Render(" ACCESS "))
		b.WriteString("\n  ")
		b.WriteString(m.plan.Access)
		b.WriteString("\n")
	}
	writeSection := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		b.WriteString("\n")
		b.WriteString(m.theme.Style("header").Render(" " + title + " "))
		b.WriteString("\n")
		for _, item := range items {
			b.WriteString("  " + item + "\n")
		}
	}
	writeSection("LOCAL CHANGES", m.plan.LocalChanges)
	writeSection("PROVIDER CHANGES", m.plan.ProviderChanges)
	writeSection("REVERSIBILITY", m.plan.Reversibility)

	// Warnings
	if len(m.plan.Warnings) > 0 {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("attention").Render("Warnings:"))
		b.WriteString("\n")
		for _, warning := range m.plan.Warnings {
			b.WriteString(fmt.Sprintf("  ! %s\n", warning))
		}
	}

	// Exact technical plan.
	b.WriteString("\nTechnical plan:\n")
	hasIrreversible := false
	for i, step := range m.plan.Steps {
		mark := " "
		if step.Destructive {
			mark = "!"
		}
		if step.Irreversible {
			mark = "X"
			hasIrreversible = true
		}
		b.WriteString(fmt.Sprintf("  [%s] %d. %s", mark, i+1, step.Summary))
		if step.Destructive {
			b.WriteString(" [destructive]")
		}
		if step.Irreversible {
			b.WriteString(" [irreversible]")
		}
		b.WriteString("\n")
	}

	// Plan fingerprint (for debugging/staleness detection)
	if m.plan.Fingerprint != "" {
		b.WriteString("\n")
		fp := m.plan.Fingerprint
		if len(fp) > 8 {
			fp = fp[:8]
		}
		b.WriteString(m.theme.Style("muted").Render(fmt.Sprintf("Fingerprint: %s", fp)))
		b.WriteString("\n")
	}

	if m.status != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("intervention").Render("  " + m.status))
		b.WriteString("\n")
	}
	if m.applying {
		b.WriteString("\nApplying...\n")
	} else {
		b.WriteString("\n")
		if hasIrreversible || intent == "delete" {
			b.WriteString(m.theme.Style("intervention").Render("This operation cannot be undone."))
			b.WriteString("\n")
			b.WriteString("[esc] cancel    [enter] confirm    [q] quit\n")
		} else {
			b.WriteString("[esc] cancel    [enter] apply    [q] quit\n")
		}
	}
	return b.String()
}

func (m *Model) renderOperationProgress() string {
	if m.operation == nil {
		return "No operation in progress"
	}
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(fmt.Sprintf(" OPERATION: %s ", m.operation.State)))
	b.WriteString("\n\n")

	// Show steps with execution states
	for _, step := range m.operation.Steps {
		// Determine step status icon
		icon := "○" // pending
		switch step.State {
		case "running":
			icon = "◐"
		case "succeeded":
			icon = "●"
		case "failed":
			icon = "✗"
		case "compensated":
			icon = "↩"
		case "skipped":
			icon = "⊘"
		}

		// Apply color based on state
		stepText := fmt.Sprintf("  %s %s", icon, step.Summary)
		switch step.State {
		case "succeeded":
			stepText = m.theme.Style("stable").Render(stepText)
		case "failed":
			stepText = m.theme.Style("intervention").Render(stepText)
		case "running":
			stepText = m.theme.Style("attention").Render(stepText)
		case "compensated", "skipped":
			stepText = m.theme.Style("muted").Render(stepText)
		}

		b.WriteString(stepText)
		b.WriteString("\n")

		// Show error if present
		if step.Error != "" {
			b.WriteString(m.theme.Style("intervention").Render("      Error: " + step.Error))
			b.WriteString("\n")
		}
	}

	// Show operation events
	if len(m.opEvents) > 0 {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("muted").Render("Events:"))
		b.WriteString("\n")
		for _, evt := range m.opEvents {
			b.WriteString(fmt.Sprintf("  * %s\n", evt))
		}
	}

	b.WriteString(fmt.Sprintf("\nState: %s\n", m.operation.State))
	if m.operation.Error != "" {
		b.WriteString(m.theme.Style("intervention").Render(fmt.Sprintf("Error: %s\n", m.operation.Error)))
	}
	if m.status != "" {
		b.WriteString(m.theme.Style("intervention").Render("  " + m.status))
		b.WriteString("\n")
	}
	b.WriteString("\n[esc] back\n")
	return b.String()
}

// providerStateLabel maps a provider's availability to a plain-language label
// and a theme style. Each state is named explicitly so a provider whose client
// is missing is never presented the same way as one that is merely
// unconfigured, or as one Portico does not implement at all.
func providerStateLabel(availability string) (string, string) {
	switch availability {
	case "ready":
		return "Ready", "stable"
	case "unconfigured":
		return "Setup required", "attention"
	case "client_missing":
		return "Client not installed", "attention"
	case "experimental":
		return "Experimental — not usable", "attention"
	case "not_implemented":
		return "Not implemented", "muted"
	case "degraded":
		return "Temporarily unavailable", "intervention"
	default:
		return "Unknown", "muted"
	}
}

func (m *Model) renderProviders() string {
	// If in provider setup mode, show the setup UI
	if m.providerSetupStep > 0 {
		return m.renderProviderSetup()
	}

	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" PROVIDERS "))
	b.WriteString("\n\n")
	if len(m.snapshot.Providers) == 0 {
		b.WriteString("No providers are catalogued.\n\n")
	}
	for _, p := range m.snapshot.Providers {
		label, style := providerStateLabel(p.Availability)
		b.WriteString(fmt.Sprintf("  %s  ", p.DisplayName))
		b.WriteString(m.theme.Style(style).Render(label))
		b.WriteString("\n")

		// A provider that cannot be used must say why and what to do about it,
		// rather than being omitted or shown as merely unconfigured.
		if p.LastError != "" {
			b.WriteString(m.theme.Style("muted").Render("    " + p.LastError))
			b.WriteString("\n")
		}
		for _, action := range p.SetupActions {
			b.WriteString(m.theme.Style("muted").Render("    → " + action))
			b.WriteString("\n")
		}

		// Capabilities are only meaningful for a provider with a live adapter.
		if p.Capabilities != nil && p.Availability != "not_implemented" && p.Availability != "client_missing" {
			if p.Capabilities.TemporaryAddresses {
				b.WriteString(m.theme.Style("stable").Render("    ✓ Temporary addresses"))
				b.WriteString("\n")
			}
			renderCapability := func(supported bool, name string) {
				if supported && p.Authenticated {
					b.WriteString(m.theme.Style("stable").Render("    ✓ " + name))
				} else if supported {
					b.WriteString(m.theme.Style("muted").Render("    ○ " + name + " (account setup required)"))
				} else {
					b.WriteString(m.theme.Style("muted").Render("    ✗ " + name + " (not supported)"))
				}
				b.WriteString("\n")
			}
			renderCapability(p.Capabilities.CustomHostnames, "Permanent hostnames")
			renderCapability(len(p.Capabilities.ProtectionModes) > 0, "Access protection")
		}

		// Show configured accounts
		for _, account := range p.Accounts {
			accLabel := account.Label
			if accLabel == "" {
				accLabel = account.ID
			}
			accountStatus := account.Status
			if accountStatus == "" {
				accountStatus = "configured"
			}
			b.WriteString(fmt.Sprintf("      • %s — %s\n", accLabel, accountStatus))
		}
		b.WriteString("\n")
	}
	b.WriteString("[a] add account    [esc] back    [q] quit\n")
	return b.String()
}

func (m *Model) renderProviderSetup() string {
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" ADD CLOUDFLARE ACCOUNT "))
	b.WriteString("\n\n")

	switch m.providerSetupStep {
	case 1:
		b.WriteString("Step 1/5: Account ID\n\n")
		b.WriteString("Enter your Cloudflare account ID:\n")
		b.WriteString(fmt.Sprintf("> %s_\n", m.providerSetupID))
	case 2:
		b.WriteString("Step 2/5: Label\n\n")
		b.WriteString("Enter a friendly label for this account:\n")
		b.WriteString(fmt.Sprintf("> %s_\n", m.providerSetupLabel))
	case 3:
		b.WriteString("Step 3/5: Zone ID\n\n")
		b.WriteString("Enter your Cloudflare zone ID, or press enter to skip.\n")
		b.WriteString("A zone is only needed for permanent hostnames and DNS.\n")
		b.WriteString("Without one you can still create tunnels with temporary addresses.\n")
		b.WriteString(fmt.Sprintf("> %s_\n", m.providerSetupZoneID))
	case 4:
		b.WriteString("Step 4/5: API Token\n\n")
		b.WriteString("Enter your Cloudflare API token:\n")
		// Mask the credential for security
		maskedCred := ""
		if len(m.providerSetupCred) > 0 {
			maskedCred = "••••••••"
		}
		b.WriteString(fmt.Sprintf("> %s_\n", maskedCred))
	case 5:
		b.WriteString("Step 5/5: Confirm\n\n")
		b.WriteString("Review your configuration:\n\n")
		b.WriteString(fmt.Sprintf("  Account ID: %s\n", m.providerSetupID))
		b.WriteString(fmt.Sprintf("  Label:      %s\n", m.providerSetupLabel))
		if m.providerSetupZoneID != "" {
			b.WriteString(fmt.Sprintf("  Zone ID:    %s\n", m.providerSetupZoneID))
		}
		b.WriteString("  API Token:  ••••••••\n\n")
		b.WriteString("Press enter to confirm, or esc to go back and edit.\n")
	}

	if m.providerSetupError != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.Style("intervention").Render("Error: " + m.providerSetupError))
		b.WriteString("\n")
	}

	b.WriteString("\n[esc] back    [enter] continue\n")
	return b.String()
}

func (m *Model) renderRepair() string {
	conn := m.SelectedConnection()
	if conn == nil {
		return "No connection selected"
	}
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(fmt.Sprintf(" REPAIR: %s ", conn.Name)))
	b.WriteString("\n\n")

	// Show verification results if we have pre-repair diagnostics
	if len(m.preRepairDiagnostics) > 0 && m.repairConnectionID == conn.ID {
		b.WriteString(m.theme.Style("header").Render(" REPAIR VERIFICATION "))
		b.WriteString("\n\n")

		preCount := len(m.preRepairDiagnostics)
		postCount := len(m.diagnostics)

		if postCount == 0 {
			b.WriteString(m.theme.Style("stable").Render("✓ All issues resolved!"))
			b.WriteString("\n\n")
			b.WriteString(fmt.Sprintf("Fixed %d issue(s):\n", preCount))
			for _, d := range m.preRepairDiagnostics {
				b.WriteString(fmt.Sprintf("  ✓ %s: %s\n", d.Segment, d.Summary))
			}
		} else if postCount < preCount {
			b.WriteString(m.theme.Style("attention").Render("⚠ Partial repair"))
			b.WriteString("\n\n")
			b.WriteString(fmt.Sprintf("Fixed %d of %d issue(s)\n\n", preCount-postCount, preCount))

			b.WriteString("Remaining issues:\n")
			for _, d := range m.diagnostics {
				b.WriteString(fmt.Sprintf("  ✗ %s: %s\n", d.Segment, d.Summary))
				if d.Explanation != "" {
					b.WriteString("      " + d.Explanation + "\n")
				}
			}
		} else {
			b.WriteString(m.theme.Style("intervention").Render("✗ Repair did not resolve issues"))
			b.WriteString("\n\n")
			b.WriteString("Current issues:\n")
			for _, d := range m.diagnostics {
				b.WriteString(fmt.Sprintf("  ✗ %s: %s\n", d.Segment, d.Summary))
				if d.Explanation != "" {
					b.WriteString("      " + d.Explanation + "\n")
				}
			}
		}

		b.WriteString("\n[r] run diagnostics again    [esc] back    [q] quit\n")
		return b.String()
	}

	// Normal diagnostic display
	if m.diagnostics == nil {
		b.WriteString("Running diagnostics...\n")
	} else if len(m.diagnostics) == 0 {
		b.WriteString("No findings — connection looks healthy.\n")
	} else {
		for _, d := range m.diagnostics {
			b.WriteString(fmt.Sprintf("[%s] %s: %s\n", d.Severity, d.Segment, d.Summary))
			if d.Explanation != "" {
				b.WriteString("      " + d.Explanation + "\n")
			}
		}
	}
	if len(m.diagnostics) > 0 {
		b.WriteString("\n[enter] preview repair    [r] run diagnostics    [esc] cancel    [q] quit\n")
	} else {
		b.WriteString("\n[r] run diagnostics    [esc] cancel    [q] quit\n")
	}
	return b.String()
}

func (m *Model) renderDiscovery() string {
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" DISCOVER LOCAL SERVICES "))
	b.WriteString("\n\n")
	if len(m.discovery) == 0 {
		b.WriteString("No local listeners found.\n")
	} else {
		for i, svc := range m.discovery {
			prefix := "  "
			if i == m.discoverySelected {
				prefix = "> "
			}
			label := svc.Address
			if svc.Process != "" {
				separator := " — "
				if m.useASCII {
					separator = " - "
				}
				label += separator + svc.Process
			}
			b.WriteString(fmt.Sprintf("%s%s (%s, %s)\n", prefix, label, svc.Protocol, svc.Confidence))
		}
	}
	b.WriteString("\n[enter] use selected service    [esc] back    [q] quit\n")
	return b.String()
}

// operationDuration reports how long a completed operation took. It returns
// false when either timestamp is missing or unparseable, so the caller omits
// the field rather than rendering a misleading zero.
func operationDuration(op ipc.OperationDTO) (time.Duration, bool) {
	if op.StartedAt == "" || op.CompletedAt == "" {
		return 0, false
	}
	started, err := time.Parse(time.RFC3339, op.StartedAt)
	if err != nil {
		return 0, false
	}
	completed, err := time.Parse(time.RFC3339, op.CompletedAt)
	if err != nil {
		return 0, false
	}
	d := completed.Sub(started)
	if d < 0 {
		return 0, false
	}
	return d, true
}

func (m *Model) renderOperations() string {
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" OPERATIONS "))
	b.WriteString("\n\n")

	if len(m.operations) == 0 {
		// "No operations found" is an authoritative claim that no work has
		// happened. Only make it when history was actually read.
		if !m.operationsAvailable {
			b.WriteString(m.theme.Style("intervention").Render("Operation history is unavailable.\n"))
			if m.operationsUnavailable != "" {
				b.WriteString(fmt.Sprintf("\nReason: %s\n", m.operationsUnavailable))
			}
			b.WriteString("\nThis does not mean no operations have run.\n")
		} else {
			b.WriteString("No operations have run yet.\n")
		}
		b.WriteString("\n[esc] back    [q] quit\n")
		return b.String()
	}

	// Show operations list
	for i, op := range m.operations {
		prefix := "  "
		if i == m.opsSelectedIdx {
			prefix = "> "
		}

		// Format operation status.
		//
		// Operations and steps use different vocabularies: an operation reaches
		// "completed" (controller.OperationStateCompleted) while a step reaches
		// "succeeded" (store.StepSucceeded). This switch previously tested for
		// "succeeded", which an operation is never set to, so a successful
		// operation was never styled as one.
		status := op.State
		statusStyle := "muted"
		switch op.State {
		case "running", "pending":
			statusStyle = "attention"
		case "completed":
			statusStyle = "stable"
		case "failed":
			statusStyle = "intervention"
		}

		// The operation's intent comes from the plan it executed. This used to
		// display the plan ID, which is an opaque identifier, not an intent.
		intent := op.Intent
		if intent == "" {
			intent = "unknown"
		}

		// Format connection ID (truncate if too long)
		connID := op.ConnectionID
		if len(connID) > 20 {
			connID = connID[:17] + "..."
		}

		line := fmt.Sprintf("%s[%s] %s - %s", prefix, status, intent, connID)
		b.WriteString(m.theme.Style(statusStyle).Render(line))
		b.WriteString("\n")

		// Show selected operation details
		if i == m.opsSelectedIdx {
			b.WriteString("\n")
			b.WriteString(m.theme.Style("header").Render(" Operation Details "))
			b.WriteString("\n")
			b.WriteString(fmt.Sprintf("  ID:         %s\n", op.ID))
			b.WriteString(fmt.Sprintf("  Connection: %s\n", op.ConnectionID))
			b.WriteString(fmt.Sprintf("  Intent:     %s\n", intent))
			if op.ProviderID != "" {
				b.WriteString(fmt.Sprintf("  Provider:   %s\n", op.ProviderID))
			}
			b.WriteString(fmt.Sprintf("  Plan:       %s\n", op.PlanID))
			b.WriteString(fmt.Sprintf("  State:      %s\n", op.State))
			if op.StartedAt != "" {
				b.WriteString(fmt.Sprintf("  Started:    %s\n", op.StartedAt))
			}
			if op.CompletedAt != "" {
				b.WriteString(fmt.Sprintf("  Completed:  %s\n", op.CompletedAt))
			}
			if d, ok := operationDuration(op); ok {
				b.WriteString(fmt.Sprintf("  Duration:   %s\n", d))
			}
			if op.Fingerprint != "" {
				b.WriteString(fmt.Sprintf("  Plan hash:  %s\n", op.Fingerprint))
			}
			if op.Error != "" {
				b.WriteString(m.theme.Style("intervention").Render(fmt.Sprintf("  Error:      %s\n", op.Error)))
			}

			// Show steps if any
			if len(op.Steps) > 0 {
				b.WriteString("\n  Steps:\n")
				for _, step := range op.Steps {
					stepStatus := "○"
					stepStyle := "muted"
					switch step.State {
					case "running":
						stepStatus = "◐"
						stepStyle = "attention"
					case "succeeded":
						stepStatus = "●"
						stepStyle = "stable"
					case "failed":
						stepStatus = "✗"
						stepStyle = "intervention"
					case "skipped":
						stepStatus = "⊘"
					}
					stepLine := fmt.Sprintf("    %s %s", stepStatus, step.Summary)
					b.WriteString(m.theme.Style(stepStyle).Render(stepLine))
					b.WriteString("\n")
					if step.Error != "" {
						b.WriteString(m.theme.Style("intervention").Render(fmt.Sprintf("      Error: %s", step.Error)))
						b.WriteString("\n")
					}
				}
			}
		}
	}

	b.WriteString("\n[esc] back    [q] quit\n")
	return b.String()
}

func (m *Model) renderHelp() string {
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" HELP "))
	b.WriteString("\n\n")

	// Use prevScreen for contextual help since m.screen is ScreenHelp.
	sourceScreen := m.prevScreen
	if sourceScreen == "" {
		sourceScreen = ScreenHome
	}

	switch sourceScreen {
	case ScreenHome:
		b.WriteString("Home Screen:\n")
		b.WriteString("  n        New connection wizard\n")
		b.WriteString("  enter    Inspect selected connection\n")
		b.WriteString("  space    Open/close selected connection\n")
		b.WriteString("  r        Repair selected connection\n")
		b.WriteString("  a        Discover local services\n")
		b.WriteString("  d        Preview deletion of selected connection\n")
		b.WriteString("  o        View operations\n")
		b.WriteString("  p        Providers\n")
		b.WriteString("  s        Setup — what Portico needs and what is already configured\n")
		b.WriteString("  up/k     Select previous\n")
		b.WriteString("  down/j   Select next\n")
	case ScreenInspect:
		b.WriteString("Inspect Screen:\n")
		b.WriteString("  ←/h      Previous tab\n")
		b.WriteString("  →/l      Next tab\n")
		b.WriteString("  esc      Back to home\n")
	case ScreenPlanPreview:
		b.WriteString("Plan Preview:\n")
		b.WriteString("  enter    Apply plan\n")
		b.WriteString("  esc      Cancel\n")
	case ScreenOperationProgress:
		b.WriteString("Operation Progress:\n")
		b.WriteString("  esc      Back to home\n")
	case ScreenRepair:
		b.WriteString("Repair Screen:\n")
		b.WriteString("  enter    Preview repair plan\n")
		b.WriteString("  r        Re-run diagnostics\n")
		b.WriteString("  esc      Back to home\n")
	case ScreenDiscovery:
		b.WriteString("Discovery Screen:\n")
		b.WriteString("  up/k     Select previous service\n")
		b.WriteString("  down/j   Select next service\n")
		b.WriteString("  enter    Create connection from selected service\n")
		b.WriteString("  esc      Back to home\n")
	case ScreenOperations:
		b.WriteString("Operations Screen:\n")
		b.WriteString("  up/k     Select previous operation\n")
		b.WriteString("  down/j   Select next operation\n")
		b.WriteString("  esc      Back to home\n")
	case ScreenProviders:
		b.WriteString("Providers Screen:\n")
		b.WriteString("  a        Add account\n")
		b.WriteString("  esc      Back to home\n")
	case ScreenRecovery:
		b.WriteString("Recovery Screen:\n")
		b.WriteString("  r        Retry connection to supervisor\n")
		b.WriteString("  q        Quit\n")
	default:
		b.WriteString("General:\n")
	}

	b.WriteString("\nGlobal:\n")
	b.WriteString("  esc      Back to home (from most screens)\n")
	b.WriteString("  q        Quit (from home screen)\n")
	b.WriteString("  ctrl+c   Force quit\n")
	b.WriteString("  ?        This help screen\n")

	b.WriteString("\n[esc] back\n")
	return b.String()
}

func (m *Model) renderEmergencyHome() string {
	var b strings.Builder
	b.WriteString("Portico needs at least 60 columns.\n")
	if selected := m.SelectedConnection(); selected != nil {
		b.WriteString(fmt.Sprintf("\nSelected: %s\nState: %s\n", selected.Name, selected.UserState))
		if selected.Error != "" {
			b.WriteString("Problem: " + selected.Error + "\n")
		}
	}
	b.WriteString("\nResize the terminal or press r to repair.\n")
	return b.String()
}

func renderHeader(width int, th Theme, useASCII bool) string {
	header := " PORTICO "
	if width > 40 {
		// Account for the prefix (2 chars) and suffix (2 chars) in the padding.
		prefixLen := 2 // "┌─" or "+-"
		suffixLen := 2 // "─┐" or "-+"
		padding := width - len(header) - prefixLen - suffixLen
		if padding < 0 {
			padding = 0
		}
		line := "─"
		if useASCII {
			line = "-"
		}
		header += strings.Repeat(line, padding)
	}
	if useASCII {
		return th.Style("header").Render("+-" + header + "-+")
	}
	return th.Style("header").Render("┌─" + header + "─┐")
}

func renderConnectionList(conns []ipc.ConnectionDTO, selectedID string, width int, th Theme, useASCII bool) string {
	var b strings.Builder

	if len(conns) == 0 {
		b.WriteString(th.Style("muted").Render("  No connections"))
		b.WriteString("\n")
		return b.String()
	}

	for _, c := range conns {
		prefix := "  "
		style := th.Style("normal")
		if c.ID == selectedID {
			prefix = "▸ "
			if useASCII {
				prefix = "> "
			}
			style = th.Style("selected")
		}

		glyph := renderStateGlyph(c.UserState, useASCII)
		line := fmt.Sprintf("%s%s %s", prefix, glyph, c.Name)
		if c.UserState != "" {
			line += "  " + th.Style("muted").Render(strings.ToLower(c.UserState))
		}
		b.WriteString(style.Render(line))
		b.WriteString("\n")
	}

	return b.String()
}

func renderStateGlyph(state string, useASCII bool) string {
	if useASCII {
		switch state {
		case "Open":
			return "*"
		case "Unstable":
			return "o"
		case "Closed":
			return "O"
		case "Needs attention":
			return "X"
		default:
			return "."
		}
	}
	switch state {
	case "Open":
		return "●"
	case "Unstable":
		return "◐"
	case "Closed":
		return "○"
	case "Needs attention":
		return "╳"
	default:
		return "◌"
	}
}

// --------------- public helpers (for testing) ---------------

// ApplySnapshot updates the model with a snapshot and returns the rendered view string.
func (m *Model) ApplySnapshot(snap ipc.SnapshotDTO) string {
	m.snapshot = snap
	m.ready = true
	return m.renderHome()
}

// Ready returns true if the model has received its first snapshot.
func (m *Model) Ready() bool {
	return m.ready
}

// Ensure Model implements tea.Model.
var _ tea.Model = (*Model)(nil)

// SetAltScreen enables or disables the alternate screen.
func (m *Model) SetAltScreen(on bool) {
	// alt screen is set in View()
}
