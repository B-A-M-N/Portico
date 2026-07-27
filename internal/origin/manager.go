package origin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"sync"

	"github.com/B-A-M-N/portico/internal/core"
)

// Manager owns local origins for the lifetime of a supervisor.  It keeps the
// runnable process/server separate from ConnectionProfile, which is desired
// state and must never contain runtime process data.
type Manager struct {
	mu      sync.Mutex
	active  map[core.ConnectionID]*managedOrigin
	cleared bool // set to true during StopAll to prevent new starts
}

func NewManager() *Manager {
	return &Manager{
		active: make(map[core.ConnectionID]*managedOrigin),
	}
}

// Plan resolves the stable loopback URL that a provider may place in an
// immutable plan. It performs no process or network mutation.
func (m *Manager) Plan(connectionID core.ConnectionID, source core.SourceSpec) (*core.ResolvedOrigin, error) {
	switch source.Kind {
	case core.SourceDirectory:
		if source.Directory == nil {
			return nil, fmt.Errorf("directory source is missing its configuration")
		}
		// Constructors validate the root eagerly (without opening a listener),
		// so a preview cannot promise an openable directory that does not exist.
		cfg := Config{Path: source.Directory.Path}
		if source.Directory.Mode == core.DirectoryModeWrites {
			if _, err := NewBuiltinFileBrowser(cfg); err != nil {
				return nil, err
			}
		} else if _, err := NewBuiltinStatic(cfg); err != nil {
			return nil, err
		}
		return ownedHTTPOrigin(directoryPort(connectionID)), nil
	case core.SourceCommand:
		if source.Command == nil {
			return nil, fmt.Errorf("command source is missing its configuration")
		}
		return commandOrigin(source.Command)
	case core.SourceMCP:
		if source.MCP == nil {
			return nil, fmt.Errorf("MCP source is missing its configuration")
		}
		if source.MCP.Command != nil {
			return commandOrigin(source.MCP.Command)
		}
		u, err := url.ParseRequestURI(source.MCP.Endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("MCP endpoint must be an absolute HTTP URL")
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("MCP endpoint scheme %q is not supported", u.Scheme)
		}
		if u.User != nil {
			return nil, fmt.Errorf("MCP endpoint must not embed credentials")
		}
		return &core.ResolvedOrigin{URL: u.String(), Protocol: core.ProtocolHTTP, Owned: false}, nil
	default:
		return nil, fmt.Errorf("origin manager cannot plan source kind %q", source.Kind)
	}
}

func ownedHTTPOrigin(port int) *core.ResolvedOrigin {
	return &core.ResolvedOrigin{
		URL:      "http://127.0.0.1:" + strconv.Itoa(port),
		Protocol: core.ProtocolHTTP,
		Owned:    true,
	}
}

func commandOrigin(command *core.CommandSpec) (*core.ResolvedOrigin, error) {
	if command.Port < 1 || command.Port > 65535 {
		return nil, fmt.Errorf("command source requires a port from 1 through 65535")
	}
	if command.Protocol != "" && command.Protocol != core.ProtocolHTTP {
		return nil, fmt.Errorf("command source protocol %q is not supported; Portico-managed commands currently require HTTP", command.Protocol)
	}
	return ownedHTTPOrigin(command.Port), nil
}

// Start creates the source represented by the profile and verifies it bound
// to exactly the URL recorded in the plan. Calling Start repeatedly is safe.
// Start and Stop for the same connection ID are serialized by a per-origin
// lock. The manager-level mutex is held only for map access.
func (m *Manager) Start(ctx context.Context, connectionID core.ConnectionID, source core.SourceSpec, expectedURL string) error {
	if source.Kind != core.SourceDirectory && source.Kind != core.SourceCommand &&
		(source.Kind != core.SourceMCP || source.MCP == nil || source.MCP.Command == nil) {
		return nil
	}

	m.mu.Lock()
	if m.cleared {
		m.mu.Unlock()
		return fmt.Errorf("origin manager is stopped")
	}

	// If a managed origin already exists for this connection, stop it
	// before starting a new one. We snapshot the reference under the
	// manager lock so stopping outside the lock operates on the exact
	// entry we found, not a newer one that may have been inserted.
	old := m.active[connectionID]
	if old != nil {
		m.mu.Unlock()
		_ = old.stopUnderlying(ctx)
		m.mu.Lock()
	}

	// Create the underlying origin (outside per-origin lock).
	orig, err := newOwnedOrigin(connectionID, source)
	if err != nil {
		m.mu.Unlock()
		return err
	}

	managed := newManagedOrigin(orig)
	m.active[connectionID] = managed
	m.mu.Unlock()

	// Transition to starting under the per-origin lock.
	managed.SetState(originStateStarting)

	actualURL, err := orig.Start(ctx)

	if err != nil {
		managed.SetState(originStateCrashed)
		managed.SetLastError(err)
		m.mu.Lock()
		delete(m.active, connectionID)
		m.mu.Unlock()
		return err
	}

	if actualURL != expectedURL {
		_ = orig.Stop(context.Background())
		managed.SetState(originStateStopped)
		managed.SetLastError(fmt.Errorf("bounded %s, expected %s", actualURL, expectedURL))
		m.mu.Lock()
		delete(m.active, connectionID)
		m.mu.Unlock()
		return fmt.Errorf("owned origin bound %s, but plan requires %s", actualURL, expectedURL)
	}

	// Success: record URL and set running state under the per-origin lock.
	managed.SetURL(actualURL)
	managed.SetState(originStateRunning)
	return nil
}

