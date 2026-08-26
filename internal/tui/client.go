package tui

import (
	"context"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// SupervisorClient is the subset of the IPC client the TUI depends on.
// The root model is built against this interface so tests can substitute
// a fake without a real socket; production passes *ipc.Client.
type SupervisorClient interface {
	GetSnapshot(ctx context.Context) (*ipc.SnapshotDTO, error)
	GetConnectionDetail(ctx context.Context, id string) (*ipc.ConnectionDetailDTO, error)
	ConnectionLogs(ctx context.Context, id string, lines int) (*ipc.ConnectionLogsDTO, error)
	Readiness(ctx context.Context) (*ipc.ReadinessDTO, error)
	SupportExport(ctx context.Context) (*ipc.SupportExportDTO, error)
	SetLaunchMode(ctx context.Context, mode string) (*ipc.LaunchModeDTO, error)
	// Settings and UpdateSettings reach the durable operational settings the
	// supervisor owns. The TUI reads and writes them here rather than touching
	// the config file, which only the supervisor opens.
	Settings(ctx context.Context) (*ipc.SettingsDTO, error)
	UpdateSettings(ctx context.Context, req ipc.SettingsRequest) (*ipc.SettingsDTO, error)
	RotateSecretKey(ctx context.Context) (*ipc.RotateSecretKeyDTO, error)
	// Telemetry is the traffic sample a provider supplies for a connection,
	// when it supplies one. The IPC route, the DTO and the ngrok
	// implementation all existed while no client asked for them.
	Telemetry(ctx context.Context, id string) (*ipc.TelemetryDTO, error)
	// ReverifyProviderAccount checks a stored credential against the provider
	// without exposing or changing it.
	ReverifyProviderAccount(ctx context.Context, providerID, accountID string) (
		*ipc.ReverifyProviderAccountResponse, error)
	// ReplaceProviderAccountCredential rotates the secret behind an existing
	// account, keeping its identity so connections using it keep working.
	ReplaceProviderAccountCredential(ctx context.Context, providerID, accountID, credential string) (
		*ipc.ReplaceCredentialResponse, error)
	ProviderSetupFlow(ctx context.Context, providerID string) (*ipc.SetupFlowDTO, error)
	RecommendProvider(ctx context.Context, req ipc.ProviderRecommendationRequest) (
		*ipc.ProviderRecommendationResponse, error)
	PlanOpen(ctx context.Context, connID string) (*ipc.PlanDTO, error)
	PlanClose(ctx context.Context, connID string) (*ipc.PlanDTO, error)
	PlanRepair(ctx context.Context, connID string) (*ipc.PlanDTO, error)
	PlanDelete(ctx context.Context, connID string) (*ipc.PlanDTO, error)
	PlanEdit(ctx context.Context, connID string, req ipc.UpdateConnectionRequest) (*ipc.PlanDTO, error)
	ApplyPlan(ctx context.Context, planID string) (*ipc.OperationDTO, error)
	ApplyPlanWithIdempotency(ctx context.Context, planID, idempotencyKey string) (*ipc.OperationDTO, error)
	GetOperation(ctx context.Context, operationID string) (*ipc.OperationDTO, error)
	GetOperationHistory(ctx context.Context) (*ipc.OperationHistoryDTO, error)
	GetOperationHistoryLimit(ctx context.Context, limit int) (*ipc.OperationHistoryDTO, error)
	GetOperationEvents(ctx context.Context, operationID string) ([]ipc.EventDTO, error)
	CreateConnection(ctx context.Context, req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error)
	CloneConnection(ctx context.Context, id string, req ipc.CloneConnectionRequest) (*ipc.ConnectionDTO, error)
	ConnectEventStream(ctx context.Context, lastSeq int64) (*ipc.EventStream, error)
	Diagnostics(ctx context.Context, connID string) ([]ipc.DiagnosticDTO, error)
	Discovery(ctx context.Context) (*ipc.DiscoveryDTO, error)
	RefreshDiscovery(ctx context.Context) (*ipc.DiscoveryDTO, error)
	ConfigureProviderAccount(ctx context.Context, providerID string, req ipc.ConfigureProviderAccountRequest) (*ipc.ConfigureProviderAccountResponse, error)
	RemoveProviderAccount(ctx context.Context, providerID, accountID, fingerprint string) (*ipc.RemoveProviderAccountResponse, error)
	PreviewProviderAccountRemoval(ctx context.Context, providerID, accountID string) (*ipc.AccountRemovalPreviewDTO, error)
}

// Ensure the real client satisfies the interface.
var _ SupervisorClient = (*ipc.Client)(nil)
