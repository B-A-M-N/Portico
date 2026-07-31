package supervisor

import (
	"fmt"
	"os"
	"strings"
)

// providerCredentialRef derives the credential reference for one account.
//
// Every writer must derive it here. Two writers building the reference
// differently for the same account produced two encrypted credential rows, and
// because provider_credentials is keyed on the reference while deletion removes
// only the one the account currently points at, the other survived the account
// being deleted — a secret left in the database after the user was told it was
// gone, reachable by nothing.
func providerCredentialRef(providerID, accountID string) string {
	return fmt.Sprintf("%s:%s:api-token", providerID, accountID)
}

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
	if mode, pinned := launchModeOverride(); pinned {
		return mode
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.launch == LaunchManual {
		return LaunchManual
	}
	return LaunchAuto
}

// launchModeOverride reports the mode forced by the environment, if any.
//
// This is separated from launchMode so callers can tell "the mode is auto"
// apart from "the mode is auto and nothing you do here will change it". A
// toggle that silently fails to take effect reads as a bug; one that says it is
// pinned, and by what, is actionable.
func launchModeOverride() (string, bool) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(launchModeEnv)))
	if mode == "" {
		return "", false
	}
	if mode == LaunchManual {
		return LaunchManual, true
	}
	return LaunchAuto, true
}

// ValidLaunchMode reports whether a mode name is one Portico accepts.
//
// Callers at a boundary must reject an unrecognised mode rather than letting
// SetLaunchMode's fallback coerce it: a typo silently arming every connection
// is the opposite of what this gate is for.
func ValidLaunchMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case LaunchManual, LaunchAuto:
		return true
	}
	return false
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
