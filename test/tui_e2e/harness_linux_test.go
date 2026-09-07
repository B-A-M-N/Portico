//go:build linux

package tui_e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

const (
	e2eEnabledEnv  = "PORTICO_TUI_E2E"
	defaultTimeout = 15 * time.Second
)

type fixture struct {
	t      *testing.T
	binary string
	root   string
	env    []string
}

type session struct {
	t       *testing.T
	fixture *fixture
	width   int
	height  int
	master  *os.File
	cmd     *exec.Cmd

	mu       sync.Mutex
	raw      []byte
	terminal *terminal
	chunks   chan []byte
	done     chan struct{}
	readErr  error
	closed   bool

	commands []string
}

func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv(e2eEnabledEnv) != "1" {
		t.Skipf("real PTY acceptance is opt-in; run with %s=1 (make tui-e2e does this)", e2eEnabledEnv)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	binary := os.Getenv("PORTICO_TUI_E2E_BINARY")
	if binary == "" {
		workingDir, err := os.Getwd()
		if err != nil {
			t.Fatalf("resolve test directory: %v", err)
		}
		binary = filepath.Join(workingDir, "..", "..", "portico")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatalf("resolve TUI binary: %v", err)
	}
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("real TUI binary %q is unavailable: %v (run make build first)", binary, err)
	}
	if info.IsDir() {
		t.Fatalf("TUI binary path is a directory: %s", binary)
	}

	root := t.TempDir()
	paths := map[string]string{}
	for _, name := range []string{"config", "data", "state", "runtime", "home"} {
		paths[name] = filepath.Join(root, name)
		if err := os.MkdirAll(paths[name], 0700); err != nil {
			t.Fatalf("create hermetic %s directory: %v", name, err)
		}
	}
	if err := os.Chmod(paths["runtime"], 0700); err != nil {
		t.Fatalf("secure runtime directory: %v", err)
	}

	env := append([]string(nil), os.Environ()...)
	env = setEnv(env, "XDG_CONFIG_HOME", paths["config"])
	env = setEnv(env, "XDG_DATA_HOME", paths["data"])
	env = setEnv(env, "XDG_STATE_HOME", paths["state"])
	env = setEnv(env, "XDG_RUNTIME_DIR", paths["runtime"])
	env = setEnv(env, "HOME", paths["home"])
	env = setEnv(env, "TERM", "xterm-256color")
	env = setEnv(env, "PORTICO_DEV", "true")
	env = setEnv(env, "PORTICO_ASCII", "1")
	env = setEnv(env, "NO_COLOR", "1")
	return &fixture{t: t, binary: binary, root: root, env: env}
}

func (f *fixture) startTUI(width, height int) *session {
	f.t.Helper()
	master, slave, err := openPTY(width, height)
	if err != nil {
		f.t.Fatalf("allocate PTY: %v", err)
	}

	cmd := exec.Command(f.binary)
	cmd.Env = f.env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = master.Close()
		f.t.Fatalf("start real TUI: %v", err)
	}
	_ = slave.Close()

	s := &session{
		t:        f.t,
		fixture:  f,
		width:    width,
		height:   height,
		master:   master,
		cmd:      cmd,
		terminal: newTerminal(width, height),
		chunks:   make(chan []byte, 32),
		done:     make(chan struct{}),
	}
	f.t.Cleanup(s.cleanup)
	go s.readLoop()
	return s
}

func (s *session) readLoop() {
	defer close(s.done)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.raw = append(s.raw, chunk...)
			s.terminal.feed(chunk)
			s.mu.Unlock()
			select {
			case s.chunks <- chunk:
			default:
			}
		}
		if err != nil {
			s.mu.Lock()
			s.readErr = err
			s.mu.Unlock()
			return
		}
	}
}

func (s *session) send(key string) {
	s.t.Helper()
	data, ok := keyBytes(key)
	if !ok {
		s.t.Fatalf("unknown terminal key %q", key)
	}
	s.sendRaw(key, data)
	// Bubble Tea intentionally waits briefly after a bare ESC to distinguish
	// the Escape key from the prefix of an ANSI sequence. Without this pause a
	// test's `esc` followed immediately by the next route key can be parsed as
	// Alt+<key>, which is exactly the kind of terminal-boundary bug this suite
	// needs to avoid masking.
	if key == "esc" {
		time.Sleep(120 * time.Millisecond)
	}
}

// chooseOutcome selects a visible outcome row by its rendered label. The first
// prepared goals are shown immediately; the remaining catalog is behind More,
// so PTY tests exercise the same disclosure a user sees instead of relying on
// storage indices that are not part of the interface.
func (s *session) chooseOutcome(label string) {
	s.t.Helper()
	for attempts := 0; attempts < 40; attempts++ {
		current := s.screen()
		if strings.Contains(current, "> "+label) {
			s.send("enter")
			return
		}
		if strings.Contains(current, "> More connection types") {
			s.send("enter")
			s.waitForScreenChange(current)
			continue
		}
		s.send("down")
		s.waitForScreenChange(current)
	}
	s.t.Fatalf("could not select outcome %q:\n%s", label, s.debug())
}

