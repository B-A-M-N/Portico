package tui

import "context"

// Bootstrapper is the narrow dependency the TUI needs to bring up the
// supervisor. It is the boundary that lets the TUI start even when no
// supervisor is running, show a meaningful recovery screen, and retry the
// full sequence rather than merely retrying a snapshot fetch against a dead
// process.
//
// The production implementation is *app.Launcher. Tests substitute a fake
// so recovery paths can be exercised without a real socket.
type Bootstrapper interface {
	// TryBootstrap performs the complete bring-up sequence: ensure the
	// supervisor is running, verify it is healthy, and confirm the socket
	// is writable. It returns a nil error only when a subsequent
	// GetSnapshot would succeed.
	TryBootstrap(ctx context.Context) error

	// SupervisorLogPath is where the supervisor writes its structured log.
	// Shown on the recovery screen so a user can find evidence without
	// knowing the XDG layout.
	SupervisorLogPath() string

	// SocketPath is the Unix socket the supervisor listens on. Shown on
	// the recovery screen so a user can check permissions and staleness.
	SocketPath() string
}
