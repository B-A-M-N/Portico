//go:build linux

package origin

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// TestParseProcStat_ProcessGroup returns the process group, not session ID.
func TestParseProcStat_ProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "600")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	}()

	pid := cmd.Process.Pid
	pgrp, err := ProcessGroupOf(pid)
	if err != nil {
		t.Fatalf("ProcessGroupOf: %v", err)
	}
	expectedPgrp, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if pgrp != expectedPgrp {
		t.Fatalf("ProcessGroupOf = %d, want %d", pgrp, expectedPgrp)
	}

	startTime, err := StartTimeOf(pid)
	if err != nil {
		t.Fatalf("StartTimeOf: %v", err)
	}
	if startTime == 0 {
		t.Fatal("StartTimeOf returned zero")
	}
}

// TestParseProcStat_FieldIndexes proves that parsed pgrp and startTime
// match syscall.Getpgid and are both nonzero.
func TestParseProcStat_FieldIndexes(t *testing.T) {
	cmd := exec.Command("sleep", "600")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	}()

	ps, err := parseProcStat(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if ps.pgrp == 0 {
		t.Fatal("parseProcStat returned zero pgrp")
	}
	if ps.startTime == 0 {
		t.Fatal("parseProcStat returned zero startTime")
	}

	// Cross-check with syscall.
	expectedPgrp, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if ps.pgrp != expectedPgrp {
		t.Fatalf("pgrp mismatch: parsed %d, Getpgid %d", ps.pgrp, expectedPgrp)
	}
}

// TestStopProcessGroup_DoesNotSignalUnrelatedGroup starts a child in a new
// process group, then verifies that stopping an unrelated LocalCommand does
// not send signals to the child's group.
func TestStopProcessGroup_DoesNotSignalUnrelatedGroup(t *testing.T) {
	// Create a separate process group with a sleep child.
	cmd := exec.Command("sleep", "600")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	childPgrp := cmd.Process.Pid

	manager := NewManager()
	source := core.SourceSpec{
		Kind:      core.SourceDirectory,
		Directory: &core.DirectorySpec{Path: t.TempDir()},
	}
	resolved, err := manager.Plan("stop-test", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), "stop-test", source, resolved.URL); err != nil {
		t.Fatal(err)
	}

	// Stopping the LocalCommand should not kill the sleep process.
	if err := manager.Stop(context.Background(), "stop-test"); err != nil {
		t.Fatalf("manager.Stop: %v", err)
	}

	// Verify the sleep child is still alive.
	if err := syscall.Kill(-childPgrp, 0); err != nil {
		t.Fatalf("unrelated process group was signaled: %v", err)
	}
	syscall.Kill(-childPgrp, syscall.SIGKILL)
}

var devNull = openDevNull()

func openDevNull() *os.File {
	f, err := os.Open(os.DevNull)
	if err != nil {
		panic(err)
	}
	return f
}
