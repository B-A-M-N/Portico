package origin

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/B-A-M-N/portico/internal/core"
)

// Type enumerates supported origin backends.
type Type string

const (
	TypeLocalHTTP          Type = "local:http"
	TypeLocalCommand       Type = "local:command"
	TypeDockerContainer    Type = "docker:container"
	TypeDockerCompose      Type = "docker:compose-service"
	TypeBuiltinStatic      Type = "builtin:static"
	TypeBuiltinFileBrowser Type = "builtin:file-browser"
)

// ValidTypes returns all supported origin type strings.
func ValidTypes() []Type {
	return []Type{
		TypeLocalHTTP,
		TypeLocalCommand,
		TypeDockerContainer,
		TypeDockerCompose,
		TypeBuiltinStatic,
		TypeBuiltinFileBrowser,
	}
}

// Config holds origin-specific configuration derived from CLI flags.
type Config struct {
	Type Type

	// local:http
	URL       string // Upstream URL (e.g., http://127.0.0.1:3000)
	HealthURL string // Override health check URL

	// local:command
	Command    string
	Args       []string
	Dir        string
	Env        map[string]string
	Port       int
	HealthPath string
	Shell      bool
	KillSignal string

	// builtin:static / builtin:file-browser
	Path         string // Root directory
	Index        string // Index file (static)
	SPA          bool   // SPA mode (static)
	CacheControl string // Cache-Control header (static)
	ListenPort   int    // Optional fixed loopback port for owned HTTP origins

	// builtin:file-browser
	AllowUpload bool
	AllowDelete bool
	AllowRename bool
	ShowHidden  bool
	Download    bool
	ReadOnly    bool

	// docker:container
	Image         string
	ContainerPort int
	PublishPort   string // e.g., "127.0.0.1:38080"
	Entrypoint    string
	Network       string
	Remove        bool
	DockerBin     string

	// docker:compose-service
	ComposeFile string
	ServiceName string
	ProjectName string
	Build       bool
	UpDetached  bool

	// Shared
	WaitForReady string // Duration string for startup timeout
}

// Origin represents a runnable local application backend.
type Origin interface {
	// Type returns the origin type identifier.
	Type() Type

	// Start launches the origin and returns the loopback URL (http://host:port)
	// it is listening on. Must bind to 127.0.0.1 only.
	// Blocks until the origin is ready to accept connections. The origin's
	// lifetime is owned by the manager after Start returns successfully;
	// the supplied context is only used for startup cancellation.
	Start(ctx context.Context) (loopbackURL string, err error)

	// Stop gracefully shuts down the origin and its process group.
	Stop(ctx context.Context) error

	// Logs returns a reader that streams the origin's combined output.
	// Returns nil if the origin has no log stream (e.g., local:http proxy).
	Logs() io.ReadCloser

	// Healthy returns nil if the origin is responsive.
	Healthy(ctx context.Context) error
}

// ProcessBackedOrigin is an optional interface implemented by origins that
// own a local process. ProcessBackedOrigin returns the full process identity
// (pid, start time, executable, command hash) and its process group ID.
// Non-process origins (static, file browser, existing service) do not
// implement this interface.
type ProcessBackedOrigin interface {
	Origin
	Identity() (core.ProcessIdentity, bool)
	ProcessGroupID() int
}

// OriginLifecycleState describes the current stage of an owned origin's life.
type OriginLifecycleState string

const (
	// originStatePlanned means the manager has been asked to start it but it
	// has not been created yet.
	originStatePlanned OriginLifecycleState = "planned"
	// originStateStarting means Start() is in progress.
	originStateStarting OriginLifecycleState = "starting"
	// originStateRunning means the origin is healthy and accepting traffic.
	originStateRunning OriginLifecycleState = "running"
	// originStateStopping means Stop() is in progress.
	originStateStopping OriginLifecycleState = "stopping"
	// originStateStopped means the origin was stopped cleanly.
	originStateStopped OriginLifecycleState = "stopped"
	// originStateCrashed means the origin exited unexpectedly.
	originStateCrashed OriginLifecycleState = "crashed"
	// originStateUnknown means the origin state cannot be determined.
	originStateUnknown OriginLifecycleState = "unknown"
)

// managedOrigin wraps an Origin with a per-origin lifecycle mutex, generation
// counter, and status tracking. Only methods on managedOrigin may transition
// state; external code reads state atomically through Accessor methods.
type managedOrigin struct {
	mu       sync.Mutex
	generation uint64
	state    OriginLifecycleState
	origin   Origin
	url      string
	lastErr  error
}

// newManagedOrigin creates a new managed origin entry.
func newManagedOrigin(orig Origin) *managedOrigin {
	return &managedOrigin{state: originStatePlanned, origin: orig}
}

// WithOrigin executes fn with the current origin (under lock) and returns the result.
func (m *managedOrigin) WithOrigin(fn func(Origin) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fn(m.origin)
}

// State returns the current lifecycle state.
func (m *managedOrigin) State() OriginLifecycleState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// StateAndURL returns the current state and URL atomically.
func (m *managedOrigin) StateAndURL() (OriginLifecycleState, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, m.url
}

// SetState transitions to a new state atomically. Returns the previous state.
func (m *managedOrigin) SetState(newState OriginLifecycleState) OriginLifecycleState {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.state
	m.state = newState
	return old
}

// URL returns the resolved origin URL.
func (m *managedOrigin) URL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.url
}

// SetURL sets the origin URL under lock.
func (m *managedOrigin) SetURL(u string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.url = u
}

// Origin returns the origin object (non-mutating).
func (m *managedOrigin) Origin() Origin {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.origin
}

// Generation returns the current generation counter.
func (m *managedOrigin) Generation() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}

// IncrementGeneration atomically bumps the generation counter and returns the new value.
func (m *managedOrigin) IncrementGeneration() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generation++
	return m.generation
}

// SetOrigin sets the origin under lock.
func (m *managedOrigin) SetOrigin(o Origin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.origin = o
}

// SetLastError records a last error under lock.
func (m *managedOrigin) SetLastError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastErr = err
}

// LastError returns the last error.
func (m *managedOrigin) LastError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// stopUnderlying delegates Stop to the underlying Origin (under lock).
func (m *managedOrigin) stopUnderlying(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.origin == nil {
		return nil
	}
	return m.origin.Stop(ctx)
}

// healthy delegates Healthy to the underlying Origin (under lock).
func (m *managedOrigin) healthy(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.origin == nil {
		return fmt.Errorf("no origin started yet")
	}
	return m.origin.Healthy(ctx)
}

// originImpl returns the underlying Origin (under lock) for type assertions.
// External callers should prefer the typed accessor methods on Manager.
func (m *managedOrigin) originImpl() Origin {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.origin
}
