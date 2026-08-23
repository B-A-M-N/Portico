package origin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// ErrIdentityPartial is returned when an adopted or owned process has only a
// PID or other partially populated identity. Partial identities must never be
// accepted for signaling or adoption.
var ErrIdentityPartial = errors.New("origin: partial process identity")

// LocalCommand launches a local command and exposes its HTTP port. The
// started process is owned by the LocalCommand for its lifetime; the supplied
// Start context is only used for startup cancellation. The process runs in a
// dedicated process group so child listeners can be terminated atomically.
type LocalCommand struct {
	cfg            Config
	cmd            *exec.Cmd
	done           chan struct{}
	processGroupID int
	identity       core.ProcessIdentity
	logMu          sync.Mutex
	logs           []byte
}

// configureProcessGroup sets SysProcAttr so the started process and any of
// its descendants form a new process group whose leader is the direct child.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
}

// pidFromCmd returns the PID for an exec.Cmd (cross-platform-safe via Process).
func pidFromCmd(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

const maxCommandLogBytes = 1 << 20

type commandLogWriter struct{ command *LocalCommand }

func (w commandLogWriter) Write(data []byte) (int, error) {
	w.command.logMu.Lock()
	defer w.command.logMu.Unlock()
	if len(data) >= maxCommandLogBytes {
		w.command.logs = append(w.command.logs[:0], data[len(data)-maxCommandLogBytes:]...)
		return len(data), nil
	}
	overflow := len(w.command.logs) + len(data) - maxCommandLogBytes
	if overflow > 0 {
		copy(w.command.logs, w.command.logs[overflow:])
		w.command.logs = w.command.logs[:len(w.command.logs)-overflow]
	}
	w.command.logs = append(w.command.logs, data...)
	return len(data), nil
}

// NewLocalCommand creates a LocalCommand origin.
func NewLocalCommand(cfg Config) (*LocalCommand, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("--cmd is required for local:command origin")
	}
	if cfg.Port == 0 {
		return nil, fmt.Errorf("--port is required for local:command origin")
	}
	return &LocalCommand{cfg: cfg}, nil
}

func (l *LocalCommand) Type() Type {
	return TypeLocalCommand
}

