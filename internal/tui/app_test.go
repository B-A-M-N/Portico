package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// fakeClient implements SupervisorClient without a real socket.
type fakeClient struct {
	mu sync.Mutex

	zones       []ipc.ZoneDTO
	listZonesErr error

	snapshot    ipc.SnapshotDTO
	snapshotErr error
	plan        *ipc.PlanDTO
	planErr     error
	operation   *ipc.OperationDTO
	applyErr    error
	created     *ipc.ConnectionDTO
	createErr   error

	removalPreview        *ipc.AccountRemovalPreviewDTO
	removedFingerprint    string
	removedProvider       string
	removedAccount        string
	removeAccountResponse *ipc.RemoveProviderAccountResponse
	removeAccountErr      error

	createRequest      *ipc.CreateConnectionRequest
	historyLimit       int
	supportExportErr   error
	applyKeys          []string
	eventsFor          string
	operationEvents    []ipc.EventDTO
	operationEventsErr error
	cloneSource        string
	cloneRequest       *ipc.CloneConnectionRequest
	cloneErr           error
	editRequest        *ipc.UpdateConnectionRequest
	editPlan           *ipc.PlanDTO
	editErr            error

	detail    *ipc.ConnectionDetailDTO
	detailErr error
	history   *ipc.OperationHistoryDTO
	logs      *ipc.ConnectionLogsDTO
	readiness *ipc.ReadinessDTO

	recommendation    *ipc.ProviderRecommendationResponse
	recommendErr      error
	recommendRequests []ipc.ProviderRecommendationRequest

	setupFlow      *ipc.SetupFlowDTO
	setupFlowErr   error
	setupFlowAsked []string

	// Credential validation / discovery during setup.
	validateResponse *ipc.ConfigureProviderAccountResponse
	validateErr      error
	validateRequests []ipc.ConfigureProviderAccountRequest

	launchMode    *ipc.LaunchModeDTO
	launchModeErr error

	// Operational settings, as the supervisor would hold them.
	settingsDTO       *ipc.SettingsDTO
	settingsErr       error
	updateSettingsErr error
	settingsCalls     int
	settingsRequests  []ipc.SettingsRequest

	// Telemetry, and the connections it was asked for.
	telemetryDTO *ipc.TelemetryDTO
	telemetryErr error
	telemetryIDs []string

	// Credential replacement. The credential itself is never retained.
	replaceCredentialResponse *ipc.ReplaceCredentialResponse
	replaceCredentialErr      error
	replacedProvider          string
	replacedAccount           string
	replacedCredentialLen     int

	// Account re-verification.
	reverifyResponse   *ipc.ReverifyProviderAccountResponse
	reverifyErr        error
	reverifiedProvider string
	reverifiedAccount  string
	// launchModeAsked records every mode the TUI requested, so a test can pin
	// which direction the toggle asked for rather than only what it displayed.
	launchModeAsked []string

	snapshotCalls  int
	planOpenCalls  int
	planCloseCalls int
	applyCalls     int
	createCalls    int
	detailCalls    int
	detailIDs      []string
}

func (f *fakeClient) Readiness(_ context.Context) (*ipc.ReadinessDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readiness != nil {
		return f.readiness, nil
	}
	return &ipc.ReadinessDTO{Summary: "nothing configured", LaunchMode: "auto"}, nil
}

// RecommendProvider stands in for the supervisor's evaluation. Returning a
// Cloudflare recommendation keeps the existing walkthrough tests describing the
// same flow they always did.
func (f *fakeClient) RecommendProvider(_ context.Context, req ipc.ProviderRecommendationRequest) (
	*ipc.ProviderRecommendationResponse, error,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recommendRequests = append(f.recommendRequests, req)
	if f.recommendErr != nil {
		return nil, f.recommendErr
	}
	if f.recommendation != nil {
		return f.recommendation, nil
	}
	return &ipc.ProviderRecommendationResponse{
		Recommended: &ipc.ProviderChoiceDTO{ProviderID: "cloudflare", DisplayName: "Cloudflare"},
	}, nil
}

func (f *fakeClient) ProviderSetupFlow(_ context.Context, providerID string) (*ipc.SetupFlowDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setupFlowAsked = append(f.setupFlowAsked, providerID)
	if f.setupFlowErr != nil {
		return nil, f.setupFlowErr
	}
	if f.setupFlow != nil {
		return f.setupFlow, nil
	}
	return &ipc.SetupFlowDTO{ProviderID: providerID}, nil
}

func (f *fakeClient) SetLaunchMode(_ context.Context, mode string) (*ipc.LaunchModeDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchModeAsked = append(f.launchModeAsked, mode)
	if f.launchModeErr != nil {
		return nil, f.launchModeErr
	}
	if f.launchMode != nil {
		return f.launchMode, nil
	}
	return &ipc.LaunchModeDTO{Mode: mode}, nil
}

func (f *fakeClient) ConnectionLogs(_ context.Context, _ string, _ int) (*ipc.ConnectionLogsDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logs != nil {
		return f.logs, nil
	}
	return &ipc.ConnectionLogsDTO{Available: true}, nil
}

func (f *fakeClient) GetConnectionDetail(_ context.Context, id string) (*ipc.ConnectionDetailDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detailCalls++
	f.detailIDs = append(f.detailIDs, id)
	return f.detail, f.detailErr
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
	// A real plan always names the connection it was prepared for. A fake that
	// omitted it let an unidentifiable plan look acceptable in tests.
	if f.plan != nil && f.plan.ConnectionID == "" {
		f.plan.ConnectionID = connID
	}
	f.planOpenCalls++
	return f.plan, f.planErr
}

func (f *fakeClient) PlanClose(ctx context.Context, connID string) (*ipc.PlanDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A real plan always names the connection it was prepared for. A fake that
	// omitted it let an unidentifiable plan look acceptable in tests.
	if f.plan != nil && f.plan.ConnectionID == "" {
		f.plan.ConnectionID = connID
	}
	f.planCloseCalls++
	return f.plan, f.planErr
}

func (f *fakeClient) PlanRepair(ctx context.Context, connID string) (*ipc.PlanDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A real plan always names the connection it was prepared for. A fake that
	// omitted it let an unidentifiable plan look acceptable in tests.
	if f.plan != nil && f.plan.ConnectionID == "" {
		f.plan.ConnectionID = connID
	}
	return f.plan, f.planErr
}

