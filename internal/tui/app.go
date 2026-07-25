package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/paoloanzn/portico/internal/ipc"
	"github.com/paoloanzn/portico/internal/tui/route"
	"github.com/paoloanzn/portico/internal/tui/screens"
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
	ScreenRepair            ScreenID = "repair"
	ScreenProviders         ScreenID = "providers"
	ScreenSettings          ScreenID = "settings"
	ScreenHelp              ScreenID = "help"
	ScreenQuit              ScreenID = "quit"
)

// Model is the root Bubble Tea model (SPEC §17.3).
type Model struct {
	width      int
	height     int
	ready      bool
	screen     ScreenID
	selectedID int

	snapshot    ipc.SnapshotDTO
	plan        *ipc.PlanDTO
	operation   *ipc.OperationDTO
	diagnostics []ipc.DiagnosticDTO
	opEvents    []string

	lastEventSeq int64
	stream       *ipc.EventStream
	streamCancel context.CancelFunc

	keys  KeyMap
	theme Theme

	wizard *screens.WizardModel

	client SupervisorClient
	err    error
	status string
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
	return Model{
		screen: ScreenHome,
		keys:   DefaultKeyMap,
		theme:  DefaultTheme,
		client: client,
	}
}

// --------------- Bubble Tea integration ---------------

// Init returns the startup command (SPEC §17.4).
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.requestSnapshot(),
		m.connectEventStream(),
	)
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
				// No initial state yet — nothing else to show.
				m.err = msg.Err
			} else {
				m.status = fmt.Sprintf("snapshot refresh failed: %v", msg.Err)
			}
			return m, nil
		}
		m.snapshot = msg.Snapshot
		m.ready = true
		return m, nil

	case planLoadedMsg:
		if msg.Err != nil {
			m.status = fmt.Sprintf("plan failed: %v", msg.Err)
			return m, nil
		}
		m.status = ""
		m.plan = msg.Plan
		m.screen = ScreenPlanPreview
		return m, nil

	case planAppliedMsg:
		if msg.Err != nil {
			m.status = fmt.Sprintf("apply failed: %v", msg.Err)
			return m, nil
		}
		m.status = ""
		m.operation = msg.Operation
		m.opEvents = nil
		m.plan = nil
		m.screen = ScreenOperationProgress
		return m, m.requestSnapshot()

	case diagnosticsMsg:
		if msg.Err != nil {
			m.status = fmt.Sprintf("diagnostics failed: %v", msg.Err)
			return m, nil
		}
		m.status = ""
		m.diagnostics = msg.Findings
		return m, nil

	case screens.ConnectionCreatedMsg:
		if m.wizard != nil {
			m.wizard.HandleCreated(msg)
		}
		if msg.Err != nil {
			m.status = fmt.Sprintf("create failed: %v", msg.Err)
			return m, nil
		}
		m.status = ""
		return m, m.requestSnapshot()

	case eventMsg:
		m.lastEventSeq = msg.Event.Sequence
		cmd := m.handleEvent(msg.Event)
		// Schedule next event wait
		return m, tea.Batch(cmd, m.waitForEvent())

	case errorMsg:
		m.err = msg.Err
		return m, nil

	case eventStreamReadyMsg:
		m.stream = msg.Stream
		m.streamCancel = msg.Cancel
		return m, m.waitForEvent()

	case resyncMsg:
		// Replay gap — reload snapshot
		return m, m.requestSnapshot()
	}

	return m, nil
}

