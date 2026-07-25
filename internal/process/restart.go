package process

import "time"

// RestartBackoff computes restart delays per the SPEC §11.3 schedule.
//
// Attempt 1: 1 second
// Attempt 2: 2 seconds
// Attempt 3: 5 seconds
// Attempt 4: 10 seconds
// Attempt 5: 30 seconds
// Then: mark unstable and require repair
// Window: 10 minutes
//
// The zero value uses the SPEC defaults. Schedule, Window, and MaxAttempts
// may be overridden (e.g. for tests) before the first NextDelay call.
type RestartBackoff struct {
	// Schedule overrides the default delay schedule. The last entry is
	// reused when MaxAttempts exceeds the schedule length.
	Schedule []time.Duration
	// Window overrides the default 10-minute attempt window.
	Window time.Duration
	// MaxAttempts overrides the default limit of 5 attempts per window.
	MaxAttempts int

	attempts    int
	windowStart time.Time
}

const (
	restartWindow      = 10 * time.Minute
	maxRestartAttempts = 5
)

// defaultRestartSchedule is the SPEC §11.3 backoff schedule.
var defaultRestartSchedule = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

// NextDelay returns the delay before the next restart attempt.
// Returns 0 and false if the maximum attempts have been exceeded within the window.
func (rb *RestartBackoff) NextDelay(now time.Time) (time.Duration, bool) {
	schedule := rb.Schedule
	if len(schedule) == 0 {
		schedule = defaultRestartSchedule
	}
	window := rb.Window
	if window <= 0 {
		window = restartWindow
	}
	maxAttempts := rb.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = maxRestartAttempts
	}

	// Reset window if enough time has passed.
	if now.After(rb.windowStart.Add(window)) {
		rb.attempts = 0
		rb.windowStart = now
	}

	if rb.windowStart.IsZero() {
		rb.windowStart = now
	}

	rb.attempts++
	if rb.attempts > maxAttempts {
		// Mark unstable — no more automatic restarts.
		return 0, false
	}
	idx := rb.attempts - 1
	if idx >= len(schedule) {
		idx = len(schedule) - 1
	}
	return schedule[idx], true
}

// Attempts returns the current attempt count.
func (rb *RestartBackoff) Attempts() int {
	return rb.attempts
}

// Reset resets the backoff state.
func (rb *RestartBackoff) Reset() {
	rb.attempts = 0
	rb.windowStart = time.Time{}
}
