package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Starting when the supervisor will not.
//
// RunTUI performed EnsureSupervisor and a health check before Bubble Tea
// started, so a supervisor that would not start dropped the user back to a shell
// with a raw error — while the TUI already had a recovery screen, structured
// errors and a retry action that nothing could reach.
//
// The TUI now starts regardless and the bring-up sequence runs from its init
// command. Retry runs the whole sequence again: a bare snapshot retry against an
// unchanged dead supervisor would loop without progress.

// fakeBootstrapper stands in for *app.Launcher.
//
// It is the narrow dependency the TUI needs — three methods — rather than the
// application layer, which is what lets these paths be exercised without a real
// socket or a real supervisor process.
type fakeBootstrapper struct {
	mu sync.Mutex
	// results are returned in order, one per attempt, so a test can make the
	// first attempt fail and a later one succeed.
	results  []error
	attempts int
	logPath  string
	socket   string
}

func (b *fakeBootstrapper) TryBootstrap(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	index := b.attempts
	b.attempts++
	if index < len(b.results) {
		return b.results[index]
	}
	if len(b.results) == 0 {
		return nil
	}
	// Beyond the scripted attempts, the last outcome persists: a supervisor that
	// is broken stays broken until something changes.
	return b.results[len(b.results)-1]
}

func (b *fakeBootstrapper) SupervisorLogPath() string { return b.logPath }
func (b *fakeBootstrapper) SocketPath() string        { return b.socket }

func (b *fakeBootstrapper) attemptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// bootstrapFailures are the ways bring-up actually fails, with the error each
// one produces. These are the six cases the audit names.
func bootstrapFailures() map[string]error {
	return map[string]error{
		"supervisor missing": fmt.Errorf(
			"could not start the supervisor: %w", errors.New("executable not found")),
		"supervisor exits during startup": fmt.Errorf(
			"could not start the supervisor: %w", errors.New("process exited with status 1")),
		"stale socket": fmt.Errorf(
			"supervisor started but is not responding: %w", errors.New("dial unix: connection refused")),
		"socket permission failure": fmt.Errorf(
			"could not start the supervisor: %w", errors.New("dial unix: permission denied")),
		"health failure": fmt.Errorf(
			"supervisor started but is not responding: %w", errors.New("health check returned 503")),
		"health times out": fmt.Errorf(
			"supervisor did not become ready after 10 attempts"),
	}
}

// TestBootstrapFailureEntersRecovery pins that every failure mode reaches the
// recovery screen rather than the shell.
func TestBootstrapFailureEntersRecovery(t *testing.T) {
	for name, failure := range bootstrapFailures() {
		t.Run(name, func(t *testing.T) {
			boot := &fakeBootstrapper{
				results: []error{failure},
				logPath: "/tmp/portico/supervisor.log",
				socket:  "/tmp/portico/portico.sock",
			}
			m := newModel(&fakeClient{}, boot)
			m.width = 100

			// Init runs the sequence off the update loop, so the TUI is already
			// running when it fails.
			cmd := m.Init()
			if cmd == nil {
				t.Fatal("the TUI did not attempt bring-up at all")
			}
			msg := cmd()
			failed, ok := msg.(bootstrapFailedMsg)
			if !ok {
				t.Fatalf("bring-up produced %T, want bootstrapFailedMsg", msg)
			}

			next, _ := m.Update(failed)
			m = next.(Model)
			if m.screen != ScreenRecovery {
				t.Fatalf("screen = %q, want recovery", m.screen)
			}

			view := m.View().Content
			// A concise explanation, in the user's terms.
			if !strings.Contains(view, "supervisor") {
				t.Errorf("recovery does not explain what is unreachable:\n%s", view)
			}
			// A concrete next action, and the retry.
			if !strings.Contains(view, "What you can do") {
				t.Errorf("recovery offers no next action:\n%s", view)
			}
			if !strings.Contains(view, "Retry") {
				t.Errorf("recovery does not offer a retry:\n%s", view)
			}
			// The log path, so evidence can be found without knowing the layout.
			if !strings.Contains(view, boot.logPath) {
				t.Errorf("recovery does not say where the supervisor log is:\n%s", view)
			}
			// The socket, which is what a permission or staleness problem is about.
			if !strings.Contains(view, boot.socket) {
				t.Errorf("recovery does not name the socket:\n%s", view)
			}
			// The technical detail is present but secondary: it appears after
			// the actions, not instead of them.
			detailAt := strings.Index(view, "Technical detail")
			actionsAt := strings.Index(view, "What you can do")
			if detailAt < 0 {
				t.Errorf("recovery discards the technical detail entirely:\n%s", view)
			} else if detailAt < actionsAt {
				t.Error("the technical detail is shown before what the user can do")
			}
		})
	}
}