func (f *fakeClient) PlanDelete(ctx context.Context, connID string) (*ipc.PlanDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A real plan always names the connection it was prepared for. A fake that
	// omitted it let an unidentifiable plan look acceptable in tests.
	if f.plan != nil && f.plan.ConnectionID == "" {
		f.plan.ConnectionID = connID
	}
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

func (f *fakeClient) GetOperationHistory(ctx context.Context) (*ipc.OperationHistoryDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.history != nil {
		return f.history, nil
	}
	return &ipc.OperationHistoryDTO{Operations: []ipc.OperationDTO{}, Available: true}, nil
}

func (f *fakeClient) CreateConnection(ctx context.Context, req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.createRequest = &req
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

// ValidateProviderAccount answers with the configured discovery result, or a
// validated single-account response when none is set.
func (f *fakeClient) ValidateProviderAccount(ctx context.Context, providerID string, req ipc.ConfigureProviderAccountRequest) (*ipc.ConfigureProviderAccountResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validateRequests = append(f.validateRequests, req)
	if f.validateErr != nil {
		return nil, f.validateErr
	}
	if f.validateResponse != nil {
		return f.validateResponse, nil
	}
	return &ipc.ConfigureProviderAccountResponse{Validated: true}, nil
}

func (f *fakeClient) GetOperationHistoryLimit(ctx context.Context, limit int) (*ipc.OperationHistoryDTO, error) {
	f.mu.Lock()
	f.historyLimit = limit
	f.mu.Unlock()
	if f.history != nil {
		return f.history, nil
	}
	return f.GetOperationHistory(ctx)
}

func (f *fakeClient) ApplyPlanWithIdempotency(ctx context.Context, planID, idempotencyKey string) (*ipc.OperationDTO, error) {
	f.mu.Lock()
	f.applyKeys = append(f.applyKeys, idempotencyKey)
	f.mu.Unlock()
	return f.ApplyPlan(ctx, planID)
}

func (f *fakeClient) SupportExport(ctx context.Context) (*ipc.SupportExportDTO, error) {
	if f.supportExportErr != nil {
		return nil, f.supportExportErr
	}
	return &ipc.SupportExportDTO{GeneratedAt: "2026-01-01T00:00:00Z", OS: "linux"}, nil
}

func (f *fakeClient) GetOperationEvents(ctx context.Context, operationID string) ([]ipc.EventDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventsFor = operationID
	if f.operationEventsErr != nil {
		return nil, f.operationEventsErr
	}
	return f.operationEvents, nil
}

func (f *fakeClient) CloneConnection(ctx context.Context, id string, req ipc.CloneConnectionRequest) (*ipc.ConnectionDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cloneSource = id
	f.cloneRequest = &req
	if f.cloneErr != nil {
		return nil, f.cloneErr
	}
	return &ipc.ConnectionDTO{ID: "conn-copy", Name: req.Name}, nil
}

func (f *fakeClient) PlanEdit(ctx context.Context, connID string, req ipc.UpdateConnectionRequest) (*ipc.PlanDTO, error) {
	f.editRequest = &req
	if f.editErr != nil {
		return nil, f.editErr
	}
	if f.editPlan != nil {
		return f.editPlan, nil
	}
	return &ipc.PlanDTO{ID: "plan-edit", ConnectionID: connID, Intent: "edit"}, nil
}

// removeAccountErr, when set, is what the supervisor answers a removal with.
func (f *fakeClient) PreviewProviderAccountRemoval(ctx context.Context, providerID, accountID string) (*ipc.AccountRemovalPreviewDTO, error) {
	if f.removalPreview != nil {
		return f.removalPreview, nil
	}
	return &ipc.AccountRemovalPreviewDTO{
		ProviderID: providerID, AccountID: accountID,
		Removable: true, CredentialStored: true, Fingerprint: "fp-fake",
		Consequences: []string{"Portico will forget the credential it stored for this account."},
	}, nil
}

func (f *fakeClient) RemoveProviderAccount(ctx context.Context, providerID, accountID, fingerprint string) (*ipc.RemoveProviderAccountResponse, error) {
	f.removedProvider, f.removedAccount = providerID, accountID
	f.removedFingerprint = fingerprint
	if f.removeAccountErr != nil {
		return f.removeAccountResponse, f.removeAccountErr
	}
	if f.removeAccountResponse != nil {
		return f.removeAccountResponse, nil
	}
	return &ipc.RemoveProviderAccountResponse{Removed: true}, nil
}

// Settings, UpdateSettings, Telemetry and ReverifyProviderAccount complete the
// SupervisorClient surface. Each records what was asked so a test can assert
// the TUI reached the supervisor rather than answering locally.

func (f *fakeClient) Settings(_ context.Context) (*ipc.SettingsDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settingsCalls++
	if f.settingsErr != nil {
		return nil, f.settingsErr
	}
	if f.settingsDTO != nil {
		return f.settingsDTO, nil
	}
	return &ipc.SettingsDTO{LaunchMode: "auto", DefaultOnDisconnect: "keep_alive"}, nil
}

func (f *fakeClient) UpdateSettings(_ context.Context, req ipc.SettingsRequest) (*ipc.SettingsDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settingsRequests = append(f.settingsRequests, req)
	if f.updateSettingsErr != nil {
		return nil, f.updateSettingsErr
	}
	current := f.settingsDTO
	if current == nil {
		current = &ipc.SettingsDTO{LaunchMode: "auto", DefaultOnDisconnect: "keep_alive"}
	}
	updated := *current
	if req.LaunchMode != nil {
		updated.LaunchMode = *req.LaunchMode
	}
	if req.DefaultAutoStart != nil {
		updated.DefaultAutoStart = *req.DefaultAutoStart
	}
	if req.DefaultOnDisconnect != nil {
		updated.DefaultOnDisconnect = *req.DefaultOnDisconnect
	}
	if req.ClientTunnelBin != nil {
		updated.ClientTunnelBin = *req.ClientTunnelBin
	}
	f.settingsDTO = &updated
	return &updated, nil
}

func (f *fakeClient) RotateSecretKey(_ context.Context) (*ipc.RotateSecretKeyDTO, error) {
	return &ipc.RotateSecretKeyDTO{Version: 2, RotatedAt: "2026-08-25T00:00:00Z"}, nil
}

func (f *fakeClient) Telemetry(_ context.Context, id string) (*ipc.TelemetryDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.telemetryIDs = append(f.telemetryIDs, id)
	if f.telemetryErr != nil {
		return nil, f.telemetryErr
	}
	return f.telemetryDTO, nil
}

func (f *fakeClient) ReplaceProviderAccountCredential(
	_ context.Context, providerID, accountID, credential string) (*ipc.ReplaceCredentialResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replacedProvider, f.replacedAccount = providerID, accountID
	// The length is recorded, never the value: a test fake that keeps a
	// credential is a test fake that can leak one into a failure message.
	f.replacedCredentialLen = len(credential)
	if f.replaceCredentialErr != nil {
		return nil, f.replaceCredentialErr
	}
	if f.replaceCredentialResponse != nil {
		return f.replaceCredentialResponse, nil
	}
	return &ipc.ReplaceCredentialResponse{
		AccountID: accountID, Validated: true, Status: "usable",
	}, nil
}

func (f *fakeClient) ReverifyProviderAccount(_ context.Context, providerID, accountID string) (
	*ipc.ReverifyProviderAccountResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reverifiedProvider, f.reverifiedAccount = providerID, accountID
	if f.reverifyErr != nil {
		return f.reverifyResponse, f.reverifyErr
	}
	if f.reverifyResponse != nil {
		return f.reverifyResponse, nil
	}
	return &ipc.ReverifyProviderAccountResponse{Validated: true, Status: "usable"}, nil
}

func (f *fakeClient) ListProviderAccountZones(_ context.Context, providerID, accountID string) (
	*ipc.ListProviderAccountZonesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listZonesErr != nil {
		return nil, f.listZonesErr
	}
	return &ipc.ListProviderAccountZonesResponse{ProviderID: providerID, AccountID: accountID, Zones: f.zones}, nil
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
	m := newModel(client, nil)
	next, _ := m.Update(snapshotMsg{Snapshot: snap})
	return next.(Model)
}

func testSnapshot() ipc.SnapshotDTO {
	return ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{
			{ID: "conn-1", Name: "web", DesiredState: "closed", UserState: "Closed"},
		},
		// The wizard derives what it can offer from declared capability, so a
		// snapshot with no providers offers nothing — which is correct, and
		// means a fixture has to describe a real provider to exercise a flow.
		Providers: []ipc.ProviderDTO{{
			ID: "cloudflare", DisplayName: "Cloudflare",
			Availability: "ready", Readiness: "ready", Selectable: true,
			// What the real supervisor derives from the provider's own
			// declaration: Cloudflare stores an account.
			SetupKind: "account",
			Accounts:  []ipc.ProviderAccountDTO{{ID: "acct-1", Label: "Personal", Status: "authenticated"}},
			Capabilities: &ipc.CapabilitySetDTO{
				TemporaryAddresses: true,
				CustomHostnames:    true,
				ManagedDNS:         true,
				ProtectionModes:    []string{"none", "email_otp"},
				Protocols:          []string{"http", "https"},
			},
		}},
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
	// The wizard opens on the outcome question, not a source-type question.
	if !strings.Contains(content, "What are you trying to do?") {
		t.Errorf("view missing wizard outcome step, got:\n%s", content)
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
			m := newModel(&fakeClient{}, nil)
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

	// Walk the wizard: outcome, name, which service, address, port, protocol,
	// exposure, protection, provider.
	//
	// The service question comes before the address: publishing something already
	// running should not require having found its address elsewhere first. With no
	// scan result, the manual-entry option is the only choice, and taking it opens
	// the address field — which is the path this test then follows.
	steps := []string{"enter"}                         // outcome: share a web app temporarily
	steps = append(steps, "d", "e", "m", "o", "enter") // name: "demo"
	steps = append(steps, "enter")                     // which service: enter an address manually
	steps = append(steps, "enter")                     // address: empty (port next)
	steps = append(steps, "8", "0", "8", "0", "enter") // port: 8080
	steps = append(steps, "enter")                     // protocol: http (default)
	steps = append(steps, "enter")                     // health probe: yes (default)
	steps = append(steps, "enter")                     // health path: service root
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
	m := newModel(&fakeClient{}, nil)
	snap := ipc.SnapshotDTO{
		Connections: []ipc.ConnectionDTO{{ID: "c1", Name: "web"}},
		LastSeq:     42,
	}
	next, cmd := m.Update(snapshotMsg{Snapshot: snap})
	m = next.(Model)

	if m.lastEventSeq != 42 {
		t.Fatalf("lastEventSeq = %d, want 42", m.lastEventSeq)
	}
	if m.eventStreams.state != eventStreamConnecting {
		t.Fatalf("event stream state = %q, want connecting before ready result", m.eventStreams.state)
	}
	if cmd == nil {
		t.Fatal("expected SSE connect command after first snapshot")
	}
}

func TestSnapshotDoesNotReconnectSSEOnRefresh(t *testing.T) {
	m := newModel(&fakeClient{}, nil)
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

func TestEventStreamInitialFailureEntersReconnecting(t *testing.T) {
	m := newModel(&fakeClient{}, nil)
	m.Update(snapshotMsg{Snapshot: ipc.SnapshotDTO{LastSeq: 1}})

	next, cmd := m.Update(streamErrorMsg{
		Generation: m.eventStreams.generation,
		Err:        errors.New("socket unavailable"),
	})
	m = next.(Model)

	if m.eventStreams.state != eventStreamReconnecting {
		t.Fatalf("event stream state = %q, want reconnecting", m.eventStreams.state)
	}
	if cmd == nil {
		t.Fatal("expected reconnect command after initial connection failure")
	}
	if m.wizard != nil {
		t.Fatal("unexpected wizard while testing root-only state")
	}
}

func TestEventStreamReadyClearsReconnectStatusAndMarksLive(t *testing.T) {
	m := newModel(&fakeClient{}, nil)
	m.Update(snapshotMsg{Snapshot: ipc.SnapshotDTO{LastSeq: 1}})
	m.Update(streamErrorMsg{
		Generation: m.eventStreams.generation,
		Err:        errors.New("socket unavailable"),
	})
	m.status = "event stream interrupted: socket unavailable; reconnecting"

	next, cmd := m.Update(eventStreamReadyMsg{
		Generation: m.eventStreams.generation,
		Stream:     &ipc.EventStream{},
		Cancel:     func() {},
	})
	m = next.(Model)

	if !m.eventStreams.live() {
		t.Fatalf("event stream state = %q, want live", m.eventStreams.state)
	}
	if m.status != "" {
		t.Fatalf("status = %q, want reconnect warning cleared", m.status)
	}
	if cmd == nil {
		t.Fatal("expected event read command after stream becomes live")
	}
	m.closeEventStream()
}

func TestEventStreamResyncTransitionsAndSuccessfulSnapshotReconnects(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.eventStreams.generation = 1
	m.eventStreams.state = eventStreamLive
	m.lastEventSeq = 10

	next, _ := m.Update(eventMsg{Generation: 1, Event: ipc.EventDTO{Type: "resync_required"}})
	m = next.(Model)
	if m.eventStreams.state != eventStreamResyncing {
		t.Fatalf("event stream state = %q after resync, want resyncing", m.eventStreams.state)
	}

	next, cmd := m.Update(snapshotMsg{Snapshot: ipc.SnapshotDTO{LastSeq: 11}})
	m = next.(Model)
	if m.eventStreams.state != eventStreamConnecting {
		t.Fatalf("event stream state = %q after snapshot, want connecting", m.eventStreams.state)
	}
	if m.eventStreams.generation != 2 {
		t.Fatalf("event stream generation = %d after snapshot, want 2", m.eventStreams.generation)
	}
	if cmd == nil {
		t.Fatal("expected reconnect command after successful resync snapshot")
	}
}

func TestStaleEventStreamResultsCannotSupersedeNewerState(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.eventStreams.generation = 2
	m.eventStreams.state = eventStreamReconnecting

	next, _ := m.Update(streamErrorMsg{Generation: 1, Err: errors.New("old attempt")})
	if next.(Model).eventStreams.state != eventStreamReconnecting {
		t.Fatal("stale error changed current state")
	}
	next, cmd := m.Update(eventMsg{Generation: 1, Event: ipc.EventDTO{Type: "connection.updated"}})
	_ = next
	if cmd != nil {
		t.Fatal("stale event scheduled follow-up work")
	}

	next, _ = m.Update(eventStreamReadyMsg{Generation: 1, Stream: &ipc.EventStream{}, Cancel: func() {}})
	m = next.(Model)
	if m.eventStreams.live() {
		t.Fatal("stale ready result resurrected superseded stream state")
	}
	if m.stream != nil {
		t.Fatal("stale ready result installed stream transport")
	}
}

func TestEventStreamShutdownCancellationDoesNotScheduleReconnect(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.eventStreams.generation = 1
	m.eventStreams.state = eventStreamReconnecting

	next, cmd := m.Update(streamErrorMsg{
		Generation: 1,
		Err:        fmt.Errorf("event read: %w", context.Canceled),
	})
	m = next.(Model)

	if m.eventStreams.state != eventStreamReconnecting {
		t.Fatalf("event stream state = %q after exit, want unchanged reconnecting", m.eventStreams.state)
	}
	if cmd != nil {
		t.Fatal("shutdown cancellation scheduled a reconnect after exit")
	}
}

func TestResyncClosesStreamAndRequestsSnapshot(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.lastEventSeq = 10
	m.eventStreams.generation = 1
	m.eventStreams.state = eventStreamLive

	evt := ipc.EventDTO{Type: "resync_required", Sequence: 0}
	next, cmd := m.Update(eventMsg{Generation: 1, Event: evt})
	m = next.(Model)

	if m.eventStreams.live() {
		t.Fatal("event stream should not be live after resync")
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
	next, _ := m.Update(eventMsg{Generation: 1, Event: evt})
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
	next, _ := m.Update(eventMsg{Generation: 1, Event: evt})
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
	next, _ = m.Update(eventMsg{Generation: 1, Event: evt2})
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
	m := newModel(&fakeClient{}, nil)
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned nil command")
	}
	if m.eventStreams.live() {
		t.Fatal("event stream should not be live before first snapshot")
	}
}

// TestTUIBootToHomeToQuit is an end-to-end smoke test that runs the TUI
// with a fake supervisor client through tea.NewProgram. It verifies the
// TUI starts in boot screen, transitions to home after snapshot, and can quit cleanly.
func TestTUIBootToHomeToQuit(t *testing.T) {
	// Create a fake client with a snapshot containing one connection
	fake := &fakeClient{
		snapshot: ipc.SnapshotDTO{
			Connections: []ipc.ConnectionDTO{
				{ID: "conn-1", Name: "test-connection", UserState: "open", ProviderID: "cloudflare", PublicAddress: "https://test.example.com"},
			},
			LastSeq: 1,
		},
	}

	// Create the TUI model with the fake client
	model := newModel(fake, nil)

	// Run the TUI program with a short timeout context
	// Use WithInputOSFile(0) to simulate stdin without needing a real TTY
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	program := tea.NewProgram(
		model,
		tea.WithContext(ctx),
		tea.WithInput(bytes.NewReader(nil)), // No input in tests
		tea.WithOutput(io.Discard),
	)

	// Run the program - it will start in boot screen, request snapshot,
	// transition to home, then context will cancel (simulating quit)
	_, err := program.Run()
	if err != nil {
		// Context cancellation is expected when test times out or we cancel
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("TUI program exited with error: %v", err)
		}
	}

	// Verify the model went through the expected states
	if fake.snapshotCalls == 0 {
		t.Fatal("expected GetSnapshot to be called at least once")
	}

	// The model should have received the snapshot and moved to home screen
	if model.screen != ScreenHome && model.screen != ScreenQuit {
		t.Logf("Final screen: %s", model.screen)
	}
}

// TestTUINewConnectionWizard runs the wizard flow with a fake client.
// This is a basic smoke test to ensure the wizard doesn't panic on startup.
func TestTUINewConnectionWizard(t *testing.T) {
	fake := &fakeClient{
		created: &ipc.ConnectionDTO{
			ID:   "conn-new",
			Name: "wizard-connection",
		},
		plan: &ipc.PlanDTO{
			ID:     "plan-1",
			Intent: "open",
			Steps: []ipc.StepDTO{
				{Summary: "Validate account"},
				{Summary: "Create tunnel"},
				{Summary: "Start connector"},
				{Summary: "Verify endpoint"},
			},
		},
		operation: &ipc.OperationDTO{
			ID:    "op-1",
			State: "completed",
			Steps: []ipc.StepDTO{{Summary: "Validate account", State: "succeeded"}, {Summary: "Create tunnel", State: "succeeded"}, {Summary: "Start connector", State: "succeeded"}, {Summary: "Verify endpoint", State: "succeeded"}},
		},
	}

	model := newModel(fake, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	program := tea.NewProgram(
		model,
		tea.WithContext(ctx),
		tea.WithInput(bytes.NewReader(nil)), // No input in tests
		tea.WithOutput(io.Discard),
	)

	_, err := program.Run()
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("TUI program exited with error: %v", err)
		}
	}
}

// TestRecoveryScreenIsReachable verifies that when the initial snapshot fails,
// the recovery screen is rendered (not the bare error view).
func TestRecoveryScreenIsReachable(t *testing.T) {
	m := newModel(&fakeClient{snapshotErr: errors.New("connection refused")}, nil)
	next, _ := m.Update(snapshotMsg{Err: errors.New("connection refused")})
	nm := next.(Model)

	if nm.screen != ScreenRecovery {
		t.Fatalf("screen = %q, want %q", nm.screen, ScreenRecovery)
	}

	view := nm.View().Content
	if !strings.Contains(view, "Retry") {
		t.Fatalf("recovery view should contain retry action, got:\n%s", view)
	}
	if !strings.Contains(view, "supervisor") {
		t.Fatalf("recovery view should mention supervisor, got:\n%s", view)
	}
}

// TestHelpNavigationReturnsToSourceScreen verifies that pressing ? saves the
// source screen and pressing esc returns to it.
func TestHelpNavigationReturnsToSourceScreen(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.screen = ScreenProviders

	m, _ = press(t, m, "?")
	if m.screen != ScreenHelp {
		t.Fatalf("screen after ? = %q, want %q", m.screen, ScreenHelp)
	}
	if m.prevScreen != ScreenProviders {
		t.Fatalf("prevScreen = %q, want %q", m.prevScreen, ScreenProviders)
	}

	// The help describes the screen it was opened from, not a generic key
	// encyclopedia. It is generated from that screen's action set, so it cannot
	// name a key the screen does not accept nor omit one it does.
	view := m.View().Content
	title, _ := screenHelp(ScreenProviders)
	if !strings.Contains(view, title) {
		t.Fatalf("help should describe the providers screen (%q), got:\n%s", title, view)
	}
	for _, action := range m.actionsFor(ScreenProviders).Advertised() {
		if action.Label == "" {
			continue
		}
		if !strings.Contains(view, action.Label) {
			t.Errorf("help omits the advertised action %q:\n%s", action.Label, view)
		}
	}

	m, _ = press(t, m, "esc")
	if m.screen != ScreenProviders {
		t.Fatalf("screen after esc from help = %q, want %q", m.screen, ScreenProviders)
	}
}

// TestHelpFromHomeShowsHomeContent verifies help shows home-specific content.
func TestHelpFromHomeShowsHomeContent(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())

	m, _ = press(t, m, "?")
	if m.screen != ScreenHelp {
		t.Fatalf("screen = %q, want %q", m.screen, ScreenHelp)
	}

	view := m.View().Content
	title, body := screenHelp(ScreenHome)
	if !strings.Contains(view, title) {
		t.Fatalf("help from home should describe the home screen (%q), got:\n%s", title, view)
	}
	// The description says what the screen is for, not only which keys it takes.
	if len(body) == 0 {
		t.Fatal("the home screen help has no description of the task")
	}
	if !strings.Contains(view, body[0]) {
		t.Fatalf("help omits the home screen description, got:\n%s", view)
	}
}

// TestInspectTabNavigation verifies that left/right keys switch inspect tabs.
func TestInspectTabNavigation(t *testing.T) {
	snap := testSnapshot()
	m := readyModel(&fakeClient{}, snap)

	m, _ = press(t, m, "enter")
	if m.screen != ScreenInspect {
		t.Fatalf("screen = %q, want %q", m.screen, ScreenInspect)
	}
	if m.inspect == nil {
		t.Fatal("inspect model should be initialized")
	}

	view := m.View().Content
	if !strings.Contains(view, "Overview") {
		t.Fatalf("default inspect tab should show Overview, got:\n%s", view)
	}

	m, _ = press(t, m, "right")
	view = m.View().Content
	if !strings.Contains(view, "Route") {
		t.Fatalf("after right, inspect should show Route tab, got:\n%s", view)
	}

	m, _ = press(t, m, "right")
	view = m.View().Content
	if !strings.Contains(view, "Activity") {
		t.Fatalf("after second right, inspect should show Activity tab, got:\n%s", view)
	}

	m, _ = press(t, m, "left")
	view = m.View().Content
	if !strings.Contains(view, "Route") {
		t.Fatalf("after left, inspect should show Route tab, got:\n%s", view)
	}
}

// TestListViewportKeepsSelectionVisible verifies that the list offset adjusts
// to keep the selected item visible when there are many connections.
func TestListViewportKeepsSelectionVisible(t *testing.T) {
	snap := ipc.SnapshotDTO{}
	for i := 0; i < 20; i++ {
		snap.Connections = append(snap.Connections, ipc.ConnectionDTO{
			ID:        fmt.Sprintf("conn-%d", i),
			Name:      fmt.Sprintf("service-%d", i),
			UserState: "Closed",
		})
	}
	m := readyModel(&fakeClient{}, snap)
	m.height = 15

	m.selectedID = "conn-19"
	view := m.View().Content

	if !strings.Contains(view, "service-19") {
		t.Fatalf("selected connection should be visible in viewport, got:\n%s", view)
	}
	if !strings.Contains(view, "above") {
		t.Fatalf("should show scroll-up indicator, got:\n%s", view)
	}
}

// TestNavigationStack verifies that the navigation stack works correctly
// for back navigation with Esc.
func TestNavigationStack(t *testing.T) {
	snap := testSnapshot()
	m := readyModel(&fakeClient{}, snap)

	// Start at home
	if m.screen != ScreenHome {
		t.Fatalf("initial screen = %q, want %q", m.screen, ScreenHome)
	}

	// Navigate to providers
	m, _ = press(t, m, "p")
	if m.screen != ScreenProviders {
		t.Fatalf("screen after p = %q, want %q", m.screen, ScreenProviders)
	}
	if len(m.navStack) != 1 || m.navStack[0] != ScreenHome {
		t.Fatalf("navStack = %v, want [home]", m.navStack)
	}

	// Navigate to help from providers
	m, _ = press(t, m, "?")
	if m.screen != ScreenHelp {
		t.Fatalf("screen after ? = %q, want %q", m.screen, ScreenHelp)
	}
	// Help uses prevScreen, not navStack
	if m.prevScreen != ScreenProviders {
		t.Fatalf("prevScreen = %q, want %q", m.prevScreen, ScreenProviders)
	}

	// Esc from help returns to providers
	m, _ = press(t, m, "esc")
	if m.screen != ScreenProviders {
		t.Fatalf("screen after esc from help = %q, want %q", m.screen, ScreenProviders)
	}

	// Esc from providers returns to home via nav stack
	m, _ = press(t, m, "esc")
	if m.screen != ScreenHome {
		t.Fatalf("screen after esc from providers = %q, want %q", m.screen, ScreenHome)
	}
	if len(m.navStack) != 0 {
		t.Fatalf("navStack should be empty at home, got %v", m.navStack)
	}

	// Esc from home does nothing
	m, _ = press(t, m, "esc")
	if m.screen != ScreenHome {
		t.Fatalf("screen after esc from home = %q, want %q", m.screen, ScreenHome)
	}
}

// TestNavigationStackWithInspect verifies navigation through inspect screen.
func TestNavigationStackWithInspect(t *testing.T) {
	snap := testSnapshot()
	m := readyModel(&fakeClient{}, snap)

	// Enter inspect
	m, _ = press(t, m, "enter")
	if m.screen != ScreenInspect {
		t.Fatalf("screen after enter = %q, want %q", m.screen, ScreenInspect)
	}
	if len(m.navStack) != 1 || m.navStack[0] != ScreenHome {
		t.Fatalf("navStack = %v, want [home]", m.navStack)
	}

	// Open help from inspect
	m, _ = press(t, m, "?")
	if m.screen != ScreenHelp {
		t.Fatalf("screen after ? = %q, want %q", m.screen, ScreenHelp)
	}
	if m.prevScreen != ScreenInspect {
		t.Fatalf("prevScreen = %q, want %q", m.prevScreen, ScreenInspect)
	}

	// Esc returns to inspect
	m, _ = press(t, m, "esc")
	if m.screen != ScreenInspect {
		t.Fatalf("screen after esc = %q, want %q", m.screen, ScreenInspect)
	}

	// Esc returns to home
	m, _ = press(t, m, "esc")
	if m.screen != ScreenHome {
		t.Fatalf("screen after esc = %q, want %q", m.screen, ScreenHome)
	}
}

func TestOperationsScreen(t *testing.T) {
	snap := testSnapshot()
	m := readyModel(&fakeClient{}, snap)

	// Press 'o' to open operations screen
	m, cmd := press(t, m, "o")
	if m.screen != ScreenOperations {
		t.Fatalf("screen after 'o' = %q, want %q", m.screen, ScreenOperations)
	}
	if cmd == nil {
		t.Fatal("expected loadOperationsCmd to be returned")
	}

	// Simulate operations loaded message
	ops := []ipc.OperationDTO{
		{ID: "op-1", ConnectionID: "conn-1", State: "completed", PlanID: "plan-1"},
		{ID: "op-2", ConnectionID: "conn-2", State: "failed", PlanID: "plan-2", Error: "test error"},
	}
	next, _ := m.Update(operationsLoadedMsg{Token: m.historyRequests.start("0"), Operations: ops})
	m = next.(Model)

	if len(m.operations) != 2 {
		t.Fatalf("expected 2 operations, got %d", len(m.operations))
	}
	if m.opsSelectedIdx != 0 {
		t.Fatalf("expected selected index 0, got %d", m.opsSelectedIdx)
	}

	// Navigate down
	m, _ = press(t, m, "down")
	if m.opsSelectedIdx != 1 {
		t.Fatalf("expected selected index 1 after down, got %d", m.opsSelectedIdx)
	}

	// Navigate up
	m, _ = press(t, m, "up")
	if m.opsSelectedIdx != 0 {
		t.Fatalf("expected selected index 0 after up, got %d", m.opsSelectedIdx)
	}

	// Esc returns to home
	m, _ = press(t, m, "esc")
	if m.screen != ScreenHome {
		t.Fatalf("screen after esc = %q, want %q", m.screen, ScreenHome)
	}
}

// TestEnterOnConnectionLoadsAuthoritativeDetail pins the inspect screen to the
// supervisor's detail view. The root model previously declared a
// connectionDetail field that nothing ever populated, because no IPC route or
// client method existed to fetch it, so inspect rendered placeholders.
func TestEnterOnConnectionLoadsAuthoritativeDetail(t *testing.T) {
	fake := &fakeClient{
		detail: &ipc.ConnectionDetailDTO{
			Revision:  7,
			Resources: []ipc.ManagedResourceDTO{{Type: "tunnel", ExternalID: "tun-abc", Ownership: "managed"}},
			Processes: []ipc.ProcessDTO{{PID: 4242, Status: "running"}},
		},
	}
	m := readyModel(fake, testSnapshot())

	next, cmd := m.Update(keyMsg("enter"))
	m = next.(Model)
	if m.screen != ScreenInspect {
		t.Fatalf("screen = %q, want inspect", m.screen)
	}
	if cmd == nil {
		t.Fatal("entering inspect issued no command to load connection detail")
	}

	// Entering inspect now loads detail and logs together, so the command is a
	// batch; find the detail message within it.
	detailMsg, ok := findDetailMsg(cmd())
	if !ok {
		t.Fatal("entering inspect issued no command that loads connection detail")
	}
	if detailMsg.ConnectionID != "conn-1" {
		t.Fatalf("detail requested for %q, want conn-1", detailMsg.ConnectionID)
	}
	if fake.detailCalls != 1 {
		t.Fatalf("GetConnectionDetail called %d times, want 1", fake.detailCalls)
	}

	next, _ = m.Update(detailMsg)
	m = next.(Model)
	if m.connectionDetail == nil {
		t.Fatal("connectionDetail was not stored on the model")
	}
	if m.inspect == nil || m.inspect.Detail == nil {
		t.Fatal("inspect screen did not receive the detail")
	}
	if m.inspect.Detail.Revision != 7 {
		t.Fatalf("inspect detail revision = %d, want 7", m.inspect.Detail.Revision)
	}

	// External identifiers live on the Technical tab.
	for i := 0; i < 3; i++ {
		next, _ = m.Update(keyMsg("right"))
		m = next.(Model)
	}
	view := m.View().Content
	for _, want := range []string{"tun-abc", "4242"} {
		if !strings.Contains(view, want) {
			t.Fatalf("inspect view does not show %q:\n%s", want, view)
		}
	}
}

// TestStaleConnectionDetailIsIgnored ensures a slow reply for a connection the
// user already navigated away from cannot overwrite the current one's detail.
func TestStaleConnectionDetailIsIgnored(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)

	next, _ = m.Update(connectionDetailMsg{
		ConnectionID: "some-other-connection",
		Detail:       &ipc.ConnectionDetailDTO{Revision: 99},
	})
	m = next.(Model)

	if m.connectionDetail != nil {
		t.Fatal("detail for a different connection was applied to the current one")
	}
}

