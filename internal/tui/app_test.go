package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// fakeClient implements SupervisorClient without a real socket.
type fakeClient struct {
	mu sync.Mutex

	snapshot    ipc.SnapshotDTO
	snapshotErr error
	plan        *ipc.PlanDTO
	planErr     error
	operation   *ipc.OperationDTO
	applyErr    error
	created     *ipc.ConnectionDTO
	createErr   error

	snapshotCalls  int
	planOpenCalls  int
	planCloseCalls int
	applyCalls     int
	createCalls    int
}

func (f *fakeClient) GetSnapshot(ctx context.Context) (*ipc.SnapshotDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotCalls++
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	snap := f.snapshot
	return &snap, nil
}

func (f *fakeClient) PlanOpen(ctx context.Context, connID string) (*ipc.PlanDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planOpenCalls++
	return f.plan, f.planErr
}

func (f *fakeClient) PlanClose(ctx context.Context, connID string) (*ipc.PlanDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planCloseCalls++
	return f.plan, f.planErr
}

func (f *fakeClient) PlanRepair(ctx context.Context, connID string) (*ipc.PlanDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plan, f.planErr
}

func (f *fakeClient) PlanDelete(ctx context.Context, connID string) (*ipc.PlanDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plan, f.planErr
}

func (f *fakeClient) ApplyPlan(ctx context.Context, planID string) (*ipc.OperationDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyCalls++
	return f.operation, f.applyErr
}

func (f *fakeClient) GetOperation(ctx context.Context, operationID string) (*ipc.OperationDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.operation, f.applyErr
}

func (f *fakeClient) CreateConnection(ctx context.Context, req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.created != nil {
		return f.created, nil
	}
	return &ipc.ConnectionDTO{ID: "conn-new", Name: req.Name}, nil
}

func (f *fakeClient) ConnectEventStream(ctx context.Context, lastSeq int64) (*ipc.EventStream, error) {
	return nil, errors.New("no event stream in tests")
}

func (f *fakeClient) Diagnostics(ctx context.Context, connID string) ([]ipc.DiagnosticDTO, error) {
	return []ipc.DiagnosticDTO{}, nil
}

func (f *fakeClient) Discovery(ctx context.Context) (*ipc.DiscoveryDTO, error) {
	return &ipc.DiscoveryDTO{}, nil
}

func (f *fakeClient) RefreshDiscovery(ctx context.Context) (*ipc.DiscoveryDTO, error) {
	return &ipc.DiscoveryDTO{}, nil
}

func (f *fakeClient) ConfigureProviderAccount(ctx context.Context, providerID string, req ipc.ConfigureProviderAccountRequest) (*ipc.ConfigureProviderAccountResponse, error) {
	return &ipc.ConfigureProviderAccountResponse{RestartRequired: false}, nil
}

// --------------- helpers ---------------

func keyMsg(key string) tea.KeyPressMsg {
	switch key {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	default:
		r := []rune(key)[0]
		return tea.KeyPressMsg{Code: r, Text: key}
	}
}

func press(t *testing.T, m Model, key string) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(keyMsg(key))
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	return nm, cmd
}

func readyModel(client SupervisorClient, snap ipc.SnapshotDTO) Model {
	m := newModel(client)
	next, _ := m.Update(snapshotMsg{Snapshot: snap})
	return next.(Model)
}

func testSnapshot() ipc.SnapshotDTO {
	return ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{
			{ID: "conn-1", Name: "web", DesiredState: "closed", UserState: "Closed"},
		},
	}
}

// --------------- tests ---------------

func TestPressNShowsNewConnectionScreen(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())

	m, _ = press(t, m, "n")

	if m.screen != ScreenNewConnection {
		t.Fatalf("screen = %q, want %q", m.screen, ScreenNewConnection)
	}
	if m.wizard == nil {
		t.Fatal("wizard not initialized")
	}
	content := m.View().Content
	if !strings.Contains(content, "NEW CONNECTION") {
		t.Errorf("view missing wizard header, got:\n%s", content)
	}
	if !strings.Contains(content, "What should be reachable?") {
		t.Errorf("view missing wizard intent step, got:\n%s", content)
	}
}

