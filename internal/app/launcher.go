package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"charm.land/bubbletea/v2"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/lock"
	"github.com/B-A-M-N/portico/internal/tui"
	"github.com/charmbracelet/x/term"
)

// Launcher provides shared application bootstrapping for TUI and supervisor.
type Launcher struct {
	Paths Paths
}

// NewLauncher creates a new Launcher with default paths.
func NewLauncher() *Launcher {
	return &Launcher{
		Paths: DefaultPaths(),
	}
}

// EnsureSupervisor ensures a supervisor is running, starting one if necessary.
func (l *Launcher) EnsureSupervisor(ctx context.Context) error {
	if l.isSupervisorRunning(ctx) {
		slog.Info("supervisor already running")
		return nil
	}
	return l.StartSupervisor(ctx)
}

// isSupervisorRunning checks if the supervisor is running via health check.
func (l *Launcher) isSupervisorRunning(ctx context.Context) bool {
	client := ipc.NewClient(l.Paths.SocketPath)
	err := client.Health(ctx)
	return err == nil
}

// StartSupervisor starts the supervisor in detached mode.
func (l *Launcher) StartSupervisor(ctx context.Context) error {
	slog.Info("starting supervisor", "socket", l.Paths.SocketPath)

	lockDir := filepath.Dir(l.Paths.SocketPath)
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}

	// ---- Serialized startup via a launch-specific lock file ----
	// This lock is distinct from the supervisor's lifetime lock
	// (portico-supervisor.lock). It serializes concurrent launcher
	// processes without conflicting with the supervisor's own lock.
	launchLockPath := filepath.Join(lockDir, "portico-launch.lock")
	sf, err := os.OpenFile(launchLockPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("open launch lock: %w", err)
	}
	defer sf.Close()

	// Try non-blocking first. If it fails, another launcher is starting
	// the supervisor or it is already running.
	if err := lock.Lock(sf); err != nil {
		// Lock held — check if supervisor is actually healthy.
		if l.isSupervisorRunning(ctx) {
			slog.Info("supervisor already running")
			return nil
		}
		// Lock held but supervisor not responding — might be a stale lock
		// or another launcher starting up. Wait for the lock.
		if err := lock.LockBlocking(sf); err != nil {
			return fmt.Errorf("could not acquire launch lock: %w", err)
		}
		// Got the lock. Re-check health — the other launcher may have
		// successfully started the supervisor.
		if l.isSupervisorRunning(ctx) {
			lock.Unlock(sf)
			slog.Info("supervisor already running")
			return nil
		}
	}
	// Launch lock acquired — we are now responsible for starting the supervisor.
	// Hold this lock until the child is confirmed healthy to prevent another
	// launcher from racing us.
	defer lock.Unlock(sf)

	// Resolve executable
	exe, err := Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	// Prepare log file - use a stable log path instead of PID-based
	logDir := l.Paths.LogDir
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logFile := filepath.Join(logDir, "supervisor.log")

	logF, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	cmd := exec.Command(exe, "supervisor", "run")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	devNull, err := os.OpenFile("/dev/null", os.O_RDONLY, 0)
	if err != nil {
		logF.Close()
		return fmt.Errorf("open /dev/null: %w", err)
	}
	cmd.Stdin = devNull
	cmd.Stdout = logF
	cmd.Stderr = logF

	if err := cmd.Start(); err != nil {
		logF.Close()
		devNull.Close()
		return fmt.Errorf("start supervisor: %w", err)
	}

	// Close parent's file descriptors after child inherits them
	logF.Close()
	devNull.Close()

	// Check for early child exit
	exitCh := make(chan error, 1)
	go func() {
		exitCh <- cmd.Wait()
	}()

	select {
	case err := <-exitCh:
		return fmt.Errorf("supervisor exited early: %w (log path: %s)", err, logFile)
	case <-time.After(500 * time.Millisecond):
	}

	if err := l.waitForReady(ctx); err != nil {
		return fmt.Errorf("supervisor ready timeout: %w (log path: %s)", err, logFile)
	}

	slog.Info("supervisor started successfully")
	return nil
}

// waitForReady waits for the supervisor to become ready.
func (l *Launcher) waitForReady(ctx context.Context) error {
	maxAttempts := 10
	baseDelay := 100 * time.Millisecond
	maxDelay := 2 * time.Second

	client := ipc.NewClient(l.Paths.SocketPath)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := client.Health(ctx)
		if err == nil {
			return nil
		}

		delay := baseDelay
		for i := 0; i < attempt; i++ {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}

	return fmt.Errorf("supervisor did not become ready after %d attempts", maxAttempts)
}

// RunTUI launches the Bubble Tea TUI connected to the supervisor.
// Returns the actual Bubble Tea error — does not swallow terminal errors.
func (l *Launcher) RunTUI(ctx context.Context) error {
	if !isInteractive() {
		return fmt.Errorf("TUI requires an interactive terminal")
	}

	if err := l.EnsureSupervisor(ctx); err != nil {
		return fmt.Errorf("ensure supervisor: %w", err)
	}

	client := ipc.NewClient(l.Paths.SocketPath)

	if err := client.Health(ctx); err != nil {
		return fmt.Errorf("supervisor health check failed: %w (log path: %s)",
			err, filepath.Join(l.Paths.LogDir, "supervisor.log"))
	}

	rootModel := tui.New(client)

	program := tea.NewProgram(
		rootModel,
		tea.WithContext(ctx),
		tea.WithInput(os.Stdin),
		tea.WithOutput(os.Stdout),
	)

	if _, err := program.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	return nil
}

// RunSupervisor returns an error — use the supervisor package directly.
func (l *Launcher) RunSupervisor(ctx context.Context) error {
	return fmt.Errorf("use supervisor.RunSupervisor(ctx) instead")
}

// isInteractive checks if stdin is an interactive terminal.
func isInteractive() bool {
	return IsTerminalInput()
}

// IsTerminalInput checks if stdin is a terminal.
func IsTerminalInput() bool {
	return IsTerminal(os.Stdin.Fd())
}

// IsTerminalOutput checks if stdout is a terminal.
func IsTerminalOutput() bool {
	return IsTerminal(os.Stdout.Fd())
}

// IsTerminal checks if the given file descriptor is a terminal.
func IsTerminal(fd uintptr) bool {
	return term.IsTerminal(fd)
}

// CleanupLock removes stale lock files.
func CleanupLock(paths Paths) error {
	lockDir := filepath.Dir(paths.SocketPath)

	conn, err := net.Dial("unix", paths.SocketPath)
	if err == nil {
		conn.Close()
		return nil
	}

	for _, name := range []string{"portico.lock", "portico-startup.lock", "portico-supervisor.lock", "portico-launch.lock"} {
		lockPath := filepath.Join(lockDir, name)
		if _, err := os.Stat(lockPath); err == nil {
			slog.Info("removing stale lock file", "path", lockPath)
			os.Remove(lockPath)
		}
	}
	return nil
}

// ConnectToSupervisor creates an IPC client to the supervisor.
func (l *Launcher) ConnectToSupervisor() *ipc.Client {
	return ipc.NewClient(l.Paths.SocketPath)
}

// SupervisorHealthCheck performs a health check on the supervisor.
func (l *Launcher) SupervisorHealthCheck(ctx context.Context) error {
	client := ipc.NewClient(l.Paths.SocketPath)
	return client.Health(ctx)
}

// GetSocketPath returns the supervisor socket path.
func (l *Launcher) GetSocketPath() string {
	return l.Paths.SocketPath
}

// GetPaths returns the resolved paths.
func (l *Launcher) GetPaths() Paths {
	return l.Paths
}