func (s *session) sendRaw(label string, data []byte) {
	s.t.Helper()
	s.mu.Lock()
	s.commands = append(s.commands, fmt.Sprintf("send %s %q", label, data))
	s.mu.Unlock()
	if _, err := s.master.Write(data); err != nil && !s.isClosed() {
		s.t.Fatalf("send %s: %v", label, err)
	}
}

func (s *session) typeText(value string) {
	s.t.Helper()
	for _, r := range value {
		s.send(string(r))
	}
}

// typeSecretText writes credential material to the PTY without putting the
// value in the command transcript. The terminal capture still proves what the
// TUI rendered, while the failure artifact cannot become a second secret leak
// merely because the test exercised a password field.
func (s *session) typeSecretText(value string) {
	s.t.Helper()
	s.mu.Lock()
	s.commands = append(s.commands, fmt.Sprintf("type secret <%d chars>", len([]rune(value))))
	s.mu.Unlock()
	if _, err := s.master.Write([]byte(value)); err != nil && !s.isClosed() {
		s.t.Fatalf("send secret text: %v", err)
	}
}

// pasteText drives the bracketed-paste bytes a terminal sends. This is kept
// separate from typeText so the suite proves paste handling rather than merely
// many individual printable key presses.
func (s *session) pasteText(value string) {
	s.t.Helper()
	data := append([]byte("\x1b[200~"), []byte(value)...)
	data = append(data, []byte("\x1b[201~")...)
	s.sendRaw("paste", data)
}

func (s *session) resize(width, height int) {
	s.t.Helper()
	if err := resizePTY(s.master, width, height); err != nil {
		s.t.Fatalf("resize to %dx%d: %v", width, height, err)
	}
	s.mu.Lock()
	s.width, s.height = width, height
	s.commands = append(s.commands, fmt.Sprintf("resize %dx%d", width, height))
	s.terminal.resize(width, height)
	s.mu.Unlock()
}

func (s *session) waitFor(needle string) {
	s.t.Helper()
	deadline := time.NewTimer(defaultTimeout)
	defer deadline.Stop()
	for {
		if strings.Contains(strings.ToLower(s.screen()), strings.ToLower(needle)) {
			return
		}
		select {
		case <-s.chunks:
		case <-s.done:
			s.t.Fatalf("TUI exited while waiting for %q\n%s", needle, s.debug())
		case <-deadline.C:
			s.t.Fatalf("timed out waiting for %q\n%s", needle, s.debug())
		}
	}
}

// waitForScreenChange proves a navigation key caused a new terminal frame to
// be rendered. Waiting for a string that was already on screen can pass even
// when the key was ignored, which is how a broken page-down path escaped the
// original black-box checks.
func (s *session) waitForScreenChange(before string) {
	s.t.Helper()
	deadline := time.NewTimer(defaultTimeout)
	defer deadline.Stop()
	for {
		if current := s.screen(); current != before {
			return
		}
		select {
		case <-s.chunks:
		case <-s.done:
			s.t.Fatalf("TUI exited while waiting for a screen change\n%s", s.debug())
		case <-deadline.C:
			s.t.Fatalf("timed out waiting for a screen change\n%s", s.debug())
		}
	}
}

func (s *session) page(key string) {
	s.t.Helper()
	before := s.screen()
	s.send(key)
	s.waitForScreenChange(before)
}

// escUntil presses esc repeatedly until one of the markers appears, waiting
// for a repaint after each press. The back stack behind a provider-setup form
// varies with how it was opened, and guessing its depth made the test pin
// navigation internals rather than user-visible screens.
func (s *session) escUntil(markers ...string) {
	s.t.Helper()
	for range 10 {
		screen := s.screen()
		for _, marker := range markers {
			if strings.Contains(screen, marker) {
				return
			}
		}
		s.send("esc")
		s.waitForScreenChange(screen)
	}
	s.t.Fatalf("none of %v reached within ten esc presses:\n%s", markers, s.debug())
}

func (s *session) waitExit() {
	s.t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if s.cmd.ProcessState != nil {
			return
		}
		select {
		case <-s.done:
			return
		case <-deadline.C:
			s.t.Fatalf("TUI did not exit\n%s", s.debug())
		}
	}
}

func (s *session) screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal.text()
}

func (s *session) rawOutput() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.raw...)
}

func (s *session) debug() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal.debug()
}

func (s *session) assertNoOverflow() {
	s.t.Helper()
	for number, line := range strings.Split(s.screen(), "\n") {
		if width := ansi.StringWidth(line); width > s.width {
			s.t.Fatalf("screen line %d exceeds PTY width %d: %d cells\n%s", number+1, s.width, width, s.debug())
		}
	}
}

