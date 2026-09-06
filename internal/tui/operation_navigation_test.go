package tui

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

func TestBackFromCompletedOperationSkipsClearedPlanPreview(t *testing.T) {
	m := newModel(nil, nil)
	m.screen = ScreenOperationProgress
	m.navStack = []ScreenID{ScreenHome, ScreenInspect, ScreenPlanPreview}
	m.operation = &ipc.OperationDTO{ID: "op-1", State: ipc.OperationCompleted}
	// planAppliedMsg clears the approved plan before entering progress.
	m.plan = nil

	next, _, handled := m.goBack()
	if !handled {
		t.Fatal("operation back was not handled")
	}
	if next.screen != ScreenInspect {
		t.Fatalf("screen after completed operation back = %q, want %q", next.screen, ScreenInspect)
	}
	if len(next.navStack) != 1 || next.navStack[0] != ScreenHome {
		t.Fatalf("navigation stack after completed operation back = %#v, want [home]", next.navStack)
	}
}
