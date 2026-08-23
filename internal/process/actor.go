package process

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// stopReq asks an actor to terminate its process and exit.
type stopReq struct {
	timeout time.Duration
	reply   chan error
}

// actorIO holds the per-launch pipe/writer resources for log capture.
type actorIO struct {
	readers []*os.File
	closers []io.Closer
	wg      sync.WaitGroup
}

// close tears down log plumbing: it closes the pipe read ends (unblocking
// any copier still reading), waits for the copiers to finish, then closes
// the rotating writers. All descriptors are released explicitly.
func (aio *actorIO) close() {
	if aio == nil {
		return
	}
	for _, r := range aio.readers {
		_ = r.Close()
	}
	aio.wg.Wait()
	for _, closer := range aio.closers {
		_ = closer.Close()
	}
}

// actor is the per-connection process owner. A single goroutine (run) owns
// the exec.Cmd, log pipes, restart timer, and exit status. External callers
// interact only through channels (stopCh) and the published snapshot (rec).
type actor struct {
	connID  core.ConnectionID
	spec    core.ProcessSpec
	adopted bool // started life as an externally started (adopted) process

	// Policy configuration, copied from the Manager (injectable for tests).
	schedule     []time.Duration
	window       time.Duration
	maxAttempts  int
	pollInterval time.Duration
	logMaxSize   int64
	logMaxFiles  int
	emit         func(ProcessEvent)

	stopCh   chan stopReq
	started  chan struct{} // closed once the first launch attempt finished
	done     chan struct{} // closed when the actor goroutine exits
	startErr error         // valid after started is closed

	// Run-loop-owned state. Only the owner goroutine touches these after
	// the initial launch (which happens-before the run goroutine starts).
	cmd    *exec.Cmd
	waitCh chan error
	aio    *actorIO

	// Published snapshot, guarded by mu.
	mu  sync.Mutex
	rec ManagedProcess
}

// snapshot returns a point-in-time copy of the process record.
func (a *actor) snapshot() *ManagedProcess {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := a.rec
	return &cp
}

func (a *actor) identity() core.ProcessIdentity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rec.Identity
}

func (a *actor) setStatus(s ProcessStatus) {
	a.mu.Lock()
	a.rec.Status = s
	a.mu.Unlock()
}

func (a *actor) publishEvent(eventType ProcessEventType, err error) {
	if a.emit == nil {
		return
	}
	snapshot := a.snapshot()
	event := ProcessEvent{
		ConnectionID: a.connID,
		Type:         eventType,
		Identity:     snapshot.Identity,
		Status:       snapshot.Status,
		Restarts:     snapshot.Restarts,
		Timestamp:    time.Now().UTC(),
	}
	if err != nil {
		event.Error = err.Error()
	} else {
		event.Error = snapshot.LastExitError
	}
	a.emit(event)
}

// recordExit publishes the outcome of an unexpected process exit.
func (a *actor) recordExit(err error) {
	a.mu.Lock()
	a.rec.LastExitAt = time.Now().UTC()
	if err != nil {
		a.rec.LastExitError = err.Error()
		a.rec.Status = ProcessStatusCrashed
	} else {
		a.rec.LastExitError = ""
		a.rec.Status = ProcessStatusStopped
	}
	ch := a.rec.ExitCh
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- err:
		default:
		}
	}
	a.publishEvent(ProcessEventExited, err)
}