func (s *session) stopTUI() {
	s.t.Helper()
	if s.cmd == nil || s.cmd.Process == nil || s.cmd.ProcessState != nil {
		return
	}
	if _, err := s.master.Write([]byte{3}); err != nil && !s.isClosed() {
		s.t.Logf("send Ctrl+C during TUI cleanup: %v", err)
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
	_ = s.master.Close()
	s.closed = true
}

func (s *session) cleanup() {
	if s == nil {
		return
	}
	s.stopTUI()
	if s.master != nil && !s.closed {
		_ = s.master.Close()
		s.closed = true
	}
	// The TUI starts the supervisor detached. Every test gets an isolated
	// runtime/data root, so stop it explicitly instead of leaking a process into
	// the next test or the developer's installation.
	if s.fixture != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, s.fixture.binary, "supervisor", "stop")
		cmd.Env = s.fixture.env
		_ = cmd.Run()
	}
	if s.t.Failed() {
		s.writeArtifacts()
	}
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *session) writeArtifacts() {
	root := os.Getenv("PORTICO_TUI_E2E_ARTIFACT_DIR")
	if root == "" {
		workingDir, err := os.Getwd()
		if err == nil {
			root = filepath.Join(workingDir, "..", "..", "artifacts", "tui-e2e")
		} else {
			root = filepath.Join("artifacts", "tui-e2e")
		}
	}
	name := strings.NewReplacer("/", "_", "\\", "_", " ", "_").Replace(s.t.Name())
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.t.Logf("write TUI E2E artifacts: %v", err)
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "terminal.raw"), s.rawOutput(), 0600)
	_ = os.WriteFile(filepath.Join(dir, "terminal.txt"), []byte(s.screen()+"\n"), 0600)
	s.mu.Lock()
	commands := strings.Join(s.commands, "\n") + "\n"
	s.mu.Unlock()
	_ = os.WriteFile(filepath.Join(dir, "commands.log"), []byte(commands), 0600)
	_ = os.WriteFile(filepath.Join(dir, "failure.txt"), []byte(s.debug()+"\n"), 0600)
	if log, err := findFile(s.fixture.root, "supervisor.log"); err == nil {
		if data, readErr := os.ReadFile(log); readErr == nil {
			_ = os.WriteFile(filepath.Join(dir, "supervisor.log"), data, 0600)
		}
	}
	if data, err := os.ReadFile(filepath.Join(s.fixture.root, "data", "portico", "portico.db")); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "state.db"), data, 0600)
	}
	s.t.Logf("TUI E2E failure artifacts: %s", dir)
}

func runCLI(t *testing.T, f *fixture, args ...string) string {
	t.Helper()
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	cmd := exec.Command(f.binary, args...)
	cmd.Env = f.env
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("portico %s: %v\n%s\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	// The CLI's slog diagnostics go to stderr; JSON and table output go to
	// stdout. Mixing the two made `list --json` unparseable whenever the
	// launcher logged ("supervisor already running") on the way through.
	return stdout.String()
}

// runCLIWithStdin runs the CLI with the given bytes on stdin. Secure-credential
// acquisition through --credential-stdin is a production contract, so the PTY
// suite must be able to exercise it the way a script would.
func runCLIWithStdin(t *testing.T, f *fixture, stdin string, args ...string) string {
	t.Helper()
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	cmd := exec.Command(f.binary, args...)
	cmd.Env = f.env
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("portico %s: %v\n%s\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate fixture TCP port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func keyBytes(key string) ([]byte, bool) {
	switch key {
	case "enter":
		return []byte("\r"), true
	case "esc":
		return []byte("\x1b"), true
	case "f1":
		return []byte("\x1b[11~"), true
	case "f2":
		return []byte("\x1b[12~"), true
	case "up":
		return []byte("\x1b[A"), true
	case "down":
		return []byte("\x1b[B"), true
	case "left":
		return []byte("\x1b[D"), true
	case "right":
		return []byte("\x1b[C"), true
	case "pgup":
		return []byte("\x1b[5~"), true
	case "pgdown":
		return []byte("\x1b[6~"), true
	case "home":
		return []byte("\x1b[H"), true
	case "end":
		return []byte("\x1b[F"), true
	case "delete":
		return []byte("\x1b[3~"), true
	case "ctrl-c":
		return []byte{3}, true
	case "ctrl-a":
		return []byte{1}, true
	case "ctrl-u":
		return []byte{21}, true
	case "backspace":
		return []byte{127}, true
	default:
		if len(key) == 1 {
			return []byte(key), true
		}
		return nil, false
	}
}

func findFile(root, base string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == base {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", errors.New("file not found")
	}
	return found, nil
}
