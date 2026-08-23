package ipc

import "context"

// NullHandler answers every IPC request with nothing.
//
// Embed it in a test handler and override only the methods that test
// exercises, so a vertical test does not have to implement thirty-one methods
// it does not care about.
type NullHandler struct{}

func (NullHandler) HandleSnapshot() (*SnapshotDTO, error)                          { return nil, nil }
func (NullHandler) HandleListConnections() ([]ConnectionDTO, error)                { return nil, nil }
func (NullHandler) HandleGetConnection(string) (*ConnectionDTO, error)             { return nil, nil }
func (NullHandler) HandleGetConnectionDetail(string) (*ConnectionDetailDTO, error) { return nil, nil }
func (NullHandler) HandleCloneConnection(string, CloneConnectionRequest) (*ConnectionDTO, error) {
	return nil, nil
}
func (NullHandler) HandleCreateConnection(CreateConnectionRequest) (*ConnectionDTO, error) {
	return nil, nil
}
func (NullHandler) HandleUpdateConnection(string, UpdateConnectionRequest) (*ConnectionDTO, error) {
	return nil, nil
}
func (NullHandler) HandlePlanOpen(string) (*PlanDTO, error)                          { return nil, nil }
func (NullHandler) HandlePlanClose(string) (*PlanDTO, error)                         { return nil, nil }
func (NullHandler) HandlePlanEdit(string, UpdateConnectionRequest) (*PlanDTO, error) { return nil, nil }
func (NullHandler) HandlePlanRepair(string) (*PlanDTO, error)                        { return nil, nil }
func (NullHandler) HandlePlanDelete(string) (*PlanDTO, error)                        { return nil, nil }
func (NullHandler) HandleApplyPlan(string, string) (*OperationDTO, error)            { return nil, nil }
func (NullHandler) HandleListProviders() ([]ProviderDTO, error)                      { return nil, nil }
func (NullHandler) HandleProviderRecommendation(ProviderRecommendationRequest) (*ProviderRecommendationResponse, error) {
	return nil, nil
}
func (NullHandler) HandleAuthenticateProvider(string) error { return nil }
func (NullHandler) HandleConfigureProviderAccount(string, ConfigureProviderAccountRequest) (*ConfigureProviderAccountResponse, error) {
	return nil, nil
}
func (NullHandler) HandleReverifyProviderAccount(string, string, ReverifyProviderAccountRequest) (*ReverifyProviderAccountResponse, error) {
	return nil, nil
}
func (NullHandler) HandleReplaceProviderAccountCredential(string, string, ReplaceCredentialRequest) (*ReplaceCredentialResponse, error) {
	return nil, nil
}
func (NullHandler) HandleAccountRemovalPreview(string, string) (*AccountRemovalPreviewDTO, error) {
	return nil, nil
}
func (NullHandler) HandleRemoveProviderAccount(string, string, RemoveProviderAccountRequest) (*RemoveProviderAccountResponse, error) {
	return nil, nil
}
func (NullHandler) HandleProviderSetupFlow(string) (*SetupFlowDTO, error)         { return nil, nil }
func (NullHandler) HandleGetOperation(string) (*OperationDTO, error)              { return nil, nil }
func (NullHandler) HandleGetOperationEvents(string) ([]EventDTO, error)           { return nil, nil }
func (NullHandler) HandleOperationHistory() (*OperationHistoryDTO, error)         { return nil, nil }
func (NullHandler) HandleOperationHistoryLimit(int) (*OperationHistoryDTO, error) { return nil, nil }
func (NullHandler) HandleDiscovery() (*DiscoveryDTO, error)                       { return nil, nil }
func (NullHandler) HandleRefreshDiscovery() (*DiscoveryDTO, error)                { return nil, nil }
func (NullHandler) HandleDiagnostics(string) ([]DiagnosticDTO, error)             { return nil, nil }
func (NullHandler) HandleConnectionLogs(string, int) (*ConnectionLogsDTO, error)  { return nil, nil }
func (NullHandler) HandleTelemetry(string) (*TelemetryDTO, error)                 { return nil, nil }
func (NullHandler) HandleReadiness() (*ReadinessDTO, error)                       { return nil, nil }
func (NullHandler) HandleSetLaunchMode(string) (*LaunchModeDTO, error)            { return nil, nil }
func (NullHandler) HandleSettings() (*SettingsDTO, error)                         { return nil, nil }
func (NullHandler) HandleUpdateSettings(SettingsRequest) (*SettingsDTO, error)    { return nil, nil }
func (NullHandler) HandleSupportExport() (*SupportExportDTO, error)               { return nil, nil }
func (NullHandler) HandleSupervisorStop(context.Context) error                    { return nil }