// launch starts the process with log pipes attached to rotating writers.
// Child stdout/stderr are never attached directly to files: output flows
// through pipes into RotatingWriter (rename-based rotation, size threshold),
// so logs append across restarts and rotate instead of truncating.
func (a *actor) launch() error {
	spec := a.spec

	var writers []*RotatingWriter
	closeWriters := func() {
		for _, w := range writers {
			_ = w.Close()
		}
	}

	var stdoutW, stderrW io.Writer
	var redactorClosers []io.Closer
	redactions := processLogRedactions(spec)
	if spec.StdoutPath != "" {
		w, err := NewRotatingWriter(spec.StdoutPath, a.logMaxSize, a.logMaxFiles)
		if err != nil {
			return fmt.Errorf("process stdout: %w", err)
		}
		stdoutW = NewRedactingWriter(w, redactions)
		writers = append(writers, w)
		redactorClosers = append(redactorClosers, stdoutW.(io.Closer))
	}
	if spec.StderrPath != "" {
		if spec.StderrPath == spec.StdoutPath && stdoutW != nil {
			// Same path: share one writer so rotation is coherent.
			stderrW = NewRedactingWriter(writers[0], redactions)
			redactorClosers = append(redactorClosers, stderrW.(io.Closer))
		} else {
			w, err := NewRotatingWriter(spec.StderrPath, a.logMaxSize, a.logMaxFiles)
			if err != nil {
				// Do not leak the stdout writer on stderr-open failure.
				closeWriters()
				return fmt.Errorf("process stderr: %w", err)
			}
			stderrW = NewRedactingWriter(w, redactions)
			writers = append(writers, w)
			redactorClosers = append(redactorClosers, stderrW.(io.Closer))
		}
	}

	cmd := exec.Command(spec.Executable, spec.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = spec.Dir
	if len(spec.Env) > 0 {
		cmd.Env = minimalEnvWithOverride(spec.Env...)
	} else {
		cmd.Env = minimalEnv()
	}

	type pipePair struct {
		r, w *os.File
		dst  io.Writer
	}
	var pipes []pipePair
	closePipes := func() {
		for _, p := range pipes {
			_ = p.r.Close()
			_ = p.w.Close()
		}
	}
	if stdoutW != nil {
		r, w, err := os.Pipe()
		if err != nil {
			closeWriters()
			return fmt.Errorf("process stdout pipe: %w", err)
		}
		cmd.Stdout = w
		pipes = append(pipes, pipePair{r: r, w: w, dst: stdoutW})
	}
	if stderrW != nil {
		r, w, err := os.Pipe()
		if err != nil {
			closePipes()
			closeWriters()
			return fmt.Errorf("process stderr pipe: %w", err)
		}
		cmd.Stderr = w
		pipes = append(pipes, pipePair{r: r, w: w, dst: stderrW})
	}

	if err := cmd.Start(); err != nil {
		closePipes()
		closeWriters()
		return fmt.Errorf("process start: %w", err)
	}

	closers := append(redactorClosers, make([]io.Closer, 0, len(writers))...)
	for _, writer := range writers {
		closers = append(closers, writer)
	}
	aio := &actorIO{closers: closers}
	// The parent must close the child's write ends so the copiers see EOF
	// when the process group exits.
	for _, p := range pipes {
		_ = p.w.Close()
		aio.readers = append(aio.readers, p.r)
		aio.wg.Add(1)
		go func(dst io.Writer, src *os.File) {
			defer aio.wg.Done()
			_, _ = io.Copy(dst, src)
		}(p.dst, p.r)
	}

	// Capture actual process identity using /proc, waiting briefly for the
	// post-exec cmdline to be visible so the stored hash is trustworthy.
	// P0 #11: Complete four-field identity must be part of successful launch.
	// If capture fails, we clean up the just-created process and return a
	// startup failure rather than publishing an incomplete identity.
	ident, err := captureIdentityStable(cmd.Process.Pid, 500*time.Millisecond)
	// captureIdentityStable may return a nil error with an incomplete identity
	// (e.g. for short-lived processes that already exited before we could read
	// /proc). Treat both errors and incomplete identity the same way.
	if err != nil || !ident.Complete() {
		// For short-lived processes that already exited, /proc/<pid>/cmdline
		// is empty. Fall back to spec-based identity so short-lived commands
		// can still succeed.
		if spec.Restart == core.RestartNever {
			// Process won't be restarted — spec-based identity is acceptable
			// since we won't need to signal a zombie.
			ident = core.ProcessIdentity{
				PID:            cmd.Process.Pid,
				ExecutablePath: spec.Executable,
				CommandHash:    computeSpecHash(spec),
				StartTime:      uint64(time.Now().Unix()),
			}
		} else if err != nil {
			// Clean up the directly-owned process before returning failure.
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			aio.close()
			return fmt.Errorf("capturing process identity: %w", err)
		}
	}
	// Verify the identity is complete (all four fields).
	if !ident.Complete() {
		// For processes that will be restarted, an incomplete identity on
		// the first attempt is acceptable — the restart loop creates a new
		// process and captures its identity on the next attempt.
		if spec.Restart == core.RestartNever {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			aio.close()
			return fmt.Errorf("incomplete process identity captured: PID=%d exe=%s", ident.PID, ident.ExecutablePath)
		}
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	a.cmd = cmd
	a.waitCh = waitCh
	a.aio = aio

	a.mu.Lock()
	a.rec.Cmd = cmd
	a.rec.Identity = ident
	a.rec.StartedAt = time.Now().UTC()
	a.rec.Status = ProcessStatusRunning
	a.mu.Unlock()

	slog.Info("connector process started",
		"connection", a.connID,
		"pid", ident.PID,
		"executable", spec.Executable,
	)
	a.publishEvent(ProcessEventStarted, nil)
	return nil
}

// processLogRedactions derives the set of secret values that must be redacted
// from a connector's logs. It starts from explicit spec.Redactions, then
// recognizes:
//   - Flag-value pairs whose flag name suggests a secret (--token-file, --api-key, etc.)
//   - Environment variable values whose name suggests a secret (NGROK_AUTHTOKEN, etc.)
//
// The value (not the flag/name) is added to the redaction set. This handles the
// common pattern where a secret lives in the argument following a flag, or in an
// environment variable value.
func processLogRedactions(spec core.ProcessSpec) []string {
	seen := make(map[string]bool)
	redactions := make([]string, 0, len(spec.Redactions)+4)
	for _, r := range spec.Redactions {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		redactions = append(redactions, r)
	}

	// Flag names whose argument is likely a secret.
	secretFlags := map[string]bool{
		"--token-file": true, "--token": true,
		"--credential": true, "--credential-file": true,
		"--api-key": true, "--api-token": true,
		"--secret": true, "--secret-key": true,
		"--client-secret": true, "--client-id": true,
		"--access-token": true, "--refresh-token": true,
	}

	for i, arg := range spec.Args {
		// Handle --flag=value form.
		if name, value, found := strings.Cut(arg, "="); found {
			if secretFlags[name] && value != "" && !seen[value] {
				seen[value] = true
				redactions = append(redactions, value)
			}
			continue
		}
		// Handle --flag value form.
		if secretFlags[arg] && i+1 < len(spec.Args) {
			value := spec.Args[i+1]
			if value != "" && !seen[value] {
				seen[value] = true
				redactions = append(redactions, value)
			}
		}
	}

	// Environment variable names whose values are secrets.
	secretEnvNames := map[string]bool{
		"NGROK_AUTHTOKEN": true, "NGROK_API_KEY": true,
		"CF_API_TOKEN": true, "CF_API_KEY": true,
		"TUNNEL_TOKEN": true, "TUNNEL_TOKEN_FILE": true,
		"CONTROL_PLANE_API_KEY": true,
	}
	for _, env := range spec.Env {
		name, value, found := strings.Cut(env, "=")
		if !found {
			continue
		}
		upper := strings.ToUpper(name)
		if secretEnvNames[upper] && value != "" && !seen[value] {
			seen[value] = true
			redactions = append(redactions, value)
		}
	}

	return redactions
}

// run is the actor main loop. It owns the process lifecycle: waiting for
// exit, servicing stop requests, and scheduling restarts with backoff
// (1s, 2s, 5s, 10s, 30s; max 5 attempts per 10-minute window, then the
// process is marked unstable and automatic restarts cease).
func (a *actor) run() {
	defer close(a.done)

	backoff := RestartBackoff{
		Schedule:    a.schedule,
		Window:      a.window,
		MaxAttempts: a.maxAttempts,
	}
	owned := !a.adopted

	for {
		var exitErr error
		if owned {
			var stopped bool
			exitErr, stopped = a.waitOwned()
			if stopped {
				return
			}
		} else {
			if stopped := a.waitAdopted(); stopped {
				return
			}
			exitErr = errors.New("adopted process exited or was replaced")
			a.recordExit(exitErr)
		}

		if !a.shouldRestart(exitErr) {
			return
		}

		// Restart loop. Every attempt — including failed launches —
		// consumes one slot in the backoff window.
		restarted := false
		for !restarted {
			delay, ok := backoff.NextDelay(time.Now())
			if !ok {
				a.setStatus(ProcessStatusUnstable)
				slog.Warn("connector process unstable: restart budget exhausted",
					"connection", a.connID, "attempts", backoff.Attempts())
				a.publishEvent(ProcessEventUnstable, errors.New("restart budget exhausted"))
				return
			}
			if !a.sleepOrStop(delay) {
				return
			}
			a.setStatus(ProcessStatusStarting)
			if err := a.launch(); err != nil {
				a.recordExit(err)
				continue
			}
			a.mu.Lock()
			a.rec.Restarts++
			a.mu.Unlock()
			a.publishEvent(ProcessEventRestarted, nil)
			restarted = true
		}
		owned = true
	}
}

// shouldRestart applies the spec restart policy to an unexpected exit.
func (a *actor) shouldRestart(exitErr error) bool {
	switch a.spec.Restart {
	case core.RestartAlways:
		return true
	case core.RestartOnFailure:
		return exitErr != nil
	default:
		return false
	}
}

// waitOwned blocks until the owned process exits or a stop is requested.
func (a *actor) waitOwned() (exitErr error, stopped bool) {
	select {
	case err := <-a.waitCh:
		a.aio.close()
		a.aio = nil
		a.recordExit(err)
		if err != nil {
			slog.Warn("connector process exited",
				"connection", a.connID, "pid", a.identity().PID, "err", err)
		}
		return err, false
	case req := <-a.stopCh:
		err := a.terminateOwned(req.timeout)
		a.aio.close()
		a.aio = nil
		if err == nil {
			a.setStatus(ProcessStatusStopped)
			a.publishEvent(ProcessEventStopped, nil)
		}
		req.reply <- err
		return nil, true
	}
}

// terminateOwned gracefully stops the owned process: identity check, SIGTERM
// to the process group, wait up to timeout, then SIGKILL to the group.
func (a *actor) terminateOwned(timeout time.Duration) error {
	// Already exited?
	select {
	case <-a.waitCh:
		return nil
	default:
	}

	// Verify identity before signaling. Never signal on a PID alone.
	if verr := verifyIdentityFields(a.identity()); verr != nil {
		// The child may have just exited (identity unreadable); give the
		// wait goroutine a moment to confirm before giving up.
		select {
		case <-a.waitCh:
			return nil
		case <-time.After(2 * time.Second):
			return fmt.Errorf("identity check failed: %w", verr)
		}
	}

	// SIGTERM the whole process group (Setpgid makes the child the leader).
	pgid := a.cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("process signal: %w", err)
		}
	}

	select {
	case <-a.waitCh:
		return nil
	case <-time.After(timeout):
	}

	// Force kill the process group.
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		_ = a.cmd.Process.Kill()
	}
	<-a.waitCh
	return nil
}