// Stop stops an owned origin. It is intentionally idempotent so close,
// delete, compensation, and supervisor shutdown can all use it safely.
// On failure the entry is retained (with URL, state, and error) so callers
// can retry or perform diagnostics.
func (m *Manager) Stop(ctx context.Context, connectionID core.ConnectionID) error {
	m.mu.Lock()
	managed, ok := m.active[connectionID]
	m.mu.Unlock()
	if !ok {
		return nil
	}

	// Prevent stopping while a new start is in progress.
	prev := managed.SetState(originStateStopping)
	if prev == originStateStopping || prev == originStateStopped {
		return nil // already stopping or stopped — idempotent
	}

	orig := managed.Origin()
	if orig == nil {
		managed.SetState(originStateStopped)
		m.mu.Lock()
		delete(m.active, connectionID)
		m.mu.Unlock()
		return nil
	}

	err := orig.Stop(ctx)

	if err != nil {
		managed.SetLastError(err)
		// Leave as-is for diagnostics.
		return err
	}

	managed.SetState(originStateStopped)
	m.mu.Lock()
	delete(m.active, connectionID)
	m.mu.Unlock()
	return nil
}

// StopAll is used by supervisor shutdown to ensure owned sources never leak.
// It honours the caller's context deadline, snapshots entries under the lock,
// then stops them outside the lock. Failed entries are retained so a retry
// or forced cleanup can still find them. An aggregated error is returned.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	snapshot := make(map[core.ConnectionID]*managedOrigin, len(m.active))
	for connID, mo := range m.active {
		snapshot[connID] = mo
	}
	// Clear active so no new starts succeed; cleared is already true
	// from where StopAll was called, but be explicit.
	for connID := range m.active {
		delete(m.active, connID)
	}
	m.mu.Unlock()

	var firstErr error
	for connID, mo := range snapshot {
		if err := mo.stopUnderlying(ctx); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			slog.Warn("origin shutdown failed", "connection", connID, "err", err)
		}
		// If stop failed, retain the managed entry so diagnostics can
		// still find it. Successful stops have already set state to
		// stopped and removed the entry from active.
		if firstErr != nil && mo.State() != originStateStopped {
			m.mu.Lock()
			if _, exists := m.active[connID]; !exists {
				m.active[connID] = mo
			}
			m.mu.Unlock()
		}
	}

	return firstErr
}

// Observe returns the current OriginRuntime for a connection. It returns
// (zero, false) when the connection does not own a local origin or no
// origin has been started yet.
func (m *Manager) Observe(connectionID core.ConnectionID) (core.OriginRuntime, bool) {
	m.mu.Lock()
	mo, ok := m.active[connectionID]
	m.mu.Unlock()
	if !ok {
		return core.OriginRuntime{}, false
	}
	_, url := mo.StateAndURL()
	state := mo.State()
	runtime := core.OriginRuntime{
		Ownership: core.OriginOwnershipOwned,
		URL:       url,
	}
	// Crashed or unknown state from lifecycle tracking.
	if state == originStateCrashed || state == originStateUnknown {
		runtime.Status = core.OriginStatusCrashed
		if err := mo.LastError(); err != nil {
			runtime.LastError = err.Error()
		}
		return runtime, true
	}
	// Check actual health if we think we're running.
	if err := mo.healthy(context.Background()); err != nil {
		runtime.Status = core.OriginStatusCrashed
		runtime.LastError = err.Error()
		return runtime, true
	}
	runtime.Status = core.OriginStatusRunning
	// Attach process identity if applicable.
	pbo, ok := mo.originImpl().(ProcessBackedOrigin)
	if !ok {
		return runtime, true
	}
	id, hasID := pbo.Identity()
	if !hasID {
		return runtime, true
	}
	runtime.PID = id.PID
	runtime.StartTime = id.StartTime
	runtime.Executable = id.ExecutablePath
	runtime.CommandHash = id.CommandHash
	return runtime, true
}

func newOwnedOrigin(connectionID core.ConnectionID, source core.SourceSpec) (Origin, error) {
	switch source.Kind {
	case core.SourceDirectory:
		cfg := Config{Path: source.Directory.Path, ListenPort: directoryPort(connectionID)}
		if source.Directory.Mode == core.DirectoryModeWrites {
			cfg.Type = TypeBuiltinFileBrowser
			cfg.AllowUpload = source.Directory.AllowUpload
			cfg.AllowDelete = source.Directory.AllowDelete
			cfg.Download = true
			return NewBuiltinFileBrowser(cfg)
		}
		cfg.Type = TypeBuiltinStatic
		cfg.SPA = source.Directory.SPAFallback
		return NewBuiltinStatic(cfg)
	case core.SourceCommand:
		return commandSourceOrigin(source.Command)
	case core.SourceMCP:
		return commandSourceOrigin(source.MCP.Command)
	default:
		return nil, fmt.Errorf("source kind %q does not own a local origin", source.Kind)
	}
}

func commandSourceOrigin(command *core.CommandSpec) (Origin, error) {
	if _, err := commandOrigin(command); err != nil {
		return nil, err
	}
	return NewLocalCommand(Config{
		Type:    TypeLocalCommand,
		Command: command.Executable,
		Args:    command.Args,
		Dir:     command.WorkingDir,
		Env:     command.Env,
		Port:    command.Port,
		Shell:   command.UseShell,
	})
}

// directoryPort is deterministic per connection so a previewed plan remains
// executable after restart. A collision fails before provider mutation and is
// reported clearly rather than silently routing to another local service.
func directoryPort(connectionID core.ConnectionID) int {
	digest := sha256.Sum256([]byte(connectionID))
	return 40000 + ((int(digest[0])<<8 | int(digest[1])) % 10000)
}
