package tui

import (
	"context"
	"errors"
)

// eventStreamState names every phase of the supervisor event stream. A
// connection attempt is not live until its matching ready result arrives, and
// rebuilding after a replay gap is distinct from either connection phase.
type eventStreamState string

const (
	eventStreamConnecting   eventStreamState = "connecting"
	eventStreamReconnecting eventStreamState = "reconnecting"
	eventStreamResyncing    eventStreamState = "resyncing"
	eventStreamLive         eventStreamState = "live"
)

// eventStreamStates holds state that changes only on the Bubble Tea update
// loop. The generation prevents a result from a superseded attempt from
// replacing state owned by a newer attempt or by shutdown.
type eventStreamStates struct {
	state      eventStreamState
	generation uint64
}

func (s *eventStreamStates) begin(generation uint64) bool {
	if generation <= s.generation {
		return false
	}
	s.generation = generation
	s.state = eventStreamConnecting
	return true
}

func (s *eventStreamStates) reconnect(generation uint64) bool {
	if generation != s.generation || s.state == eventStreamResyncing {
		return false
	}
	s.state = eventStreamReconnecting
	return true
}

func (s *eventStreamStates) resync(generation uint64) bool {
	if generation != s.generation || s.state != eventStreamLive {
		return false
	}
	s.state = eventStreamResyncing
	return true
}

func (s *eventStreamStates) ready(generation uint64) bool {
	if generation != s.generation {
		return false
	}
	if s.state == eventStreamConnecting || s.state == eventStreamReconnecting {
		s.state = eventStreamLive
		return true
	}
	return false
}

func (s *eventStreamStates) live() bool {
	return s.state == eventStreamLive
}

func (s *eventStreamStates) beginResyncRecovery() bool {
	if s.state != eventStreamResyncing {
		return false
	}
	s.generation++
	s.state = eventStreamConnecting
	return true
}

// contextDone distinguishes shutdown cancellation from a supervisor failure.
func contextDone(err error) bool {
	return errors.Is(err, context.Canceled)
}
