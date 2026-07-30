package process

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// ErrIdentityMismatch is returned when a process's observed identity
// (start time, executable path, command hash) does not match the expected
// identity, or when the process cannot be found/inspected at all. Callers
// must never signal the PID after receiving this error; typical handling
// is to set the connector status to unknown and create a finding.
var ErrIdentityMismatch = errors.New("process identity mismatch")

// ErrIdentityPartial is returned when an identity is missing one or more
// required fields. Partial identities are never trusted and never adopted;
// callers must classify the process as unknown.
var ErrIdentityPartial = errors.New("process identity partial")

// Manager manages connector processes with strict identity validation.
// Each connection is owned by a per-connection actor goroutine that owns
// the exec.Cmd, log pipes, restart timer, and exit status. The manager map
// only holds actor handles; the manager mutex is never held while sleeping,
// waiting, or signaling.
type Manager struct {
	mu        sync.Mutex
	actors    map[core.ConnectionID]*actor
	eventMu   sync.RWMutex
	eventSink ProcessEventSink

	// Restart/log policy. Package-private and injectable for tests.
	restartSchedule []time.Duration // nil => SPEC schedule 1s,2s,5s,10s,30s
	restartWindow   time.Duration   // 0 => 10 minutes
	restartMax      int             // 0 => 5 attempts per window
	pollInterval    time.Duration   // adopted-process liveness poll interval
	logMaxSize      int64
	logMaxFiles     int
}

// ProcessEventType is a normalized lifecycle transition emitted by a process
// actor. The supervisor persists these transitions into runtime state and its
// durable client event stream.
type ProcessEventType string

const (
	ProcessEventStarted   ProcessEventType = "started"
	ProcessEventExited    ProcessEventType = "exited"
	ProcessEventRestarted ProcessEventType = "restarted"
	ProcessEventStopped   ProcessEventType = "stopped"
	ProcessEventUnstable  ProcessEventType = "unstable"
)

type ProcessEvent struct {
	ConnectionID core.ConnectionID
	Type         ProcessEventType
	Identity     core.ProcessIdentity
	Status       ProcessStatus
	Restarts     int
	Error        string
	Timestamp    time.Time
}

type ProcessEventSink func(ProcessEvent)

// ManagedProcess is a tracked connector process. Values returned by the
// manager are point-in-time snapshots; they are safe to read without locks.
type ManagedProcess struct {
	ConnectionID core.ConnectionID
	Identity     core.ProcessIdentity
	Cmd          *exec.Cmd
	Spec         core.ProcessSpec
	StartedAt    time.Time
	Status       ProcessStatus
	ExitCh       chan error

	// Adopted is true when the process was started externally and adopted
	// via Adopt rather than launched by this manager.
	Adopted bool
	// Restarts counts automatic restart attempts performed by the manager.
	Restarts int
	// LastExitError holds the error from the most recent unexpected exit.
	LastExitError string
	// LastExitAt is when the process most recently exited.
	LastExitAt time.Time
}

// IsUnstable reports whether the process exhausted its restart budget
// (5 attempts within a 10-minute window per SPEC §11.3).
func (mp *ManagedProcess) IsUnstable() bool {
	return mp.Status == ProcessStatusUnstable
}

// ProcessStatus describes the status of a managed process.
type ProcessStatus string

const (
	ProcessStatusRunning  ProcessStatus = "running"
	ProcessStatusStopped  ProcessStatus = "stopped"
	ProcessStatusCrashed  ProcessStatus = "crashed"
	ProcessStatusStarting ProcessStatus = "starting"
	// ProcessStatusUnstable means the restart budget was exhausted
	// (max attempts within the window) and automatic restarts stopped.
	// core has no unstable ConnectorStatus, so it is tracked here.
	ProcessStatusUnstable ProcessStatus = "unstable"
)

// ProcessConfig is the configuration for starting a managed process.
type ProcessConfig struct {
	ConnectionID core.ConnectionID
	Spec         core.ProcessSpec
}

// NewManager creates a new process manager.
func NewManager() *Manager {
	return &Manager{
		actors:       make(map[core.ConnectionID]*actor),
		pollInterval: time.Second,
		logMaxSize:   maxLogSize,
		logMaxFiles:  maxLogFiles,
	}
}