// waitAdopted polls the adopted process for liveness until it exits or a
// stop is requested. Returns true when stopped by request.
func (a *actor) waitAdopted() (stopped bool) {
	interval := a.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := verifyIdentityFields(a.identity()); err != nil {
				// Process exited or the PID was reused.
				return false
			}
		case req := <-a.stopCh:
			err := a.terminateAdopted(req.timeout)
			if err == nil {
				a.setStatus(ProcessStatusStopped)
			}
			req.reply <- err
			return true
		}
	}
}

// terminateAdopted stops an adopted process. Identity is re-verified before
// every signal; the process group is only signaled when the adopted process
// is its own group leader (otherwise unrelated processes could be hit).
func (a *actor) terminateAdopted(timeout time.Duration) error {
	ident := a.identity()
	if verifyIdentityFields(ident) != nil {
		// Already gone or replaced: never signal.
		return nil
	}

	signalGroup := func(sig syscall.Signal) {
		if pgid, err := syscall.Getpgid(ident.PID); err == nil && pgid == ident.PID {
			if syscall.Kill(-pgid, sig) == nil {
				return
			}
		}
		_ = syscall.Kill(ident.PID, sig)
	}

	signalGroup(syscall.SIGTERM)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if verifyIdentityFields(ident) != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if verifyIdentityFields(ident) == nil {
		signalGroup(syscall.SIGKILL)
	}
	return nil
}

// sleepOrStop waits for the restart delay. Returns false if a stop request
// arrived during the wait (the restart is cancelled and the stop is acked).
func (a *actor) sleepOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case req := <-a.stopCh:
		a.setStatus(ProcessStatusStopped)
		req.reply <- nil
		return false
	}
}

// requestStop asks the actor to terminate and waits until the actor
// goroutine has fully exited. Safe to call on an already-finished actor.
func (a *actor) requestStop(timeout time.Duration) error {
	req := stopReq{timeout: timeout, reply: make(chan error, 1)}
	select {
	case a.stopCh <- req:
		err := <-req.reply
		<-a.done
		return err
	case <-a.done:
		// Actor already exited (process stopped/crashed/unstable).
		return nil
	}
}
