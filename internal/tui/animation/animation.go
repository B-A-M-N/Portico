package animation

import "time"

// State represents animation state with demand-based scheduling.
// Idle state = 0 FPS. No permanent global ticker.
type State struct {
	RouteTransition bool
	TrafficUntil    time.Time
	Spinner         bool
	ToastUntil      time.Time
	lastFrame       time.Time
}

// Frame represents one animation frame.
type Frame struct {
	Progress float64 // 0.0 to 1.0 for transitions
}

// NewDemand creates demand that says we need animation.
type Demand struct {
	RouteTransition bool
	TrafficUntil    time.Time
	Spinner         bool
	ToastUntil      time.Time
}

// NextFrame returns the duration until the next frame should render, and whether one is needed.
func (s *State) NextFrame(now time.Time) (time.Duration, bool) {
	demand := s.demandAt(now)
	if !demand {
		return 0, false
	}

	if s.lastFrame.IsZero() {
		s.lastFrame = now
		return 0, true
	}

	// Determine target FPS from demand type
	fps := 0
	switch {
	case s.RouteTransition:
		fps = 20
	case now.Before(s.TrafficUntil):
		fps = 8
	case s.Spinner:
		fps = 10
	case now.Before(s.ToastUntil):
		fps = 4
	}

	if fps == 0 {
		return 0, false
	}

	interval := time.Second / time.Duration(fps)
	elapsed := now.Sub(s.lastFrame)

	if elapsed >= interval {
		s.lastFrame = now
		return 0, true
	}

	return interval - elapsed, true
}

// demandAt returns true if any animation is needed at the given time.
func (s *State) demandAt(now time.Time) bool {
	return s.RouteTransition ||
		now.Before(s.TrafficUntil) ||
		s.Spinner ||
		now.Before(s.ToastUntil)
}

// StartRouteTransition begins a route reconstruction animation.
func (s *State) StartRouteTransition() {
	s.RouteTransition = true
}

// EndRouteTransition stops the route reconstruction animation.
func (s *State) EndRouteTransition() {
	s.RouteTransition = false
}

// StartTraffic starts traffic particles for a duration.
func (s *State) StartTraffic(duration time.Duration) {
	s.TrafficUntil = time.Now().Add(duration)
}