// SetEventSink registers the supervisor's lifecycle listener. It may be set
// before or after actors exist; callbacks are made without a manager lock.
func (pm *Manager) SetEventSink(sink ProcessEventSink) {
	pm.eventMu.Lock()
	pm.eventSink = sink
	pm.eventMu.Unlock()
}

func (pm *Manager) emit(event ProcessEvent) {
	pm.eventMu.RLock()
	sink := pm.eventSink
	pm.eventMu.RUnlock()
	if sink != nil {
		sink(event)
	}
}

// newActor builds an actor carrying the manager's policy configuration.
func (pm *Manager) newActor(connID core.ConnectionID, spec core.ProcessSpec, adopted bool) *actor {
	return &actor{
		connID:       connID,
		spec:         spec,
		adopted:      adopted,
		schedule:     pm.restartSchedule,
		window:       pm.restartWindow,
		maxAttempts:  pm.restartMax,
		pollInterval: pm.pollInterval,
		logMaxSize:   pm.logMaxSize,
		logMaxFiles:  pm.logMaxFiles,
		emit:         pm.emit,
		stopCh:       make(chan stopReq),
		started:      make(chan struct{}),
		done:         make(chan struct{}),
		rec: ManagedProcess{
			ConnectionID: connID,
			Spec:         spec,
			Status:       ProcessStatusStarting,
			ExitCh:       make(chan error, 1),
			Adopted:      adopted,
		},
	}
}

// Start starts a connector process.
// The provided context is used only for the initial startup validation.
// The process lifetime is managed separately and is NOT tied to this context.
func (pm *Manager) Start(ctx context.Context, cfg ProcessConfig) (*ManagedProcess, error) {
	_ = ctx // process lifetime is intentionally decoupled from the caller context
	for {
		pm.mu.Lock()
		if a, ok := pm.actors[cfg.ConnectionID]; ok {
			pm.mu.Unlock()
			<-a.started
			select {
			case <-a.done:
				// Actor terminated (stopped, crashed without restart, or
				// unstable): replace it with a fresh one.
				pm.removeActor(cfg.ConnectionID, a)
				continue
			default:
				return a.snapshot(), nil
			}
		}
		a := pm.newActor(cfg.ConnectionID, cfg.Spec, false)
		pm.actors[cfg.ConnectionID] = a
		pm.mu.Unlock()

		// Launch outside the manager lock.
		if err := a.launch(); err != nil {
			a.startErr = err
			close(a.started)
			close(a.done)
			pm.removeActor(cfg.ConnectionID, a)
			return nil, err
		}
		close(a.started)
		go a.run()
		return a.snapshot(), nil
	}
}

// Adopt takes ownership of an externally started connector process after
// verifying ALL available identity fields (start time, executable path,
// command hash) against the live process via /proc. On any mismatch — or if
// the process cannot be inspected — the PID is never signaled, the process is
// not added, and an error wrapping ErrIdentityMismatch is returned so callers
// can set the connector status to unknown and create a finding.
func (pm *Manager) Adopt(connID core.ConnectionID, identity core.ProcessIdentity, spec core.ProcessSpec) error {
	if err := verifyIdentityFields(identity); err != nil {
		return err
	}

	pm.mu.Lock()
	if a, ok := pm.actors[connID]; ok {
		select {
		case <-a.done:
			delete(pm.actors, connID)
		default:
			pm.mu.Unlock()
			return fmt.Errorf("connection %s already has a managed process", connID)
		}
	}
	a := pm.newActor(connID, spec, true)
	a.rec.Identity = identity
	a.rec.Status = ProcessStatusRunning
	a.rec.StartedAt = time.Now().UTC()
	pm.actors[connID] = a
	pm.mu.Unlock()

	close(a.started)
	go a.run()

	slog.Info("connector process adopted", "connection", connID, "pid", identity.PID)
	return nil
}

// Stop stops a connector process by sending SIGTERM to its process group,
// then SIGKILL after the timeout. Identity is verified before signaling.
func (pm *Manager) Stop(connID core.ConnectionID, timeout time.Duration) error {
	pm.mu.Lock()
	a, ok := pm.actors[connID]
	pm.mu.Unlock()

	if !ok {
		return fmt.Errorf("no process for connection: %s", connID)
	}

	// Request the actor to terminate. No manager lock held while waiting.
	err := a.requestStop(timeout)
	if err != nil {
		return err
	}
	pm.removeActor(connID, a)
	return nil
}

