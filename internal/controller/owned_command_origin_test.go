package controller

import (
	"context"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/origin"
	"github.com/B-A-M-N/portico/internal/provider/mock"
)

// TestController_OwnedCommandOriginLifecycle validates that a command-backed
// origin survives the terminal completion of the open operation. This is the
// regression for audit finding #1 ("Command-owned origins are killed when
// the open operation completes"): before the fix the operation context was
// wired through exec.CommandContext, so the deferred cancel on operation
// exit killed the launched command after Portico reported the connection
// open.
func TestController_OwnedCommandOriginLifecycle(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for command-origin lifecycle test")
	}
	port := freeLoopbackPort(t)

	controller := New(newTestRegistry(mock.New()), newTestJournal())
	controller.SetOriginManager(origin.NewManager())
	profile := &core.ConnectionProfile{
		Name: "owned-command",
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{Kind: core.SourceCommand, Command: &core.CommandSpec{
					Executable: "python3",
					Args:       []string{"-m", "http.server", strconv.Itoa(port)},
					Port:       port,
				}},
				Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
				Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
			},
		},
		Driver:  core.DriverSelection{ProviderID: "mock"},
		Desired: core.DesiredClosed,
	}
	if _, _, err := controller.CreateProfile(context.Background(), profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	openPlan, err := controller.PlanOpen(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	if err := controller.SavePlan(openPlan); err != nil {
		t.Fatalf("SavePlan(open): %v", err)
	}
	operation, err := controller.ApplyPlan(context.Background(), openPlan.ID)
	if err != nil {
		t.Fatalf("ApplyPlan(open): %v", err)
	}
	result := awaitOperationTerminal(t, controller, operation.ID)
	if result.State != OperationStateCompleted {
		t.Fatalf("open state = %s: %v", result.State, result.Error)
	}

	originURL := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: 3 * time.Second}
	if response, err := client.Get(originURL); err != nil {
		t.Fatalf("origin unavailable after open terminal: %v", err)
	} else {
		response.Body.Close()
	}
	// A second probe confirms the command is still alive after the
	// operation goroutine has exited and its context canceled.
	if response, err := client.Get(originURL); err != nil {
		t.Fatalf("origin unavailable on second probe after open terminal: %v", err)
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
	if response, err := client.Get(originURL); err == nil {
		response.Body.Close()
		t.Fatal("origin still accepts requests after close")
	}
}

// TestOriginManager_StopAllTerminatesProcessGroup is a regression for audit
// finding #16 ("Command-origin stop does not terminate the process group").
// It launches a shell that spawns a python3 listener as a descendant and
// asserts that the entire group is reaped by StopAll.
func TestOriginManager_StopAllTerminatesProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for process-group termination test")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is required for process-group termination test")
	}
	var port int
	var manager *origin.Manager
	var connID core.ConnectionID
	var cfg core.SourceSpec
	var resolved *core.ResolvedOrigin
	var startErr error
	for attempt := 0; attempt < 10; attempt++ {
		port = freeLoopbackPort(t)
		manager = origin.NewManager()
		connID = core.ConnectionID("pgroup-" + strconv.Itoa(port))
		// Run the shell explicitly: "sh -c 'python3 -m http.server PORT & wait'"
		// so the listener becomes a descendant of sh in the same process group.
		cfg = core.SourceSpec{Kind: core.SourceCommand, Command: &core.CommandSpec{
			Executable: "sh",
			Args: []string{
				"-c", "python3 -m http.server " + strconv.Itoa(port) + " & wait",
			},
			Port: port,
		}}
		resolved, startErr = manager.Plan(connID, cfg)
		if startErr != nil {
			t.Fatalf("Plan: %v", startErr)
		}
		startErr = manager.Start(context.Background(), connID, cfg, resolved.URL)
		if startErr == nil {
			break
		}
	}
	if startErr != nil {
		t.Fatalf("Start after retries: %v", startErr)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	if response, err := client.Get(resolved.URL); err != nil {
		t.Fatalf("descendant listener not reachable: %v", err)
	} else {
		response.Body.Close()
	}

	// StopAll must terminate the descendant too.
	if err := manager.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}

	// Wait briefly for the descendant to be reaped and the port to be
	// released; verify by trying to bind the same port.
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			l.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant still owns port %d after StopAll", port)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// freeLoopbackPort reserves a loopback port for a child process to bind.
//
// Asking the kernel for :0 and closing the listener leaves a window in which
// another test — including one in a package running concurrently — can take the
// port before the child binds it, which surfaced as an intermittent
// "address already in use" failure under go test -race ./...
//
// The port is re-checked immediately before being returned, and a taken port is
// discarded rather than handed out, which closes all but a vanishingly small
// window.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()

		// Confirm the port is still claimable before handing it out.
		probe, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		probe.Close()
		return port
	}
	t.Fatal("could not reserve a free loopback port")
	return 0
}