func TestProviderAndInspectViewsShowSelectedAccount(t *testing.T) {
	snap := testSnapshot()
	snap.Connections[0].ProviderID = "cloudflare"
	snap.Connections[0].ProviderAccountID = "account-work"
	snap.Providers = []ipc.ProviderDTO{{
		ID: "cloudflare", DisplayName: "Cloudflare", Authenticated: true,
		Accounts: []ipc.ProviderAccountDTO{{ID: "account-work", Label: "Work", Status: "authenticated"}},
	}}
	m := readyModel(&fakeClient{}, snap)
	m.screen = ScreenInspect
	if view := m.View().Content; !strings.Contains(view, "Account:  account-work") {
		t.Fatalf("inspect view omits selected account:\n%s", view)
	}
	m.screen = ScreenProviders
	if view := m.View().Content; !strings.Contains(view, "Work — authenticated") {
		t.Fatalf("provider view omits account summary:\n%s", view)
	}
}

func TestPlanLoadingIsAsync(t *testing.T) {
	tests := []struct {
		name      string
		desired   string
		wantOpen  int
		wantClose int
	}{
		{name: "closed connection plans open", desired: "closed", wantOpen: 1, wantClose: 0},
		{name: "open connection plans close", desired: "open", wantOpen: 0, wantClose: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeClient{plan: &ipc.PlanDTO{ID: "plan-1", Intent: "open"}}
			snap := testSnapshot()
			snap.Connections[0].DesiredState = tt.desired
			m := readyModel(fake, snap)

			m, cmd := press(t, m, "space")

			// Update must not block on IPC: no call yet, screen unchanged,
			// and the work is returned as a command.
			if cmd == nil {
				t.Fatal("expected a command for async plan load, got nil")
			}
			if fake.planOpenCalls != 0 || fake.planCloseCalls != 0 {
				t.Fatal("Update performed synchronous IPC call")
			}
			if m.screen != ScreenHome {
				t.Fatalf("screen = %q before plan loads, want home", m.screen)
			}

			// Executing the command performs the IPC call and yields the
			// typed message that moves the model to the preview screen.
			msg := cmd()
			loaded, ok := msg.(planLoadedMsg)
			if !ok {
				t.Fatalf("cmd returned %T, want planLoadedMsg", msg)
			}
			if loaded.Err != nil {
				t.Fatalf("unexpected plan error: %v", loaded.Err)
			}
			if fake.planOpenCalls != tt.wantOpen || fake.planCloseCalls != tt.wantClose {
				t.Fatalf("plan calls open=%d close=%d, want open=%d close=%d",
					fake.planOpenCalls, fake.planCloseCalls, tt.wantOpen, tt.wantClose)
			}

			next, _ := m.Update(msg)
			m = next.(Model)
			if m.screen != ScreenPlanPreview {
				t.Fatalf("screen = %q after planLoadedMsg, want plan preview", m.screen)
			}
			if m.plan == nil || m.plan.ID != "plan-1" {
				t.Fatalf("plan not stored on model: %+v", m.plan)
			}
		})
	}
}

func TestPlanLoadErrorShowsStatus(t *testing.T) {
	fake := &fakeClient{planErr: fmt.Errorf("boom")}
	m := readyModel(fake, testSnapshot())

	m, cmd := press(t, m, "space")
	if cmd == nil {
		t.Fatal("expected a command")
	}
	next, _ := m.Update(cmd())
	m = next.(Model)

	if m.screen != ScreenHome {
		t.Fatalf("screen = %q after plan error, want home", m.screen)
	}
	if !strings.Contains(m.status, "boom") {
		t.Fatalf("status = %q, want plan error surfaced", m.status)
	}
	if !strings.Contains(m.View().Content, "boom") {
		t.Error("view does not show the status/error line")
	}
}

