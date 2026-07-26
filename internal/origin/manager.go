package origin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strconv"
	"sync"

	"github.com/B-A-M-N/portico/internal/core"
)

// Manager owns local origins for the lifetime of a supervisor.  It keeps the
// runnable process/server separate from ConnectionProfile, which is desired
// state and must never contain runtime process data.
type Manager struct {
	mu     sync.Mutex
	active map[core.ConnectionID]Origin
}

func NewManager() *Manager {
	return &Manager{active: make(map[core.ConnectionID]Origin)}
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
func (m *Manager) Start(ctx context.Context, connectionID core.ConnectionID, source core.SourceSpec, expectedURL string) error {
	if source.Kind != core.SourceDirectory && source.Kind != core.SourceCommand &&
		(source.Kind != core.SourceMCP || source.MCP == nil || source.MCP.Command == nil) {
		return nil
	}

	m.mu.Lock()
	if running := m.active[connectionID]; running != nil {
		if err := running.Healthy(ctx); err == nil {
			m.mu.Unlock()
			return nil
		}
		delete(m.active, connectionID)
		m.mu.Unlock()
		_ = running.Stop(ctx)
		m.mu.Lock()
	}

	origin, err := newOwnedOrigin(connectionID, source)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	// Publish the origin before starting it so a concurrent close or shutdown
	// can always find and stop it. Start verifies this registration afterwards
	// before reporting success to the connector step.
	m.active[connectionID] = origin
	m.mu.Unlock()

	actualURL, err := origin.Start(ctx)
	if err != nil {
		m.remove(connectionID, origin)
		return err
	}
	if actualURL != expectedURL {
		_ = origin.Stop(context.Background())
		m.remove(connectionID, origin)
		return fmt.Errorf("owned origin bound %s, but plan requires %s", actualURL, expectedURL)
	}

	m.mu.Lock()
	stillActive := m.active[connectionID] == origin
	m.mu.Unlock()
	if !stillActive {
		_ = origin.Stop(context.Background())
		return fmt.Errorf("owned origin was stopped while starting")
	}
	return nil
}

func (m *Manager) remove(connectionID core.ConnectionID, expected Origin) {
	m.mu.Lock()
	if m.active[connectionID] == expected {
		delete(m.active, connectionID)
	}
	m.mu.Unlock()
}

// Stop stops an owned origin. It is intentionally idempotent so close,
// delete, compensation, and supervisor shutdown can all use it safely.
func (m *Manager) Stop(ctx context.Context, connectionID core.ConnectionID) error {
	m.mu.Lock()
	origin := m.active[connectionID]
	delete(m.active, connectionID)
	m.mu.Unlock()
	if origin == nil {
		return nil
	}
	return origin.Stop(ctx)
}

// StopAll is used by supervisor shutdown to ensure owned sources never leak.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	active := m.active
	m.active = make(map[core.ConnectionID]Origin)
	m.mu.Unlock()
	var firstErr error
	for _, origin := range active {
		if err := origin.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