// Start launches the command and waits until the port is ready. The supplied
// context is used only for startup cancellation and health polling; the
// launched process is owned by the LocalCommand and is unaffected by the
// caller's context cancellation once the start has succeeded.
//
// During startup, identity is not yet recorded so stopProcessGroupUnsafe
// is used (which simply signals the known PID). After Start succeeds,
// Stop uses full identity verification before signaling.
func (l *LocalCommand) Start(ctx context.Context) (string, error) {
	// Build the command. Use exec.Command (NOT CommandContext) so the
	// caller's context cannot terminate the process after Start returns.
	var cmd *exec.Cmd
	if l.cfg.Shell {
		// When Shell is true, the full command line is in l.cfg.Command and
		// l.cfg.Args must be empty.  This avoids the ambiguity of having two
		// sources for the command text.
		cmd = exec.Command("sh", "-c", l.cfg.Command)
	} else {
		cmd = exec.Command(l.cfg.Command, l.cfg.Args...)
	}

	if l.cfg.Dir != "" {
		cmd.Dir = l.cfg.Dir
	}

	// Use the allowlisted environment built by the supervisor. Children
	// must never inherit the supervisor's full environment.
	cmd.Env = allowlistedEnv(l.cfg.Env)

	// Run in a dedicated process group so stop can signal the entire group.
	configureProcessGroup(cmd)

	// Capture output.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	go func() {
		_, _ = io.Copy(commandLogWriter{command: l}, pr)
		_ = pr.Close()
	}()

	if err := cmd.Start(); err != nil {
		pw.Close()
		return "", fmt.Errorf("starting command: %w", err)
	}
	l.cmd = cmd
	l.processGroupID = pidFromCmd(cmd)
	l.done = make(chan struct{})

	// P0 #12: Capture identity IMMEDIATELY after start, not after
	// readiness. This ensures we have a complete identity for signaling
	// even if the readiness poll is cancelled/timed out.
	id, err := l.recordIdentity()
	if err != nil {
		// Identity is required for downstream signaling.
		_ = l.stopProcessGroupUnsafe()
		pw.Close()
		return "", fmt.Errorf("record identity: %w", err)
	}
	l.identity = id

	// Close write end when process exits.
	go func() {
		_ = cmd.Wait()
		pw.Close()
		close(l.done)
	}()

	// Wait for the port to be ready.
	originURL := fmt.Sprintf("http://127.0.0.1:%d", l.cfg.Port)
	healthURL := originURL
	if l.cfg.HealthPath != "" {
		healthURL = originURL + l.cfg.HealthPath
	}

	timeout := 60 * time.Second
	if l.cfg.WaitForReady != "" {
		if d, err := time.ParseDuration(l.cfg.WaitForReady); err == nil {
			timeout = d
		}
	}

	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}

	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return originURL, nil
			}
		}

		// Check if the process exited without racing exec.Cmd.Wait, which owns
		// ProcessState in the waiter goroutine.
		select {
		case <-l.done:
			exitCode := -1
			if l.cmd.ProcessState != nil {
				exitCode = l.cmd.ProcessState.ExitCode()
			}
			return "", fmt.Errorf("command exited before becoming ready (exit code %d)", exitCode)
		default:
		}

		if time.Now().After(deadline) {
			l.stopProcessGroupUnsafe()
			return "", fmt.Errorf("command not ready on port %d after %s", l.cfg.Port, timeout)
		}

		select {
		case <-ctx.Done():
			// Startup canceled before readiness: terminate the process group.
			l.stopProcessGroupUnsafe()
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Stop gracefully shuts down the origin by signaling the process group.
// It honours ctx for the total bounded deadline.
func (l *LocalCommand) Stop(ctx context.Context) error {
	if l.cmd == nil || l.cmd.Process == nil {
		return nil
	}
	// Already exited? Return immediately without spending five seconds.
	select {
	case <-l.done:
		return nil
	default:
	}
	return l.stopProcessGroup(ctx)
}

// stopProcessGroupUnsafe signals the known PID without identity verification.
// This is used during startup when identity has not yet been recorded.
// It is safe to signal the PID returned by exec.Cmd.Process because we just
// started it and PID reuse within the milliseconds between Start() and
// this call is effectively impossible. After identity is recorded, Stop()
// uses full identity verification before signaling.
func (l *LocalCommand) stopProcessGroupUnsafe() error {
	if l.cmd == nil || l.cmd.Process == nil {
		return nil
	}
	pid := pidFromCmd(l.cmd)
	if pid <= 0 {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-l.done:
		return nil
	case <-time.After(5 * time.Second):
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = l.cmd.Process.Kill()
	select {
	case <-l.done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("cmd.Wait did not return within 5s after SIGKILL (pid %d)", pid)
	}
}

// stopProcessGroup verifies the live process identity before signaling.
// PID reuse cannot redirect the signal because /proc/[pid] stat is read
// and compared against the stored identity (pid, start time, executable,
// command hash, process group) before any signal is sent.
func (l *LocalCommand) stopProcessGroup(ctx context.Context) error {
	ident, ok := l.Identity()
	if !ok || !ident.Complete() {
		return fmt.Errorf("origin: cannot stop without complete process identity")
	}
	if err := verifyOriginIdentity(ident, l.processGroupID); err != nil {
		return fmt.Errorf("origin identity mismatch, refusing to signal: %w", err)
	}
	pid := ident.PID
	pgid := l.processGroupID
	if pgid <= 0 {
		return fmt.Errorf("origin: no valid process group ID recorded")
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(15 * time.Second)
	}
	termDeadline := deadline.Add(-2 * time.Second)
	if time.Now().Before(termDeadline) {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	}

	select {
	case <-l.done:
		return nil
	case <-time.After(time.Until(termDeadline)):
	}

	// Escalate to SIGKILL on the group.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = l.cmd.Process.Kill()
	select {
	case <-l.done:
		return nil
	case <-time.After(time.Until(deadline)):
		return fmt.Errorf("origin process %d did not exit within deadline", pid)
	}
}

// Identity returns the recorded four-field process identity. The boolean is
// false until successful readiness recording.
func (l *LocalCommand) Identity() (core.ProcessIdentity, bool) {
	if !l.identity.Complete() {
		return core.ProcessIdentity{}, false
	}
	return l.identity, true
}

// ProcessGroupID returns the dedicated process group the origin runs in.
func (l *LocalCommand) ProcessGroupID() int {
	return l.processGroupID
}

// recordIdentity captures the four-field identity of the running process.
// Partial identities are rejected as ErrIdentityPartial.
func (l *LocalCommand) recordIdentity() (core.ProcessIdentity, error) {
	if l.cmd == nil || l.cmd.Process == nil {
		return core.ProcessIdentity{}, errors.New("command not started")
	}
	pid := pidFromCmd(l.cmd)
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return core.ProcessIdentity{}, fmt.Errorf("read exe: %w", err)
	}
	startTime, err := StartTimeOf(pid)
	if err != nil {
		return core.ProcessIdentity{}, fmt.Errorf("read start time: %w", err)
	}
	cmdHash, err := readCommandHash(pid)
	if err != nil {
		return core.ProcessIdentity{}, fmt.Errorf("read command hash: %w", err)
	}
	id := core.ProcessIdentity{
		PID:            pid,
		StartTime:      startTime,
		ExecutablePath: exe,
		CommandHash:    cmdHash,
	}
	if !id.Complete() {
		return core.ProcessIdentity{}, ErrIdentityPartial
	}
	return id, nil
}

func (l *LocalCommand) Logs() io.ReadCloser {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	return io.NopCloser(bytes.NewReader(append([]byte(nil), l.logs...)))
}

func (l *LocalCommand) Healthy(ctx context.Context) error {
	if l.cmd == nil || l.cmd.Process == nil {
		return fmt.Errorf("command not running")
	}
	select {
	case <-l.done:
		exitCode := -1
		if l.cmd.ProcessState != nil {
			exitCode = l.cmd.ProcessState.ExitCode()
		}
		return fmt.Errorf("command exited (code %d)", exitCode)
	default:
	}
	return nil
}