func TestSnapshotMsgUpdatesModel(t *testing.T) {
	tests := []struct {
		name       string
		msg        snapshotMsg
		pre        func(m Model) Model
		wantReady  bool
		wantErr    bool
		wantStatus bool
		wantConns  int
	}{
		{
			name:      "snapshot applies connections and marks ready",
			msg:       snapshotMsg{Snapshot: testSnapshot()},
			wantReady: true,
			wantConns: 1,
		},
		{
			name:    "initial snapshot error is fatal",
			msg:     snapshotMsg{Err: fmt.Errorf("socket gone")},
			wantErr: true,
		},
		{
			name: "refresh error after ready goes to status line",
			msg:  snapshotMsg{Err: fmt.Errorf("socket gone")},
			pre: func(m Model) Model {
				next, _ := m.Update(snapshotMsg{Snapshot: testSnapshot()})
				return next.(Model)
			},
			wantReady:  true,
			wantStatus: true,
			wantConns:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(&fakeClient{})
			if tt.pre != nil {
				m = tt.pre(m)
			}
			next, _ := m.Update(tt.msg)
			m = next.(Model)

			if m.ready != tt.wantReady {
				t.Errorf("ready = %v, want %v", m.ready, tt.wantReady)
			}
			if (m.err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", m.err, tt.wantErr)
			}
			if (m.status != "") != tt.wantStatus {
				t.Errorf("status = %q, wantStatus = %v", m.status, tt.wantStatus)
			}
			if got := len(m.ConnectionList()); got != tt.wantConns {
				t.Errorf("connections = %d, want %d", got, tt.wantConns)
			}
		})
	}
}

func TestEmergencyHomeLayoutIsReadable(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.width = 50
	view := m.renderHome()
	if !strings.Contains(view, "Portico needs at least 60 columns") {
		t.Fatalf("emergency layout not rendered: %q", view)
	}
}

func TestASCIIConnectionListUsesASCIIGlyphs(t *testing.T) {
	got := renderConnectionList([]ipc.ConnectionDTO{{ID: "c1", Name: "example", UserState: "Open"}}, "c1", 80, MonochromeTheme, true)
	if !strings.Contains(got, "> * example") {
		t.Fatalf("ASCII list did not use fallback glyphs: %q", got)
	}
	if strings.ContainsAny(got, "▸●◐○╳◌") {
		t.Fatalf("ASCII list contained Unicode status glyphs: %q", got)
	}
}

func TestApplyPlanFromPreviewIsAsync(t *testing.T) {
	fake := &fakeClient{
		plan:      &ipc.PlanDTO{ID: "plan-1", Intent: "open"},
		operation: &ipc.OperationDTO{ID: "op-1", PlanID: "plan-1", State: "running"},
	}
	m := readyModel(fake, testSnapshot())

	// Load the plan preview.
	m, cmd := press(t, m, "space")
	next, _ := m.Update(cmd())
	m = next.(Model)
	if m.screen != ScreenPlanPreview {
		t.Fatalf("screen = %q, want plan preview", m.screen)
	}

	// Enter applies the plan via a command — no synchronous IPC.
	m, cmd = press(t, m, "enter")
	if cmd == nil {
		t.Fatal("expected apply command, got nil")
	}
	if fake.applyCalls != 0 {
		t.Fatal("Update applied the plan synchronously")
	}

	msg := cmd()
	applied, ok := msg.(planAppliedMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want planAppliedMsg", msg)
	}
	if applied.Err != nil {
		t.Fatalf("unexpected apply error: %v", applied.Err)
	}
	next, _ = m.Update(msg)
	m = next.(Model)
	if m.screen != ScreenOperationProgress {
		t.Fatalf("screen = %q after apply, want operation progress", m.screen)
	}
	if m.operation == nil || m.operation.ID != "op-1" {
		t.Fatalf("operation not stored on model: %+v", m.operation)
	}
}

func TestApplyPlanPreviewIgnoresRepeatedEnterWhileRequestIsInFlight(t *testing.T) {
	fake := &fakeClient{
		plan:      &ipc.PlanDTO{ID: "plan-1", Intent: "open"},
		operation: &ipc.OperationDTO{ID: "op-1", PlanID: "plan-1", State: "running"},
	}
	m := readyModel(fake, testSnapshot())
	m, cmd := press(t, m, "space")
	next, _ := m.Update(cmd())
	m = next.(Model)

	m, firstApply := press(t, m, "enter")
	if firstApply == nil || !m.applying {
		t.Fatal("first enter did not begin asynchronous apply")
	}
	m, secondApply := press(t, m, "enter")
	if secondApply != nil {
		t.Fatal("second enter started a duplicate apply request")
	}
	if fake.applyCalls != 0 {
		t.Fatal("apply request ran synchronously")
	}
	msg := firstApply()
	if fake.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1", fake.applyCalls)
	}
	next, _ = m.Update(msg)
	m = next.(Model)
	if m.applying {
		t.Fatal("apply state remained set after completion")
	}
}

