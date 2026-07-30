package controller

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/origin"
	"github.com/B-A-M-N/portico/internal/provider/mock"
)

func TestController_OwnedDirectoryOriginLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(root+"/index.html", []byte("managed"), 0600); err != nil {
		t.Fatal(err)
	}
	controller := New(newTestRegistry(mock.New()), newTestJournal())
	controller.SetOriginManager(origin.NewManager())
	profile := &core.ConnectionProfile{
		Name: "owned-directory",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{Kind: core.SourceDirectory, Directory: &core.DirectorySpec{
					Path: root, Mode: core.DirectoryModeRead,
				}},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver: core.DriverSelection{ProviderID: "mock"},
		Desired:    core.DesiredClosed,
	}
	if _, _, err := controller.CreateProfile(context.Background(), profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	openPlan, err := controller.PlanOpen(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	startIndex := -1
	for i, step := range openPlan.Steps {
		if step.Kind == core.StepStartOrigin {
			startIndex = i
			break
		}
	}
	if startIndex < 0 {
		t.Fatal("open plan has no start_origin step")
	}
	originURL := openPlan.Steps[startIndex].Technical.Parameters["origin_url"]
	if err := controller.SavePlan(openPlan); err != nil {
		t.Fatalf("SavePlan(open): %v", err)
	}
	operation, err := controller.ApplyPlan(context.Background(), openPlan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan(open): %v", err)
	}
	if result := awaitOperationTerminal(t, controller, operation.ID); result.State != OperationStateCompleted {
		t.Fatalf("open state = %s: %v", result.State, result.Error)
	}
	if response, err := http.Get(originURL); err != nil {
		t.Fatalf("origin unavailable after open: %v", err)
	} else {
		response.Body.Close()
	}

	closePlan, err := controller.PlanClose(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("PlanClose: %v", err)
	}
	if closePlan.Steps[len(closePlan.Steps)-1].Kind != core.StepStopOrigin {
		t.Fatalf("last close step = %s, want stop_origin", closePlan.Steps[len(closePlan.Steps)-1].Kind)
	}
	if err := controller.SavePlan(closePlan); err != nil {
		t.Fatalf("SavePlan(close): %v", err)
	}
	operation, err = controller.ApplyPlan(context.Background(), closePlan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan(close): %v", err)
	}
	if result := awaitOperationTerminal(t, controller, operation.ID); result.State != OperationStateCompleted {
		t.Fatalf("close state = %s: %v", result.State, result.Error)
	}
	if _, err := http.Get(originURL); err == nil {
		t.Fatal("origin still accepted requests after close")
	}
}