// TestRecoveryRetryRunsTheWholeSequence pins the requirement that retry is not a
// bare snapshot fetch.
//
// Retrying GetSnapshot against an unchanged dead supervisor cannot succeed. The
// retry has to start the supervisor, check that it answers, and only then load
// the state.
func TestRecoveryRetryRunsTheWholeSequence(t *testing.T) {
	boot := &fakeBootstrapper{results: []error{errors.New("supervisor is not running")}}
	m := newModel(&fakeClient{}, boot)
	m.width = 100

	next, _ := m.Update(bootstrapFailedMsg{Err: boot.results[0]})
	m = next.(Model)
	if boot.attemptCount() != 0 {
		t.Fatalf("attempts before retry = %d, want 0", boot.attemptCount())
	}

	action, ok := m.actionsFor(ScreenRecovery).Find(ActionRetry)
	if !ok || !action.Enabled {
		t.Fatal("recovery does not offer a retry action")
	}
	m, cmd := press(t, m, action.primaryKey())
	if cmd == nil {
		t.Fatal("retry produced no command")
	}
	// Retry goes back through boot, not straight to a snapshot.
	if m.screen != ScreenBoot {
		t.Fatalf("screen after retry = %q, want boot", m.screen)
	}
	// The error is cleared so the retry is not rendered as still failing.
	if m.err != nil {
		t.Error("retry kept the previous error")
	}

	cmd()
	if boot.attemptCount() != 1 {
		t.Fatalf("retry made %d bring-up attempts, want 1", boot.attemptCount())
	}
}

// TestSuccessfulRecoveryReachesHome pins the whole sequence end to end:
// a failure, a retry that works, and the connections loaded.
func TestSuccessfulRecoveryReachesHome(t *testing.T) {
	// The first attempt fails; the second succeeds.
	boot := &fakeBootstrapper{results: []error{errors.New("supervisor is not running"), nil}}
	client := &fakeClient{snapshot: twoConnectionSnapshot()}
	m := newModel(client, boot)
	m.width = 100

	// Fail into recovery.
	next, _ := m.Update(bootstrapFailedMsg{Err: boot.results[0]})
	m = next.(Model)
	if m.screen != ScreenRecovery {
		t.Fatalf("screen = %q, want recovery", m.screen)
	}

	// Retry. The scripted first attempt fails, which returns the user to
	// recovery rather than stranding them on the boot screen.
	m, cmd := press(t, m, "r")
	if m.screen != ScreenBoot {
		t.Fatalf("retry did not re-enter bring-up: screen = %q", m.screen)
	}
	failure, ok := cmd().(bootstrapFailedMsg)
	if !ok {
		t.Fatal("the scripted first attempt did not fail")
	}
	next, _ = m.Update(failure)
	m = next.(Model)
	if m.screen != ScreenRecovery {
		t.Fatalf("a failed retry left the user on %q, not recovery", m.screen)
	}

	// Retry again: this attempt succeeds.
	m, cmd = press(t, m, "r")
	msg := cmd()
	done, ok := msg.(bootstrapDoneMsg)
	if !ok {
		t.Fatalf("the successful attempt produced %T, want bootstrapDoneMsg", msg)
	}

	// Bring-up succeeding asks for the snapshot; the snapshot lands on Home.
	next, snapCmd := m.Update(done)
	m = next.(Model)
	if snapCmd == nil {
		t.Fatal("a successful bring-up did not go on to load the state")
	}
	snapshot, ok := snapCmd().(snapshotMsg)
	if !ok {
		t.Fatal("bring-up did not request a snapshot")
	}
	next, _ = m.Update(snapshot)
	m = next.(Model)

	if m.screen != ScreenHome {
		t.Fatalf("screen after recovery = %q, want home", m.screen)
	}
	if !m.ready {
		t.Fatal("the model is on Home without having loaded any state")
	}
	if len(m.ConnectionList()) != 2 {
		t.Fatalf("connections after recovery = %d, want 2", len(m.ConnectionList()))
	}
	// And the recovery error is gone, rather than lingering on Home.
	if m.err != nil {
		t.Errorf("the recovery error survived onto Home: %v", m.err)
	}
}

// TestTUIStartsWithoutASupervisor pins that construction does not require one.
//
// This is the architectural change item 1 asks for: the TUI must be able to
// start when the supervisor is unavailable, which means nothing in its
// construction may depend on reaching one.
func TestTUIStartsWithoutASupervisor(t *testing.T) {
	boot := &fakeBootstrapper{results: []error{errors.New("nothing is running")}}
	m := newModel(&fakeClient{snapshotErr: errors.New("no socket")}, boot)
	m.width = 100

	// The model renders before any bring-up has happened.
	if view := m.View().Content; view == "" {
		t.Fatal("the TUI rendered nothing before bring-up")
	}
	// And the dependency it holds is the narrow one, not the application layer.
	var _ Bootstrapper = boot
}

// TestRecoveryUsesTheStructuredError pins that a supervisor-described failure is
// presented as an intervention rather than reduced to one line.
func TestRecoveryUsesTheStructuredError(t *testing.T) {
	m := newModel(&fakeClient{}, &fakeBootstrapper{})
	m.width = 100
	m.err = &UserFacingError{
		Summary:     "Portico's supervisor is not running",
		Explanation: "It was stopped and nothing has started it again.",
		NextActions: []string{"Start it with portico supervisor run"},
		Technical:   "dial unix /run/portico.sock: connect: no such file",
		Retryable:   true,
	}
	m.screen = ScreenRecovery

	view := m.View().Content
	for _, want := range []string{
		"Portico's supervisor is not running",
		"It was stopped and nothing has started it again.",
		"Start it with portico supervisor run",
		"dial unix /run/portico.sock",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("recovery discards %q from the structured error:\n%s", want, view)
		}
	}
}

// Ensure the fake satisfies the interface the TUI actually depends on.
var _ Bootstrapper = (*fakeBootstrapper)(nil)

// Ensure the fake client still satisfies the client interface, so this file
// fails to compile if the two drift.
var _ SupervisorClient = (*fakeClient)(nil)

var _ = ipc.SnapshotDTO{}
