package tui

import "testing"

func TestEventStreamStateRequiredTransitions(t *testing.T) {
	s := eventStreamStates{state: eventStreamConnecting}
	if !s.begin(1) {
		t.Fatal("first stream attempt was not accepted")
	}
	if !s.ready(1) {
		t.Fatal("ready result for connecting attempt was not accepted")
	}
	if !s.live() {
		t.Fatalf("state = %q after ready, want live", s.state)
	}
	if s.ready(1) {
		t.Fatal("duplicate ready result was accepted")
	}
	if !s.resync(1) {
		t.Fatal("live stream did not enter resyncing")
	}
	if !s.begin(2) {
		t.Fatal("post-resync snapshot did not begin a new attempt")
	}
	if !s.ready(2) {
		t.Fatal("post-resync ready result was not accepted")
	}
	if !s.live() {
		t.Fatalf("state = %q after post-resync ready, want live", s.state)
	}
}

func TestEventStreamStateFailureAndStaleResults(t *testing.T) {
	s := eventStreamStates{state: eventStreamConnecting, generation: 2}
	if s.ready(1) {
		t.Fatal("stale ready result was accepted")
	}
	if s.state != eventStreamConnecting {
		t.Fatalf("state = %q after stale result, want connecting", s.state)
	}
	if !s.ready(2) {
		t.Fatal("current ready result was not accepted")
	}
	if !s.reconnect(2) {
		t.Fatal("live stream failure did not enter reconnecting")
	}
	if !s.reconnect(2) {
		t.Fatal("repeated connection failure did not schedule another reconnect")
	}
	if s.ready(1) {
		t.Fatal("stale ready result resurrected superseded state")
	}
	if s.state != eventStreamReconnecting {
		t.Fatalf("state = %q after stale ready result, want reconnecting", s.state)
	}
}

func TestEventStreamStateResyncRequiresLiveAndFreshGeneration(t *testing.T) {
	s := eventStreamStates{state: eventStreamResyncing, generation: 2}
	if s.resync(2) {
		t.Fatal("resync was accepted while already resyncing")
	}
	if s.ready(2) {
		t.Fatal("ready result was accepted while resyncing")
	}
	s.state = eventStreamLive
	if s.resync(1) {
		t.Fatal("stale generation caused resync")
	}
	if !s.resync(2) {
		t.Fatal("fresh live stream did not enter resyncing")
	}
}
