package tunnel

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	flareexec "github.com/B-A-M-N/portico/internal/exec"
	"github.com/natefinch/lumberjack"
)

// quickTunnelURLRe matches the trycloudflare.com URL that cloudflared prints.
var quickTunnelURLRe = regexp.MustCompile(`trycloudflare\.com/[a-zA-Z0-9_-]+`)

// QuickTunnelResult holds the result of a quick tunnel start.
type QuickTunnelResult struct {
	PID           int
	PublicAddress string
	Identity      ProcessIdentity
	LogPath       string
}

// ProcessIdentity holds identity info about a running process.
type ProcessIdentity struct {
	PID     int
	LogPath string
}

// Connector manages the cloudflared process.
type Connector interface {
	Run(ctx context.Context, token string) (pid int, err error)
	// RunQuick starts cloudflared in quick tunnel mode (--url), used for temporary exposure.
	// url is the origin URL (e.g., http://localhost:8080).
	// It returns the result including the assigned public address.
	RunQuick(ctx context.Context, url string) (QuickTunnelResult, error)
	Stop(ctx context.Context) error
	Logs() io.ReadCloser
	Healthy() error
	// ExitCh returns a channel that is closed when the cloudflared process exits.
	// Returns nil if the process is not running.
	ExitCh() <-chan struct{}
	// LogFilePath returns the path to the persistent log file on disk.
	LogFilePath() string
}

// ProcessConnector runs cloudflared as a subprocess.
type ProcessConnector struct {
	mu     sync.Mutex
	bin    string // Path to cloudflared binary.
	runner *flareexec.Runner
	logDir string // Directory for persistent log files.
}

// NewProcessConnector creates a connector for the given cloudflared binary.
func NewProcessConnector(cloudflaredBin string) *ProcessConnector {
	if cloudflaredBin == "" {
		cloudflaredBin = "cloudflared"
	}

	logDir := porticoConnectorLogDir()
	if logDir != "" {
		_ = os.MkdirAll(logDir, 0700)
	}

	return &ProcessConnector{bin: cloudflaredBin, logDir: logDir}
}

// porticoConnectorLogDir follows the same XDG state contract as the
// supervisor. Raw cloudflared output is runtime state, not configuration, so
// it must never be written into the legacy Flare configuration directory.
func porticoConnectorLogDir() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "portico", "logs")
}

// Run starts `cloudflared tunnel --no-autoupdate run --token <token>`.
func (c *ProcessConnector) Run(ctx context.Context, token string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Build log file path with rotation: max 10MB, keep 5 backups, compress
	var logWriter io.Writer
	if c.logDir != "" {
		logFile := filepath.Join(c.logDir, "cloudflared.log")
		_ = os.MkdirAll(c.logDir, 0700)
		logWriter = &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    10, // megabytes
			MaxBackups: 5,
			MaxAge:     30, // days
			Compress:   true,
		}
	}

	runner, err := flareexec.Start(ctx, flareexec.RunOpts{
		Name: c.bin,
		Args: []string{
			"tunnel",
			"--no-autoupdate",
			"run",
			"--token", token,
		},
		LogWriter: logWriter,
	})
	if err != nil {
		return 0, fmt.Errorf("starting cloudflared: %w", err)
	}

	c.runner = runner
	return runner.PID(), nil
}

// RunQuick starts `cloudflared tunnel --no-autoupdate --url <url>` for quick tunnels.
// It parses the trycloudflare.com URL from the process output and returns it
// in the QuickTunnelResult along with the PID.
func (c *ProcessConnector) RunQuick(ctx context.Context, url string) (QuickTunnelResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Build log file path with rotation: max 10MB, keep 5 backups, compress
	var logWriter io.Writer
	var logFile string
	if c.logDir != "" {
		logFile = filepath.Join(c.logDir, "cloudflared-quick.log")
		_ = os.MkdirAll(c.logDir, 0700)
		logWriter = &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    10, // megabytes
			MaxBackups: 5,
			MaxAge:     30, // days
			Compress:   true,
		}
	}

	runner, err := flareexec.Start(ctx, flareexec.RunOpts{
		Name: c.bin,
		Args: []string{
			"tunnel",
			"--no-autoupdate",
			"--url", url,
		},
		LogWriter: logWriter,
	})
	if err != nil {
		return QuickTunnelResult{}, fmt.Errorf("starting cloudflared quick tunnel: %w", err)
	}

	c.runner = runner

	// Wait briefly for cloudflared to print the URL, then extract it.
	publicAddr := ""
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		logs := runner.Logs()
		buf := make([]byte, 4096)
		n, _ := logs.Read(buf)
		logs.Close()
		if n > 0 {
			addr, err := ExtractQuickTunnelURL(string(buf[:n]))
			if err == nil {
				publicAddr = addr
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	return QuickTunnelResult{
		PID:           runner.PID(),
		PublicAddress: publicAddr,
		Identity: ProcessIdentity{
			PID:     runner.PID(),
			LogPath: logFile,
		},
		LogPath: logFile,
	}, nil
}

// Stop gracefully stops the cloudflared process.
func (c *ProcessConnector) Stop(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.runner == nil {
		return nil
	}
	return c.runner.Stop()
}

// Logs returns a reader for cloudflared's output.
func (c *ProcessConnector) Logs() io.ReadCloser {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.runner == nil {
		return io.NopCloser(strings.NewReader(""))
	}
	return c.runner.Logs()
}

// Healthy returns nil if cloudflared is still running.
func (c *ProcessConnector) Healthy() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.runner == nil {
		return fmt.Errorf("cloudflared not started")
	}
	if !c.runner.Running() {
		exitErr := c.runner.ExitError()
		if exitErr != nil {
			return fmt.Errorf("cloudflared process exited: %w", exitErr)
		}
		return fmt.Errorf("cloudflared process exited")
	}
	return nil
}

// ExitCh returns a channel that is closed when the cloudflared process exits.
func (c *ProcessConnector) ExitCh() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.runner == nil {
		// Return a closed channel so callers don't block forever.
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return c.runner.ExitCh()
}

// LogFilePath returns the path to the persistent log file on disk.
func (c *ProcessConnector) LogFilePath() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.runner == nil {
		return ""
	}
	return c.runner.LogFilePath()
}

// ExtractQuickTunnelURL parses the trycloudflare.com URL from cloudflared output.
// Returns a validated absolute HTTPS URL, or an error if the output does not
// contain a valid Quick Tunnel address.
func ExtractQuickTunnelURL(output string) (string, error) {
	matches := quickTunnelURLRe.FindStringSubmatch(output)
	if len(matches) == 0 {
		return "", fmt.Errorf("no Quick Tunnel URL found in output")
	}
	// The regex returns a bare hostname. Construct a validated absolute URL.
	raw := "https://" + matches[0]
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid Quick Tunnel URL %q: %w", raw, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("invalid Quick Tunnel URL %q", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("Quick Tunnel URL must not contain userinfo")
	}
	return parsed.String(), nil
}