// View renders the current state (SPEC §17.8 — pure).
func (m Model) View() tea.View {
	// Check error before ready — snapshot errors should display
	if m.err != nil {
		content := m.renderError()
		v := tea.NewView(content)
		v.AltScreen = true
		return v
	}
	if !m.ready {
		content := m.renderLoading()
		v := tea.NewView(content)
		v.AltScreen = true
		return v
	}

	var content string
	switch m.screen {
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
	default:
		content = m.renderHome()
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

// --------------- commands ---------------
//
// Commands capture what they need before returning the closure so that
// nothing running off the update loop reads or mutates the model.

func (m *Model) diagnosticsCmd(connID string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		if client == nil {
			return diagnosticsMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		findings, err := client.Diagnostics(context.Background(), connID)
		return diagnosticsMsg{Findings: findings, Err: err}
	}
}

func (m *Model) requestSnapshot() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		if client == nil {
			return snapshotMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		snap, err := client.GetSnapshot(context.Background())
		if err != nil {
			return snapshotMsg{Err: err}
		}
		return snapshotMsg{Snapshot: *snap}
	}
}

func (m *Model) planOpenCmd(connID string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		if client == nil {
			return planLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		plan, err := client.PlanOpen(context.Background(), connID)
		return planLoadedMsg{Plan: plan, Err: err}
	}
}

func (m *Model) planCloseCmd(connID string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		if client == nil {
			return planLoadedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		plan, err := client.PlanClose(context.Background(), connID)
		return planLoadedMsg{Plan: plan, Err: err}
	}
}

func (m *Model) applyPlanCmd(planID string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		if client == nil {
			return planAppliedMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		op, err := client.ApplyPlan(context.Background(), planID)
		return planAppliedMsg{Operation: op, Err: err}
	}
}

func (m *Model) connectEventStream() tea.Cmd {
	client := m.client
	lastSeq := m.lastEventSeq
	return func() tea.Msg {
		if client == nil {
			return errorMsg{Err: fmt.Errorf("no supervisor connection")}
		}
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := client.ConnectEventStream(ctx, lastSeq)
		if err != nil {
			cancel()
			return errorMsg{Err: fmt.Errorf("event stream: %w", err)}
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
			return errorMsg{Err: fmt.Errorf("event read: %w", err)}
		}
		return eventMsg{Event: *evt}
	}
}

// handleEvent reacts to a supervisor event. It only mutates the model on
// the update loop and returns any follow-up work as a command.
func (m *Model) handleEvent(evt ipc.EventDTO) tea.Cmd {
	switch {
	case evt.Type == "supervisor.shutdown":
		m.err = fmt.Errorf("supervisor shut down")
		return nil

	case strings.HasPrefix(evt.Type, "operation."):
		if data, ok := evt.Data.(map[string]interface{}); ok {
			if summary, ok := data["summary"].(string); ok && summary != "" {
				m.opEvents = append(m.opEvents, summary)
			}
		}
		// Operation progress changes connection state — reload snapshot.
		return m.requestSnapshot()

	case strings.HasPrefix(evt.Type, "connection."):
		// Reload snapshot on change.
		return m.requestSnapshot()
	}
	return nil
}

// --------------- key handling ---------------

func (m Model) handleKeyPress(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	key := msg.String()

	if key == "ctrl+c" {
		return m, tea.Quit
	}

	// While the wizard is active, it owns the keyboard.
	if m.screen == ScreenNewConnection {
		return m.handleWizardKey(key)
	}

	switch key {
	case "q":
		if m.screen == ScreenHome {
			return m, tea.Quit
		}
		m.screen = ScreenHome
		return m, nil

	case "esc":
		if m.screen != ScreenHome {
			m.screen = ScreenHome
		}
		return m, nil

	case "up", "k":
		if m.selectedID > 0 {
			m.selectedID--
		}

	case "down", "j":
		conns := m.ConnectionList()
		if m.selectedID < len(conns)-1 {
			m.selectedID++
		}

	case "enter":
		if m.screen == ScreenHome && m.SelectedConnection() != nil {
			m.screen = ScreenInspect
		} else if m.screen == ScreenPlanPreview && m.plan != nil {
			// Apply the previewed plan asynchronously.
			return m, m.applyPlanCmd(m.plan.ID)
		}

	case "n":
		if m.screen == ScreenHome {
			m.wizard = screens.NewWizard(m.client)
			m.status = ""
			m.screen = ScreenNewConnection
		}

	case "r":
		if m.screen == ScreenHome && m.SelectedConnection() != nil {
			m.screen = ScreenRepair
			m.diagnostics = nil
			return m, m.diagnosticsCmd(m.SelectedConnection().ID)
		}
		if m.screen == ScreenRepair && m.SelectedConnection() != nil {
			// Re-run diagnostics.
			m.diagnostics = nil
			return m, m.diagnosticsCmd(m.SelectedConnection().ID)
		}

	case "p":
		m.screen = ScreenProviders

	case "?":
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

// handleWizardKey routes keys to the new-connection wizard.
func (m Model) handleWizardKey(key string) (Model, tea.Cmd) {
	if m.wizard == nil {
		m.screen = ScreenHome
		return m, nil
	}

	switch m.wizard.Step() {
	case screens.WizardStepIntent:
		// Backing out of the first step returns home.
		if key == "esc" || key == "q" {
			m.wizard = nil
			m.screen = ScreenHome
			return m, nil
		}
	case screens.WizardStepComplete:
		// Done — return home and refresh.
		if key == "enter" || key == "esc" || key == "q" {
			m.wizard = nil
			m.screen = ScreenHome
			return m, m.requestSnapshot()
		}
	case screens.WizardStepCreating:
		// Creation in flight; ignore input until the result arrives.
		return m, nil
	}

	return m, m.wizard.HandleKey(key)
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
	if m.selectedID < 0 || m.selectedID >= len(conns) {
		return nil
	}
	return &conns[m.selectedID]
}

// --------------- rendering ---------------

func (m *Model) renderLoading() string {
	return lipgloss.NewStyle().
		Foreground(m.theme.Muted).
		Render("Portico — connecting to supervisor...")
}

func (m *Model) renderError() string {
	return lipgloss.NewStyle().
		Foreground(m.theme.Intervention).
		Render(fmt.Sprintf("Error: %s", m.err))
}

func (m *Model) renderHome() string {
	var b strings.Builder

	b.WriteString(renderHeader(m.width, m.theme))
	b.WriteString("\n\n")
	b.WriteString(HeaderStyle.Render(" CONNECTIONS "))
	b.WriteString("\n")
	b.WriteString(renderConnectionList(m.ConnectionList(), m.selectedID, m.width, m.theme))

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
		routeStr := route.RenderRoute(vm, m.width, false)
		if routeStr != "" {
			b.WriteString("\n")
			b.WriteString(routeStr)
		}
	}

	if m.status != "" {
		b.WriteString("\n")
		b.WriteString(InterventionStyle.Render("  " + m.status))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(helpRow(m.keys))
	return b.String()
}

// renderNewConnection renders the new-connection wizard screen.
func (m *Model) renderNewConnection() string {
	if m.wizard == nil {
		return m.renderHome()
	}
	var b strings.Builder
	b.WriteString(HeaderStyle.Render(" NEW CONNECTION "))
	b.WriteString("\n\n")
	b.WriteString(m.wizard.View())
	if m.status != "" {
		b.WriteString("\n\n")
		b.WriteString(InterventionStyle.Render("  " + m.status))
	}
	return b.String()
}

func (m *Model) renderInspect() string {
	conn := m.SelectedConnection()
	if conn == nil {
		return "No connection selected"
	}
	var b strings.Builder
	b.WriteString(HeaderStyle.Render(fmt.Sprintf(" %s ", conn.Name)))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("ID:       %s\n", conn.ID))
	b.WriteString(fmt.Sprintf("State:    %s\n", conn.UserState))
	b.WriteString(fmt.Sprintf("Provider: %s\n", conn.ProviderID))
	if conn.PublicAddress != "" {
		b.WriteString(fmt.Sprintf("Address:  %s\n", conn.PublicAddress))
	}
	if conn.PrivateAddress != "" {
		b.WriteString(fmt.Sprintf("Local:    %s\n", conn.PrivateAddress))
	}
	if conn.ConnectorPID > 0 {
		b.WriteString(fmt.Sprintf("PID:      %d\n", conn.ConnectorPID))
	}
	if conn.Error != "" {
		b.WriteString(fmt.Sprintf("\nError: %s\n", conn.Error))
	}
	b.WriteString("\n[esc] back    [q] quit\n")
	return b.String()
}

func (m *Model) renderPlanPreview() string {
	if m.plan == nil {
		return "No plan"
	}
	var b strings.Builder
	b.WriteString(HeaderStyle.Render(fmt.Sprintf(" PLAN: %s ", m.plan.Intent)))
	b.WriteString("\n\n")
	for _, step := range m.plan.Steps {
		mark := " "
		if step.Destructive {
			mark = "!"
		}
		b.WriteString(fmt.Sprintf("  [%s] %s\n", mark, step.Summary))
	}
	if m.status != "" {
		b.WriteString("\n")
		b.WriteString(InterventionStyle.Render("  " + m.status))
		b.WriteString("\n")
	}
	b.WriteString("\n[esc] cancel    [enter] apply    [q] quit\n")
	return b.String()
}

func (m *Model) renderOperationProgress() string {
	if m.operation == nil {
		return "No operation in progress"
	}
	var b strings.Builder
	b.WriteString(HeaderStyle.Render(fmt.Sprintf(" OPERATION: %s ", m.operation.State)))
	b.WriteString("\n\n")
	for _, step := range m.operation.Steps {
		b.WriteString(fmt.Sprintf("  - %s\n", step.Summary))
	}
	for _, evt := range m.opEvents {
		b.WriteString(fmt.Sprintf("  * %s\n", evt))
	}
	b.WriteString(fmt.Sprintf("\nState: %s\n", m.operation.State))
	if m.operation.Error != "" {
		b.WriteString(fmt.Sprintf("Error: %s\n", m.operation.Error))
	}
	if m.status != "" {
		b.WriteString(InterventionStyle.Render("  " + m.status))
		b.WriteString("\n")
	}
	b.WriteString("\n[esc] back\n")
	return b.String()
}

func (m *Model) renderProviders() string {
	var b strings.Builder
	b.WriteString(HeaderStyle.Render(" PROVIDERS "))
	b.WriteString("\n\n")
	for _, p := range m.snapshot.Providers {
		status := "✓"
		if !p.Authenticated {
			status = "✗"
		}
		b.WriteString(fmt.Sprintf("  %s %s (%s)\n", status, p.DisplayName, p.ID))
	}
	b.WriteString("\n[esc] back    [q] quit\n")
	return b.String()
}

func (m *Model) renderRepair() string {
	conn := m.SelectedConnection()
	if conn == nil {
		return "No connection selected"
	}
	var b strings.Builder
	b.WriteString(HeaderStyle.Render(fmt.Sprintf(" REPAIR: %s ", conn.Name)))
	b.WriteString("\n\n")
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
	b.WriteString("\n[esc] cancel    [r] run diagnostics    [q] quit\n")
	return b.String()
}

func (m *Model) renderHelp() string {
	return `Portico — Connection Manager

  n        New connection wizard
  enter    Inspect selected connection
  space    Open/close selected connection
  r        Repair selected connection
  p        Providers
  up/k     Select previous
  down/j   Select next
  esc      Back
  q        Quit
  ?        Help

[esc] back    [q] quit
`
}

func renderHeader(width int, th Theme) string {
	header := " PORTICO "
	if width > 40 {
		padding := width - len(header) - 2
		if padding < 0 {
			padding = 0
		}
		header += strings.Repeat("─", padding)
	}
	return lipgloss.NewStyle().
		Foreground(th.Structure).
		Render("┌─" + header + "─┐")
}

func renderConnectionList(conns []ipc.ConnectionDTO, selected int, width int, th Theme) string {
	var b strings.Builder

	if len(conns) == 0 {
		b.WriteString(MutedStyle.Render("  No connections"))
		b.WriteString("\n")
		return b.String()
	}

	for i, c := range conns {
		prefix := "  "
		style := NormalStyle
		if i == selected {
			prefix = "▸ "
			style = SelectedStyle
		}

		glyph := renderStateGlyph(c.UserState)
		line := fmt.Sprintf("%s%s %s", prefix, glyph, c.Name)
		if c.UserState != "" {
			line += "  " + MutedStyle.Render(strings.ToLower(c.UserState))
		}
		b.WriteString(style.Render(line))
		b.WriteString("\n")
	}

	return b.String()
}

func renderStateGlyph(state string) string {
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
