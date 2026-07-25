package tui

import (
	"context"

	"github.com/paoloanzn/portico/internal/ipc"
)

// SupervisorClient is the subset of the IPC client the TUI depends on.
// The root model is built against this interface so tests can substitute
// a fake without a real socket; production passes *ipc.Client.
type SupervisorClient interface {
	GetSnapshot(ctx context.Context) (*ipc.SnapshotDTO, error)
	PlanOpen(ctx context.Context, connID string) (*ipc.PlanDTO, error)
	PlanClose(ctx context.Context, connID string) (*ipc.PlanDTO, error)
	ApplyPlan(ctx context.Context, planID string) (*ipc.OperationDTO, error)
	CreateConnection(ctx context.Context, req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error)
	ConnectEventStream(ctx context.Context, lastSeq int64) (*ipc.EventStream, error)
}

// Ensure the real client satisfies the interface.
var _ SupervisorClient = (*ipc.Client)(nil)