// removeActor deletes the actor from the map if it is still the registered one.
func (pm *Manager) removeActor(connID core.ConnectionID, a *actor) {
	pm.mu.Lock()
	if pm.actors[connID] == a {
		delete(pm.actors, connID)
	}
	pm.mu.Unlock()
}

// GetProcess returns a snapshot of the managed process for a connection.
func (pm *Manager) GetProcess(connID core.ConnectionID) (*ManagedProcess, bool) {
	pm.mu.Lock()
	a, ok := pm.actors[connID]
	pm.mu.Unlock()
	if !ok {
		return nil, false
	}
	return a.snapshot(), true
}

// Observe returns a ConnectorHandle for the managed process,
// conforming to core.ConnectorProcessService.
func (pm *Manager) Observe(connID core.ConnectionID) (core.ConnectorHandle, bool) {
	mp, ok := pm.GetProcess(connID)
	if !ok {
		return core.ConnectorHandle{}, false
	}
	return core.ConnectorHandle{
		ConnectionID: mp.ConnectionID,
		Identity:     mp.Identity,
		PID:          mp.Identity.PID,
	}, true
}

// StartConnector starts a connector process and returns a
// core.ConnectorHandle, conforming to core.ConnectorProcessService.
func (pm *Manager) StartConnector(ctx context.Context, cfg ProcessConfig) (core.ConnectorHandle, error) {
	mp, err := pm.Start(ctx, cfg)
	if err != nil {
		return core.ConnectorHandle{}, err
	}
	pid := 0
	if mp.Cmd != nil && mp.Cmd.Process != nil {
		pid = mp.Cmd.Process.Pid
	}
	return core.ConnectorHandle{
		ConnectionID: mp.ConnectionID,
		Identity:     mp.Identity,
		PID:          pid,
	}, nil
}

// ListProcesses returns snapshots of all managed processes.
func (pm *Manager) ListProcesses() []*ManagedProcess {
	pm.mu.Lock()
	actors := make([]*actor, 0, len(pm.actors))
	for _, a := range pm.actors {
		actors = append(actors, a)
	}
	pm.mu.Unlock()

	result := make([]*ManagedProcess, 0, len(actors))
	for _, a := range actors {
		result = append(result, a.snapshot())
	}
	return result
}

// Cleanup stops all actors and removes all process records. Actors verify
// identity before signaling and use process-group signals. The manager
// mutex is never held while waiting for actors to exit.
func (pm *Manager) Cleanup() {
	pm.mu.Lock()
	actors := make([]*actor, 0, len(pm.actors))
	for _, a := range pm.actors {
		actors = append(actors, a)
	}
	pm.actors = make(map[core.ConnectionID]*actor)
	pm.mu.Unlock()

	var wg sync.WaitGroup
	for _, a := range actors {
		wg.Add(1)
		go func(a *actor) {
			defer wg.Done()
			if err := a.requestStop(2 * time.Second); err != nil {
				slog.Warn("cleanup stop failed", "connection", a.connID, "err", err)
			}
		}(a)
	}
	wg.Wait()
}

// minimalEnv returns a controlled default environment for connector children.
// This prevents inheriting the supervisor's full environment which may
// contain provider credentials, keyring variables, or unrelated secrets.
// minimalEnv is the filtered environment a connector process receives.
//
// The full environment is deliberately not inherited: it routinely carries
// credentials for unrelated services, and a connector has no business seeing
// them. But HOME must be the real one. It was hardcoded to /root, so under any
// non-root user — the normal case — a client that reads its own configuration
// file looked in a directory it could not read and failed to start. That is how
// the ngrok agent came to exit immediately with no usable explanation.
func minimalEnv() []string {
	home := os.Getenv("HOME")
	if home == "" {
		if resolved, err := os.UserHomeDir(); err == nil {
			home = resolved
		}
	}
	if home == "" {
		home = "/tmp"
	}

	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	return []string{
		"PATH=" + path,
		"HOME=" + home,
		"LANG=C.UTF-8",
		"TZ=UTC",
	}
}
