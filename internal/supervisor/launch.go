package supervisor

import (
	"os"
	"strings"
)

// Launch modes.
const (
	// LaunchManual starts nothing automatically. Connections are opened one at
	// a time, deliberately.
	LaunchManual = "manual"
	// LaunchAuto honours each connection's own AutoStart setting.
	LaunchAuto = "auto"
)

// launchModeEnv overrides the launch mode without editing anything.
const launchModeEnv = "PORTICO_LAUNCH_MODE"

// launchMode reports whether the supervisor arms connections at startup.
//
// It is a single gate in front of the per-connection AutoStart flag rather than
// a replacement for it: manual means nothing starts by itself whatever the
// connections say, and auto honours each connection's own choice. That gives one
// switch for "don't touch anything yet" without losing per-connection control.
//
// The default is auto so an existing setup keeps behaving as it did.
func (s *Supervisor) launchMode() string {
	if mode := strings.ToLower(strings.TrimSpace(os.Getenv(launchModeEnv))); mode != "" {
		if mode == LaunchManual {
			return LaunchManual
		}
		return LaunchAuto
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.launch == LaunchManual {
		return LaunchManual
	}
	return LaunchAuto
}

// SetLaunchMode changes the gate at runtime, so it can be toggled from the UI
// rather than requiring a config file to be edited.
func (s *Supervisor) SetLaunchMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.EqualFold(mode, LaunchManual) {
		s.launch = LaunchManual
		return
	}
	s.launch = LaunchAuto
}
