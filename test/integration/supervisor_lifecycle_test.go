package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/process"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/provider/mock"
	"github.com/B-A-M-N/portico/internal/store"
	"github.com/B-A-M-N/portico/internal/supervisor"
)

func TestSupervisorIPCConnectionLifecycle(t *testing.T) {
	root := t.TempDir()
	paths := app.Paths{
		SocketPath:   filepath.Join(root, "run", "portico.sock"),
		DatabasePath: filepath.Join(root, "data", "portico.db"),
		ConfigPath:   filepath.Join(root, "config", "config.toml"),
		LogDir:       filepath.Join(root, "state", "logs"),
		ConnectorDir: filepath.Join(root, "state", "connectors"),
	}
	st, err := store.Open(paths.DatabasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	registry := provider.NewRegistry()
	if err := registry.Add(mock.New()); err != nil {
		t.Fatalf("register mock provider: %v", err)
	}
	sup, err := supervisor.New(paths, registry, process.NewManager(), st)
	if err != nil {
		_ = st.Close()
		t.Fatalf("new supervisor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Start(ctx) }()

	client := ipc.NewClient(paths.SocketPath)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := client.Health(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("supervisor did not become healthy")
		}
		time.Sleep(20 * time.Millisecond)
	}

	conn, err := client.CreateConnection(context.Background(), ipc.CreateConnectionRequest{
		Version: 1,
		Name:    "integration-service",
		Source: ipc.SourceDTO{
			Kind:     "existing_service",
			Existing: &ipc.ExistingSourceDTO{Address: "127.0.0.1:8080", Protocol: "http"},
		},
		Exposure:   ipc.ExposureDTO{Mode: "temporary_public"},
		Protection: ipc.ProtectionDTO{Kind: "none"},
		Provider:   ipc.ProviderSelectionDTO{ProviderID: "mock"},
	})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	apply := func(plan *ipc.PlanDTO) string {
		t.Helper()
		op, err := client.ApplyPlan(context.Background(), plan.ID)
		if err != nil {
			t.Fatalf("apply %s plan: %v", plan.Intent, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			current, getErr := client.GetOperation(context.Background(), op.ID)
			if getErr != nil {
				t.Fatalf("get operation: %v", getErr)
			}
			switch current.State {
			case "completed":
				return op.ID
			case "failed":
				t.Fatalf("%s operation failed: %s", plan.Intent, current.Error)
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s operation timed out in state %s", plan.Intent, current.State)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	openPlan, err := client.PlanOpen(context.Background(), conn.ID)
	if err != nil {
		t.Fatalf("plan open: %v", err)
	}
	openOperationID := apply(openPlan)
	stepResults, err := st.GetStepResults(context.Background(), core.OperationID(openOperationID))
	if err != nil {
		t.Fatalf("load durable step results: %v", err)
	}
	if len(stepResults) != len(openPlan.Steps) {
		t.Fatalf("durable step results = %d, want %d", len(stepResults), len(openPlan.Steps))
	}
	for _, result := range stepResults {
		if result.Status != string(store.StepSucceeded) {
			t.Fatalf("step %s status = %s, want succeeded", result.StepID, result.Status)
		}
	}

	closePlan, err := client.PlanClose(context.Background(), conn.ID)
	if err != nil {
		t.Fatalf("plan close: %v", err)
	}
	apply(closePlan)

	deletePlan, err := client.PlanDelete(context.Background(), conn.ID)
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	apply(deletePlan)

	if _, err := client.GetConnection(context.Background(), conn.ID); err == nil {
		t.Fatal("deleted connection remains visible")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervisor shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop")
	}

	if err := st.Health(context.Background()); err == nil {
		t.Fatal("store remained usable after supervisor shutdown")
	}
}