func TestWizardCreateFlowIsAsync(t *testing.T) {
	fake := &fakeClient{}
	m := readyModel(fake, testSnapshot())

	m, _ = press(t, m, "n")

	// Walk the wizard: intent (existing service), name, address, port,
	// protocol (http), exposure (temporary), protection (none), provider (cloudflare).
	steps := []string{"enter"}                         // intent: existing service
	steps = append(steps, "d", "e", "m", "o", "enter") // name: "demo"
	steps = append(steps, "enter")                     // address: empty (port next)
	steps = append(steps, "8", "0", "8", "0", "enter") // port: 8080
	steps = append(steps, "enter")                     // protocol: http (default)
	steps = append(steps, "enter")                     // exposure: temporary_public
	steps = append(steps, "enter")                     // protection: none
	steps = append(steps, "enter")                     // provider: cloudflare

	var cmd tea.Cmd
	for _, k := range steps {
		m, _ = press(t, m, k)
	}
	if m.wizard.Step() != screens.WizardStepReview {
		t.Fatalf("wizard step = %d, want review", m.wizard.Step())
	}

	// Submit: the create happens via command, not inside Update.
	m, cmd = press(t, m, "enter")
	if cmd == nil {
		t.Fatal("expected create command, got nil")
	}
	if fake.createCalls != 0 {
		t.Fatal("Update created the connection synchronously")
	}
	if m.wizard.Step() != screens.WizardStepCreating {
		t.Fatalf("wizard step = %d, want creating", m.wizard.Step())
	}

	msg := cmd()
	created, ok := msg.(screens.ConnectionCreatedMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want ConnectionCreatedMsg", msg)
	}
	if created.Err != nil {
		t.Fatalf("unexpected create error: %v", created.Err)
	}

	next, refresh := m.Update(msg)
	m = next.(Model)
	if m.wizard.Step() != screens.WizardStepComplete {
		t.Fatalf("wizard step = %d, want complete", m.wizard.Step())
	}
	if refresh == nil {
		t.Fatal("expected snapshot refresh command after creation")
	}

	// Enter on the done screen returns home and refreshes.
	m, cmd = press(t, m, "enter")
	if m.screen != ScreenHome {
		t.Fatalf("screen = %q, want home", m.screen)
	}
	if cmd == nil {
		t.Fatal("expected snapshot refresh command on return home")
	}
}

// --------------- Phase 1 correctness tests ---------------

func TestSnapshotSetsEventCursorAndConnectsSSE(t *testing.T) {
	m := newModel(&fakeClient{})
	snap := ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{{ID: "c1", Name: "web"}},
		LastSeq:     42,
	}
	next, cmd := m.Update(snapshotMsg{Snapshot: snap})
	m = next.(Model)

	if m.lastEventSeq != 42 {
		t.Fatalf("lastEventSeq = %d, want 42", m.lastEventSeq)
	}
	if !m.streamConnected {
		t.Fatal("streamConnected should be true after first snapshot")
	}
	if cmd == nil {
		t.Fatal("expected SSE connect command after first snapshot")
	}
}

func TestSnapshotDoesNotReconnectSSEOnRefresh(t *testing.T) {
	m := newModel(&fakeClient{})
	snap := ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{{ID: "c1", Name: "web"}},
		LastSeq:     10,
	}
	next, _ := m.Update(snapshotMsg{Snapshot: snap})
	m = next.(Model)

	snap.LastSeq = 20
	next, cmd := m.Update(snapshotMsg{Snapshot: snap})
	m = next.(Model)

	if m.lastEventSeq != 20 {
		t.Fatalf("lastEventSeq = %d, want 20", m.lastEventSeq)
	}
	if cmd != nil {
		t.Fatal("refresh snapshot should not trigger SSE reconnect")
	}
}

func TestResyncClosesStreamAndRequestsSnapshot(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.lastEventSeq = 10
	m.streamConnected = true

	evt := ipc.EventDTO{Type: "resync_required", Sequence: 0}
	next, cmd := m.Update(eventMsg{Event: evt})
	m = next.(Model)

	if m.streamConnected {
		t.Fatal("streamConnected should be false after resync")
	}
	if cmd == nil {
		t.Fatal("expected snapshot request command after resync")
	}
}

func TestEventDeduplicationBySequence(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.lastEventSeq = 10

	evt := ipc.EventDTO{
		Type:     "operation.step",
		Sequence: 5,
		Data:     map[string]interface{}{"summary": "old event"},
	}
	next, _ := m.Update(eventMsg{Event: evt})
	m = next.(Model)

	if len(m.opEvents) != 0 {
		t.Fatalf("duplicate event was not filtered, opEvents = %v", m.opEvents)
	}
}