// TestConnectionDetailErrorKeepsPreviousDetail ensures a failed refresh reports
// staleness instead of blanking a view that is still showing real state.
func TestConnectionDetailErrorKeepsPreviousDetail(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)

	next, _ = m.Update(connectionDetailMsg{
		ConnectionID: "conn-1",
		Detail:       &ipc.ConnectionDetailDTO{Revision: 3},
	})
	m = next.(Model)

	next, _ = m.Update(connectionDetailMsg{
		ConnectionID: "conn-1",
		Err:          errors.New("supervisor unreachable"),
	})
	m = next.(Model)

	if m.connectionDetail == nil || m.connectionDetail.Revision != 3 {
		t.Fatal("a failed detail refresh discarded the last known state")
	}
	if !strings.Contains(m.status, "unavailable") {
		t.Fatalf("status %q does not report that detail is unavailable", m.status)
	}
}

// TestActivityTabDoesNotFabricateMetrics ensures the inspect screen never
// presents traffic telemetry it does not collect as merely-empty data.
//
// It used to state flatly that telemetry is not collected for any provider. That
// was a statement about the interface presented as one about Portico: the IPC
// route, the DTO and the ngrok implementation all existed and nothing asked for
// them. What must never happen is the opposite error — a zero shown for a
// counter nobody measured.
func TestActivityTabDoesNotFabricateMetrics(t *testing.T) {
	t.Run("a provider that reports nothing says so", func(t *testing.T) {
		m := activityTabModel(t, &ipc.TelemetryDTO{
			Available:   false,
			Unavailable: "provider does not expose traffic telemetry",
		})

		view := m.View().Content
		for _, forbidden := range []string{
			"Request rate: --", "Error count:  --", "Latency:      --",
			"Requests since it opened: 0",
		} {
			if strings.Contains(view, forbidden) {
				t.Fatalf("the activity tab fabricates a metric: %q\n%s", forbidden, view)
			}
		}
		if !strings.Contains(view, "does not expose traffic telemetry") {
			t.Fatalf("the activity tab does not say why there are no figures:\n%s", view)
		}
	})

	t.Run("a provider that reports counts shows them", func(t *testing.T) {
		m := activityTabModel(t, &ipc.TelemetryDTO{
			Available:       true,
			HasCounts:       true,
			RequestCount:    4213,
			ConnectionCount: 7,
			SampledAt:       time.Now().UTC().Format(time.RFC3339),
		})

		view := m.View().Content
		if !strings.Contains(view, "4213") {
			t.Fatalf("the request count the provider reported is not shown:\n%s", view)
		}
		if !strings.Contains(view, "7") {
			t.Fatalf("the connection count the provider reported is not shown:\n%s", view)
		}
		// ngrok reports counts and no byte totals. A zero would be
		// indistinguishable from a measured zero, so the absence is stated.
		if !strings.Contains(view, "does not report byte totals") {
			t.Fatalf("an unmeasured counter is not reported as unmeasured:\n%s", view)
		}
		if strings.Contains(view, "0 B") {
			t.Fatalf("an unmeasured byte total was shown as zero:\n%s", view)
		}
	})
}

