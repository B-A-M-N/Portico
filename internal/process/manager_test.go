package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/paoloanzn/portico/internal/core"
)

func requireBinaries(t *testing.T, bins ...string) {
	t.Helper()
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("required binary %q not found: %v", b, err)
		}
	}
}

// pollUntil polls fn until it returns true or the deadline expires.
func pollUntil(t *testing.T, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fn()
}

// startExternal launches an external process (not via the manager) and
// returns its captured identity. The process is reaped in the background
// and killed at test cleanup.
func startExternal(t *testing.T, name string, args ...string) (*exec.Cmd, core.ProcessIdentity) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start external process: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	})
	ident, err := CaptureProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("capture identity: %v", err)
	}
	return cmd, ident
}

func TestStartCapturesLogsThroughPipes(t *testing.T) {
	requireBinaries(t, "sh")
	dir := t.TempDir()
	stdout := filepath.Join(dir, "out.log")
	stderr := filepath.Join(dir, "err.log")

	// Pre-existing content must survive (append mode, no truncation).
	if err := os.WriteFile(stdout, []byte("previous\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := NewManager()
	defer pm.Cleanup()

	connID := core.ConnectionID("logs")
	_, err := pm.Start(context.Background(), ProcessConfig{
		ConnectionID: connID,
		Spec: core.ProcessSpec{
			Executable: "sh",
			Args:       []string{"-c", "echo hello; echo oops >&2"},
			StdoutPath: stdout,
			StderrPath: stderr,
			Restart:    core.RestartNever,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ok := pollUntil(t, 5*time.Second, func() bool {
		mp, found := pm.GetProcess(connID)
		return found && mp.Status == ProcessStatusStopped
	})
	if !ok {
		t.Fatal("process did not reach stopped status")
	}

	outData, err := os.ReadFile(stdout)
	if err != nil {
		t.Fatalf("read stdout log: %v", err)
	}
	if !strings.Contains(string(outData), "previous") {
		t.Errorf("stdout log was truncated: %q", outData)
	}
	if !strings.Contains(string(outData), "hello") {
		t.Errorf("stdout log missing output: %q", outData)
	}
	errData, err := os.ReadFile(stderr)
	if err != nil {
		t.Fatalf("read stderr log: %v", err)
	}
	if !strings.Contains(string(errData), "oops") {
		t.Errorf("stderr log missing output: %q", errData)
	}
}

func TestAdoptVerifiesIdentityAndStops(t *testing.T) {
	requireBinaries(t, "sleep")
	_, ident := startExternal(t, "sleep", "60")

	pm := NewManager()
	pm.pollInterval = 20 * time.Millisecond
	defer pm.Cleanup()

	connID := core.ConnectionID("adopted")
	spec := core.ProcessSpec{Executable: "sleep", Args: []string{"60"}, Restart: core.RestartNever}
	if err := pm.Adopt(connID, ident, spec); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	mp, ok := pm.GetProcess(connID)
	if !ok {
		t.Fatal("adopted process not tracked")
	}
	if !mp.Adopted {
		t.Error("expected Adopted=true")
	}
	if mp.Status != ProcessStatusRunning {
		t.Errorf("expected running status, got %s", mp.Status)
	}
	if mp.Identity != ident {
		t.Errorf("identity mismatch: %+v != %+v", mp.Identity, ident)
	}

	if _, ok := pm.Observe(connID); !ok {
		t.Error("Observe did not find adopted process")
	}

	if err := pm.Stop(connID, 2*time.Second); err != nil {
		t.Fatalf("Stop adopted: %v", err)
	}
	ok = pollUntil(t, 3*time.Second, func() bool {
		return !IsProcessRunning(ident.PID) || verifyIdentityFields(ident) != nil
	})
	if !ok {
		t.Error("adopted process still running after Stop")
	}
	if _, found := pm.GetProcess(connID); found {
		t.Error("adopted process still tracked after Stop")
	}
}

func TestAdoptRejectsIdentityMismatch(t *testing.T) {
	requireBinaries(t, "sleep")
	cmd, ident := startExternal(t, "sleep", "60")

	pm := NewManager()
	defer pm.Cleanup()

	spec := core.ProcessSpec{Executable: "sleep", Args: []string{"60"}}

	cases := map[string]core.ProcessIdentity{
		"start time (PID reuse)": {
			PID:            ident.PID,
			StartTime:      ident.StartTime + 1,
			ExecutablePath: ident.ExecutablePath,
			CommandHash:    ident.CommandHash,
		},
		"executable path": {
			PID:            ident.PID,
			StartTime:      ident.StartTime,
			ExecutablePath: "/usr/bin/definitely-not-sleep",
			CommandHash:    ident.CommandHash,
		},
		"command hash": {
			PID:            ident.PID,
			StartTime:      ident.StartTime,
			ExecutablePath: ident.ExecutablePath,
			CommandHash:    "deadbeef",
		},
		"invalid pid": {
			PID: -1,
		},
	}

	for name, fake := range cases {
		connID := core.ConnectionID("mismatch-" + name)
		err := pm.Adopt(connID, fake, spec)
		if err == nil {
			t.Fatalf("%s: Adopt succeeded with mismatched identity", name)
		}
		if !errors.Is(err, ErrIdentityMismatch) {
			t.Errorf("%s: error is not ErrIdentityMismatch: %v", name, err)
		}
		if _, found := pm.GetProcess(connID); found {
			t.Errorf("%s: mismatched process was added", name)
		}
	}

	// The live process must never have been signaled.
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Errorf("external process was signaled/killed during failed adoption: %v", err)
	}
}

func TestAdoptRejectsDeadPID(t *testing.T) {
	requireBinaries(t, "sleep")
	cmd, ident := startExternal(t, "sleep", "60")
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
	pollUntil(t, 2*time.Second, func() bool {
		return verifyIdentityFields(ident) != nil
	})

	pm := NewManager()
	defer pm.Cleanup()
	err := pm.Adopt(core.ConnectionID("dead"), ident, core.ProcessSpec{Executable: "sleep"})
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("expected ErrIdentityMismatch for dead PID, got %v", err)
	}
}

func TestRestartBackoffSchedule(t *testing.T) {
	var rb RestartBackoff
	now := time.Unix(1000, 0)

	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
	}
	for i, w := range want {
		d, ok := rb.NextDelay(now)
		if !ok {
			t.Fatalf("attempt %d: unexpected exhaustion", i+1)
		}
		if d != w {
			t.Errorf("attempt %d: delay = %v, want %v", i+1, d, w)
		}
	}

	// Sixth attempt inside the window: unstable.
	if _, ok := rb.NextDelay(now.Add(time.Minute)); ok {
		t.Error("expected exhaustion after 5 attempts within window")
	}

	// After the 10-minute window passes, the schedule resets.
	d, ok := rb.NextDelay(now.Add(11 * time.Minute))
	if !ok || d != 1*time.Second {
		t.Errorf("window reset: got (%v, %v), want (1s, true)", d, ok)
	}
}

func TestRestartBackoffInjectableSchedule(t *testing.T) {
	rb := RestartBackoff{
		Schedule:    []time.Duration{time.Millisecond, 2 * time.Millisecond},
		MaxAttempts: 3,
	}
	now := time.Unix(1000, 0)
	got := []time.Duration{}
	for {
		d, ok := rb.NextDelay(now)
		if !ok {
			break
		}
		got = append(got, d)
	}
	// Last schedule entry is reused for the third attempt.
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 2 * time.Millisecond}
	if len(got) != len(want) {
		t.Fatalf("attempts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("attempt %d: %v, want %v", i+1, got[i], want[i])
		}
	}
}

func TestRestartAlwaysMarksUnstableAfterMaxAttempts(t *testing.T) {
	requireBinaries(t, "sh")
	pm := NewManager()
	pm.restartSchedule = []time.Duration{time.Millisecond}
	defer pm.Cleanup()

	connID := core.ConnectionID("crashy")
	_, err := pm.Start(context.Background(), ProcessConfig{
		ConnectionID: connID,
		Spec: core.ProcessSpec{
			Executable: "sh",
			Args:       []string{"-c", "exit 1"},
			Restart:    core.RestartAlways,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ok := pollUntil(t, 10*time.Second, func() bool {
		mp, found := pm.GetProcess(connID)
		return found && mp.Status == ProcessStatusUnstable
	})
	if !ok {
		mp, _ := pm.GetProcess(connID)
		t.Fatalf("process never marked unstable; last: %+v", mp)
	}

	mp, _ := pm.GetProcess(connID)
	if !mp.IsUnstable() {
		t.Error("IsUnstable() = false")
	}
	if mp.Restarts != maxRestartAttempts {
		t.Errorf("Restarts = %d, want %d", mp.Restarts, maxRestartAttempts)
	}
	if mp.LastExitError == "" {
		t.Error("LastExitError not recorded")
	}
	if mp.LastExitAt.IsZero() {
		t.Error("LastExitAt not recorded")
	}
}

func TestRestartAlwaysRecovers(t *testing.T) {
	requireBinaries(t, "sh", "sleep")
	pm := NewManager()
	pm.restartSchedule = []time.Duration{time.Millisecond}
	defer pm.Cleanup()

	dir := t.TempDir()
	marker := filepath.Join(dir, "ran-once")
	// First run exits immediately; restarted runs sleep.
	script := fmt.Sprintf("if [ -e %s ]; then sleep 60; else touch %s; exit 1; fi", marker, marker)

	connID := core.ConnectionID("recovers")
	_, err := pm.Start(context.Background(), ProcessConfig{
		ConnectionID: connID,
		Spec: core.ProcessSpec{
			Executable: "sh",
			Args:       []string{"-c", script},
			Restart:    core.RestartAlways,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ok := pollUntil(t, 5*time.Second, func() bool {
		mp, found := pm.GetProcess(connID)
		return found && mp.Status == ProcessStatusRunning && mp.Restarts >= 1
	})
	if !ok {
		mp, _ := pm.GetProcess(connID)
		t.Fatalf("process did not restart into running state; last: %+v", mp)
	}

	mp, _ := pm.GetProcess(connID)
	if mp.Restarts != 1 {
		t.Errorf("Restarts = %d, want 1", mp.Restarts)
	}
	if err := pm.Stop(connID, 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStopDuringBackoffCancelsRestart(t *testing.T) {
	requireBinaries(t, "sh")
	pm := NewManager()
	pm.restartSchedule = []time.Duration{time.Minute} // long delay: stop must interrupt it
	defer pm.Cleanup()

	connID := core.ConnectionID("backoff-stop")
	_, err := pm.Start(context.Background(), ProcessConfig{
		ConnectionID: connID,
		Spec: core.ProcessSpec{
			Executable: "sh",
			Args:       []string{"-c", "exit 1"},
			Restart:    core.RestartAlways,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the crash to be observed.
	pollUntil(t, 5*time.Second, func() bool {
		mp, found := pm.GetProcess(connID)
		return found && mp.Status == ProcessStatusCrashed
	})

	doneCh := make(chan error, 1)
	go func() { doneCh <- pm.Stop(connID, 2*time.Second) }()
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked during restart backoff (mutex/timer held?)")
	}
	if _, found := pm.GetProcess(connID); found {
		t.Error("process still tracked after Stop")
	}
}

func TestProcessGroupStop(t *testing.T) {
	requireBinaries(t, "sh", "sleep")
	pm := NewManager()
	defer pm.Cleanup()

	connID := core.ConnectionID("pgroup")
	mp, err := pm.Start(context.Background(), ProcessConfig{
		ConnectionID: connID,
		Spec: core.ProcessSpec{
			Executable: "sh",
			Args:       []string{"-c", "sleep 60 & sleep 60 & wait"},
			Restart:    core.RestartNever,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := mp.Identity.PID

	// Give the shell a moment to spawn its children, then confirm the
	// process group is alive.
	ok := pollUntil(t, 2*time.Second, func() bool {
		return syscall.Kill(-pgid, 0) == nil
	})
	if !ok {
		t.Fatal("process group not alive before stop")
	}

	if err := pm.Stop(connID, 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The whole group — including the background sleeps — must be gone.
	ok = pollUntil(t, 5*time.Second, func() bool {
		err := syscall.Kill(-pgid, 0)
		return errors.Is(err, syscall.ESRCH)
	})
	if !ok {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		t.Fatal("process group members survived Stop")
	}
}

func TestStopUnknownConnection(t *testing.T) {
	pm := NewManager()
	if err := pm.Stop(core.ConnectionID("nope"), time.Second); err == nil {
		t.Error("expected error for unknown connection")
	}
}

func TestStartExistingRunningReturnsSameProcess(t *testing.T) {
	requireBinaries(t, "sleep")
	pm := NewManager()
	defer pm.Cleanup()

	connID := core.ConnectionID("dup")
	cfg := ProcessConfig{
		ConnectionID: connID,
		Spec:         core.ProcessSpec{Executable: "sleep", Args: []string{"60"}, Restart: core.RestartNever},
	}
	mp1, err := pm.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	mp2, err := pm.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	if mp1.Identity.PID != mp2.Identity.PID {
		t.Errorf("second Start launched a new process: %d != %d", mp1.Identity.PID, mp2.Identity.PID)
	}
	if err := pm.Stop(connID, 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartFailureReturnsErrorAndUntracked(t *testing.T) {
	pm := NewManager()
	defer pm.Cleanup()

	connID := core.ConnectionID("bad-exe")
	_, err := pm.Start(context.Background(), ProcessConfig{
		ConnectionID: connID,
		Spec:         core.ProcessSpec{Executable: "/nonexistent/portico-test-binary"},
	})
	if err == nil {
		t.Fatal("expected start error")
	}
	if _, found := pm.GetProcess(connID); found {
		t.Error("failed process still tracked")
	}
}

func TestCleanupNoGoroutineLeaks(t *testing.T) {
	requireBinaries(t, "sleep")
	baseline := runtime.NumGoroutine()

	pm := NewManager()
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		connID := core.ConnectionID(fmt.Sprintf("leak-%d", i))
		_, err := pm.Start(context.Background(), ProcessConfig{
			ConnectionID: connID,
			Spec: core.ProcessSpec{
				Executable: "sleep",
				Args:       []string{"60"},
				StdoutPath: filepath.Join(dir, fmt.Sprintf("out-%d.log", i)),
				StderrPath: filepath.Join(dir, fmt.Sprintf("err-%d.log", i)),
				Restart:    core.RestartAlways,
			},
		})
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
	}

	pm.Cleanup()

	if got := len(pm.ListProcesses()); got != 0 {
		t.Errorf("ListProcesses after Cleanup = %d, want 0", got)
	}

	ok := pollUntil(t, 5*time.Second, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+2
	})
	if !ok {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Errorf("goroutine leak after Cleanup: baseline=%d now=%d\n%s",
			baseline, runtime.NumGoroutine(), buf[:n])
	}
}
