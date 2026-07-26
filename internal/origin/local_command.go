package origin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// LocalCommand launches a local command and exposes its HTTP port.
type LocalCommand struct {
	cfg   Config
	cmd   *exec.Cmd
	done  chan struct{}
	logMu sync.Mutex
	logs  []byte
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

// Start launches the command and waits until the port is ready.
func (l *LocalCommand) Start(ctx context.Context) (string, error) {
	// Build the command.
	var cmd *exec.Cmd
	if l.cfg.Shell {
		// Shell evaluation is opt-in only. It is never inferred from spaces in
		// a command string because that would silently change user input into
		// shell syntax.
		cmd = exec.CommandContext(ctx, "sh", "-c", l.cfg.Command)
	} else {
		cmd = exec.CommandContext(ctx, l.cfg.Command, l.cfg.Args...)
	}

	if l.cfg.Dir != "" {
		cmd.Dir = l.cfg.Dir
	}

	// Set environment.
	cmd.Env = os.Environ()
	for k, v := range l.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

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
	l.done = make(chan struct{})

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
			l.Stop(ctx)
			return "", fmt.Errorf("command not ready on port %d after %s", l.cfg.Port, timeout)
		}

		select {
		case <-ctx.Done():
			l.Stop(ctx)
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (l *LocalCommand) Stop(_ context.Context) error {
	if l.cmd != nil && l.cmd.Process != nil {
		_ = l.cmd.Process.Signal(os.Interrupt)
		select {
		case <-l.done:
		case <-time.After(10 * time.Second):
			_ = l.cmd.Process.Kill()
			<-l.done
		}
	}
	return nil
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