// activityTabModel opens Inspect on the Activity tab with the given telemetry.
func activityTabModel(t *testing.T, telemetry *ipc.TelemetryDTO) Model {
	t.Helper()
	client := &fakeClient{
		detail:       &ipc.ConnectionDetailDTO{},
		telemetryDTO: telemetry,
	}
	m := readyModel(client, testSnapshot())
	m.width = 100

	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)
	next, _ = m.Update(connectionDetailMsg{ConnectionID: "conn-1", Detail: &ipc.ConnectionDetailDTO{}})
	m = next.(Model)
	next, _ = m.Update(telemetryLoadedMsg{
		Token:     m.telemetryRequests.start("conn-1"),
		Telemetry: telemetry,
	})
	m = next.(Model)

	// Overview -> Route -> Activity.
	for i := 0; i < 2; i++ {
		next, _ = m.Update(keyMsg("right"))
		m = next.(Model)
	}
	return m
}

// TestOperationsScreenDistinguishesEmptyFromUnavailable pins the operations
// screen against reporting an unreadable history as an authoritative empty one.
func TestOperationsScreenDistinguishesEmptyFromUnavailable(t *testing.T) {
	t.Run("available and empty", func(t *testing.T) {
		m := readyModel(&fakeClient{}, testSnapshot())
		next, _ := m.Update(operationsLoadedMsg{Token: m.historyRequests.start("0"), Operations: nil, Available: true})
		m = next.(Model)
		m.screen = ScreenOperations

		view := m.View().Content
		if !strings.Contains(view, "has not done anything yet") {
			t.Fatalf("empty history not reported as authoritative:\n%s", view)
		}
		if strings.Contains(view, "could not read") {
			t.Fatalf("readable empty history was reported as unreadable:\n%s", view)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		m := readyModel(&fakeClient{}, testSnapshot())
		next, _ := m.Update(operationsLoadedMsg{
			Token:       m.historyRequests.start("0"),
			Operations:  nil,
			Available:   false,
			Unavailable: "database is locked",
		})
		m = next.(Model)
		m.screen = ScreenOperations

		view := m.View().Content
		if !strings.Contains(view, "could not read its history") {
			t.Fatalf("unreadable history was not reported as unreadable:\n%s", view)
		}
		if !strings.Contains(view, "database is locked") {
			t.Fatalf("unavailability reason was not shown:\n%s", view)
		}
		if strings.Contains(view, "has not done anything yet") {
			t.Fatalf("unreadable history claimed no operations have run:\n%s", view)
		}
	})
}

// TestOperationsScreenShowsIntentNotPlanID pins the fix for the operations list
// rendering the opaque plan ID in place of the operation's intent.
func TestOperationsScreenShowsIntentNotPlanID(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	next, _ := m.Update(operationsLoadedMsg{
		Token:     m.historyRequests.start("0"),
		Available: true,
		Operations: []ipc.OperationDTO{{
			ID: "op-1", PlanID: "plan-7f3a9c", ConnectionID: "conn-1",
			State: "completed", Intent: "open", ProviderID: "cloudflare",
			StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:00:09Z",
		}},
	})
	m = next.(Model)
	m.screen = ScreenOperations

	view := m.View().Content
	// The intent is stated as the action taken, in the words a user would use,
	// rather than as the wire value or the plan's opaque ID.
	if !strings.Contains(view, "Opened") {
		t.Fatalf("operation intent not shown as an action:\n%s", view)
	}
	if strings.Contains(view, "plan-7f3a9c") && !strings.Contains(view, "TECHNICAL DETAIL") {
		t.Fatalf("the plan ID leads the row instead of being technical detail:\n%s", view)
	}
	// The connection is named, not identified.
	if !strings.Contains(view, "web") {
		t.Fatalf("the connection name is not shown:\n%s", view)
	}
	// The provider remains available, under technical detail.
	if !strings.Contains(view, "cloudflare") {
		t.Fatalf("operation provider not shown:\n%s", view)
	}
	if !strings.Contains(view, "9s") {
		t.Fatalf("operation duration not shown:\n%s", view)
	}
}

// TestOperationsScreenStylesCompletedAsSuccess pins the operation-state
// vocabulary. Operations reach "completed"; only steps reach "succeeded". The
// list styled on "succeeded", so a successful operation was rendered muted,
// indistinguishable from an unknown state.
func TestOperationsScreenStylesCompletedAsSuccess(t *testing.T) {
	completed := renderOperationStyleFor(t, "completed")
	failed := renderOperationStyleFor(t, "failed")
	unknown := renderOperationStyleFor(t, "some-unknown-state")

	if completed == unknown {
		t.Fatal("a completed operation renders identically to an unknown state")
	}
	if completed == failed {
		t.Fatal("a completed operation renders identically to a failed one")
	}
}

// renderOperationStyleFor renders the operations list for a single operation in
// the given state and returns the styled line.
func renderOperationStyleFor(t *testing.T, state string) string {
	t.Helper()
	m := readyModel(&fakeClient{}, testSnapshot())
	next, _ := m.Update(operationsLoadedMsg{
		Token:     m.historyRequests.start("0"),
		Available: true,
		Operations: []ipc.OperationDTO{{
			ID: "op-1", PlanID: "plan-1", ConnectionID: "conn-1",
			State: state, Intent: "open", StartedAt: "2026-01-01T00:00:00Z",
		}},
	})
	m = next.(Model)
	m.screen = ScreenOperations
	for _, line := range strings.Split(m.View().Content, "\n") {
		if strings.Contains(line, state) {
			return line
		}
	}
	t.Fatalf("no rendered line contained state %q", state)
	return ""
}

// TestEditStringIsRuneAware pins audit item 21. Text editing used byte
// operations: len(key) == 1 discarded every multi-byte character outright, and
// backspace sliced a single byte off the end, splitting code points and leaving
// invalid UTF-8 in the field.
func TestEditStringIsRuneAware(t *testing.T) {
	for name, input := range map[string]string{
		"accented latin": "é",
		"arabic":         "ع",
		"cjk":            "中",
		"emoji":          "🔥",
		"combining mark": "é",
		"ascii":          "a",
	} {
		t.Run(name, func(t *testing.T) {
			got := ""
			for _, r := range input {
				got = editString(got, string(r))
			}
			if got != input {
				t.Fatalf("typing %q produced %q; multi-byte input was dropped", input, got)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("field holds invalid UTF-8: %q", got)
			}
		})
	}
}

// TestEditStringBackspaceRemovesWholeRunes ensures deletion never splits a
// multi-byte character.
func TestEditStringBackspaceRemovesWholeRunes(t *testing.T) {
	s := "aé中🔥"
	for range utf8.RuneCountInString(s) {
		s = editString(s, "backspace")
		if !utf8.ValidString(s) {
			t.Fatalf("backspace produced invalid UTF-8: %q", s)
		}
	}
	if s != "" {
		t.Fatalf("field = %q after deleting every rune, want empty", s)
	}
	// Backspace on an empty field must be a no-op, not a panic.
	if got := editString("", "backspace"); got != "" {
		t.Fatalf("backspace on empty field = %q", got)
	}
}

// TestEditStringIgnoresNamedAndControlKeys ensures navigation keys are not
// inserted as text.
func TestEditStringIgnoresNamedAndControlKeys(t *testing.T) {
	for _, key := range []string{"enter", "left", "right", "up", "down", "tab", "\x00", "\x7f"} {
		if got := editString("abc", key); got != "abc" {
			t.Fatalf("key %q was inserted as text: %q", key, got)
		}
	}
}

const setupProbeSecret = "cf-token-DO-NOT-LEAK-9f3a"

// cloudflareSetupFlow is the flow Cloudflare declares, as the TUI now receives
// it: the fields come from the provider rather than from the screen.
func cloudflareSetupFlow() *ipc.SetupFlowDTO {
	return &ipc.SetupFlowDTO{
		ProviderID: "cloudflare",
		Kind:       "account",
		Summary:    "Configure a Cloudflare account.",
		Fields: []ipc.SetupFieldDTO{
			{ID: "account_id", Label: "Account ID", Required: true},
			{ID: "label", Label: "Label"},
			{ID: "zone_id", Label: "Zone ID"},
			{ID: "credential", Label: "API token", Secret: true, Required: true},
		},
	}
}

// startSetupWithFlow opens provider setup and delivers the declared flow.
func startSetupWithFlow(t *testing.T, flow *ipc.SetupFlowDTO) Model {
	t.Helper()
	m := readyModel(&fakeClient{setupFlow: flow}, testSnapshot())
	next, cmd := m.beginProviderSetup(flow.ProviderID)
	m = next
	if cmd == nil {
		t.Fatal("beginning setup issued no command to load the flow")
	}
	delivered, _ := m.Update(cmd())
	return delivered.(Model)
}

// enterCredentialStep drives provider setup as far as the credential field with
// the probe secret typed in.
func enterCredentialStep(t *testing.T) Model {
	t.Helper()
	m := startSetupWithFlow(t, cloudflareSetupFlow())
	for _, r := range "account-a" {
		next, _ := m.Update(keyMsg(string(r)))
		m = next.(Model)
	}
	for range 3 { // past account ID, label and zone
		next, _ := m.Update(keyMsg("enter"))
		m = next.(Model)
	}
	if got := m.providerSetupIndex; got != 3 {
		t.Fatalf("setup index = %d, want 3 (the credential field)", got)
	}
	for _, r := range setupProbeSecret {
		next, _ := m.Update(keyMsg(string(r)))
		m = next.(Model)
	}
	if m.providerSetupValue("credential") != setupProbeSecret {
		t.Fatalf("credential not captured: %q", m.providerSetupValue("credential"))
	}
	return m
}

// TestCredentialIsClearedOnEveryExitPath pins audit item 18. The credential was
// cleared only on success, so cancelling, stepping back past the credential
// field, or quitting left the secret resident in the model.
func TestCredentialIsClearedOnEveryExitPath(t *testing.T) {
	t.Run("back past the credential step", func(t *testing.T) {
		m := enterCredentialStep(t)
		next, _ := m.Update(keyMsg("esc"))
		m = next.(Model)
		if m.providerSetupValue("credential") != "" {
			t.Fatalf("credential survived stepping back: %q", m.providerSetupValue("credential"))
		}
	})

	t.Run("cancelling setup", func(t *testing.T) {
		m := enterCredentialStep(t)
		// Back to the zone step, then to label, account, then out.
		for range 4 {
			next, _ := m.Update(keyMsg("esc"))
			m = next.(Model)
		}
		if m.screen == ScreenProviderSetup {
			t.Fatalf("setup screen = %s, want it closed (exited)", m.screen)
		}
		if m.providerSetupValue("credential") != "" || m.providerSetupValue("account_id") != "" {
			t.Fatalf("setup state survived cancellation: cred=%q id=%q", m.providerSetupValue("credential"), m.providerSetupValue("account_id"))
		}
	})

	t.Run("validation failure", func(t *testing.T) {
		m := enterCredentialStep(t)
		next, _ := m.Update(providerAccountConfiguredMsg{
			Generation: m.providerSetupRequests.next(),
			Err:        errors.New("token rejected"),
		})
		m = next.(Model)
		if m.providerSetupValue("credential") != "" {
			t.Fatalf("rejected credential stayed in memory: %q", m.providerSetupValue("credential"))
		}
		// The answers that were accepted must survive so they are not retyped.
		if m.providerSetupValue("account_id") != "account-a" {
			t.Fatalf("accepted answers were discarded: id=%q", m.providerSetupValue("account_id"))
		}
	})

	t.Run("quit", func(t *testing.T) {
		m := enterCredentialStep(t)
		next, _ := m.Update(keyMsg("ctrl+c"))
		m = next.(Model)
		if m.providerSetupValue("credential") != "" {
			t.Fatalf("credential survived shutdown: %q", m.providerSetupValue("credential"))
		}
	})

	t.Run("success", func(t *testing.T) {
		m := enterCredentialStep(t)
		next, _ := m.Update(providerAccountConfiguredMsg{
			Generation: m.providerSetupRequests.next(),
			Response:   &ipc.ConfigureProviderAccountResponse{Validated: true},
		})
		m = next.(Model)
		if m.providerSetupValue("credential") != "" {
			t.Fatalf("credential survived success: %q", m.providerSetupValue("credential"))
		}
	})
}

// TestCredentialNeverAppearsInAnyRenderedView ensures the secret is not printed
// on the setup screen, in a status line, or in an error message.
func TestCredentialNeverAppearsInAnyRenderedView(t *testing.T) {
	m := enterCredentialStep(t)

	// While it is held, the credential must still be masked on screen.
	if view := m.View().Content; strings.Contains(view, setupProbeSecret) {
		t.Fatalf("credential rendered on the setup screen:\n%s", view)
	}

	// A backend error must not echo the credential back into the UI.
	next, _ := m.Update(providerAccountConfiguredMsg{
		Generation: m.providerSetupRequests.next(),
		Err:        errors.New("token rejected for account account-a"),
	})
	m = next.(Model)

	for _, screen := range []ScreenID{
		ScreenHome, ScreenProviders, ScreenOperations, ScreenHelp, ScreenDiscovery,
	} {
		m.screen = screen
		if view := m.View().Content; strings.Contains(view, setupProbeSecret) {
			t.Fatalf("credential leaked into the %s view:\n%s", screen, view)
		}
	}
	if strings.Contains(m.status, setupProbeSecret) {
		t.Fatalf("credential leaked into the status line: %q", m.status)
	}
	if strings.Contains(m.providerSetupError, setupProbeSecret) {
		t.Fatalf("credential leaked into the setup error: %q", m.providerSetupError)
	}
}

// TestLogsTabDistinguishesUnreadableFromEmpty pins the last of the
// authoritative-empty-state fixes. An unreadable log subsystem must not render
// as "the connector produced no output".
func TestLogsTabDistinguishesUnreadableFromEmpty(t *testing.T) {
	openLogsTab := func(t *testing.T, logs *ipc.ConnectionLogsDTO) string {
		t.Helper()
		m := readyModel(&fakeClient{logs: logs}, testSnapshot())
		next, _ := m.Update(keyMsg("enter"))
		m = next.(Model)
		next, _ = m.Update(connectionLogsMsg{ConnectionID: "conn-1", Logs: logs})
		m = next.(Model)
		for range 4 {
			next, _ = m.Update(keyMsg("right"))
			m = next.(Model)
		}
		return m.View().Content
	}

	t.Run("read and empty", func(t *testing.T) {
		view := openLogsTab(t, &ipc.ConnectionLogsDTO{Available: true})
		if !strings.Contains(view, "has not written any output yet") {
			t.Fatalf("empty logs not reported authoritatively:\n%s", view)
		}
		if strings.Contains(view, "unavailable") {
			t.Fatalf("readable empty logs reported as unavailable:\n%s", view)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		view := openLogsTab(t, &ipc.ConnectionLogsDTO{
			Available:   false,
			Unavailable: "no connector process is being supervised",
		})
		if !strings.Contains(view, "unavailable") {
			t.Fatalf("unreadable logs not reported as such:\n%s", view)
		}
		if !strings.Contains(view, "does not mean the connector produced no output") {
			t.Fatalf("unreadable logs do not disclaim emptiness:\n%s", view)
		}
	})

	t.Run("lines are rendered with their stream", func(t *testing.T) {
		view := openLogsTab(t, &ipc.ConnectionLogsDTO{
			Available: true,
			Lines: []ipc.LogLineDTO{
				{Stream: "stdout", Text: "INF Connection established"},
				{Stream: "stderr", Text: "WRN retrying"},
			},
		})
		for _, want := range []string{"stdout", "INF Connection established", "stderr", "WRN retrying"} {
			if !strings.Contains(view, want) {
				t.Fatalf("log view missing %q:\n%s", want, view)
			}
		}
	})
}

// findDetailMsg locates a connectionDetailMsg in a message that may be a batch
// of several commands.
func findDetailMsg(msg tea.Msg) (connectionDetailMsg, bool) {
	switch typed := msg.(type) {
	case connectionDetailMsg:
		return typed, true
	case tea.BatchMsg:
		for _, cmd := range typed {
			if cmd == nil {
				continue
			}
			if found, ok := findDetailMsg(cmd()); ok {
				return found, true
			}
		}
	}
	return connectionDetailMsg{}, false
}

// TestSetupScreenShowsWhatIsNeededAndWhatIsAlreadyThere pins the screen that
// exists to make setup navigable: it must state the position, surface
// credentials already present, and give a next action for what is missing.
func TestSetupScreenShowsWhatIsNeededAndWhatIsAlreadyThere(t *testing.T) {
	readiness := &ipc.ReadinessDTO{
		Summary:    "1 provider ready. 1 of 2 connections need attention.",
		LaunchMode: "manual",
		Providers: []ipc.ProviderReadinessDTO{
			{
				ID: "ngrok", DisplayName: "ngrok", Blocked: true,
				Availability: "unconfigured",
				Summary:      "A credential was found on this machine. Finish setup to use it.",
				Reason:       "no account is configured",
				Credentials: []ipc.CredentialSourceDTO{
					{Kind: "client_config", Location: "/home/u/.config/ngrok/ngrok.yml", Present: true,
						Description: "The ngrok agent's own saved token."},
					{Kind: "environment", Location: "NGROK_AUTHTOKEN", Present: false,
						Action: "Export NGROK_AUTHTOKEN, or run: ngrok config add-authtoken <token>"},
				},
			},
		},
		Connections: []ipc.ConnectionReadinessDTO{
			{ID: "c1", Name: "web", Ready: false, Blockers: []string{"A permanent address needs a hostname."}},
		},
	}

	m := readyModel(&fakeClient{readiness: readiness}, testSnapshot())
	next, cmd := m.Update(keyMsg("s"))
	m = next.(Model)
	if m.screen != ScreenSetup {
		t.Fatalf("screen = %q, want setup", m.screen)
	}
	if cmd == nil {
		t.Fatal("opening setup issued no command to load readiness")
	}
	next, _ = m.Update(readinessMsg{Readiness: readiness})
	m = next.(Model)

	view := m.View().Content

	// The first thing shown must answer "can I use this yet?".
	if !strings.Contains(view, "1 provider ready") {
		t.Fatalf("setup view does not lead with the overall position:\n%s", view)
	}
	// A credential already on the machine must read as a finding, not a demand.
	if !strings.Contains(view, "found") || !strings.Contains(view, "ngrok.yml") {
		t.Fatalf("setup view does not surface the existing credential:\n%s", view)
	}
	// The missing one must come with something to do about it.
	if !strings.Contains(view, "add-authtoken") {
		t.Fatalf("setup view does not offer the next action:\n%s", view)
	}
	// A blocked connection must say why in plain language.
	if !strings.Contains(view, "needs a hostname") {
		t.Fatalf("setup view does not explain the connection blocker:\n%s", view)
	}
	// The launch gate must be legible rather than a bare mode name.
	if !strings.Contains(view, "nothing opens by itself") {
		t.Fatalf("setup view does not explain the launch mode:\n%s", view)
	}
}

// openSetupWith puts the model on the setup screen with a loaded readiness
// view, which is the state the launch-mode key operates in.
func openSetupWith(t *testing.T, fake *fakeClient, readiness *ipc.ReadinessDTO) Model {
	t.Helper()
	m := readyModel(fake, testSnapshot())
	next, _ := m.Update(keyMsg("s"))
	m = next.(Model)
	next, _ = m.Update(readinessMsg{Readiness: readiness})
	return next.(Model)
}