func TestOperationEventsFilteredByID(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.operation = &ipc.OperationDTO{ID: "op-1"}

	evt := ipc.EventDTO{
		Type:        "operation.step",
		Sequence:    11,
		OperationID: "op-other",
		Data:        map[string]interface{}{"summary": "other op step"},
	}
	next, _ := m.Update(eventMsg{Event: evt})
	m = next.(Model)

	if len(m.opEvents) != 0 {
		t.Fatalf("unrelated operation event entered opEvents: %v", m.opEvents)
	}

	evt2 := ipc.EventDTO{
		Type:        "operation.step",
		Sequence:    12,
		OperationID: "op-1",
		Data:        map[string]interface{}{"summary": "our op step"},
	}
	next, _ = m.Update(eventMsg{Event: evt2})
	m = next.(Model)

	if len(m.opEvents) != 1 || m.opEvents[0] != "our op step" {
		t.Fatalf("expected matching operation event in opEvents, got %v", m.opEvents)
	}
}

func TestUpDownOnlyAffectsHomeScreen(t *testing.T) {
	snap := ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{
			{ID: "c1", Name: "alpha"},
			{ID: "c2", Name: "beta"},
			{ID: "c3", Name: "gamma"},
		},
	}
	m := readyModel(&fakeClient{}, snap)
	if m.selectedID != "c1" {
		t.Fatalf("initial selectedID = %q, want c1", m.selectedID)
	}

	m, _ = press(t, m, "down")
	if m.selectedID != "c2" {
		t.Fatalf("after down on Home, selectedID = %q, want c2", m.selectedID)
	}

	m.screen = ScreenInspect
	m, _ = press(t, m, "down")
	if m.selectedID != "c2" {
		t.Fatalf("down on Inspect changed selectedID to %q, want c2", m.selectedID)
	}
	m, _ = press(t, m, "up")
	if m.selectedID != "c2" {
		t.Fatalf("up on Inspect changed selectedID to %q, want c2", m.selectedID)
	}

	m.screen = ScreenPlanPreview
	m, _ = press(t, m, "down")
	if m.selectedID != "c2" {
		t.Fatalf("down on PlanPreview changed selectedID to %q, want c2", m.selectedID)
	}

	m.screen = ScreenRepair
	m, _ = press(t, m, "up")
	if m.selectedID != "c2" {
		t.Fatalf("up on Repair changed selectedID to %q, want c2", m.selectedID)
	}
}

func TestSelectionPreservedByIDAcrossSnapshots(t *testing.T) {
	snap := ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{
			{ID: "c1", Name: "alpha"},
			{ID: "c2", Name: "beta"},
			{ID: "c3", Name: "gamma"},
		},
		LastSeq: 1,
	}
	m := readyModel(&fakeClient{}, snap)
	m.selectedID = "c2"

	reordered := ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{
			{ID: "c3", Name: "gamma"},
			{ID: "c1", Name: "alpha"},
			{ID: "c2", Name: "beta"},
		},
		LastSeq: 2,
	}
	next, _ := m.Update(snapshotMsg{Snapshot: reordered})
	m = next.(Model)

	if m.selectedID != "c2" {
		t.Fatalf("selection after reorder = %q, want c2", m.selectedID)
	}
}

func TestSelectionFallsBackWhenConnectionDeleted(t *testing.T) {
	snap := ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{
			{ID: "c1", Name: "alpha"},
			{ID: "c2", Name: "beta"},
		},
		LastSeq: 1,
	}
	m := readyModel(&fakeClient{}, snap)
	m.selectedID = "c2"

	updated := ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{
			{ID: "c1", Name: "alpha"},
		},
		LastSeq: 2,
	}
	next, _ := m.Update(snapshotMsg{Snapshot: updated})
	m = next.(Model)

	if m.selectedID != "c1" {
		t.Fatalf("selection after deletion = %q, want c1 (fallback)", m.selectedID)
	}
}

func TestInitOnlyRequestsSnapshot(t *testing.T) {
	m := newModel(&fakeClient{})
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned nil command")
	}
	if m.streamConnected {
		t.Fatal("streamConnected should be false before first snapshot")
	}
}
